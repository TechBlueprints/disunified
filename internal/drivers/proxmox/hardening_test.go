package proxmox

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Hardening for three independent writers (docs/drivers/proxmox-on-node.md
// §5.2-§5.4), on the captures as the tests elsewhere edit them.

// quietCollector is a collector on the capture text whose log is kept.
func quietCollector(fixture string) (*Collector, *FixtureRunner, *bytes.Buffer) {
	r := &FixtureRunner{Fixture: fixture}
	c := NewCollector(r)
	c.ManageLLDP = false
	var buf bytes.Buffer
	c.Log = log.New(&buf, "", 0)
	return c, r, &buf
}

// withTags gives guest id's config in the capture these tags.
func withTags(fx string, id, tags string) string {
	r := &FixtureRunner{Fixture: fx}
	r.applyTags("qm", id, tags)
	return r.Fixture
}

// asTemplate marks guest id (on node) a template, as `qm template` would.
func asTemplate(t *testing.T, fx, node, id string) string {
	t.Helper()
	head := "## /etc/pve/nodes/" + node + "/qemu-server/" + id + ".conf\n"
	if !strings.Contains(fx, head) {
		t.Fatalf("no %s in the capture", head)
	}
	return strings.Replace(fx, head, head+"template: 1\n", 1)
}

func poll(t *testing.T, c *Collector, n int) *devicemodel.Snapshot {
	t.Helper()
	var snap *devicemodel.Snapshot
	for i := 0; i < n; i++ {
		var err error
		if snap, err = c.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return snap
}

func TestIgnoredTagsAreLoggedOnce(t *testing.T) {
	fx := loadFixture(t, "collect-node2.txt") // converged: every guest tagged
	// VM 101 lives on proxmox-1: this node only reads its tag. One above
	// the 51 guest ports, then one for node numbering.
	for _, tc := range []struct{ tag, says string }{
		{"unifi.p53.c", "VM 101 on proxmox-1 carries unifi.p53.c, above this node's 51 guest ports"},
		{"unifi.p2.h.proxmox-1", "VM 101 on proxmox-1 carries unifi.p2.h.proxmox-1, a tag for node numbering; this bridge numbers by cluster"},
	} {
		c, r, buf := quietCollector(withTags(fx, "101", tc.tag))
		poll(t, c, 3)
		if n := strings.Count(buf.String(), tc.says); n != 1 {
			t.Errorf("%s: logged %d times over three polls, want once:\n%s", tc.tag, n, buf.String())
		}
		if w := commandsWith(r, "--tags"); len(w) != 0 {
			t.Errorf("%s: a guest on another node was retagged from here: %v", tc.tag, w)
		}
	}
	// On its owner the same tag is logged once and replaced once.
	c, r, buf := quietCollector(withTags(fx, "100", "unifi.p53.c"))
	poll(t, c, 3)
	if n := strings.Count(buf.String(), "VM 100 on proxmox-2 carries unifi.p53.c"); n != 1 {
		t.Errorf("owner logged the ignored tag %d times:\n%s", n, buf.String())
	}
	if w := commandsWith(r, "qm set 100 --tags"); len(w) != 1 || w[0] != "qm set 100 --tags 'unifi.p1.c'" {
		t.Errorf("owner rewrites = %v, want VM 100 back at port 1 once", w)
	}
}

func TestGuestPortsOption(t *testing.T) {
	fx := loadFixture(t, "collect-node2.txt")
	// 40 guest ports: the slots between them and the node's three NICs
	// are empty, the NICs stay at the top, and VM 999 keeps port 25.
	c, r, _ := quietCollector(fx)
	c.GuestPorts = 40
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Ports) != 54 || snap.Ports[24].IfName != "vm999-net0" {
		t.Fatalf("ports=%d, port 25 = %q", len(snap.Ports), snap.Ports[24].IfName)
	}
	for i := 40; i < 51; i++ {
		if p := snap.Ports[i]; p.IfName != "" || p.Enabled {
			t.Errorf("port %d above the guest ports = %+v, want an empty slot", p.Index, p)
		}
	}
	if snap.Ports[53].IfName == "" {
		t.Error("the node's NICs left the top ports")
	}
	if w := commandsWith(r, "--tags"); len(w) != 0 {
		t.Errorf("tags rewritten with every port in range: %v", w)
	}
	// 28, with VM 100 (this node's) tagged for port 30: out of range here,
	// so it is logged and renumbered to the lowest free port.
	c, r, buf := quietCollector(withTags(fx, "100", "unifi.p30.c"))
	c.GuestPorts = 28
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "VM 100 on proxmox-2 carries unifi.p30.c, above this node's 28 guest ports") {
		t.Errorf("no log of the ignored tag:\n%s", buf.String())
	}
	if w := commandsWith(r, "qm set 100 --tags"); len(w) != 1 || w[0] != "qm set 100 --tags 'unifi.p1.c'" {
		t.Errorf("VM 100 not renumbered: %v", w)
	}
	// 52 leaves no room for the node's three NICs below 54: Start fails.
	c, r, _ = quietCollector(fx)
	c.GuestPorts = 52
	if _, err := c.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "guest_ports=52") {
		t.Errorf("Start with guest_ports=52: %v", err)
	}
	if len(r.Commands) != 0 {
		t.Errorf("a failed Start ran %v", r.Commands)
	}
}

func TestOpenGuestPorts(t *testing.T) {
	open := func(opts map[string]string) (*Collector, error) {
		d, err := Driver{}.Open(context.Background(), devicemodel.DriverConfig{SSH: "root@192.0.2.11", Options: opts})
		if err != nil {
			return nil, err
		}
		return d.(*Collector), nil
	}
	c, err := open(nil)
	if err != nil || c.GuestPorts != 0 {
		t.Fatalf("unset: %v, %d", err, c.GuestPorts)
	}
	c, err = open(map[string]string{"guest_ports": "40"})
	if err != nil || c.GuestPorts != 40 {
		t.Fatalf("40: %v", err)
	}
	if c, err = open(map[string]string{"ports": "32", "guest_ports": "28"}); err != nil || c.GuestPorts != 28 || c.Ports != 32 {
		t.Fatalf("ports 32, guest_ports 28: %v", err)
	}
	for _, bad := range []map[string]string{{"guest_ports": "0"}, {"guest_ports": "x"}, {"guest_ports": "54"}, {"ports": "32", "guest_ports": "40"}} {
		if _, err := open(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestFailedTagWriteLoggedOnce(t *testing.T) {
	c, r, buf := quietCollector(loadFixtureRaw(t, "collect-node2.txt"))
	locked := errors.New("can't lock file '/var/lock/qemu-server/lock-100.conf' - got timeout")
	r.Fail = func(cmd string) error {
		if strings.HasPrefix(cmd, "qm set 100 --tags") {
			return locked
		}
		return nil
	}
	poll(t, c, 3)
	if w := commandsWith(r, "qm set 100 --tags"); len(w) != 3 {
		t.Errorf("tried %d times in three polls, want every poll", len(w))
	}
	log := buf.String()
	if n := strings.Count(log, "tagging qemu 100 failed"); n != 1 {
		t.Errorf("failure logged %d times, want once:\n%s", n, log)
	}
	if n := strings.Count(log, ": qm set 100 --tags"); n != 1 {
		t.Errorf("command logged %d times, want once:\n%s", n, log)
	}
	// The others were written on the first poll and are not retried.
	if w := commandsWith(r, "qm set 102 --tags"); len(w) != 1 {
		t.Errorf("VM 102 tagged %d times", len(w))
	}
	r.Fail = nil
	poll(t, c, 2)
	if n := strings.Count(buf.String(), "tagging qemu 100 succeeded"); n != 1 {
		t.Errorf("success logged %d times, want once:\n%s", n, buf.String())
	}
	if w := commandsWith(r, "qm set 100 --tags"); len(w) != 4 {
		t.Errorf("VM 100 tag writes = %d, want 4 (three failed, one that took)", len(w))
	}
}

func TestApplyPortsContinuesPastAFailure(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2.txt")
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p100, p102 := portByIf(t, snap, "vm100-net0").Index, portByIf(t, snap, "vm102-net0").Index
	desired := []devicemodel.PortDesired{
		{Index: p100, Enabled: false, VLANSet: true, NativeVLAN: 1, TaggedAll: true},
		{Index: p102, Enabled: false, VLANSet: true, NativeVLAN: 1, TaggedAll: true},
	}
	r.Fail = func(cmd string) error {
		if strings.HasPrefix(cmd, "qm set 100 --net0") {
			return errors.New("VM is locked (backup)")
		}
		return nil
	}
	n, err := c.ApplyPorts(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("the failed port's error was not returned: n=%d err=%v", n, err)
	}
	if n != 1 || len(commandsWith(r, "qm set 102 --net0")) != 1 {
		t.Fatalf("the port after the failed one was not written: n=%d %v", n, r.Commands)
	}
	// The retry writes only what failed.
	r.Fail, r.Commands = nil, nil
	n, err = c.ApplyPorts(context.Background(), desired)
	if err != nil || n != 1 || len(r.Commands) != 1 || !strings.HasPrefix(r.Commands[0], "qm set 100 --net0") {
		t.Fatalf("retry: n=%d err=%v %v", n, err, r.Commands)
	}
}

// No capture holds a template, so a captured guest is made one, the way
// the other tests edit captures (docs/drivers/proxmox-on-node.md §5.4).
func TestTemplatesLoseOurTags(t *testing.T) {
	fx := loadFixture(t, "collect-node2.txt")
	fx = asTemplate(t, fx, "proxmox-2", "120")
	fx = withTags(fx, "120", "immich;unifi.p25.c;unifi.p3.c.vmbr1;unifi.bogus")
	fx = asTemplate(t, fx, "proxmox-2", "114")
	fx = withTags(fx, "114", "unifi.p15.c")
	fx = asTemplate(t, fx, "proxmox-1", "122") // another node's template
	c, r, _ := quietCollector(fx)
	snap := poll(t, c, 1)
	for _, p := range snap.Ports {
		if p.IfName == "vm120-net0" || p.IfName == "vm114-net0" || p.IfName == "vm122-net0" {
			t.Errorf("template shown as port %d", p.Index)
		}
	}
	if p := snap.Ports[24]; p.IfName != "vm999-net0" {
		t.Errorf("port 25 = %q: a template's copy of the tag displaced VM 999", p.IfName)
	}
	want := []string{"qm set 120 --tags 'immich;unifi.p3.c.vmbr1'", "qm set 114 --delete tags"}
	got := append(commandsWith(r, "--tags"), commandsWith(r, "--delete tags")...)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("template rewrites = %v, want %v", got, want)
	}
	if strings.Contains(strings.Join(r.Commands, "\n"), "122") {
		t.Errorf("another node's template was written: %v", r.Commands)
	}
	r.Commands = nil
	poll(t, c, 1)
	if len(commandsWith(r, "qm set")) != 0 {
		t.Errorf("second poll wrote %v", r.Commands)
	}
}
