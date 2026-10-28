// Package sshrun runs commands on a Linux host over SSH for the drivers
// that read a host through its shell (Proxmox nodes, Podman hosts): key
// auth via ssh-agent or a key file, host key from known_hosts, one
// multiplexed connection reused across polls.
package sshrun

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Runner executes a command on the host. Run returns stdout; a non-zero
// exit is an error carrying stderr. Drivers take a Runner so tests can
// serve captures instead of a host.
type Runner interface {
	Run(ctx context.Context, command string, stdin string) (string, error)
	Close() error
}

// SSH is the Runner for a host: key auth via ssh-agent or a key
// file, host key from known_hosts, one multiplexed connection reused across
// polls (redialled when it drops).
type SSH struct {
	Addr string // host:port
	User string
	// KeyFile picks the identity:
	//   ""            every ssh-agent key, one at a time in agent order, then
	//                 ~/.ssh/id_ed25519 and id_rsa
	//   a public key  (".pub", or an authorized_keys line) the agent key with
	//                 that public key and no other (OpenSSH's IdentitiesOnly);
	//                 an error if the agent does not hold it
	//   a private key that key (unencrypted), without the agent
	KeyFile    string
	KnownHosts string // "" = ~/.ssh/known_hosts
	Timeout    time.Duration
	// Logf, if set, is told once per dial which key authenticated.
	Logf func(format string, args ...any)
	// Identity is the key the current connection authenticated with
	// ("<type> <SHA256 fingerprint> <comment>"), "" before the first dial.
	Identity string

	mu     sync.Mutex
	client *ssh.Client
	// agentKeys lists the agent's identities; nil = the agent at
	// SSH_AUTH_SOCK (tests substitute their own).
	agentKeys func() ([]identity, error)
}

// identity is one key the dial may authenticate with.
type identity struct {
	name   string
	signer ssh.Signer
}

func keyName(pub ssh.PublicKey, comment string) string {
	return strings.TrimSpace(pub.Type() + " " + ssh.FingerprintSHA256(pub) + " " + comment)
}

// sockAgentKeys lists the identities of the agent at SSH_AUTH_SOCK, in
// agent order, named with their comments; nil when there is no agent.
func sockAgentKeys() ([]identity, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil
	}
	ag := agent.NewClient(conn)
	signers, err := ag.Signers()
	if err != nil {
		return nil, fmt.Errorf("ssh agent: %w", err)
	}
	comments := map[string]string{}
	if keys, err := ag.List(); err == nil {
		for _, k := range keys {
			comments[string(k.Blob)] = k.Comment
		}
	}
	out := make([]identity, 0, len(signers))
	for _, s := range signers {
		out = append(out, identity{name: keyName(s.PublicKey(), comments[string(s.PublicKey().Marshal())]), signer: s})
	}
	return out, nil
}

// identities are the keys to try, each in a handshake of its own: Go's
// client gives up on every remaining key once one agent key fails to sign
// ("agent: client error: EOF" from an agent that refuses it), and it
// treats a second publickey method in the same handshake as already
// tried, so one key per handshake is the only way past a key that fails.
func (t *SSH) identities(home string) ([]identity, error) {
	listAgent := t.agentKeys
	if listAgent == nil {
		listAgent = sockAgentKeys
	}
	if t.KeyFile != "" {
		b, err := os.ReadFile(t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("ssh key %s: %w", t.KeyFile, err)
		}
		if pub, comment, _, _, err := ssh.ParseAuthorizedKey(b); err == nil {
			// A public key selects the agent identity that holds it.
			keys, err := listAgent()
			if err != nil {
				return nil, err
			}
			for _, k := range keys {
				if string(k.signer.PublicKey().Marshal()) == string(pub.Marshal()) {
					return []identity{k}, nil
				}
			}
			return nil, fmt.Errorf("ssh key %s (%s): the ssh agent (SSH_AUTH_SOCK) does not hold it", t.KeyFile, keyName(pub, comment))
		} else if strings.HasSuffix(t.KeyFile, ".pub") {
			return nil, fmt.Errorf("ssh key %s: not a public key line: %w", t.KeyFile, err)
		}
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("ssh key %s: %w", t.KeyFile, err)
		}
		return []identity{{name: keyName(signer.PublicKey(), t.KeyFile), signer: signer}}, nil
	}
	// An agent that is there but fails (a locked one) is not the end: the
	// key files are tried after it, as OpenSSH does.
	keys, agentErr := listAgent()
	if agentErr != nil {
		keys = nil
		if t.Logf != nil {
			t.Logf("ssh %s@%s: %v; trying the key files", t.User, t.Addr, agentErr)
		}
	}
	for _, kf := range []string{filepath.Join(home, ".ssh", "id_ed25519"), filepath.Join(home, ".ssh", "id_rsa")} {
		pem, err := os.ReadFile(kf)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			continue
		}
		keys = append(keys, identity{name: keyName(signer.PublicKey(), kf), signer: signer})
	}
	if len(keys) == 0 && agentErr != nil {
		return nil, agentErr
	}
	return keys, nil
}

// NewSSH builds an SSH runner; addr may omit the port (22).
func New(addr, user string) *SSH {
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
	ids, err := t.identities(home)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, errors.New("ssh: no usable key (no agent, no readable unencrypted key file)")
	}
	cfg := &ssh.ClientConfig{User: t.User, HostKeyCallback: hostKey, Timeout: t.Timeout}
	// Offer only the key types known_hosts has for this host, as OpenSSH
	// does: otherwise the server may present a key of another type and the
	// known_hosts check fails with "key mismatch" even though the host is known.
	if algos := knownKeyTypes(kh, t.Addr); len(algos) > 0 {
		var offer []string
		for _, a := range algos {
			if a == ssh.KeyAlgoRSA {
				offer = append(offer, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
			}
			offer = append(offer, a)
		}
		cfg.HostKeyAlgorithms = offer
	}
	d := net.Dialer{Timeout: t.Timeout}
	var fails []string
	for _, id := range ids {
		conn, err := d.DialContext(ctx, "tcp", t.Addr)
		if err != nil {
			return nil, fmt.Errorf("ssh dial %s: %w", t.Addr, err)
		}
		try := *cfg
		try.Auth = []ssh.AuthMethod{ssh.PublicKeys(id.signer)}
		c, chans, reqs, err := ssh.NewClientConn(conn, t.Addr, &try)
		if err != nil {
			conn.Close()
			fails = append(fails, fmt.Sprintf("%s: %v", id.name, err))
			var ke *knownhosts.KeyError
			if errors.As(err, &ke) {
				break // the host itself is wrong; no key will fix that
			}
			continue
		}
		t.client = ssh.NewClient(c, chans, reqs)
		t.Identity = id.name
		if t.Logf != nil {
			t.Logf("ssh %s@%s: authenticated with %s", t.User, t.Addr, id.name)
		}
		return t.client, nil
	}
	return nil, fmt.Errorf("ssh handshake %s@%s: %s", t.User, t.Addr, strings.Join(fails, "; "))
}

// Run executes command with stdin on the node and returns stdout.
func (t *SSH) Run(ctx context.Context, command string, stdin string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	client, err := t.dial(ctx)
	if err != nil {
		return "", err
	}
	sess, err := client.NewSession()
	if err != nil {
		t.client.Close()
		t.client = nil
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdin = strings.NewReader(stdin)
	sess.Stdout, sess.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	case err = <-done:
	}
	if err != nil {
		return stdout.String(), fmt.Errorf("%s: %w: %s", firstWords(command, 6), err, Truncate(strings.TrimSpace(stderr.String()), 300))
	}
	return stdout.String(), nil
}

// Close drops the connection.
func (t *SSH) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client == nil {
		return nil
	}
	err := t.client.Close()
	t.client = nil
	return err
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = append(f[:n], "...")
	}
	return strings.Join(f, " ")
}

// Truncate shortens s to n bytes with an ellipsis, for log lines.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// knownKeyTypes lists the key types known_hosts holds for addr (host:port),
// matching plain, "[host]:port" and hashed (|1|salt|hash) patterns.
func knownKeyTypes(khFile, addr string) []string {
	f, err := os.Open(khFile)
	if err != nil {
		return nil
	}
	defer f.Close()
	host, port, _ := net.SplitHostPort(addr)
	candidates := []string{host}
	if port != "" && port != "22" {
		candidates = []string{"[" + host + "]:" + port}
	}
	if ips, err := net.LookupHost(host); err == nil {
		for _, ip := range ips {
			if port == "" || port == "22" {
				candidates = append(candidates, ip)
			} else {
				candidates = append(candidates, "["+ip+"]:"+port)
			}
		}
	}
	var types []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "@") {
			continue
		}
		if !patternMatches(fields[0], candidates) || seen[fields[1]] {
			continue
		}
		seen[fields[1]] = true
		types = append(types, fields[1])
	}
	return types
}

func patternMatches(pattern string, hosts []string) bool {
	for _, p := range strings.Split(pattern, ",") {
		if strings.HasPrefix(p, "|1|") {
			parts := strings.Split(p, "|")
			if len(parts) != 4 {
				continue
			}
			salt, err1 := base64.StdEncoding.DecodeString(parts[2])
			want, err2 := base64.StdEncoding.DecodeString(parts[3])
			if err1 != nil || err2 != nil {
				continue
			}
			for _, h := range hosts {
				mac := hmac.New(sha1.New, salt)
				mac.Write([]byte(h))
				if hmac.Equal(mac.Sum(nil), want) {
					return true
				}
			}
			continue
		}
		for _, h := range hosts {
			if p == h {
				return true
			}
		}
	}
	return false
}

// ShellQuote single-quotes s for a POSIX shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
