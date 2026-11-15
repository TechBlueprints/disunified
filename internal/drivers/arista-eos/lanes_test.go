package aristaeos

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The Arista as a lane-switch parent (docs/drivers/breakout.md §7b), on the
// captures of 2026-11-15: the empty test cage 54 split to 4x25G
// (split-cage54/), the same with lanes 3-4 in channel group 1541 and a
// session 1542 mirroring lane 1 onto lane 2 (split-cage54-lag/), and the
// first try at the split, whose lanes 2-4 were left at 10G and errdisabled
// (split-cage54-diverged/, overrides on split-cage54).

const childMAC = "02:00:00:00:00:aa"

func splitCollector(t *testing.T, dir string, attach bool) (*Collector, *FixtureTransport, *strings.Builder) {
	t.Helper()
	ft := NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M", dir))
	c := NewCollector(ft)
	buf := &strings.Builder{}
	c.Log = log.New(buf, "", 0)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attach {
		if err := c.AttachLaneSwitch(54, devicemodel.Attachment{MAC: childMAC, Name: "lane-switch"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return c, ft, buf
}

func lanePorts(t *testing.T, c *Collector, cage int) devicemodel.LaneView {
	t.Helper()
	v, err := c.Lanes(cage)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// desiredFrom is the lanes (or ports) as they are, as a push would carry them.
func desiredFrom(ports []devicemodel.Port) []devicemodel.PortDesired {
	var out []devicemodel.PortDesired
	for _, p := range ports {
		d := devicemodel.PortDesired{Index: p.Index, Enabled: p.Enabled, Description: p.Description, StormCtrl: p.StormCtrl,
			BPDUGuard: p.BPDUGuard, STPEdge: p.STPEdge, LAG: p.LAGID, MirrorSource: p.MirrorFrom}
		if p.FECConfig != "" {
			f := p.FECConfig
			d.FEC = &f
		}
		out = append(out, d)
	}
	return out
}

func lastBatch(t *testing.T, ft *FixtureTransport) string {
	t.Helper()
	if len(ft.Configured) == 0 {
		t.Fatal("nothing configured")
	}
	return strings.Join(ft.Configured[len(ft.Configured)-1], " | ")
}

func TestLanesOfASplitCage(t *testing.T) {
	c, _, _ := splitCollector(t, "split-cage54", true)
	v := lanePorts(t, c, 54)
	if len(v.Lanes) != 4 || v.Mode != "4x25G" || v.Child.MAC != childMAC || v.CageDisabled {
		t.Fatalf("view: %d lanes, mode %q, child %+v, disabled %v", len(v.Lanes), v.Mode, v.Child, v.CageDisabled)
	}
	for i, l := range v.Lanes {
		name := "Ethernet54/" + string(rune('1'+i))
		if l.Index != i+1 || l.IfName != name || len(l.Interfaces) != 1 || l.Interfaces[0] != name || l.Lanes != 1 {
			t.Errorf("lane %d: index %d ifname %q interfaces %v lanes %d", i+1, l.Index, l.IfName, l.Interfaces, l.Lanes)
		}
		if len(l.SpeedCaps) != 1 || l.SpeedCaps[0] != 25000 || l.Media != devicemodel.MediaSFP28 || l.AdminSpeed != 25000 {
			t.Errorf("lane %d: caps %v media %v admin %d, want the one lane speed 25000, SFP28", i+1, l.SpeedCaps, l.Media, l.AdminSpeed)
		}
		if l.Up || !l.Enabled || l.LAGID != 0 || l.MirrorFrom != 0 || l.Optic != nil || l.Fault != "" {
			t.Errorf("lane %d: up %v enabled %v lag %d mirror %d optic %v fault %q", i+1, l.Up, l.Enabled, l.LAGID, l.MirrorFrom, l.Optic, l.Fault)
		}
		if l.VLAN.Mode != "trunk" || !l.VLAN.AllowAll {
			t.Errorf("lane %d: vlan %+v, want the captured trunk allowing all", i+1, l.VLAN)
		}
	}
	if v.Cage.Lanes != 4 || v.Cage.IfName != "Ethernet54" {
		t.Errorf("cage: lanes %d ifname %q", v.Cage.Lanes, v.Cage.IfName)
	}
	// The fold reports the child: its MAC and LLDP entry, never the uplink.
	n := v.Cage.Neighbor
	if n == nil || !n.Child || n.ChassisID != childMAC || n.SystemName != "lane-switch" {
		t.Errorf("the cage's neighbour: %+v", n)
	}
	found := false
	for _, m := range v.Cage.MACs {
		found = found || m.MAC == childMAC
	}
	if !found {
		t.Errorf("the cage does not report the child's MAC: %+v", v.Cage.MACs)
	}
	// LLDP local-info's chassis id, as the capture has it (the system MAC
	// on the 7160).
	if c.LocalChassisID() != "02:00:00:00:00:39" || v.ChassisID != c.LocalChassisID() || v.System.MAC != c.LocalChassisID() {
		t.Errorf("chassis id %q (view %q), system MAC %q", c.LocalChassisID(), v.ChassisID, v.System.MAC)
	}
	if _, err := c.Lanes(53); err == nil || !strings.Contains(err.Error(), "not split") {
		t.Errorf("a whole cage: %v", err)
	}
	if _, err := c.Lanes(99); err == nil {
		t.Error("no such port: want an error")
	}
}

// Cage 50 of the 2026-09-19 capture: split, with a breakout DAC (no far
// side). Every lane is a port; the fold is as before.
func TestLanesOfTheCapturedCage50(t *testing.T) {
	c, _ := newFixtureCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := lanePorts(t, c, 50)
	if len(v.Lanes) != 4 || v.Mode != "4x25G" || v.Child.MAC != "" {
		t.Fatalf("cage 50: %d lanes mode %q child %+v", len(v.Lanes), v.Mode, v.Child)
	}
	if v.Cage.Neighbor != nil {
		t.Errorf("no child attached: the cage keeps its own (no) neighbour, got %+v", v.Cage.Neighbor)
	}
}

func TestLanesReadAggregateAndMirror(t *testing.T) {
	c, _, _ := splitCollector(t, "split-cage54-lag", false)
	v := lanePorts(t, c, 54)
	for _, l := range v.Lanes {
		wantLAG, wantMirror := 0, 0
		if l.Index >= 3 {
			wantLAG = 1
		}
		if l.Index == 2 {
			wantMirror = 1
		}
		if l.LAGID != wantLAG || l.MirrorFrom != wantMirror {
			t.Errorf("lane %d: LAGID %d (%q) mirror %d, want %d, %d", l.Index, l.LAGID, l.LAG, l.MirrorFrom, wantLAG, wantMirror)
		}
		if l.LAGID > 0 && (l.LAG != "Port-Channel1541" || l.LACP == nil) {
			t.Errorf("lane %d: LAG %q LACP %+v", l.Index, l.LAG, l.LACP)
		}
	}
	// The fold: a lane switch's group is not the device's own aggregate,
	// and session 1542 is not a session of the device's own ports.
	if v.Cage.LAGID != 0 || v.Cage.LAG != "Port-Channel1541" || v.Cage.MirrorFrom != 0 {
		t.Errorf("fold: LAGID %d LAG %q mirror %d", v.Cage.LAGID, v.Cage.LAG, v.Cage.MirrorFrom)
	}
}

// With no lane switch attached, the device's own plan leaves a cage in a
// lane switch's group alone (the group outlives a removed config entry),
// with a note, and touches no session of the lanes'.
func TestCagePlanLeavesLaneGroupsAlone(t *testing.T) {
	c, ft, buf := splitCollector(t, "split-cage54-lag", false)
	snap := c.last
	desired := desiredFrom(snap.Ports)
	for i := range desired {
		if desired[i].Index == 54 {
			desired[i].LAG = 0
			desired[i].VLANSet, desired[i].NativeVLAN = true, 2
		}
	}
	if _, err := c.ApplyPorts(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	for _, b := range ft.Configured {
		s := strings.Join(b, " | ")
		if strings.Contains(s, "Ethernet54/") || strings.Contains(s, "1541") || strings.Contains(s, "1542") {
			t.Errorf("the cage plan wrote %q", s)
		}
	}
	if !strings.Contains(buf.String(), "is in Port-Channel1541, which this plan does not own") {
		t.Errorf("no note: %s", buf.String())
	}
}

func TestLanePlan(t *testing.T) {
	c, ft, buf := splitCollector(t, "split-cage54", true)
	ctx := context.Background()
	lanes := lanePorts(t, c, 54).Lanes
	desired := desiredFrom(lanes)
	desired[0].Description = "host-a"                   // name
	desired[1].Enabled = false                          // state
	desired[2].VLANSet, desired[2].NativeVLAN = true, 2 // access VLAN 2
	desired[3].SpeedMbps = 10000                        // refused: the cage's speed
	desired[2].MirrorSource = 1                         // lane 1 onto lane 3
	pct := 5.0
	desired[0].StormCtrl = &devicemodel.StormControlSpec{BroadcastPct: &pct}
	desired[0].BPDUGuard = true // the cage's: not a lane setting

	if got := c.PlanLanes(54, desired); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("PlanLanes = %v, want [1 2 3]", got)
	}
	if n, err := c.ApplyLanes(ctx, 54, desired); err != nil || n == 0 {
		t.Fatalf("ApplyLanes: %d %v", n, err)
	}
	want := "enable | configure | monitor session 1543 source Ethernet54/1 both | monitor session 1543 destination Ethernet54/3 | " +
		"interface Ethernet54/1 | description host-a | storm-control broadcast level 5 | " +
		"interface Ethernet54/2 | shutdown | " +
		"interface Ethernet54/3 | switchport mode access | switchport access vlan 2 | end | write memory"
	if got := lastBatch(t, ft); got != want {
		t.Errorf("lane batch\n%s\nwant\n%s", got, want)
	}
	if !strings.Contains(buf.String(), "lane 4 of cage 54: 10000 Mb/s asked; on EOS every lane of a cage runs one speed") {
		t.Errorf("no speed note: %s", buf.String())
	}
	// A second apply before the next collect writes nothing.
	before := len(ft.Configured)
	if n, err := c.ApplyLanes(ctx, 54, desired); err != nil || n != 0 || len(ft.Configured) != before {
		t.Errorf("second apply: %d %v, %d batches", n, err, len(ft.Configured)-before)
	}
	// Nothing on the device's own ports went into the lane batch.
	for _, b := range ft.Configured {
		for _, line := range b {
			if strings.HasPrefix(line, "interface ") && !strings.HasPrefix(line, "interface Ethernet54/") {
				t.Errorf("the lane plan wrote %q", line)
			}
		}
	}
}

func TestLanePlanAggregateAndItsUndo(t *testing.T) {
	c, ft, _ := splitCollector(t, "split-cage54-lag", true)
	ctx := context.Background()
	desired := desiredFrom(lanePorts(t, c, 54).Lanes)
	if got := c.PlanLanes(54, desired); len(got) != 0 {
		t.Errorf("the lanes as they are plan %v", got)
	}
	for i := range desired {
		desired[i].LAG, desired[i].MirrorSource = 0, 0
	}
	if _, err := c.ApplyLanes(ctx, 54, desired); err != nil {
		t.Fatal(err)
	}
	want := "enable | configure | no monitor session 1542 | interface Ethernet54/3 | no channel-group | interface Ethernet54/4 | no channel-group | end | write memory"
	if got := lastBatch(t, ft); got != want {
		t.Errorf("undo batch\n%s\nwant\n%s", got, want)
	}
	// An aggregate id past the nine a cage has is refused.
	desired[1].LAG = 12
	if got := c.PlanLanes(54, desired); len(got) != 0 {
		t.Errorf("lag 12 planned %v", got)
	}
}

// The cage on the device while a lane switch is attached: its admin state
// (every lane) and speed only; the rest is the lanes', noted, and not
// counted by PlanPorts.
func TestCagePlanWithALaneSwitch(t *testing.T) {
	c, ft, buf := splitCollector(t, "split-cage54", true)
	ctx := context.Background()
	desired := desiredFrom(c.last.Ports)
	var d54 *devicemodel.PortDesired
	for i := range desired {
		if desired[i].Index == 54 {
			d54 = &desired[i]
		}
	}
	// What the cage itself reports, as the push would carry it back: the
	// child's own presentation (no neighbour of the cage's) is not intent.
	d54.Description, d54.VLANSet, d54.NativeVLAN = "cage", true, 2
	rs := devicemodel.FECRS
	d54.FEC = &rs
	if got := c.PlanPorts(desired); len(got) != 0 {
		t.Errorf("PlanPorts = %v: the lanes' settings must not count", got)
	}
	if n, _ := c.ApplyPorts(ctx, desired); n != 0 || len(ft.Configured) != 0 {
		t.Errorf("wrote %d: %v", n, ft.Configured)
	}
	if !strings.Contains(buf.String(), "port 54: a lane switch sets its lanes") {
		t.Errorf("no note: %s", buf.String())
	}
	// Disabled on the device: every lane down, and the lane switch cannot
	// bring one up.
	d54.Enabled = false
	if got := c.PlanPorts(desired); len(got) != 1 || got[0] != 54 {
		t.Errorf("PlanPorts = %v, want [54]", got)
	}
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	want := "enable | configure | interface Ethernet54/1 | shutdown | interface Ethernet54/2 | shutdown | interface Ethernet54/3 | shutdown | interface Ethernet54/4 | shutdown | end | write memory"
	if got := lastBatch(t, ft); got != want {
		t.Errorf("cage disable\n%s\nwant\n%s", got, want)
	}
	if v := lanePorts(t, c, 54); !v.CageDisabled {
		t.Error("the view does not say the cage is disabled")
	}
	lanes := desiredFrom(lanePorts(t, c, 54).Lanes)
	lanes[0].Enabled = true
	if got := c.PlanLanes(54, lanes); len(got) != 0 {
		t.Errorf("a lane of a disabled cage planned up: %v", got)
	}
	// A join (100G on lane 1) is still the device's.
	d54.Enabled, d54.SpeedMbps = true, 100000
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if got := lastBatch(t, ft); !strings.Contains(got, "interface Ethernet54/1 | speed forced 100gfull | no shutdown") {
		t.Errorf("join batch %s", got)
	}
}

// The join of a cage whose lane switch has an aggregate and a mirror: the
// session and the channel groups go first, in the same batch.
func TestJoinDissolvesTheLaneSwitchsGroups(t *testing.T) {
	c, ft, _ := splitCollector(t, "split-cage54-lag", true)
	desired := desiredFrom(c.last.Ports)
	for i := range desired {
		if desired[i].Index == 54 {
			desired[i].SpeedMbps = 100000
		}
	}
	if _, err := c.ApplyPorts(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	want := "enable | configure | no monitor session 1542 | interface Ethernet54/3 | no channel-group | interface Ethernet54/4 | no channel-group | interface Ethernet54/1 | speed forced 100gfull | end | write memory"
	if got := lastBatch(t, ft); got != want {
		t.Errorf("join\n%s\nwant\n%s", got, want)
	}
	if _, err := c.Lanes(54); err == nil {
		t.Error("the lanes of a cage just joined are still offered")
	}
}

// The first try at the split (lanes 2-4 left at 10G): EOS errdisables them,
// and each lane reports its own fault.
func TestLanesDiverged(t *testing.T) {
	ft := NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M", "split-cage54"))
	ft.Overrides = map[string]string{}
	for _, cmd := range []string{"show interfaces", "show interfaces status errdisabled", "show interfaces transceiver properties", "show interfaces status"} {
		ft.Overrides[cmd] = filepath.Join("..", "split-cage54-diverged", fixtureName(cmd, ".json"))
	}
	c := NewCollector(ft)
	c.Log = log.New(&strings.Builder{}, "", 0)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := lanePorts(t, c, 54)
	for _, l := range v.Lanes {
		if (l.Index > 1) != strings.HasPrefix(l.Fault, "errdisabled") {
			t.Errorf("lane %d fault %q", l.Index, l.Fault)
		}
	}
	if !v.Cage.LanesDiverge {
		t.Error("the fold does not mark the lanes diverged")
	}
}

func TestAttachLaneSwitchRange(t *testing.T) {
	c, _, _ := splitCollector(t, "split-cage54", false)
	if err := c.AttachLaneSwitch(100, devicemodel.Attachment{MAC: childMAC}); err == nil {
		t.Error("cage 100: EOS has no channel group past 2000")
	}
	if err := c.AttachLaneSwitch(60, devicemodel.Attachment{MAC: childMAC}); err == nil {
		t.Error("no port 60 on the 7160")
	}
	if laneBase(54)+laneGroupSpan-1 >= laneGroupCeil || laneBase(1) <= 54 {
		t.Errorf("lane groups %d..%d", laneBase(1), laneBase(54)+laneGroupSpan-1)
	}
}

// One write at a time: while a collect or another write holds the lock, a
// lane apply waits, and gives up with its context.
func TestLaneApplyWaitsForTheLock(t *testing.T) {
	c, _, _ := splitCollector(t, "split-cage54", true)
	unlock, err := c.lockApply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ApplyLanes(ctx, 54, nil); err == nil || !strings.Contains(err.Error(), "waiting for another write") {
		t.Errorf("ApplyLanes under the lock: %v", err)
	}
	if _, err := c.Collect(ctx); err == nil {
		t.Error("Collect under the lock did not wait")
	}
	unlock()
	if _, err := c.Collect(context.Background()); err != nil {
		t.Errorf("after unlock: %v", err)
	}
}
