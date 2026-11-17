package proxmox

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// withSection replaces the body of one "@@@ <name>" section of a capture.
func withSection(t *testing.T, fx, name, body string) string {
	t.Helper()
	head := "@@@ " + name + "\n"
	i := strings.Index(fx, head)
	if i < 0 {
		t.Fatalf("capture has no section %q", name)
	}
	j := strings.Index(fx[i+len(head):], "@@@ ")
	if j < 0 {
		t.Fatalf("section %q is the last one", name)
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return fx[:i+len(head)] + body + fx[i+len(head)+j:]
}

// withoutSection removes one "@@@ <name>" section, header and body.
func withoutSection(t *testing.T, fx, name string) string {
	t.Helper()
	head := "@@@ " + name + "\n"
	i := strings.Index(fx, head)
	if i < 0 {
		t.Fatalf("capture has no section %q", name)
	}
	j := strings.Index(fx[i+len(head):], "@@@ ")
	return fx[:i] + fx[i+len(head)+j:]
}

// An unmounted /etc/pve (pmxcfs not running) leaves the capture's vmlist,
// qemu and lxc sections empty: the script's cat and globs find nothing.
// That is not a cluster with no guests: Start fails, and nothing is written.
func TestUnmountedPVEFailsStart(t *testing.T) {
	for _, name := range []string{"collect-node1.txt", "collect-node2.txt"} {
		fx := loadFixtureRaw(t, name)
		for _, s := range []string{"vmlist", "qemu", "lxc"} {
			fx = withSection(t, fx, s, "")
		}
		r := &FixtureRunner{Fixture: fx}
		_, err := NewCollector(r).Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "pmxcfs") {
			t.Errorf("%s: Start with /etc/pve unmounted: err = %v, want the pmxcfs error", name, err)
		}
		if len(r.Commands) != 0 {
			t.Errorf("%s: commands run from a failed collect: %v", name, r.Commands)
		}
	}
	// Each of the three sections is required.
	for _, s := range []string{"vmlist", "qemu", "lxc"} {
		r := &FixtureRunner{Fixture: withoutSection(t, loadFixture(t, "collect-node2.txt"), s)}
		if _, err := NewCollector(r).Start(context.Background()); err == nil || !strings.Contains(err.Error(), s) {
			t.Errorf("Start without the %s section: err = %v", s, err)
		}
	}
	// A mounted pmxcfs with no guests at all is a cluster with no guests:
	// .vmlist is there with no ids, and every guest slot is free.
	fx := loadFixture(t, "collect-node2.txt")
	// Every entry is dropped; the brace that closes "ids" (on the last
	// entry's line) stays.
	fx = withSection(t, fx, "vmlist", regexp.MustCompile(`"\d+": \{[^}]*\},?`).ReplaceAllString(sections(fx)["vmlist"], ""))
	fx = withSection(t, fx, "qemu", "")
	snap, err := NewCollector(&FixtureRunner{Fixture: fx}).Start(context.Background())
	if err != nil {
		t.Fatalf("a cluster with no guests: %v", err)
	}
	for _, p := range snap.Ports[:48] {
		if p.IfName != "" {
			t.Errorf("a cluster with no guests shows port %d as %q", p.Index, p.IfName)
		}
	}
}

// .vmlist and the config files the globs read list the same guests on the
// same nodes; a poll that sees them disagree fails (a guest created,
// destroyed or moved between the two reads).
func TestVMListCrossCheck(t *testing.T) {
	for _, name := range []string{"collect-node1.txt", "collect-node2.txt", "collect-node1-carrier.txt", "collect-node2-nft.txt"} {
		if _, err := NewCollector(&FixtureRunner{Fixture: loadFixture(t, name)}).Start(context.Background()); err != nil {
			t.Errorf("%s as captured: %v", name, err)
		}
	}
	fx := loadFixture(t, "collect-node2.txt")

	// A config header (VM 100's) gone from the qemu section.
	conf := regexp.MustCompile(`(?s)## /etc/pve/nodes/[^/\n]+/qemu-server/100\.conf\n.*?(\n## |\n@@@ )`)
	noConf := conf.ReplaceAllString(fx, "$1")
	if noConf == fx {
		t.Fatal("VM 100's config not found in the capture")
	}
	noConf = strings.Replace(noConf, "\n\n## ", "\n## ", 1)
	r := &FixtureRunner{Fixture: noConf}
	if _, err := NewCollector(r).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "100") {
		t.Errorf("VM 100 in .vmlist with no config: err = %v, want one naming 100", err)
	}
	if len(r.Commands) != 0 {
		t.Errorf("commands run from a failed collect: %v", r.Commands)
	}

	// VM 100's .vmlist entry gone.
	entry := regexp.MustCompile(`(?m)^"100": \{[^\n]*\n`)
	vml := sections(fx)["vmlist"]
	if !entry.MatchString(vml) {
		t.Fatal("VM 100 not in the capture's .vmlist")
	}
	r = &FixtureRunner{Fixture: withSection(t, fx, "vmlist", entry.ReplaceAllString(vml, ""))}
	if _, err := NewCollector(r).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "100") {
		t.Errorf("VM 100's config with no .vmlist entry: err = %v, want one naming 100", err)
	}

	// VM 100 listed on another node than the one its config is under.
	r = &FixtureRunner{Fixture: withSection(t, fx, "vmlist", strings.Replace(vml, `"100": { "node": "proxmox-2"`, `"100": { "node": "proxmox-3"`, 1))}
	if _, err := NewCollector(r).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "100") {
		t.Errorf("VM 100 on two nodes: err = %v, want one naming 100", err)
	}
}

// The script prints the kernel hostname, which may carry a domain; the
// node's name in /etc/pve/nodes/<name> is its first label. A domain on
// the hostname leaves the node's own guests local: shown live, tagged by
// this node and written by it.
func TestHostnameWithDomain(t *testing.T) {
	withDomain := func(fx string) string {
		return withSection(t, fx, "hostname", "proxmox-2.example.net")
	}

	// Read: VM 100 lives on this node and is live on port 1.
	c, r := newTestCollector(t, "collect-node2.txt")
	r.Fixture = withDomain(r.Fixture)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := snap.Ports[0]; p.IfName != "vm100-net0" || !p.Up || len(p.Interfaces) == 0 || len(p.MACs) == 0 {
		t.Errorf("VM 100 (on this node) with a domain on the hostname: %+v", p)
	}
	if snap.System.Hostname != "proxmox-2.example.net" {
		t.Errorf("reported hostname = %q, want the node's own", snap.System.Hostname)
	}

	// Write: a port write to the node's own guest goes through.
	d := devicemodel.PortDesired{Index: 1, Enabled: false, VLANSet: true, NativeVLAN: 1, TaggedAll: true}
	if n, err := c.ApplyPorts(context.Background(), []devicemodel.PortDesired{d}); err != nil || n != 1 || len(commandsWith(r, "qm set 100 --net0")) != 1 {
		t.Errorf("apply to VM 100: n=%d err=%v cmds=%v", n, err, r.Commands)
	}

	// Tags: the untagged capture gets the same tag writes as without the domain.
	plain := &FixtureRunner{Fixture: loadFixtureRaw(t, "collect-node2.txt")}
	if _, err := NewCollector(plain).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	dom := &FixtureRunner{Fixture: withDomain(loadFixtureRaw(t, "collect-node2.txt"))}
	if _, err := NewCollector(dom).Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	pt, dt := commandsWith(plain, "--tags"), commandsWith(dom, "--tags")
	if len(pt) == 0 || strings.Join(pt, "\n") != strings.Join(dt, "\n") {
		t.Errorf("tag writes with a domain on the hostname:\n%v\nwant\n%v", dt, pt)
	}
}
