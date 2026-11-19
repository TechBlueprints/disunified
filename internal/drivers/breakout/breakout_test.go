package breakout

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// The lane switch over a real parent: the sonic driver on the DX010 as
// captured on 2026-11-07 with cage 2 split to 4x25G (docs/fixtures/
// sonic-igmp-202511.20261106-f2e25f54c/collect-4x25g.txt).

const childMAC = "02:00:00:00:00:aa"

type rig struct {
	t   *testing.T
	fr  *sonic.FixtureRunner
	c   *sonic.Collector
	d   *Device
	log *strings.Builder
}

func newRig(t *testing.T) *rig {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "..", "docs", "fixtures", "sonic-igmp-202511.20261106-f2e25f54c", "collect-4x25g.txt"))
	if err != nil {
		t.Fatal(err)
	}
	buf := &strings.Builder{}
	c := sonic.NewCollector(fr)
	c.Log = log.New(buf, "", 0)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.AttachLaneSwitch(2, devicemodel.Attachment{MAC: childMAC, Name: "lane-switch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	o, err := ParseOptions(map[string]string{"parent": "core", "cage": "2", "mac": childMAC, "name": "lane-switch"})
	if err != nil {
		t.Fatal(err)
	}
	d := New(o, c, c)
	d.Log = log.New(buf, "", 0)
	return &rig{t: t, fr: fr, c: c, d: d, log: buf}
}

func (r *rig) snap() *devicemodel.Snapshot {
	r.t.Helper()
	s, err := r.d.Collect(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func TestSnapshotFourLanes(t *testing.T) {
	r := newRig(t)
	s := r.snap()
	if len(s.Ports) != Ports || s.UplinkPort() != UplinkPort {
		t.Fatalf("%d ports, uplink %d", len(s.Ports), s.UplinkPort())
	}
	sys := s.System
	if sys.MAC != childMAC || sys.Hostname != "lane-switch" || sys.Model != "Lane switch" || sys.SKU != "4x25G[10G]" || sys.Version == "" {
		t.Errorf("system %+v", sys)
	}
	if len(sys.Addresses) != 1 || sys.Addresses[0].Iface != "eth0" || sys.Addresses[0].IP != "169.254.1.170" || sys.Addresses[0].PrefixLen != 16 {
		t.Errorf("addresses %+v, want the lane switch's own link-local 169.254.1.170/16 (from its MAC), never the parent's", sys.Addresses)
	}
	for i, p := range s.Ports[:4] {
		if p.Index != i+1 || p.Media != devicemodel.MediaSFP28 || p.Optic != nil || !reflect.DeepEqual(p.SpeedCaps, []int{1000, 10000, 25000, 50000}) || p.IfName == "" {
			t.Errorf("lane %d: %+v", i+1, p)
		}
	}
	u := s.Ports[4]
	if u.Index != 5 || u.Media != devicemodel.MediaQSFP28 || !reflect.DeepEqual(u.SpeedCaps, []int{100000}) || u.Neighbor == nil || !u.Neighbor.IsBridge || u.Neighbor.Child || u.Neighbor.ChassisID != "02:00:00:00:00:01" {
		t.Errorf("uplink %+v neighbour %+v", u, u.Neighbor)
	}
	// The capture's lanes have no link; the cable is up all the same: the
	// lane switch is attached to the split cage (breakout.md 16.15).
	if !u.Up || !u.Enabled || u.SpeedMbps != 100000 || !u.FullDuplex || u.STPState != "forwarding" || u.STPRole != "root" || u.Health.LinkChanges != 1 {
		t.Errorf("uplink up %v enabled %v %d Mb/s fd %v stp %s/%s link changes %d", u.Up, u.Enabled, u.SpeedMbps, u.FullDuplex, u.STPState, u.STPRole, u.Health.LinkChanges)
	}
	if n := r.d.PortName(s.Ports[0]); n != "Eth2/1" {
		t.Errorf("lane 1 named %q", n)
	}
	if n := r.d.PortName(u); n != "Eth2" {
		t.Errorf("uplink named %q", n)
	}
	if n := r.d.DeviceName(sys); n != "host-1 Eth2" {
		t.Errorf("device named %q", n)
	}
	caps := r.d.Capabilities()
	if !caps.LACP || !caps.FEC || !caps.Jumbo || !caps.StormControl || caps.AggregateSessions != 2 || caps.STP || caps.IGMPSnooping || caps.DHCPSnooping || caps.SNMP || caps.IPACLs != 0 {
		t.Errorf("capabilities %+v", caps)
	}
	if a := r.d.CurrentAddress(); a != sys.Addresses[0].IP {
		t.Errorf("address %q, the snapshot's %q", a, sys.Addresses[0].IP)
	}
}

// lanePush is the lane switch's push as the controller sends it, for all
// five ports whatever the cage's mode: enabled, VLAN 1 untagged, speeds by
// lane (0 = auto).
func (r *rig) lanePush(speeds map[int]int) []devicemodel.PortDesired {
	r.t.Helper()
	var ds []devicemodel.PortDesired
	for i := 1; i <= Ports; i++ {
		ds = append(ds, devicemodel.PortDesired{Index: i, Enabled: true, VLANSet: true, NativeVLAN: 1, SpeedMbps: speeds[i]})
	}
	return ds
}

// apply is the lane switch's ApplyPorts; it returns the commands the
// parent ran for it.
func (r *rig) apply(ds []devicemodel.PortDesired) string {
	r.t.Helper()
	before := len(r.fr.Recorded())
	if _, err := r.d.ApplyPorts(context.Background(), ds); err != nil {
		r.t.Fatal(err)
	}
	return strings.Join(r.fr.Recorded()[before:], "\n")
}

// 2x50G from the lane switch (breakout.md 16.15): a forced 50G on a lane
// re-splits the cage; the lane switch then has two lanes, still SFP28 and
// still claiming 25G (so the drawer can set one back), and two empty
// slots; a forced 25G on one of them re-splits it to 4x25G again. Neither
// loop takes the new lanes for new ports, and the cycles after write
// nothing.
func TestLaneResplit(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.snap() // the lane switch's Start
	got := r.apply(r.lanePush(map[int]int{1: 50000}))
	t.Logf("re-split batch:\n%s", got)
	if !strings.Contains(got, "breakout -f -y Ethernet4 '2x50G'") {
		t.Fatalf("no 2x50G breakout: %s", got)
	}
	// Before the parent has read the switch again the lane plan waits.
	if got := r.apply(r.lanePush(map[int]int{1: 50000})); got != "" {
		t.Errorf("planned before the switch was read again: %s", got)
	}
	ps, err := r.c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ps.LayoutWritten, []int{2}) {
		t.Errorf("the parent's snapshot says the cage's layout was written by %v, want [2]", ps.LayoutWritten)
	}
	if ps2, _ := r.c.Collect(ctx); len(ps2.LayoutWritten) != 0 {
		t.Errorf("reported again: %v", ps2.LayoutWritten)
	}
	s := r.snap()
	if !reflect.DeepEqual(s.LayoutWritten, []int{1, 2, 3, 4}) {
		t.Errorf("the lane switch's snapshot names %v as written, want every lane slot", s.LayoutWritten)
	}
	if s.System.SKU != "2x50G" {
		t.Errorf("mode %q", s.System.SKU)
	}
	for _, p := range s.Ports[:2] {
		if p.Media != devicemodel.MediaSFP28 || p.AdminSpeed != 50000 || p.IfName == "" || !slices.Contains(p.SpeedCaps, 25000) || !slices.Contains(p.SpeedCaps, 50000) {
			t.Errorf("lane %d: media %s admin %d %q caps %v", p.Index, p.Media, p.AdminSpeed, p.IfName, p.SpeedCaps)
		}
	}
	for _, p := range s.Ports[2:4] {
		if p.IfName != "" || p.Present || p.Up || p.SpeedCaps != nil {
			t.Errorf("slot %d is not empty: %+v", p.Index, p)
		}
	}
	if u := s.Ports[4]; !u.Up || u.SpeedMbps != 100000 {
		t.Errorf("uplink through the re-split: up %v %d Mb/s", u.Up, u.SpeedMbps)
	}
	if s2 := r.snap(); s2.LayoutWritten != nil {
		t.Errorf("reported again: %v", s2.LayoutWritten)
	}
	// The next cycles: the lane switch's push and the parent's own write
	// nothing, the parent's forced 25G on the cage included (between split
	// modes the lane switch decides).
	if got := r.apply(r.lanePush(map[int]int{1: 50000})); got != "" {
		t.Errorf("the next cycle wrote %s", got)
	}
	var own []devicemodel.PortDesired
	for _, p := range ps.Ports {
		d := devicemodel.PortDesired{Index: p.Index, Enabled: p.Enabled, VLANSet: true, NativeVLAN: 1}
		if p.Index == 2 {
			d.SpeedMbps = 25000
		}
		own = append(own, d)
	}
	before := len(r.fr.Recorded())
	if _, err := r.c.ApplyPorts(ctx, own); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.fr.Recorded()[before:], "\n"); strings.Contains(got, "Ethernet4") || strings.Contains(got, "Ethernet6") {
		t.Errorf("the parent's push touched the cage: %s", got)
	}
	if !strings.Contains(r.log.String(), "only a join is taken from the cage's speed") {
		t.Errorf("no note for the cage's 25G: %s", r.log.String())
	}

	// Back: 25G on lane 2 (lane 1 back on auto).
	got = r.apply(r.lanePush(map[int]int{2: 25000}))
	t.Logf("back batch:\n%s", got)
	if !strings.Contains(got, "breakout -f -y Ethernet4 '4x25G[10G]'") {
		t.Fatalf("no 4x25G breakout: %s", got)
	}
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	s = r.snap()
	if s.System.SKU != "4x25G[10G]" || len(s.LayoutWritten) != 4 {
		t.Errorf("mode %q, written %v", s.System.SKU, s.LayoutWritten)
	}
	for _, p := range s.Ports[:4] {
		if p.IfName == "" || p.AdminSpeed != 25000 {
			t.Errorf("lane %d: %q at %d", p.Index, p.IfName, p.AdminSpeed)
		}
	}
	if got := r.apply(r.lanePush(map[int]int{2: 25000})); got != "" {
		t.Errorf("the next cycle wrote %s", got)
	}
	// Speeds of two modes at once: nothing, one note.
	if got := r.apply(r.lanePush(map[int]int{1: 50000, 2: 10000})); got != "" {
		t.Errorf("speeds of two modes wrote %s", got)
	}
	if !strings.Contains(r.log.String(), "lane 1 50000 Mb/s, lane 2 10000 Mb/s, which no one breakout mode") {
		t.Errorf("no note for two modes: %s", r.log.String())
	}
}

func TestNotAnswering(t *testing.T) {
	r := newRig(t)
	r.d.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := r.d.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "old") {
		t.Errorf("a stale parent: %v", err)
	}
	r.d.Now = nil
	o, _ := ParseOptions(map[string]string{"parent": "core", "cage": "1"})
	if _, err := New(o, r.c, r.c).Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "not split") {
		t.Errorf("a whole cage: %v", err)
	}
}

// bareParent is the split capture with no lane switch attached.
func bareParent(t *testing.T) *sonic.Collector {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "..", "docs", "fixtures", "sonic-igmp-202511.20261106-f2e25f54c", "collect-4x25g.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := sonic.NewCollector(fr)
	c.Log = log.New(&strings.Builder{}, "", 0)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

// An unattached lane switch answers (docs/drivers/breakout.md 16): the same
// snapshot as the attached one, under its own MAC and name, and it writes
// nothing. A cage attached to another MAC is refused.
func TestUnattached(t *testing.T) {
	ctx := context.Background()
	attached := newRig(t).snap()

	bare := bareParent(t)
	o, _ := ParseOptions(map[string]string{"parent": "core", "cage": "2", "mac": childMAC, "name": "lane-switch"})
	d := New(o, bare, bare)
	buf := &strings.Builder{}
	d.Log = log.New(buf, "", 0)
	snap, err := d.Collect(ctx)
	if err != nil {
		t.Fatalf("unattached: %v", err)
	}
	if snap.System.MAC != childMAC || snap.System.Hostname != "lane-switch" {
		t.Errorf("unattached identity: %s %q", snap.System.MAC, snap.System.Hostname)
	}
	snap.TakenAt, attached.TakenAt = time.Time{}, time.Time{}
	snap.System.Uptime, attached.System.Uptime = 0, 0
	snap.System.CPUPercent, attached.System.CPUPercent = 0, 0
	// The one difference: the cable is down until the lane switch is
	// attached (adopted).
	if u := snap.Ports[UplinkPort-1]; u.Up || u.SpeedMbps != 0 || u.Health.LinkChanges != 0 {
		t.Errorf("unattached, the uplink reads up %v %d Mb/s", u.Up, u.SpeedMbps)
	}
	snap.Ports[UplinkPort-1] = attached.Ports[UplinkPort-1]
	a, _ := json.Marshal(attached)
	b, _ := json.Marshal(snap)
	if string(a) != string(b) {
		t.Errorf("the unattached snapshot differs from the attached one:\n attached   %s\n unattached %s", a, b)
	}

	// Nothing is written while unattached.
	ds := []devicemodel.PortDesired{{Index: 1, Enabled: false, VLANSet: true, NativeVLAN: 1}}
	if got := d.PlanPorts(ds); got != nil {
		t.Errorf("PlanPorts unattached = %v, want nil", got)
	}
	if !strings.Contains(buf.String(), "not attached") {
		t.Errorf("no note for a push while unattached: %q", buf.String())
	}
	if _, err := d.ApplyPorts(ctx, ds); err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Errorf("ApplyPorts unattached: %v", err)
	}
	if _, err := d.EnsureVLANs(ctx, []int{7}); err == nil {
		t.Error("EnsureVLANs unattached: no error")
	}

	// The default MAC is main's: derived from the parent's.
	o, _ = ParseOptions(map[string]string{"parent": "core", "cage": "2"})
	snap, err = New(o, bare, bare).Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := bare.Lanes(2)
	want, _ := devicemodel.LaneSwitchMAC(v.System.MAC, 2)
	if snap.System.MAC != want || snap.System.Hostname != "core-cage2" {
		t.Errorf("default identity: %s %q, want %s core-cage2", snap.System.MAC, snap.System.Hostname, want)
	}

	// Attached to another MAC: this one does not answer.
	if err := bare.AttachLaneSwitch(2, devicemodel.Attachment{MAC: "02:00:00:00:00:bb", Name: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Collect(ctx); err == nil || !strings.Contains(err.Error(), "another lane switch") {
		t.Errorf("attached to another MAC: %v", err)
	}
	// Attached to this one: it applies again; detached: refused again.
	if err := bare.AttachLaneSwitch(2, devicemodel.Attachment{MAC: childMAC, Name: "lane-switch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplyPorts(ctx, nil); err != nil {
		t.Errorf("ApplyPorts attached: %v", err)
	}
	bare.DetachLaneSwitch(2)
	if _, err := d.ApplyPorts(ctx, ds); err == nil {
		t.Error("ApplyPorts after a detach: no error")
	}
}

// The push to the lane switch reaches the lanes; the uplink's settings and
// a mirror from outside the cage do not.
func TestApplyThroughTheParent(t *testing.T) {
	r := newRig(t)
	s := r.snap()
	var ds []devicemodel.PortDesired
	for _, p := range s.Ports {
		ds = append(ds, devicemodel.PortDesired{Index: p.Index, Enabled: true, VLANSet: true, NativeVLAN: 1})
	}
	ds[0].Description = "host-a"
	ds[1].MirrorSource = 5
	ds[4].Enabled, ds[4].Description = false, "cable"
	if got := r.d.PlanPorts(ds); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("PlanPorts %v", got)
	}
	n := len(r.fr.Recorded())
	if k, err := r.d.ApplyPorts(context.Background(), ds); err != nil || k != 1 {
		t.Fatalf("ApplyPorts: %d, %v", k, err)
	}
	got := r.fr.Recorded()[n:]
	if strings.Join(got, ";") != "sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet4' description 'host-a';sudo config save -y" {
		t.Errorf("wrote %q", got)
	}
	for _, want := range []string{"port 5 is the breakout cable; its settings (disabled, name \"cable\") are not applied", "lane 2: mirror source port 5 is not another lane of this cage"} {
		if !strings.Contains(r.log.String(), want) {
			t.Errorf("no note %q: %s", want, r.log.String())
		}
	}
}

func TestOptions(t *testing.T) {
	for _, bad := range []map[string]string{{"cage": "2"}, {"parent": "core"}, {"parent": "core", "cage": "0"}, {"parent": "core", "cage": "2", "mac": "nope"}, {"parent": "core", "cage": "2", "max_age": "-1s"}} {
		if _, err := ParseOptions(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	o, err := ParseOptions(map[string]string{"parent": "core", "cage": "2", "mac": "02:00:00:00:00:AB", "max_age": "1m"})
	if err != nil || o.MAC != "02:00:00:00:00:ab" || o.MaxAge != time.Minute {
		t.Errorf("%+v %v", o, err)
	}
	if _, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{Options: map[string]string{"parent": "nobody", "cage": "2"}}); err == nil || !strings.Contains(err.Error(), "not open") {
		t.Errorf("no parent: %v", err)
	}
	devicemodel.RegisterOpened("not-a-switch", notLanes{})
	defer devicemodel.UnregisterOpened("not-a-switch", notLanes{})
	if _, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{Options: map[string]string{"parent": "not-a-switch", "cage": "2"}}); err == nil || !strings.Contains(err.Error(), "cannot present lane switches") {
		t.Errorf("a parent without lanes: %v", err)
	}
}

type notLanes struct{}

func (notLanes) Collect(context.Context) (*devicemodel.Snapshot, error) { return nil, nil }
func (notLanes) Start(context.Context) (*devicemodel.Snapshot, error)   { return nil, nil }
func (notLanes) Close() error                                           { return nil }

// The parent's MAC and the cage, which main names the lane switch by in
// UniFi (docs/drivers/breakout.md 16.14): the parent's own MAC from the
// view, not the lane switch's.
func TestParentMACAndCage(t *testing.T) {
	r := newRig(t)
	if got := New(r.d.opt, r.c, r.c).ParentMAC(); got != "" {
		t.Errorf("before a collect: %q", got)
	}
	s := r.snap()
	parent, err := r.c.Lanes(2)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.d.ParentMAC(); got == "" || got != parent.System.MAC || got == s.System.MAC {
		t.Errorf("ParentMAC %q: want the parent's %q, not the lane switch's %q", got, parent.System.MAC, s.System.MAC)
	}
	if r.d.Cage() != 2 {
		t.Errorf("Cage %d", r.d.Cage())
	}
}

// A cage disabled on the parent takes the cable down with its lanes; its
// drops are the cable's own count, a lane's never (breakout.md 16.15).
func TestUplinkFollowsTheCagesAdminState(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if u := r.snap().Ports[UplinkPort-1]; !u.Up || !u.Enabled || u.Health.LinkChanges != 1 {
		t.Fatalf("attached: up %v enabled %v changes %d", u.Up, u.Enabled, u.Health.LinkChanges)
	}
	ps, err := r.c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	own := func(enabled bool) []devicemodel.PortDesired {
		var ds []devicemodel.PortDesired
		for _, p := range ps.Ports {
			d := devicemodel.PortDesired{Index: p.Index, Enabled: p.Enabled, VLANSet: true, NativeVLAN: 1}
			if p.Index == 2 {
				d.Enabled = enabled
			}
			ds = append(ds, d)
		}
		return ds
	}
	if _, err := r.c.ApplyPorts(ctx, own(false)); err != nil {
		t.Fatal(err)
	}
	u := r.snap().Ports[UplinkPort-1]
	if u.Up || u.Enabled || u.SpeedMbps != 0 || u.Health.LinkChanges != 2 {
		t.Errorf("the cage disabled: up %v enabled %v %d Mb/s changes %d, want down, disabled, one drop", u.Up, u.Enabled, u.SpeedMbps, u.Health.LinkChanges)
	}
	if _, err := r.c.ApplyPorts(ctx, own(true)); err != nil {
		t.Fatal(err)
	}
	if u := r.snap().Ports[UplinkPort-1]; !u.Up || !u.Enabled || u.Health.LinkChanges != 3 {
		t.Errorf("enabled again: up %v enabled %v changes %d, want up and 3", u.Up, u.Enabled, u.Health.LinkChanges)
	}
}
