package aristaeos

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

func TestVLANListRoundTrip(t *testing.T) {
	ids, all := parseVLANList("1-2,10,1000")
	if all || len(ids) != 4 || ids[3] != 1000 {
		t.Errorf("parse = %v %v", ids, all)
	}
	if _, all := parseVLANList("ALL"); !all {
		t.Error("ALL not recognised")
	}
	if got := formatVLANList([]int{10, 1, 2, 1000}); got != "1-2,10,1000" {
		t.Errorf("format = %q", got)
	}
	if adminSpeedMbps("100Gbps") != 100000 || adminSpeedMbps("auto") != 0 || adminSpeedMbps("2.5Gbps") != 2500 {
		t.Error("adminSpeedMbps")
	}
}

func TestSwitchportStateFromFixtures(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p2 := portByIndex(t, snap, 2)
	if p2.VLAN.Mode != "trunk" || p2.VLAN.NativeVLAN != 1 || !p2.VLAN.AllowAll || p2.AdminSpeed != 0 {
		t.Errorf("port 2 = vlan %+v speed %d", p2.VLAN, p2.AdminSpeed)
	}
	p49 := portByIndex(t, snap, 49)
	if p49.AdminSpeed != 100000 {
		t.Errorf("port 49 admin speed = %d", p49.AdminSpeed)
	}
	if len(snap.VLANs) < 5 || snap.VLANs[0] != 1 {
		t.Errorf("vlans = %v", snap.VLANs)
	}
}

func TestApplySpeedAndVLANs(t *testing.T) {
	c, ft := startedCollector(t)
	n, err := c.ApplyPorts(context.Background(), []switchmodel.PortDesired{
		{Index: 2, Enabled: true, SpeedMbps: 10000, VLANSet: true, NativeVLAN: 1, TaggedVLANs: []int{2, 10}},
		{Index: 3, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true},  // already trunk ALL native 1: no-op
		{Index: 4, Enabled: true, VLANSet: true, NativeVLAN: 2},                   // block all -> access
		{Index: 49, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true}, // optical, forced 100G, auto requested: leave speed
	})
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := strings.Join(ft.configured[0], "\n")
	for _, want := range []string{
		"interface Ethernet2\nspeed forced 10gfull\nswitchport mode trunk\nno switchport trunk native vlan\nswitchport trunk allowed vlan 1-2,10",
		"interface Ethernet4\nswitchport mode access\nswitchport access vlan 2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing:\n%s\nin:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Ethernet49/1") || strings.Contains(got, "speed auto") {
		t.Errorf("optical uplink must not be touched:\n%s", got)
	}
	// Idempotent on the updated in-memory snapshot.
	n, err = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{
		{Index: 2, Enabled: true, SpeedMbps: 10000, VLANSet: true, NativeVLAN: 1, TaggedVLANs: []int{2, 10}},
	})
	if err != nil || n != 0 {
		t.Errorf("second apply n=%d err=%v", n, err)
	}
}

func TestApplyAutoSpeedOnCopper(t *testing.T) {
	c, ft := startedCollector(t)
	// Pretend port 2 is forced; asking for auto must write "speed auto".
	c.mu.Lock()
	for i := range c.last.Ports {
		if c.last.Ports[i].Index == 2 {
			c.last.Ports[i].AdminSpeed = 10000
		}
	}
	c.mu.Unlock()
	n, err := c.ApplyPorts(context.Background(), []switchmodel.PortDesired{{Index: 2, Enabled: true}})
	if err != nil || n != 1 || !strings.Contains(strings.Join(ft.configured[0], "\n"), "interface Ethernet2\nspeed auto") {
		t.Errorf("n=%d err=%v cmds=%v", n, err, ft.configured)
	}
}

func TestEnsureVLANs(t *testing.T) {
	c, ft := startedCollector(t)
	n, err := c.EnsureVLANs(context.Background(), []int{1, 2, 69, 4000})
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := strings.Join(ft.configured[0], "\n"); got != "enable\nconfigure\nvlan 69,4000\nend\nwrite memory" {
		t.Errorf("cmds:\n%s", got)
	}
	n, _ = c.EnsureVLANs(context.Background(), []int{69})
	if n != 0 {
		t.Error("second ensure must be a no-op")
	}
}

func TestSpeedGoesToLaneOneOnly(t *testing.T) {
	c, ft := startedCollector(t)
	// Port 50 is a 4-lane breakout at 25G. Asking for 100G must write the
	// speed to lane 1 only (that joins the cage), while a VLAN change still
	// reaches every lane that exists.
	n, err := c.ApplyPorts(context.Background(), []switchmodel.PortDesired{
		{Index: 50, Enabled: true, SpeedMbps: 100000, VLANSet: true, NativeVLAN: 2},
	})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := strings.Join(ft.configured[0], "\n")
	if !strings.Contains(got, "interface Ethernet50/1\nspeed forced 100gfull\nswitchport mode access") {
		t.Errorf("lane 1 must carry the speed:\n%s", got)
	}
	if strings.Count(got, "speed forced 100gfull") != 1 {
		t.Errorf("speed must be written once, not per lane:\n%s", got)
	}
	for _, lane := range []string{"Ethernet50/2", "Ethernet50/3", "Ethernet50/4"} {
		if !strings.Contains(got, "interface "+lane+"\nswitchport mode access") {
			t.Errorf("lane %s must still get the VLAN config:\n%s", lane, got)
		}
	}
	// Splitting an unsplit cage: 25G on port 49 goes to Ethernet49/1 alone.
	ft.configured = nil
	n, err = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{{Index: 49, Enabled: true, SpeedMbps: 25000}})
	if err != nil || n != 1 || strings.Join(ft.configured[0], "\n") != "enable\nconfigure\ninterface Ethernet49/1\nspeed forced 25gfull\nend\nwrite memory" {
		t.Errorf("n=%d err=%v cmds=%v", n, err, ft.configured)
	}
	// Changing the lane speed of an already split cage reaches every lane.
	ft.configured = nil
	n, err = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{{Index: 50, Enabled: true, SpeedMbps: 10000}})
	if err != nil || n != 1 || strings.Count(strings.Join(ft.configured[0], "\n"), "speed forced 10gfull") != 4 {
		t.Errorf("lane speed must go to all 4 lanes: n=%d err=%v cmds=%v", n, err, ft.configured)
	}
}
