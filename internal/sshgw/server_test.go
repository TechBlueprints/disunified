package sshgw

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeSwitch is an SSH server that accepts user/pass and answers "exec"
// requests by echoing the command back with a prefix.
func fakeSwitch(t *testing.T, user, pass string) string {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == user && string(pw) == pass {
			return nil, nil
		}
		return nil, ssh.ErrNoAuth
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, _ := nc.Accept()
					go func() {
						for r := range creqs {
							if r.Type == "exec" {
								r.Reply(true, nil)
								ch.Write([]byte("switch> " + string(r.Payload[4:]) + "\n"))
								ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
								ch.Close()
								return
							}
							r.Reply(true, nil)
						}
					}()
				}
				sc.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func TestGatewayProxiesExec(t *testing.T) {
	upAddr := fakeSwitch(t, "stu", "secret")
	gw := &Server{
		Listen:      "127.0.0.1:0",
		HostKeyPath: filepath.Join(t.TempDir(), "hostkey"),
		Upstream:    Upstream{Addr: upAddr, User: "stu", Password: "secret"},
	}
	gw.SetCredentials(Credentials{Users: map[string]string{"admin": "$1$saltsalt$qjXMvbEw8oaL.CzflDtaK/"}})
	// Find a free port for the gateway.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	gw.Listen = ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go gw.Serve(ctx)
	time.Sleep(200 * time.Millisecond)

	// Wrong password is refused.
	_, err := ssh.Dial("tcp", gw.Listen, &ssh.ClientConfig{User: "admin", Auth: []ssh.AuthMethod{ssh.Password("nope")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("wrong password accepted")
	}
	// The controller's device password (hash pushed by the controller) works
	// and the command reaches the switch.
	c, err := ssh.Dial("tcp", gw.Listen, &ssh.ClientConfig{User: "admin", Auth: []ssh.AuthMethod{ssh.Password("password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sess.Stdout = &out
	if err := sess.Run("show version"); err != nil {
		t.Fatalf("run: %v (out %q)", err, out.String())
	}
	if out.String() != "switch> show version\n" {
		t.Errorf("output = %q", out.String())
	}
}
