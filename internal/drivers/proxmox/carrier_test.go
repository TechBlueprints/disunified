package proxmox

import (
	"context"
	"strings"
	"testing"
)

// The kernel's carrier counters, read from proxmox-1 with the 2026-11-15
// collect script (collect-node1-carrier.txt). The node had been up 19.5
// days: the mlx5 uplink NICs register with the carrier off, so their first
// counted change is a rise (13 changes = 7 up + 6 down, now up: 6 drops);
// the bond, the bridge and the firewall bridge's veths turn their carrier
// off after registering, so each counts a drop before its first link (2
// changes = 1 up + 1 down while up: 0 drops, where carrier_changes / 2 said
// 1); eno1 is admin down, never linked (1 down, carrier unreadable); a tap
// counts nothing.
func TestCarrierCountsAreDrops(t *testing.T) {
	carrier := keyValues(sections(loadFixtureRaw(t, "collect-node1-carrier.txt"))["carrier"])
	drops := map[string]uint64{
		"ens1f0np0": 6, "ens1f1np1": 3, "eno1": 0, "bond0": 0, "vmbr0": 0,
		"fwpr125p0": 0, "fwln125i0": 0, "tap119i0": 0, "lo": 0,
	}
	for name, want := range drops {
		if _, ok := carrier[name]; !ok {
			t.Fatalf("%s not in the capture's carrier section", name)
		}
		if got := carrierChanges(carrier, name) / 2; got != want {
			t.Errorf("%s (%q): %d drops, want %d", name, carrier[name], got, want)
		}
	}
	// The same counts as carrier_changes alone (an older collect script)
	// keep the old derivation: the veth's extra drop comes back.
	if got := carrierChanges(map[string]string{"fwpr125p0": "2"}, "fwpr125p0") / 2; got != 1 {
		t.Errorf("one-field line = %d drops, want carrier_changes / 2 = 1", got)
	}
	// A kernel without carrier_up_count (before 4.16) prints "-".
	if got := carrierChanges(map[string]string{"x": "5 - - 1"}, "x"); got != 5 {
		t.Errorf("no up count = %d, want carrier_changes 5", got)
	}
}

// The ports' link_down_count source: the uplink's active and standby NICs
// and the unattached eno1, and every guest port, from the capture.
func TestCarrierCountsOnPorts(t *testing.T) {
	c, _ := newTestCollector(t, "collect-node1-carrier.txt")
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for ifname, want := range map[string]uint64{"bond0-1": 6, "bond0-2": 3, "eno1": 0} {
		p := portByIf(t, snap, ifname)
		if got := p.Health.LinkChanges / 2; got != want {
			t.Errorf("%s (%v): %d drops, want %d", ifname, p.Interfaces, got, want)
		}
	}
	guests := 0
	for _, p := range snap.Ports {
		if len(p.Interfaces) == 1 && strings.HasPrefix(p.Interfaces[0], "tap") {
			guests++
			if p.Health.LinkChanges/2 != 0 {
				t.Errorf("guest port %d (%s): %d drops, want 0", p.Index, p.Interfaces[0], p.Health.LinkChanges/2)
			}
		}
	}
	if guests == 0 {
		t.Errorf("no guest port with a tap in the capture")
	}
}
