package aristaeos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSH runs `show ... | json` over a CLI session. It authenticates with the
// running ssh-agent or a private key file, and verifies the host key against
// known_hosts, exactly like a shell `ssh admin@arista` would.
//
// EOS accepts several commands in one session separated by newlines and
// prints their outputs back to back; each `| json` output is one JSON
// document, so a streaming decoder splits them. A command EOS rejects prints
// an echo line and a `% ...` error instead of JSON, which is surfaced as an
// error for the whole batch.
type SSH struct {
	Addr       string // host:port
	User       string
	KeyFile    string // "" = agent, then ~/.ssh/id_ed25519, id_rsa
	KnownHosts string // "" = ~/.ssh/known_hosts
	Timeout    time.Duration

	client *ssh.Client
}

// NewSSH builds an SSH transport. addr may omit the port (22 is assumed).
func NewSSH(addr, user string) *SSH {
	if !strings.Contains(addr, ":") {
		addr += ":22"
	}
	return &SSH{Addr: addr, User: user, Timeout: 30 * time.Second}
}

func (t *SSH) dial(ctx context.Context) (*ssh.Client, error) {
	if t.client != nil {
		return t.client, nil
	}
	home, _ := os.UserHomeDir()
	kh := t.KnownHosts
	if kh == "" {
		kh = filepath.Join(home, ".ssh", "known_hosts")
	}
	hostKey, err := knownhosts.New(kh)
	if err != nil {
		return nil, fmt.Errorf("known_hosts %s: %w", kh, err)
	}

	var auths []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" && t.KeyFile == "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	keyFiles := []string{t.KeyFile}
	if t.KeyFile == "" {
		keyFiles = []string{filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(home, ".ssh", "id_rsa")}
	}
	for _, kf := range keyFiles {
		pem, err := os.ReadFile(kf)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			continue // passphrase-protected: the agent path covers it
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if len(auths) == 0 {
		return nil, errors.New("ssh: no usable key (no agent, no readable unencrypted key file)")
	}

	cfg := &ssh.ClientConfig{
		User:            t.User,
		Auth:            auths,
		HostKeyCallback: hostKey,
		Timeout:         t.Timeout,
	}
	d := net.Dialer{Timeout: t.Timeout}
	conn, err := d.DialContext(ctx, "tcp", t.Addr)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", t.Addr, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, t.Addr, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake %s@%s: %w", t.User, t.Addr, err)
	}
	t.client = ssh.NewClient(c, chans, reqs)
	return t.client, nil
}

// Configure runs cmds verbatim (no `| json`) in one session.
func (t *SSH) Configure(ctx context.Context, cmds []string) error {
	_, err := t.exec(ctx, cmds, false)
	return err
}

// Run executes cmds (each suffixed with `| json`) in one session.
func (t *SSH) Run(ctx context.Context, cmds []string) ([]json.RawMessage, error) {
	out, err := t.exec(ctx, cmds, true)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var results []json.RawMessage
	for _, c := range cmds {
		if isModeCommand(c) {
			results = append(results, json.RawMessage("{}")) // no output, like eAPI
			continue
		}
		if !dec.More() {
			return nil, fmt.Errorf("ssh: no JSON output for %q", c)
		}
		var m json.RawMessage
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("ssh: parsing output of %q: %w", c, err)
		}
		results = append(results, m)
	}
	if dec.More() {
		return nil, fmt.Errorf("ssh: more JSON documents than commands (%d)", len(cmds))
	}
	return results, nil
}

// isModeCommand reports commands that change CLI mode and print nothing.
func isModeCommand(c string) bool {
	switch strings.Fields(c + " x")[0] {
	case "enable", "configure", "end", "exit":
		return true
	}
	return false
}

// exec runs cmds in one session and returns stdout, failing on an EOS
// `% ...` error line.
func (t *SSH) exec(ctx context.Context, cmds []string, jsonOut bool) ([]byte, error) {
	client, err := t.dial(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		// A dead connection surfaces here; drop it so the next call redials.
		t.client.Close()
		t.client = nil
		return nil, fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	var script strings.Builder
	for _, c := range cmds {
		script.WriteString(c)
		if jsonOut && !isModeCommand(c) {
			script.WriteString(" | json")
		}
		script.WriteString("\n")
	}
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(script.String()) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return nil, ctx.Err()
	case err = <-done:
	}
	out := stdout.Bytes()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("ssh run: %w: %s", err, truncate(stderr.String(), 200))
	}
	if i := bytes.Index(out, []byte("\n% ")); i >= 0 || bytes.HasPrefix(out, []byte("% ")) {
		return nil, fmt.Errorf("eos rejected a command: %s", truncate(errorLine(out), 200))
	}
	if err != nil {
		return nil, fmt.Errorf("ssh run: %w: %s", err, truncate(stderr.String(), 200))
	}
	return out, nil
}

// Close drops the connection.
func (t *SSH) Close() error {
	if t.client == nil {
		return nil
	}
	err := t.client.Close()
	t.client = nil
	return err
}

func errorLine(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "% ") {
			return line
		}
	}
	return ""
}
