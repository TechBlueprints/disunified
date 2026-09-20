package proxmox

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

// Runner executes a command on the node. Run returns stdout; a non-zero
// exit is an error carrying stderr.
type Runner interface {
	Run(ctx context.Context, command string, stdin string) (string, error)
	Close() error
}

// SSH is the Runner for a Proxmox node: key auth via ssh-agent or a key
// file, host key from known_hosts, one multiplexed connection reused across
// polls (redialled when it drops).
type SSH struct {
	Addr       string // host:port
	User       string
	KeyFile    string // "" = agent, then ~/.ssh/id_ed25519, id_rsa
	KnownHosts string // "" = ~/.ssh/known_hosts
	Timeout    time.Duration

	mu     sync.Mutex
	client *ssh.Client
}

// NewSSH builds an SSH runner; addr may omit the port (22).
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
			continue
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if len(auths) == 0 {
		return nil, errors.New("ssh: no usable key (no agent, no readable unencrypted key file)")
	}
	cfg := &ssh.ClientConfig{User: t.User, Auth: auths, HostKeyCallback: hostKey, Timeout: t.Timeout}
	// Offer only the key types known_hosts has for this host, as OpenSSH
	// does: otherwise the server may present a key of another type and the
	// known_hosts check fails with "key mismatch" even though the host is known.
	if algos := knownKeyTypes(kh, t.Addr); len(algos) > 0 {
		cfg.HostKeyAlgorithms = algos
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
		return stdout.String(), fmt.Errorf("%s: %w: %s", firstWords(command, 6), err, truncate(strings.TrimSpace(stderr.String()), 300))
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

func truncate(s string, n int) string {
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
