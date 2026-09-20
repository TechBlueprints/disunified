package proxmox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// The aggregate the controller would push for the node's two physical
// ports (bond0-1 at 54, bond0-2 at 53), as the loop translates it.
func lagDesired(lag int) []switchmodel.PortDesired {
	return []switchmodel.PortDesired{
		{Index: 54, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true, LAG: lag},
		{Index: 53, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true, LAG: lag},
	}
}

// sameSwitchFixture rewrites the capture so both NICs see the same LLDP
// chassis (the only topology a LAG is valid on). In the capture only the
// active NIC has a neighbour (lldpd announces on it alone; the standby's
// link goes to another switch), so the active NIC's neighbour entry is
// copied onto the standby.
func sameSwitchFixture(t *testing.T, fixture string) string {
	t.Helper()
	sec := sections(fixture)
	var l lldpJSON0
	if err := decodeJSON("lldp", sec["lldp"], &l); err != nil || len(l.LLDP) == 0 || len(l.LLDP[0].Interface) == 0 {
		t.Fatalf("fixture lldp: %v", err)
	}
	src := l.LLDP[0].Interface[0]
	if src.Name != "ens1f0np0" {
		t.Fatalf("fixture lldp interface = %s", src.Name)
	}
	dup := src
	dup.Name = "ens1f1np1"
	l.LLDP[0].Interface = append(l.LLDP[0].Interface, dup)
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(fixture, "@@@ lldp\n"+sec["lldp"]+"\n", "@@@ lldp\n"+string(b)+"\n", 1)
}

func TestLAGRefusedAcrossSwitches(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2.txt")
	var buf strings.Builder
	c.Log.SetOutput(&buf)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := c.ApplyPorts(context.Background(), lagDesired(1))
	if err != nil || n != 0 || len(r.Commands) != 0 {
		t.Fatalf("a LAG across two switches was applied: n=%d err=%v cmds=%v", n, err, r.Commands)
	}
	if !strings.Contains(buf.String(), "refused") || !strings.Contains(buf.String(), "different switches") {
		t.Errorf("log = %q", buf.String())
	}
}

func TestLAGConvertsTheBondToLACP(t *testing.T) {
	r := &FixtureRunner{Fixture: sameSwitchFixture(t, loadFixture(t, "collect-node2.txt"))}
	c := NewCollector(r)
	c.ManageLLDP = false
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := c.ApplyPorts(context.Background(), lagDesired(1))
	if err != nil || n != 1 {
		t.Fatalf("convert: n=%d err=%v cmds=%v", n, err, r.Commands)
	}
	want := []string{
		"pvesh set /nodes/proxmox-2/network/bond0 --bond_mode 802.3ad --bond_xmit_hash_policy layer2+3",
		"pvesh set /nodes/proxmox-2/network",
	}
	if strings.Join(r.Commands, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(r.Commands, "\n"), strings.Join(want, "\n"))
	}
	// Idempotent within the run, and the reverse once the aggregate is gone.
	n, _ = c.ApplyPorts(context.Background(), lagDesired(1))
	if n != 0 || len(r.Commands) != 2 {
		t.Errorf("second apply wrote %v", r.Commands[2:])
	}
	n, err = c.ApplyPorts(context.Background(), lagDesired(0))
	if err != nil || n != 1 || !strings.Contains(r.Commands[2], "--bond_mode active-backup --bond-primary ens1f0np0") || r.Commands[3] != "pvesh set /nodes/proxmox-2/network" {
		t.Errorf("revert: n=%d err=%v cmds=%v", n, err, r.Commands[2:])
	}
}

func TestLAGOnLACPBondIsAlreadyDone(t *testing.T) {
	fixture := strings.Replace(sameSwitchFixture(t, loadFixture(t, "collect-node2.txt")), "Bonding Mode: fault-tolerance (active-backup)", "Bonding Mode: IEEE 802.3ad Dynamic link aggregation", 1)
	r := &FixtureRunner{Fixture: fixture}
	c := NewCollector(r)
	c.ManageLLDP = false
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Ports[53].LAG != "bond0" || snap.Ports[52].LAG != "bond0" {
		t.Errorf("LACP bond not reported as a LAG: %+v %+v", snap.Ports[53].LAG, snap.Ports[52].LAG)
	}
	n, err := c.ApplyPorts(context.Background(), lagDesired(1))
	if err != nil || n != 0 || len(r.Commands) != 0 {
		t.Errorf("an existing LACP bond was rewritten: n=%d err=%v cmds=%v", n, err, r.Commands)
	}
}

func TestLAGGuardsGuestAndPartial(t *testing.T) {
	r := &FixtureRunner{Fixture: sameSwitchFixture(t, loadFixture(t, "collect-node2.txt"))}
	c := NewCollector(r)
	c.ManageLLDP = false
	var buf strings.Builder
	c.Log.SetOutput(&buf)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A guest port in the aggregate.
	d := append(lagDesired(1), switchmodel.PortDesired{Index: 1, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true, LAG: 1})
	if n, _ := c.ApplyPorts(context.Background(), d); n != 0 || len(r.Commands) != 0 || !strings.Contains(buf.String(), "guest port cannot be aggregated") {
		t.Errorf("guest in LAG: n=%d cmds=%v log=%q", n, r.Commands, buf.String())
	}
	// Only one of the bond's slaves.
	if n, _ := c.ApplyPorts(context.Background(), lagDesired(1)[:1]); n != 0 || len(r.Commands) != 0 || !strings.Contains(buf.String(), "has one member") {
		t.Errorf("partial LAG: n=%d cmds=%v", n, r.Commands)
	}
}
