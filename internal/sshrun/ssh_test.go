package sshrun

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// A test SSH server on 127.0.0.1 that accepts the listed keys and answers
// every exec with exit status 0.
type testServer struct {
	addr      string
	knownHost string // a known_hosts file naming it
	attempts  atomic.Int32
}

func newKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func startServer(t *testing.T, accept ...ssh.PublicKey) *testServer {
	t.Helper()
	host := newKey(t)
	srv := &testServer{}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		for _, k := range accept {
			if string(k.Marshal()) == string(key.Marshal()) {
				return nil, nil
			}
		}
		return nil, errors.New("key not authorised")
	}}
	cfg.AddHostKey(host)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	srv.addr = l.Addr().String()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			srv.attempts.Add(1)
			go serve(c, cfg)
		}
	}()
	srv.knownHost = filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{knownhosts.Normalize(srv.addr)}, host.PublicKey())
	if err := os.WriteFile(srv.knownHost, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv
}

func serve(c net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for r := range creqs {
				r.Reply(r.Type == "exec", nil)
				if r.Type == "exec" {
					io.Copy(io.Discard, ch)
					status := make([]byte, 4)
					binary.BigEndian.PutUint32(status, 0)
					ch.SendRequest("exit-status", false, status)
					ch.Close()
				}
			}
		}()
	}
}

// refusing is an agent key whose agent will not sign (1Password declining
// an identity: "agent: client error: EOF").
type refusing struct{ ssh.Signer }

func (refusing) Sign(io.Reader, []byte) (*ssh.Signature, error) {
	return nil, errors.New("agent: client error: EOF")
}

func fakeAgent(keys ...identity) func() ([]identity, error) {
	return func() ([]identity, error) { return keys, nil }
}

func writePub(t *testing.T, s ssh.Signer, comment string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "id.pub")
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + " " + comment + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The live case: the agent's first key (the employer's) is accepted for
// the query but its signature fails; Go's client would give up on every
// later key in the same handshake. Each key now gets its own handshake.
func TestAgentKeysTriedOneAtATime(t *testing.T) {
	work, personal := newKey(t), newKey(t)
	srv := startServer(t, work.PublicKey(), personal.PublicKey())
	r := New(srv.addr, "admin")
	r.KnownHosts = srv.knownHost
	r.agentKeys = fakeAgent(identity{name: "work", signer: refusing{work}}, identity{name: "personal", signer: personal})
	var logged []string
	r.Logf = func(f string, a ...any) { logged = append(logged, f) }
	if _, err := r.Run(context.Background(), "true", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Identity != "personal" || srv.attempts.Load() != 2 || len(logged) != 1 {
		t.Errorf("identity %q after %d handshakes, logged %v", r.Identity, srv.attempts.Load(), logged)
	}
}

// A .pub names the agent identity to use, and only that one is offered.
func TestPublicKeyFileSelectsTheAgentKey(t *testing.T) {
	work, personal := newKey(t), newKey(t)
	srv := startServer(t, work.PublicKey(), personal.PublicKey())
	r := New(srv.addr, "admin")
	r.KnownHosts = srv.knownHost
	r.KeyFile = writePub(t, personal, "personal@example.net")
	r.agentKeys = fakeAgent(identity{name: "work", signer: refusing{work}}, identity{name: "personal", signer: personal})
	if _, err := r.Run(context.Background(), "true", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Identity != "personal" || srv.attempts.Load() != 1 {
		t.Errorf("identity %q after %d handshakes; want the selected key, first try", r.Identity, srv.attempts.Load())
	}
}

func TestPublicKeyFileTheAgentLacks(t *testing.T) {
	work, personal := newKey(t), newKey(t)
	srv := startServer(t, personal.PublicKey())
	r := New(srv.addr, "admin")
	r.KnownHosts = srv.knownHost
	r.KeyFile = writePub(t, personal, "personal@example.net")
	r.agentKeys = fakeAgent(identity{name: "work", signer: work})
	_, err := r.Run(context.Background(), "true", "")
	if err == nil || !strings.Contains(err.Error(), "does not hold it") || !strings.Contains(err.Error(), "personal@example.net") {
		t.Errorf("Run = %v; want a clear 'the agent does not hold it'", err)
	}
	if srv.attempts.Load() != 0 {
		t.Error("nothing may be dialled for a key the agent lacks")
	}
}

// Every key refused: the error names each attempt.
func TestAllKeysRefused(t *testing.T) {
	a, b, other := newKey(t), newKey(t), newKey(t)
	srv := startServer(t, other.PublicKey())
	r := New(srv.addr, "admin")
	r.KnownHosts = srv.knownHost
	r.agentKeys = fakeAgent(identity{name: "key-a", signer: a}, identity{name: "key-b", signer: b})
	_, err := r.Run(context.Background(), "true", "")
	if err == nil || !strings.HasPrefix(err.Error(), "ssh handshake admin@") || !strings.Contains(err.Error(), "key-a:") || !strings.Contains(err.Error(), "key-b:") {
		t.Errorf("Run = %v", err)
	}
}
