package aristaeos

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// EAPI is the JSON-RPC 2.0 transport: POST /command-api, method runCmds,
// HTTP basic auth. This envelope has been stable since EOS 4.12; everything
// version-specific is in the per-command output, not here.
type EAPI struct {
	URL      string // https://<switch>/command-api
	Username string
	Password string
	client   *http.Client
	nextID   int
}

// NewEAPI builds an eAPI transport. insecure skips TLS verification, which is
// the normal case for a switch's self-signed certificate.
func NewEAPI(url, username, password string, insecure bool, timeout time.Duration) *EAPI {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// EOS 4.26's HTTPS server negotiates only TLS 1.2 with RSA key exchange
	// and AES-CBC (observed: TLS_RSA_WITH_AES_256_CBC_SHA; ECDHE is refused).
	// Go dropped RSA-kex suites from its defaults in 1.22, so they have to be
	// listed explicitly or the handshake fails with no useful error. Modern
	// ECDHE suites stay in the list for newer switches.
	tr.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: insecure, //nolint:gosec // switch self-signed cert
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		},
	}
	return &EAPI{
		URL:      url,
		Username: username,
		Password: password,
		client:   &http.Client{Transport: tr, Timeout: timeout},
	}
}

type rpcRequest struct {
	JSONRPC string    `json:"jsonrpc"`
	Method  string    `json:"method"`
	Params  rpcParams `json:"params"`
	ID      string    `json:"id"`
}

type rpcParams struct {
	Version int      `json:"version"`
	Cmds    []string `json:"cmds"`
	Format  string   `json:"format"`
}

type rpcResponse struct {
	Result []json.RawMessage `json:"result"`
	Error  *rpcError         `json:"error"`
}

// rpcError is eAPI's failure shape. Data carries the outputs of the commands
// that ran before the failing one, and the failing one's `errors` list.
type rpcError struct {
	Code    int               `json:"code"`
	Message string            `json:"message"`
	Data    []json.RawMessage `json:"data"`
}

func (e *rpcError) Error() string {
	var details []string
	for _, d := range e.Data {
		var v struct {
			Errors []string `json:"errors"`
		}
		if json.Unmarshal(d, &v) == nil && len(v.Errors) > 0 {
			details = append(details, strings.Join(v.Errors, "; "))
		}
	}
	if len(details) == 0 {
		return fmt.Sprintf("eAPI error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("eAPI error %d: %s (%s)", e.Code, e.Message, strings.Join(details, " | "))
}

// Run executes cmds in one request. EOS runs them in order and stops at the
// first failure, which is reported as an error for the whole batch.
func (t *EAPI) Run(ctx context.Context, cmds []string) ([]json.RawMessage, error) {
	return t.run(ctx, cmds, "json")
}

// RunText runs cmds with text output (TextRunner).
func (t *EAPI) RunText(ctx context.Context, cmds []string) ([]string, error) {
	raw, err := t.run(ctx, cmds, "text")
	if err != nil {
		return nil, err
	}
	out := make([]string, len(raw))
	for i, r := range raw {
		var o struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(r, &o); err != nil {
			return nil, fmt.Errorf("eAPI text output of %q: %w", cmds[i], err)
		}
		out[i] = o.Output
	}
	return out, nil
}

func (t *EAPI) run(ctx context.Context, cmds []string, format string) ([]json.RawMessage, error) {
	t.nextID++
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		Method:  "runCmds",
		Params:  rpcParams{Version: 1, Cmds: cmds, Format: format},
		ID:      fmt.Sprintf("stu-%d", t.nextID),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(t.Username, t.Password)

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("eAPI POST %s: %w", t.URL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("eAPI read: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("eAPI %s: HTTP 401 (bad username/password)", t.URL)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("eAPI %s: HTTP %d: %s", t.URL, resp.StatusCode, truncate(string(raw), 200))
	}
	var r rpcResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("eAPI: bad JSON-RPC reply: %w: %s", err, truncate(string(raw), 200))
	}
	if r.Error != nil {
		return nil, r.Error
	}
	if len(r.Result) != len(cmds) {
		return nil, fmt.Errorf("eAPI: sent %d commands, got %d results", len(cmds), len(r.Result))
	}
	return r.Result, nil
}

// Configure runs config-mode commands. eAPI returns an empty object per
// successful command and stops at the first failure, which Run reports.
func (t *EAPI) Configure(ctx context.Context, cmds []string) error {
	_, err := t.Run(ctx, cmds)
	return err
}

// Close is a no-op; eAPI is stateless.
func (t *EAPI) Close() error { return nil }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
