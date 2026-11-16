package sonic

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The lane switch's parent half (lanes.go, docs/drivers/breakout.md),
// against the DX010 captured on 2026-11-07 with cage 2 split to 4x25G
// (collect-4x25g.txt): every lane is admin up, forced 25G, 1500 MTU,
// untagged in Vlan4094, without link.

const laneMAC = "02:00:00:00:00:aa"

func laneRig(t *testing.T) *controlRig {
	t.Helper()
	r := rig20261106(t, fixture20261106Split)
	if err := r.c.AttachLaneSwitch(2, devicemodel.Attachment{MAC: laneMAC, Name: "lane-switch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

// laneBase is the lanes as they are, as a lane switch's push says them.
func (r *controlRig) laneBase() []devicemodel.PortDesired {
	r.t.Helper()
	v, err := r.c.Lanes(2)
	if err != nil {
		r.t.Fatal(err)
	}
	var ds []devicemodel.PortDesired
	for _, l := range v.Lanes {
		ds = append(ds, devicemodel.PortDesired{Index: l.Index, Enabled: l.Enabled, VLANSet: true, NativeVLAN: 1})
	}
	return ds
}

// applyLanes runs ApplyLanes, checks the commands, collects, and checks
// that a second apply writes nothing.
func (r *controlRig) applyLanes(name string, ds []devicemodel.PortDesired, want []string) {
	r.t.Helper()
	n := len(r.fr.Commands)
	if _, err := r.c.ApplyLanes(context.Background(), 2, ds); err != nil {
		r.t.Fatalf("%s: ApplyLanes: %v", name, err)
	}
	if got := r.fr.Commands[n:]; !reflect.DeepEqual(append([]string(nil), got...), want) {
		r.t.Errorf("%s: commands\n  got  %q\n  want %q", name, got, want)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	n = len(r.fr.Commands)
	if k, err := r.c.ApplyLanes(context.Background(), 2, ds); err != nil || k != 0 || len(r.fr.Commands) != n {
		r.t.Errorf("%s: a second apply wrote %q (%d, %v)", name, r.fr.Commands[n:], k, err)
	}
}

func TestLanesFromTheSplitCapture(t *testing.T) {
	r := laneRig(t)
	v, err := r.c.Lanes(2)
	if err != nil {
		t.Fatal(err)
	}
	if v.Mode != "4x25G[10G]" || v.CageAlias != "Eth2" || v.Child.MAC != laneMAC || v.ChassisID == "" {
		t.Errorf("view: mode %q alias %q child %+v chassis %q", v.Mode, v.CageAlias, v.Child, v.ChassisID)
	}
	if len(v.Lanes) != 4 {
		t.Fatalf("%d lanes", len(v.Lanes))
	}
	for i, l := range v.Lanes {
		name := []string{"Ethernet4", "Ethernet5", "Ethernet6", "Ethernet7"}[i]
		if l.Index != i+1 || l.IfName != name || !reflect.DeepEqual(l.Interfaces, []string{name}) || v.LaneAliases[i] != "Eth2/"+string(rune('1'+i)) {
			t.Errorf("lane %d: %d %s %v %q", i+1, l.Index, l.IfName, l.Interfaces, v.LaneAliases[i])
		}
		// The SAI's supported_speeds for a 25G lane, and its form.
		if !reflect.DeepEqual(l.SpeedCaps, []int{1000, 10000, 25000}) || l.Media != devicemodel.MediaSFP28 || l.AdminSpeed != 25000 || l.MTU != 1500 {
			t.Errorf("lane %d: caps %v media %s admin %d mtu %d", i+1, l.SpeedCaps, l.Media, l.AdminSpeed, l.MTU)
		}
		if l.VLAN.NativeVLAN != 1 {
			t.Errorf("lane %d: VLAN %+v", i+1, l.VLAN)
		}
	}
	// The parent reports the child on the cage, never as its uplink.
	snap, _ := r.c.Collect(context.Background())
	cage := port(snap, 2)
	if n := cage.Neighbor; n == nil || !n.Child || n.ChassisID != laneMAC || n.SystemName != "lane-switch" {
		t.Errorf("cage neighbour %+v", cage.Neighbor)
	}
	has := func(ms []devicemodel.MACEntry) bool {
		for _, m := range ms {
			if m.MAC == laneMAC && m.PortIndex == 2 && m.VLAN == 1 {
				return true
			}
		}
		return false
	}
	if !has(cage.MACs) || !has(snap.MACTable) {
		t.Errorf("the child's MAC is not reported on cage 2")
	}
	if snap.UplinkHint == 2 || snap.UplinkPort() == 2 {
		t.Errorf("uplink %d/%d: the child became the parent's uplink", snap.UplinkHint, snap.UplinkPort())
	}
	// A whole cage, and a cage the switch does not have.
	if _, err := r.c.Lanes(1); err == nil || !strings.Contains(err.Error(), "not split") {
		t.Errorf("cage 1: %v", err)
	}
	if _, err := r.c.Lanes(40); err == nil || !strings.Contains(err.Error(), "no port 40") {
		t.Errorf("cage 40: %v", err)
	}
	if err := r.c.AttachLaneSwitch(100, devicemodel.Attachment{MAC: laneMAC}); err == nil {
		t.Errorf("cage 100 accepted (PortChannel<100*cage+lane> needs 1..99)")
	}
}

// The lanes' own settings, each on its lane only, then back.
func TestLaneSettings(t *testing.T) {
	r := laneRig(t)
	base := r.laneBase()
	r.applyLanes("as they are", base, nil)
	pct := 10.0
	ds := edit(base, 1, func(d *devicemodel.PortDesired) { d.Description = "host-a"; d.SpeedMbps = 10000 })
	ds = edit(ds, 3, func(d *devicemodel.PortDesired) { d.TaggedVLANs = []int{2} })
	ds = edit(ds, 4, func(d *devicemodel.PortDesired) {
		d.StormCtrl = &devicemodel.StormControlSpec{BroadcastPct: &pct}
		d.Enabled = false
	})
	r.applyLanes("set", ds, []string{
		"sudo config interface speed Ethernet4 10000",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet4' description 'host-a'",
		"sudo config interface shutdown Ethernet7",
		"sudo config interface storm-control add Ethernet7 broadcast 2500000",
		"sudo config vlan member add 2 Ethernet6",
		save,
	})
	// Speed auto leaves a lane at what it runs; the rest goes back.
	r.applyLanes("back", base, []string{
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet4' description ''",
		"sudo sonic-db-cli CONFIG_DB hdel 'PORT|Ethernet4' description",
		"sudo config interface startup Ethernet7",
		"sudo config interface storm-control del Ethernet7 broadcast",
		"sudo config vlan member del 2 Ethernet6",
		save,
	})
	// A lane never splits or joins its cage.
	r.applyLanes("40G on a lane", edit(base, 2, func(d *devicemodel.PortDesired) { d.SpeedMbps = 40000 }), nil)
	if !strings.Contains(r.log.String(), "lane 2 (Ethernet5) of cage 2: 40000 Mb/s is not among the lane's speeds [1000 10000 25000]") {
		t.Errorf("no note: %s", r.log.String())
	}
	// Jumbo on the lane switch: the lanes' MTU. No VLAN's: this lane switch
	// has listed none (EnsureLaneVLANs), and the native stand-in is the
	// device's (TestLaneOnlyVLANMTU).
	r.applyLanes("jumbo", edit(edit(edit(edit(base, 1, setMTU), 2, setMTU), 3, setMTU), 4, setMTU), []string{
		"sudo config interface mtu Ethernet4 9216", "sudo config interface mtu Ethernet5 9216",
		"sudo config interface mtu Ethernet6 9216", "sudo config interface mtu Ethernet7 9216", save,
	})
}

func setMTU(d *devicemodel.PortDesired) { d.MTU = 9216 }

// The cage on the parent keeps its breakout and its admin state; nothing
// else of the cage's reaches the lanes, and a lane speed the lane switch
// forced is not the cage's to undo.
func TestOwnedCageKeepsSpeedAndAdminOnly(t *testing.T) {
	r := laneRig(t)
	base := r.baseline()
	pct := 10.0
	ds := edit(base, 2, func(d *devicemodel.PortDesired) {
		d.Description, d.TaggedVLANs = "cage", []int{2}
		d.StormCtrl = &devicemodel.StormControlSpec{BroadcastPct: &pct}
		d.LAG = 2
		d.SpeedMbps = 25000
	})
	if got := r.c.PlanPorts(ds); len(got) != 0 {
		t.Errorf("PlanPorts counts the cage's skipped settings: %v", got)
	}
	if got, _ := r.apply(ds); len(got) != 0 {
		t.Errorf("the cage's settings reached the lanes: %q", got)
	}
	r.applyLanes("lane 1 at 10G", edit(r.laneBase(), 1, func(d *devicemodel.PortDesired) { d.SpeedMbps = 10000 }), []string{
		"sudo config interface speed Ethernet4 10000", save,
	})
	if got, _ := r.apply(ds); len(got) != 0 {
		t.Errorf("the cage's 25G undid the lane's 10G: %q", got)
	}
	got, _ := r.apply(edit(ds, 2, func(d *devicemodel.PortDesired) { d.Enabled = false }))
	if strings.Join(got, ";") != "sudo config interface shutdown Ethernet4;sudo config interface shutdown Ethernet5;sudo config interface shutdown Ethernet6;sudo config interface shutdown Ethernet7;"+save {
		t.Errorf("disabling the cage: %q", got)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// While the cage is down the lane switch cannot bring a lane up.
	r.applyLanes("lanes while the cage is down", r.laneBase(), nil)
	if v, _ := r.c.Lanes(2); !v.CageDisabled {
		t.Errorf("the view does not say the cage is disabled")
	}
}

// A lane aggregate is PortChannel<100*cage+lag>; the parent's own plan
// leaves it alone, and joining the cage removes it before the breakout.
func TestLaneAggregateAndJoin(t *testing.T) {
	r := laneRig(t)
	ds := r.laneBase()
	for i := 1; i < 4; i++ {
		ds[i].LAG = 1
	}
	var want []string
	want = append(want, "sudo config portchannel add PortChannel201 --fast-rate true --min-links 1", "sudo sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel201' mtu 1500")
	for _, ln := range []string{"Ethernet5", "Ethernet6", "Ethernet7"} {
		for _, g := range []string{"224.0.1.129", "224.0.1.1", "224.0.1.22", "224.0.1.35", "224.0.1.41", "239.255.255.250", "239.255.255.253"} {
			want = append(want, "sudo sonic-db-cli CONFIG_DB del 'L2MC_STATIC_MEMBER|Vlan4094|"+g+"|"+ln+"'")
		}
		// The lane left its last VLAN: on this image (l2mcd, no sonic-swss
		// 0004) it waits for its bridge port to go before it joins (§8.15).
		want = append(want, "sudo config vlan member del 4094 "+ln, bridgePortWait(ln), "sudo config portchannel member add PortChannel201 "+ln)
	}
	want = append(want, "sudo config vlan member add -u 4094 PortChannel201", save)
	r.applyLanes("aggregate of lanes 2-4", ds, want)
	v, _ := r.c.Lanes(2)
	for _, l := range v.Lanes {
		if want := map[bool]int{true: 1}[l.Index > 1]; l.LAGID != want {
			t.Errorf("lane %d: LAGID %d (%s)", l.Index, l.LAGID, l.LAG)
		}
	}
	base := r.baseline()
	if got, _ := r.apply(base); len(got) != 0 {
		t.Errorf("the parent's plan touched the lane aggregate: %q", got)
	}
	got, _ := r.apply(speedOn(base, 2, 100000))
	// Lane 1 is the only lane in a VLAN of its own (2-4 are PortChannel201).
	got = unbound(t, got, 7, "Ethernet4", "Ethernet5", "Ethernet6", "Ethernet7")
	head := []string{
		"sudo config portchannel member del PortChannel201 Ethernet5",
		"sudo config portchannel member del PortChannel201 Ethernet6",
		"sudo config portchannel member del PortChannel201 Ethernet7",
		"sudo config vlan member del 4094 PortChannel201",
		bridgePortWait("PortChannel201"),
		"sudo config portchannel del PortChannel201",
		"sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null",
		"sudo config interface startup Ethernet4",
		"sudo config vlan member add -u 4094 Ethernet4",
		save,
	}
	if !reflect.DeepEqual(got, head) {
		t.Errorf("join\n  got  %q\n  want %q", got, head)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Lanes(2); err == nil {
		t.Errorf("the joined cage still has lanes")
	}
	if _, err := r.c.ApplyLanes(context.Background(), 2, ds); err == nil {
		t.Errorf("ApplyLanes on a joined cage did not refuse")
	}
}

// Without a lane switch attached, a split cage is the parent's, as before:
// a hand-made or leftover lane aggregate is left alone with a note.
func TestUnownedCageLeavesALaneAggregate(t *testing.T) {
	r := laneRig(t)
	ds := r.laneBase()
	ds[1].LAG, ds[2].LAG = 1, 1
	if _, err := r.c.ApplyLanes(context.Background(), 2, ds); err != nil {
		t.Fatal(err)
	}
	r2 := rig20261106(t, fixture20261106Split)
	r2.fr.Fixture = r.fr.Fixture
	if _, err := r2.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := r2.apply(r2.baseline()); len(got) != 0 {
		t.Errorf("an unattached parent's plan dissolved the lane aggregate: %q", got)
	}
	if !strings.Contains(r2.log.String(), "Ethernet5 is in PortChannel201, an aggregate this config does not own") {
		t.Errorf("no note: %s", r2.log.String())
	}
}

// detachedLaneAggregate is the split capture after a lane switch made
// PortChannel201 of lanes 2-3 and was detached (its config entry removed,
// the bridge restarted): a rig with no lane switch over that switch.
func detachedLaneAggregate(t *testing.T) *controlRig {
	t.Helper()
	r := laneRig(t)
	ds := r.laneBase()
	ds[1].LAG, ds[2].LAG = 1, 1
	if _, err := r.c.ApplyLanes(context.Background(), 2, ds); err != nil {
		t.Fatal(err)
	}
	r2 := rig20261106(t, fixture20261106Split)
	r2.fr.Fixture = r.fr.Fixture
	if _, err := r2.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r2
}

// The members taken out by hand (or lost to a forced breakout, which drops
// a member silently) leave the detached lane switch's PortChannel empty:
// nobody's plan reached it, so it stayed for good (breakout.md §7c). The
// device's plan now removes it, as an orphan of its own range, once.
func TestDetachedLaneSwitchsEmptyPortChannelGoes(t *testing.T) {
	r := detachedLaneAggregate(t)
	for _, ln := range []string{"Ethernet5", "Ethernet6"} {
		if _, err := r.fr.Run(context.Background(), "sudo config portchannel member del PortChannel201 "+ln, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The lanes the hand removal left in no VLAN take the cage's again (the
	// split cage is the device's), then the PortChannel goes: its VLAN, its
	// bridge port, itself.
	got, _ := r.apply(r.baseline())
	want := []string{
		"sudo config vlan member add -u 4094 Ethernet5",
		"sudo config vlan member add -u 4094 Ethernet6",
		"sudo config vlan member del 4094 PortChannel201",
		bridgePortWait("PortChannel201"),
		"sudo config portchannel del PortChannel201",
		save,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("commands\n  got  %q\n  want %q", got, want)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again, _ := r.apply(r.baseline()); len(again) != 0 {
		t.Errorf("a second apply wrote %q", again)
	}
}

// With the lane switch attached the range is its own: the device's plan
// leaves the empty PortChannel to it.
func TestAttachedLaneSwitchKeepsItsEmptyPortChannel(t *testing.T) {
	r := laneRig(t)
	ds := r.laneBase()
	ds[1].LAG, ds[2].LAG = 1, 1
	if _, err := r.c.ApplyLanes(context.Background(), 2, ds); err != nil {
		t.Fatal(err)
	}
	for _, ln := range []string{"Ethernet5", "Ethernet6"} {
		if _, err := r.fr.Run(context.Background(), "sudo config portchannel member del PortChannel201 "+ln, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.apply(r.baseline()); len(got) != 0 {
		t.Errorf("the device's plan wrote %q", got)
	}
}

// One with members stays, with a note naming them.
func TestDetachedLaneAggregateWithMembersIsNoted(t *testing.T) {
	r := detachedLaneAggregate(t)
	if got, _ := r.apply(r.baseline()); len(got) != 0 {
		t.Errorf("wrote %q", got)
	}
	if want := "PortChannel201 is port 2's lane switch's aggregate and no lane switch is attached to port 2: left as it is while it has members (Ethernet5, Ethernet6)"; !strings.Contains(r.log.String(), want) {
		t.Errorf("no note: %s", r.log.String())
	}
}

// One write (or collect) at a time: a lane switch's apply comes from its
// own loop.
func TestApplyLockWaits(t *testing.T) {
	r := laneRig(t)
	unlock, err := r.c.lockApply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.c.ApplyLanes(ctx, 2, r.laneBaseFromView(t)); err == nil || !strings.Contains(err.Error(), "waiting for another write") {
		t.Errorf("ApplyLanes while a write runs: %v", err)
	}
	if _, err := r.c.Collect(ctx); err == nil {
		t.Errorf("Collect while a write runs did not wait")
	}
	unlock()
	if _, err := r.c.ApplyLanes(context.Background(), 2, r.laneBaseFromView(t)); err != nil {
		t.Errorf("after the write: %v", err)
	}
}

// laneBaseFromView is laneBase without a collect (the lock is held).
func (r *controlRig) laneBaseFromView(t *testing.T) []devicemodel.PortDesired {
	v, err := r.c.Lanes(2)
	if err != nil {
		t.Fatal(err)
	}
	var ds []devicemodel.PortDesired
	for _, l := range v.Lanes {
		ds = append(ds, devicemodel.PortDesired{Index: l.Index, Enabled: l.Enabled, VLANSet: true, NativeVLAN: 1})
	}
	return ds
}

// A lane aggregate that leaves lane 1 out (the captured Flex XG push's
// lanes 2-4) is a kernel bridge port of its own: l2d takes the cage's STP
// settings for it and for lane 1 alone, from the parent's push (the lane
// switch claims no STP), as soon as the lane batch has made it, and drops
// it again when the join dissolves it (breakout.md §14 item 4).
func TestLaneAggregateTakesTheCageSTP(t *testing.T) {
	r := laneRig(t)
	r.c.L2 = true
	cageSTP := func(ds []devicemodel.PortDesired) []devicemodel.PortDesired {
		return edit(ds, 2, func(d *devicemodel.PortDesired) {
			d.STPEdgeMode, d.BPDUGuard, d.LoopProtect = "enabled", true, true
		})
	}
	conf := func() l2Conf {
		t.Helper()
		var c l2Conf
		if err := json.Unmarshal([]byte(r.fr.L2Files[l2ConfPath]), &c); err != nil {
			t.Fatalf("l2.json: %v", err)
		}
		return c
	}
	want := l2Port{Enabled: true, Edge: "enabled", BPDUGuard: true}
	check := func(stage string, on, off []string) {
		t.Helper()
		c := conf()
		for _, n := range on {
			if c.STP.Ports[n] != want {
				t.Errorf("%s: %s = %+v, want %+v", stage, n, c.STP.Ports[n], want)
			}
			if !slices.Contains(c.LoopGuard.Ports, n) {
				t.Errorf("%s: %s not under loop protection %v", stage, n, c.LoopGuard.Ports)
			}
		}
		for _, n := range off {
			if _, ok := c.STP.Ports[n]; ok || slices.Contains(c.LoopGuard.Ports, n) {
				t.Errorf("%s: %s is not a bridge port, but l2d is told about it", stage, n)
			}
		}
	}
	r.deviceCmds(rstp)
	base := cageSTP(r.baseline())
	r.apply(base)
	check("split", []string{"Ethernet4", "Ethernet5", "Ethernet6", "Ethernet7"}, []string{"PortChannel201"})

	ds := r.laneBase()
	for i := 1; i < 4; i++ {
		ds[i].LAG = 1
	}
	n := len(r.fr.L2Scripts)
	if _, err := r.c.ApplyLanes(context.Background(), 2, ds); err != nil {
		t.Fatal(err)
	}
	if len(r.fr.L2Scripts) == n || !strings.Contains(r.fr.L2Scripts[len(r.fr.L2Scripts)-1], "sudo systemctl reload disunified-l2d") {
		t.Errorf("the lane batch did not hand l2d the new bridge port: %q", r.fr.L2Scripts[n:])
	}
	check("aggregate of lanes 2-4", []string{"Ethernet4", "PortChannel201"}, []string{"Ethernet5", "Ethernet6", "Ethernet7"})
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.apply(base); len(got) != 0 {
		t.Errorf("the parent's next push wrote %q", got)
	}

	// The join dissolves the aggregate: the cage is one bridge port again.
	r.apply(speedOn(base, 2, 100000))
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.apply(speedOn(base, 2, 100000))
	check("joined", []string{"Ethernet4"}, []string{"PortChannel201", "Ethernet5"})
}

// The join with the driver's ACL tables bound to a lane aggregate (the
// DX010, 2026-11-15, portchannel-del-acl-20261115.txt): `config portchannel
// del` leaves a table naming the PortChannel, orchagent then holds the LAG
// and the breakout's YANG check refuses the dangling name. The plan rebinds
// each table without the PortChannel before it goes.
func TestLaneJoinUnbindsTheACLTables(t *testing.T) {
	r := laneRig(t)
	r.deviceCmds(devicemodel.DeviceDesired{IPACLSet: true, IPACLs: ipACLsOf(ecsPush(t, "system_cfg-ecs-acl.txt"))})
	ds := r.laneBase()
	for i := 1; i < 4; i++ {
		ds[i].LAG = 1
	}
	if _, err := r.c.ApplyLanes(context.Background(), 2, ds); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.c.mu.Lock()
	bound := r.c.state.aclTables[ipACLTable].ports
	r.c.mu.Unlock()
	if !bound["PortChannel201"] || bound["Ethernet5"] {
		t.Fatalf("the lane aggregate is not bound in place of its lanes: %v", sortIfnames(bound))
	}
	got, _ := r.apply(speedOn(r.baseline(), 2, 100000))
	var keep []string
	for p := range bound {
		if p != "PortChannel201" {
			keep = append(keep, p)
		}
	}
	unbind := "sudo config acl add table DUI_IP_ACL L3 -p " + strings.Join(sortIfnames(setOf(keep)), ",") + " -s ingress"
	ui, di := indexOf(got, unbind), indexOf(got, "sudo config portchannel del PortChannel201")
	if ui < 0 || di < 0 || ui > di {
		t.Errorf("the table must let go of PortChannel201 before it is deleted: unbind at %d, del at %d in %q", ui, di, got)
	}
	if !containsString(got, "sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null") {
		t.Errorf("no join in %q", got)
	}
	if len(r.fr.HeldLAGs) != 0 {
		t.Errorf("orchagent still holds %v", r.fr.HeldLAGs)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.c.mu.Lock()
	bound = r.c.state.aclTables[ipACLTable].ports
	r.c.mu.Unlock()
	if bound["PortChannel201"] || !bound["Ethernet4"] {
		t.Errorf("after the join: %v", sortIfnames(bound))
	}
}

// A split cage is reported in its lanes' form, a whole one as a QSFP28:
// the controller takes a FEC for a port by its reported media and speed (a
// QSFP28 at 40G and 100G only, an SFP28 at 25G; docs/drivers/sonic.md
// 10.3, live 2026-11-16). With a lane switch attached the cage keeps its
// own form: its speed is the breakout control, the lanes' FEC is the lane
// switch's (docs/breakout-lag.md 7).
func TestSplitCageReportsItsLanesForm(t *testing.T) {
	media := func(snap *devicemodel.Snapshot, idx int) devicemodel.Media {
		for _, p := range snap.Ports {
			if p.Index == idx {
				return p.Media
			}
		}
		t.Fatalf("no port %d", idx)
		return ""
	}
	bare := rig20261106(t, fixture20261106Split)
	snap, err := bare.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports {
		want := devicemodel.MediaQSFP28
		if p.Index == 2 {
			want = devicemodel.MediaSFP28
		}
		if p.Media != want {
			t.Errorf("port %d (%d lanes): media %q, want %q", p.Index, p.Lanes, p.Media, want)
		}
	}

	r := laneRig(t)
	if snap, err = r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := media(snap, 2); m != devicemodel.MediaQSFP28 {
		t.Errorf("cage 2 with a lane switch attached: media %q, want the cage's own QSFP28", m)
	}
	v, err := r.c.Lanes(2)
	if err != nil {
		t.Fatal(err)
	}
	if v.Cage.Media != devicemodel.MediaQSFP28 {
		t.Errorf("the lane view's cage: media %q, want the cage's own QSFP28", v.Cage.Media)
	}
	for _, l := range v.Lanes {
		if l.Media != devicemodel.MediaSFP28 {
			t.Errorf("lane %d: media %q, want SFP28", l.Index, l.Media)
		}
	}
}
