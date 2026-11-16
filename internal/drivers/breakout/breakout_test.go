package breakout

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"reflect"
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
	if len(sys.Addresses) != 1 || sys.Addresses[0].Iface != "eth0" || sys.Addresses[0].IP == "" {
		t.Errorf("addresses %+v (the parent's, as the stub reported its own)", sys.Addresses)
	}
	for i, p := range s.Ports[:4] {
		if p.Index != i+1 || p.Media != devicemodel.MediaSFP28 || p.Optic != nil || !reflect.DeepEqual(p.SpeedCaps, []int{1000, 10000, 25000}) || p.IfName == "" {
			t.Errorf("lane %d: %+v", i+1, p)
		}
	}
	u := s.Ports[4]
	if u.Index != 5 || u.Media != devicemodel.MediaQSFP28 || !reflect.DeepEqual(u.SpeedCaps, []int{100000}) || u.Neighbor == nil || !u.Neighbor.IsBridge || u.Neighbor.Child || u.Neighbor.ChassisID != "02:00:00:00:00:01" {
		t.Errorf("uplink %+v neighbour %+v", u, u.Neighbor)
	}
	if u.Up {
		t.Errorf("the capture's lanes have no link, the cable reads up")
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
		t.Errorf("address %q, the parent's %q", a, sys.Addresses[0].IP)
	}
}

// 2x50G: the parent re-splits the cage (its own speed control), the lane
// switch then has two lanes at 50G, drawn as QSFP tiles, and two empty
// slots.
func TestSnapshotTwoLanes(t *testing.T) {
	r := newRig(t)
	snap, _ := r.c.Collect(context.Background())
	var ds []devicemodel.PortDesired
	for _, p := range snap.Ports {
		d := devicemodel.PortDesired{Index: p.Index, Enabled: p.Enabled, VLANSet: true, NativeVLAN: 1}
		if p.Index == 2 {
			d.SpeedMbps = 50000
		}
		ds = append(ds, d)
	}
	if _, err := r.c.ApplyPorts(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.fr.Recorded(), "\n"), "breakout -f -y Ethernet4 '2x50G'") {
		t.Fatalf("no 2x50G breakout: %q", r.fr.Recorded())
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := r.snap()
	for _, p := range s.Ports[:2] {
		if p.Media != devicemodel.MediaQSFP28 || p.AdminSpeed != 50000 || p.IfName == "" {
			t.Errorf("lane %d: media %s admin %d %q", p.Index, p.Media, p.AdminSpeed, p.IfName)
		}
	}
	for _, p := range s.Ports[2:4] {
		if p.IfName != "" || p.Present || p.Up || p.SpeedCaps != nil {
			t.Errorf("slot %d is not empty: %+v", p.Index, p)
		}
	}
	if s.System.SKU != "2x50G" {
		t.Errorf("mode %q", s.System.SKU)
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
