package proxmox

import (
	"bytes"
	"context"
	"log"
	osuser "os/user"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// TestOpenTransport: options.transport picks the runner, and Open dials
// nothing either way (docs/drivers/proxmox-on-node.md §4.2).
func TestOpenTransport(t *testing.T) {
	open := func(ssh string, opts map[string]string) (*Collector, error) {
		d, err := Driver{}.Open(context.Background(), devicemodel.DriverConfig{SSH: ssh, Options: opts})
		if err != nil {
			return nil, err
		}
		return d.(*Collector), nil
	}
	me, err := osuser.Current()
	if err != nil {
		t.Fatal(err)
	}

	// Unset and ssh: the SSH runner, built without a dial (192.0.2.0/24
	// answers nothing; a dial would fail or hang here).
	for _, v := range []string{"", "ssh", " SSH "} {
		c, err := open("root@192.0.2.11", map[string]string{"transport": v})
		if err != nil {
			t.Fatalf("transport %q: %v", v, err)
		}
		s, ok := c.r.(*sshrun.SSH)
		if !ok {
			t.Fatalf("transport %q: runner %T, want *sshrun.SSH", v, c.r)
		}
		if s.Addr != "192.0.2.11:22" || s.User != "root" {
			t.Errorf("transport %q: SSH to %s@%s", v, s.User, s.Addr)
		}
	}
	if _, err := open("", nil); err == nil || !strings.Contains(err.Error(), "set ssh:") {
		t.Errorf("ssh without ssh: %v", err)
	}

	// local: no ssh: needed, the local runner.
	c, err := open("", map[string]string{"transport": "local"})
	if err != nil {
		t.Fatalf("local without ssh: %v", err)
	}
	if _, ok := c.r.(*sshrun.Local); !ok {
		t.Fatalf("local: runner %T, want *sshrun.Local", c.r)
	}

	// local with ssh: naming the service's own user: accepted, with one
	// log line for the ignored key options.
	var logs bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&logs)
	c, err = open(me.Username+"@192.0.2.11", map[string]string{"transport": "local", "ssh_key": "/nonexistent.pub", "known_hosts": "/nonexistent"})
	log.SetOutput(saved)
	if err != nil {
		t.Fatalf("local with ssh: %s@: %v", me.Username, err)
	}
	if _, ok := c.r.(*sshrun.Local); !ok {
		t.Fatalf("local: runner %T, want *sshrun.Local", c.r)
	}
	if n := strings.Count(logs.String(), "ignored (nothing is dialled)"); n != 1 || !strings.Contains(logs.String(), "options.ssh_key and options.known_hosts") {
		t.Errorf("ignored-options log (%d lines): %s", n, logs.String())
	}

	// local with ssh: naming another user: refused.
	other := "not-" + me.Username
	if _, err := open(other+"@192.0.2.11", map[string]string{"transport": "local"}); err == nil || !strings.Contains(err.Error(), "the service must run as the login user") {
		t.Errorf("local as another user: %v", err)
	}
	if _, err := open("192.0.2.11", map[string]string{"transport": "local"}); err == nil || !strings.Contains(err.Error(), "user@host") {
		t.Errorf("local with a bare host: %v", err)
	}

	// Anything else: refused.
	if _, err := open("root@192.0.2.11", map[string]string{"transport": "telnet"}); err == nil || !strings.Contains(err.Error(), "transport must be ssh or local") {
		t.Errorf("transport telnet: %v", err)
	}
}
