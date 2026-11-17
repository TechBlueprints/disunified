package main

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/config"
	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// Automatic lane switches (docs/drivers/breakout.md 16), against the DX010
// captured on 2026-11-16 with cage 3 split (collect-split-dac.txt).

const splitDAC = "../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/collect-split-dac.txt"

// laneParent is the captured switch, started, and what a cage-3 push with a
// name on it would change on it: [3] while the cage is the device's fold,
// nothing once a lane switch has the lanes.
func laneParent(t *testing.T) (*sonic.Collector, *devicemodel.Snapshot, func() []int) {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(splitDAC)
	if err != nil {
		t.Fatal(err)
	}
	c := sonic.NewCollector(fr)
	c.Log = log.New(io.Discard, "", 0)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan := func() []int {
		return c.PlanPorts([]devicemodel.PortDesired{{Index: 3, Enabled: true, Description: "to-the-rack", VLANSet: true, NativeVLAN: 1}})
	}
	return c, snap, plan
}

// watcher is newAutoLanes over a file with one parent entry "sw" and the
// extra entries given, its starts recorded instead of run.
func watcher(t *testing.T, c *sonic.Collector, snap *devicemodel.Snapshot, parent config.Device, extra ...config.Device) (*autoLanes, *[]options, *strings.Builder, *config.File) {
	t.Helper()
	parent.Name, parent.Driver = "sw", "sonic"
	if parent.LaneSwitches == "" {
		parent.LaneSwitches = config.LaneSwitchesAuto
	}
	f := &config.File{StateDir: t.TempDir(), Devices: append([]config.Device{parent}, extra...)}
	var started []options
	mk := newAutoLanes(f, parent, func(o options) { started = append(started, o) })
	if mk == nil {
		return nil, &started, nil, f
	}
	buf := &strings.Builder{}
	return mk(log.New(buf, "", 0), c, snap.System.MAC), &started, buf, f
}

func adopt(t *testing.T, f *config.File, name string) {
	t.Helper()
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef"}
	if err := (&device.Store{Path: filepath.Join(f.StateDir, "state", name, "device.json")}).Save(st); err != nil {
		t.Fatal(err)
	}
}

// A split cage gets one pending lane switch: started once, unattached, the
// parent's plan for the cage untouched.
func TestWatcherStartsOnePendingLaneSwitchPerSplitCage(t *testing.T) {
	c, snap, plan := laneParent(t)
	al, started, buf, f := watcher(t, c, snap, config.Device{Control: config.Control{Ports: "off"}})
	al.startAdopted()
	if len(*started) != 0 {
		t.Fatalf("no state file is adopted, yet %d started before the loop", len(*started))
	}
	al.tick()
	al.tick()
	if len(*started) != 1 {
		t.Fatalf("started %d lane switches, want 1 (cage 3, once)", len(*started))
	}
	o := (*started)[0]
	wantMAC, _ := devicemodel.LaneSwitchMAC(snap.System.MAC, 3)
	wantOpts := map[string]string{"parent": "sw", "cage": "3", "mac": wantMAC, "name": "sw-cage3"}
	if o.name != "sw-cage3" || o.hostname != "sw-cage3" || o.driver != config.LaneDriver || o.model != config.LaneModel || !o.laneDriver || !reflect.DeepEqual(o.driverOptions, wantOpts) {
		t.Errorf("child options: name %q hostname %q driver %q model %q lane %v options %v", o.name, o.hostname, o.driver, o.model, o.laneDriver, o.driverOptions)
	}
	if o.controlPorts != "off" {
		t.Errorf("control.ports %q, want the parent's (off)", o.controlPorts)
	}
	if o.autoLanes != nil {
		t.Error("a lane switch must not start lane switches of its own")
	}
	// A missed Forget (refused key) releases an automatic lane switch,
	// and nothing else (docs/drivers/breakout.md 16.11).
	if !o.forgetOnRejectedKey || configOptions(f, f.Devices[0]).forgetOnRejectedKey {
		t.Errorf("forget on a refused key: lane switch %v, parent %v; want true, false", o.forgetOnRejectedKey, configOptions(f, f.Devices[0]).forgetOnRejectedKey)
	}
	if v, err := c.Lanes(3); err != nil || v.Child.MAC != "" {
		t.Errorf("a pending lane switch is attached: %+v, %v", v.Child, err)
	}
	if got := plan(); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("pending: the parent's plan changes %v, want [3] (the cage is still its fold)", got)
	}
	if !strings.Contains(buf.String(), "sw-cage3") || !strings.Contains(buf.String(), "pending") {
		t.Errorf("log: %q", buf.String())
	}

	// The hand-over: the child's loop calls these on adoption and forget.
	o.onAdopted()
	if v, _ := c.Lanes(3); v.Child.MAC != wantMAC || v.Child.Name != "sw-cage3" {
		t.Errorf("after the adoption hook: child %+v", v.Child)
	}
	if got := plan(); len(got) != 0 {
		t.Errorf("adopted: the parent's plan changes %v, want nothing", got)
	}
	if !strings.Contains(buf.String(), "no controller API is configured") {
		t.Errorf("no hand-over note without an API: %q", buf.String())
	}
	o.onUnadopted()
	if v, _ := c.Lanes(3); v.Child.MAC != "" {
		t.Errorf("after the forget hook: child %+v", v.Child)
	}
	if got := plan(); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("forgotten: the parent's plan changes %v, want [3]", got)
	}
}

// An adopted lane switch is attached before the parent's loop starts: the
// parent's first plan leaves the lanes alone.
func TestAdoptedLaneSwitchIsAttachedBeforeTheParentsFirstPlan(t *testing.T) {
	c, snap, plan := laneParent(t)
	al, started, _, f := watcher(t, c, snap, config.Device{})
	adopt(t, f, "sw-cage3")
	adopt(t, f, "sw-cage7") // a whole cage's: attached too, it answers when the cage is split again
	al.startAdopted()
	if len(*started) != 2 || (*started)[0].name != "sw-cage3" || (*started)[1].name != "sw-cage7" {
		t.Fatalf("started %d, want sw-cage3 and sw-cage7", len(*started))
	}
	if got := plan(); len(got) != 0 {
		t.Errorf("the parent's first plan changes %v, want nothing (the lanes are the adopted lane switch's)", got)
	}
	al.tick()
	if len(*started) != 2 {
		t.Errorf("the watcher started cage 3 a second time (%d running)", len(*started))
	}
}

// None for a declared cage, for a name another entry has, or with
// lane_switches: off.
func TestWatcherStartsNoneWhereItShouldNot(t *testing.T) {
	c, snap, _ := laneParent(t)

	declared := config.Device{Name: "rack-lanes", Driver: config.LaneDriver, Options: map[string]string{"parent": "sw", "cage": "3"}}
	al, started, _, f := watcher(t, c, snap, config.Device{}, declared)
	adopt(t, f, "sw-cage3") // a leftover state file does not beat the declaration
	al.startAdopted()
	al.tick()
	if len(*started) != 0 {
		t.Errorf("a declared cage got an automatic lane switch (%d)", len(*started))
	}

	clash := config.Device{Name: "sw-cage3", Driver: "sonic", SSH: "admin@192.0.2.9"}
	al, started, buf, _ := watcher(t, c, snap, config.Device{}, clash)
	al.tick()
	al.tick()
	if len(*started) != 0 || strings.Count(buf.String(), "another entry") != 1 {
		t.Errorf("a name clash: %d started, log %q", len(*started), buf.String())
	}

	if al, _, _, _ := watcher(t, c, snap, config.Device{LaneSwitches: config.LaneSwitchesOff}); al != nil {
		t.Error("lane_switches: off still made a watcher")
	}
	if mk := newAutoLanes(&config.File{}, config.Device{Name: "x", Driver: config.LaneDriver, LaneSwitches: config.LaneSwitchesAuto}, nil); mk != nil {
		t.Error("a lane switch got a watcher of its own")
	}
}

// The hand-over (16.7): on the attach the cage's port goes to Auto in the
// controller when it held a forced lane speed, said once with what it was.
func TestHandOverSetsTheCageToAuto(t *testing.T) {
	c, snap, _ := laneParent(t)
	al, started, buf, _ := watcher(t, c, snap, config.Device{})
	var asked []int
	al.cageToAuto = func(_ context.Context, cage int) (map[string]any, error) {
		asked = append(asked, cage)
		return map[string]any{"autoneg": false, "speed": float64(25000), "full_duplex": true, "fec_mode": "fc-fec"}, nil
	}
	al.tick()
	if len(asked) != 0 {
		t.Fatalf("a pending lane switch changed the controller (%v)", asked)
	}
	(*started)[0].onAdopted()
	if !reflect.DeepEqual(asked, []int{3}) {
		t.Errorf("asked for %v, want [3]", asked)
	}
	if !strings.Contains(buf.String(), "port 3: set to Auto in UniFi (was autoneg false, speed 25000, full_duplex true, fec_mode fc-fec)") {
		t.Errorf("log: %q", buf.String())
	}
	// Nothing to change: nothing said.
	al.cageToAuto = func(context.Context, int) (map[string]any, error) { return nil, nil }
	n := strings.Count(buf.String(), "Auto")
	(*started)[0].onAdopted()
	if strings.Count(buf.String(), "Auto") != n {
		t.Errorf("a cage already on Auto was logged: %q", buf.String())
	}
}
