// Package sshgw is the SSH gateway: an SSH server the controller's device
// terminal can log into with the site's device credentials (the username
// and MD5-crypt password hash, and the SSH keys, that the controller pushes
// in every system_cfg), which proxies the session to the switch as the
// bridge's own user. The switch never sees the controller's credentials and
// no user is created on it; the bridge reports the gateway's address as its
// device IP so the controller connects here.
package sshgw

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Upstream is how the gateway reaches the switch.
type Upstream struct {
	Addr     string // host:port
	User     string
	Password string // password auth; key auth via Signer when set
	Signer   ssh.Signer
}

// Credentials is what the controller currently accepts for the device.
type Credentials struct {
	Users map[string]string // username -> "$1$..." hash
	Keys  []ssh.PublicKey
}

// Server is one gateway instance.
type Server struct {
	Listen      string // e.g. "192.0.2.20:22"
	HostKeyPath string // persisted ed25519 host key (generated on first run)
	Upstream    Upstream
	Logger      *log.Logger

	mu    sync.RWMutex
	creds Credentials
}

// SetCredentials replaces the accepted credentials (called on every push).
func (s *Server) SetCredentials(c Credentials) {
	s.mu.Lock()
	s.creds = c
	s.mu.Unlock()
}

func (s *Server) hostKey() (ssh.Signer, error) {
	if b, err := os.ReadFile(s.HostKeyPath); err == nil {
		return ssh.ParsePrivateKey(b)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.MkdirAll(filepath.Dir(s.HostKeyPath), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.HostKeyPath, pemBytes, 0o600); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// Serve accepts connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if s.Logger == nil {
		s.Logger = log.Default()
	}
	hk, err := s.hostKey()
	if err != nil {
		return fmt.Errorf("ssh gateway host key: %w", err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			s.mu.RLock()
			hash, ok := s.creds.Users[c.User()]
			s.mu.RUnlock()
			if ok && verifyMD5Crypt(string(pw), hash) {
				return nil, nil
			}
			return nil, fmt.Errorf("ssh gateway: password rejected for %q", c.User())
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.mu.RLock()
			defer s.mu.RUnlock()
			for _, k := range s.creds.Keys {
				if k.Type() == key.Type() && string(k.Marshal()) == string(key.Marshal()) {
					return nil, nil
				}
			}
			return nil, fmt.Errorf("ssh gateway: key rejected for %q", c.User())
		},
		ServerVersion: "SSH-2.0-switch-to-unifi",
	}
	cfg.AddHostKey(hk)

	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return fmt.Errorf("ssh gateway listen %s: %w", s.Listen, err)
	}
	s.Logger.Printf("ssh gateway listening on %s, proxying to %s as %s", s.Listen, s.Upstream.Addr, s.Upstream.User)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn, cfg)
	}
}

func (s *Server) handle(ctx context.Context, nc net.Conn, cfg *ssh.ServerConfig) {
	defer nc.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		s.Logger.Printf("ssh gateway: handshake from %s: %v", nc.RemoteAddr(), err)
		return
	}
	defer sconn.Close()
	s.Logger.Printf("ssh gateway: %s logged in as %q from %s", sconn.User(), sconn.User(), nc.RemoteAddr())
	go ssh.DiscardRequests(reqs)

	up, err := s.dialUpstream(ctx)
	if err != nil {
		s.Logger.Printf("ssh gateway: upstream %s: %v", s.Upstream.Addr, err)
		return
	}
	defer up.Close()

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only sessions are proxied")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go s.proxySession(up, ch, chReqs)
	}
}

func (s *Server) dialUpstream(ctx context.Context) (*ssh.Client, error) {
	var auths []ssh.AuthMethod
	if s.Upstream.Signer != nil {
		auths = append(auths, ssh.PublicKeys(s.Upstream.Signer))
	}
	if pw := s.Upstream.Password; pw != "" {
		// Arista EOS (and other PAM-backed sshds) advertise only publickey and
		// keyboard-interactive; plain "password" is never offered. Answer every
		// keyboard-interactive prompt with the password as well.
		auths = append(auths, ssh.Password(pw), ssh.KeyboardInteractive(
			func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = pw
				}
				return answers, nil
			}))
	}
	if len(auths) == 0 {
		return nil, errors.New("no upstream credentials")
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	c, err := d.DialContext(ctx, "tcp", s.Upstream.Addr)
	if err != nil {
		return nil, err
	}
	cc, chans, reqs, err := ssh.NewClientConn(c, s.Upstream.Addr, &ssh.ClientConfig{
		User: s.Upstream.User, Auth: auths, Timeout: 15 * time.Second,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // the switch is on the LAN; its host key is not pinned here (TODO: known_hosts)
	})
	if err != nil {
		c.Close()
		return nil, err
	}
	return ssh.NewClient(cc, chans, reqs), nil
}

// proxySession forwards one session channel: requests (pty, env, shell,
// exec, window-change, signal) go upstream; bytes flow both ways; the
// upstream exit status is relayed back.
func (s *Server) proxySession(up *ssh.Client, down ssh.Channel, downReqs <-chan *ssh.Request) {
	defer down.Close()
	upCh, upReqs, err := up.OpenChannel("session", nil)
	if err != nil {
		fmt.Fprintf(down.Stderr(), "switch-to-unifi: upstream session: %v\r\n", err)
		return
	}
	defer upCh.Close()

	// Requests from the controller's client -> the switch.
	go func() {
		for r := range downReqs {
			ok, err := upCh.SendRequest(r.Type, r.WantReply, r.Payload)
			if err != nil {
				ok = false
			}
			if r.WantReply {
				r.Reply(ok, nil)
			}
		}
	}()
	// Requests from the switch (exit-status, exit-signal) -> the client.
	go func() {
		for r := range upReqs {
			ok, _ := down.SendRequest(r.Type, r.WantReply, r.Payload)
			if r.WantReply {
				r.Reply(ok, nil)
			}
		}
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(upCh, down); upCh.CloseWrite() }()
	go func() { defer wg.Done(); io.Copy(down, upCh); io.Copy(down.Stderr(), upCh.Stderr()) }()
	wg.Wait()
}

// ParseAuthorizedKey parses "<type> <base64> [comment]".
func ParseAuthorizedKey(line string) (ssh.PublicKey, error) {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	return k, err
}
