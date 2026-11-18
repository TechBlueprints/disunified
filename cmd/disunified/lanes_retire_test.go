package main

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/config"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	aristaeos "github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/TechBlueprints/disunified/internal/unifiapi"
)

// Retire on join (docs/drivers/breakout.md 16.13), against the DX010
// captured with cage 3 split (collect-split-dac.txt): the cage is joined
// and split again through the parent's own plan (the fixture runner's model
// of the breakout), and every read the watcher counts is a collect of its
// own.

// retireRig is the captured parent with the watcher over it, a clock the
// test moves, and the Forget's requests recorded.
type retireRig struct {
	t       *testing.T
	c       *sonic.Collector
	snap    *devicemodel.Snapshot
	al      *autoLanes
	started *[]options
	log     *strings.Builder
	f       *config.File
	now     time.Time
	forgets []string
	// answers are the Forget's answers in order; the last repeats.
	answers []forgetAnswer
}

type forgetAnswer struct {
	res unifiapi.ForgetResult
	err error
}

func newRetireRig(t *testing.T, parent config.Device, extra ...config.Device) *retireRig {
	t.Helper()
	c, snap, _ := laneParent(t)
	r := &retireRig{t: t, c: c, snap: snap, now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	r.al, r.started, r.log, r.f = watcher(t, c, snap, parent, extra...)
	r.answers = []forgetAnswer{{res: unifiapi.Forgotten}}
	r.al.now = func() time.Time { return r.now }
	r.al.forget = func(_ context.Context, mac string) (unifiapi.ForgetResult, string, error) {
		r.forgets = append(r.forgets, mac)
		a := r.answers[0]
		if len(r.answers) > 1 {
			r.answers = r.answers[1:]
		}
		return a.res, "HTTP 200, rc ok", a.err
	}
	return r
}

func (r *retireRig) mac(cage int) string {
	m, err := devicemodel.LaneSwitchMAC(r.snap.System.MAC, cage)
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

// speed applies a forced speed on cage 3 through the parent's own plan
// (100G joins it, 25G splits it) and collects.
func (r *retireRig) speed(mbps int) {
	r.t.Helper()
	ctx := context.Background()
	snap, err := r.c.Collect(ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	var ds []devicemodel.PortDesired
	for _, p := range snap.Ports {
		d := devicemodel.PortDesired{Index: p.Index, Enabled: p.Enabled, Description: p.Description, VLANSet: true, NativeVLAN: 1}
		if p.Index == 3 {
			d.SpeedMbps = mbps
		}
		ds = append(ds, d)
	}
	if _, err := r.c.ApplyPorts(ctx, ds); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.c.Collect(ctx); err != nil {
		r.t.Fatal(err)
	}
}

// read is one more collect of the parent, the clock 40 s on, and a tick.
func (r *retireRig) read() { r.t.Helper(); r.readAfter(40 * time.Second) }

// readAfter is read with the clock d on.
func (r *retireRig) readAfter(d time.Duration) {
	r.t.Helper()
	if _, err := r.c.Collect(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	r.now = r.now.Add(d)
	r.al.tick()
}

// tick is a tick with no collect (the parent not answering), the clock d on.
func (r *retireRig) tick(d time.Duration) {
	r.now = r.now.Add(d)
	r.al.tick()
}

func (r *retireRig) halted(i int) bool {
	select {
	case <-(*r.started)[i].halt:
		return true
	default:
		return false
	}
}

func (r *retireRig) stateExists(name string) bool {
	_, err := os.Stat(filepath.Join(r.f.StateDir, "state", name))
	return err == nil
}

func (r *retireRig) child(cage int) string {
	v, err := r.c.Lanes(cage)
	if err != nil {
		return ""
	}
	return v.Child.Name
}

// The whole sequence: an adopted automatic lane switch, its cage joined by
// a forced 100G on the parent's port. Nothing until the fourth joined read
// two minutes on; then it is stopped, the lanes handed back, its record
// forgotten by its exact MAC once, its state directory removed; the
// parent's plan owns the joined cage and writes nothing more. A later split
// is a fresh lane switch: pending, then adopted by the bridge's own request.
func TestRetireOnJoin(t *testing.T) {
	r := newRetireRig(t, config.Device{})
	adopt(t, r.f, "sw")
	adopt(t, r.f, "sw-cage3")
	if err := os.WriteFile(filepath.Join(r.f.StateDir, "state", "sw-cage3", "layout.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var adopts []string
	r.al.adopt = func(_ context.Context, mac string) error { adopts = append(adopts, mac); return nil }
	r.al.startAdopted()
	if len(*r.started) != 1 || r.child(3) != "sw-cage3" {
		t.Fatalf("started %d, child %q: want sw-cage3 attached", len(*r.started), r.child(3))
	}
	r.al.tick()

	r.speed(100000) // the join
	for i := 1; i <= 3; i++ {
		r.read()
		if r.halted(0) || len(r.forgets) != 0 {
			t.Fatalf("retired after %d joined reads", i)
		}
	}
	r.read()
	if !r.halted(0) {
		t.Fatalf("not stopped after four joined reads over two minutes; log:\n%s", r.log)
	}
	if want := []string{r.mac(3)}; !slices.Equal(r.forgets, want) {
		t.Errorf("forgets %q, want %q (its exact MAC, once)", r.forgets, want)
	}
	if r.stateExists("sw-cage3") {
		t.Error("the lane switch's state directory (device.json, layout.json) is still there")
	}
	if !r.stateExists("sw") {
		t.Error("the parent's state went too")
	}
	for _, want := range []string{"is stopped", "detached from port 3", "forgotten in UniFi by its exact MAC", "state directory removed"} {
		if !strings.Contains(r.log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, r.log)
		}
	}

	// The parent owns the joined cage: a reconcile writes nothing.
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ds []devicemodel.PortDesired
	for _, p := range snap.Ports {
		d := devicemodel.PortDesired{Index: p.Index, Enabled: p.Enabled, Description: p.Description, VLANSet: true, NativeVLAN: 1}
		if p.Index == 3 {
			d.SpeedMbps = 100000
		}
		ds = append(ds, d)
	}
	if n, err := r.c.ApplyPorts(context.Background(), ds); err != nil || n != 0 {
		t.Errorf("the parent's reconcile after the retire changed %d ports (%v)", n, err)
	}

	// Joined: nothing more, no adoption asked.
	for i := 0; i < 5; i++ {
		r.read()
	}
	if len(r.forgets) != 1 || len(*r.started) != 1 || len(adopts) != 0 {
		t.Errorf("while joined: %d forgets, %d started, %d adopts", len(r.forgets), len(*r.started), len(adopts))
	}

	// Split again: a fresh lane switch, pending, then asked for.
	r.speed(25000)
	r.read()
	if len(*r.started) != 2 || (*r.started)[1].name != "sw-cage3" || r.child(3) != "" {
		t.Fatalf("after the split: %d started, child %q (want a fresh pending sw-cage3, unattached)", len(*r.started), r.child(3))
	}
	r.read()
	if !slices.Equal(adopts, []string{r.mac(3)}) {
		t.Errorf("adopts %q, want the fresh lane switch's MAC", adopts)
	}
}

// A split before the count is reached starts it again, and so does a gap of
// more than three minutes without a collect; ticks without a collect count
// nothing.
func TestRetireCountResets(t *testing.T) {
	r := newRetireRig(t, config.Device{})
	adopt(t, r.f, "sw-cage3")
	r.al.startAdopted()

	r.speed(100000)
	r.read()
	r.read()
	r.read()
	r.speed(25000) // split again before the fourth
	r.read()
	r.speed(100000)
	for i := 1; i <= 3; i++ {
		r.read()
	}
	if r.halted(0) {
		t.Fatal("retired on reads counted across a split")
	}
	// No collect for ten minutes: nothing.
	for i := 0; i < 40; i++ {
		r.tick(15 * time.Second)
	}
	if r.halted(0) || len(r.forgets) != 0 {
		t.Fatal("retired while the parent was not collecting")
	}
	// The gap breaks the run: four more reads are needed.
	for i := 1; i <= 3; i++ {
		r.read()
	}
	if r.halted(0) {
		t.Fatal("retired on reads counted across a gap of ten minutes")
	}
	r.read()
	if !r.halted(0) || len(r.forgets) != 1 {
		t.Errorf("not retired after four joined reads in a row (halted %v, forgets %d)", r.halted(0), len(r.forgets))
	}
}

// A split cage never retires anything, however long it runs; nor does a
// cage the parent cannot read (its port gone from the collect).
func TestSplitCageNeverRetires(t *testing.T) {
	r := newRetireRig(t, config.Device{})
	adopt(t, r.f, "sw-cage3")
	r.al.startAdopted()
	for i := 0; i < 20; i++ {
		r.read()
	}
	if r.halted(0) || len(r.forgets) != 0 {
		t.Error("a split cage's lane switch was retired")
	}
}

// The Forget refused while the update runs (api.err.DeviceBusy, 16.12): the
// state stays until the controller takes it, asked again at 30 s, the
// failure logged once.
func TestRetireForgetBusyThenTaken(t *testing.T) {
	r := newRetireRig(t, config.Device{})
	adopt(t, r.f, "sw-cage3")
	r.answers = []forgetAnswer{{err: unifiapi.ErrDeviceBusy}, {err: unifiapi.ErrDeviceBusy}, {res: unifiapi.Forgotten}}
	r.al.startAdopted()
	r.speed(100000)
	for i := 0; i < 4; i++ {
		r.read()
	}
	if len(r.forgets) != 1 || !r.stateExists("sw-cage3") {
		t.Fatalf("after a busy answer: %d forgets, state kept %v", len(r.forgets), r.stateExists("sw-cage3"))
	}
	r.tick(15 * time.Second)
	if len(r.forgets) != 1 {
		t.Errorf("asked again after 15 s (%d)", len(r.forgets))
	}
	r.tick(15 * time.Second) // 30 s after the first
	r.tick(30 * time.Second) // too soon after the second (the pause doubled to 60 s)
	r.tick(30 * time.Second)
	if len(r.forgets) != 3 || r.stateExists("sw-cage3") {
		t.Errorf("%d forgets, state kept %v: want the third taken and the state removed", len(r.forgets), r.stateExists("sw-cage3"))
	}
	if n := strings.Count(r.log.String(), "DeviceBusy"); n != 1 {
		t.Errorf("the busy answer logged %d times, want once:\n%s", n, r.log)
	}
	if len(*r.started) != 1 {
		t.Errorf("restarted while joined (%d)", len(*r.started))
	}
}

// A record the controller no longer holds (forgotten by hand, as on
// 2026-11-18) is nothing to forget: the state goes all the same.
func TestRetireNoRecord(t *testing.T) {
	r := newRetireRig(t, config.Device{})
	adopt(t, r.f, "sw-cage3")
	r.answers = []forgetAnswer{{res: unifiapi.NoRecord}}
	r.al.startAdopted()
	r.speed(100000)
	for i := 0; i < 4; i++ {
		r.read()
	}
	if r.stateExists("sw-cage3") || !strings.Contains(r.log.String(), "holds no record") {
		t.Errorf("state kept %v; log:\n%s", r.stateExists("sw-cage3"), r.log)
	}
}

// lane_switches: manual, and a file with no controller API: stopped and
// handed back, the state and the record left to the operator; a split
// starts it again from its state file, attached.
func TestRetireManualAndNoAPIStopOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent config.Device
		noAPI  bool
		want   string
	}{
		{"manual", config.Device{LaneSwitches: config.LaneSwitchesManual}, false, "lane_switches: manual"},
		{"no API", config.Device{}, true, "no controller API"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRetireRig(t, tc.parent)
			if tc.noAPI {
				r.al.forget = nil
			}
			adopt(t, r.f, "sw-cage3")
			r.al.startAdopted()
			r.speed(100000)
			for i := 0; i < 4; i++ {
				r.read()
			}
			if !r.halted(0) || len(r.forgets) != 0 || !r.stateExists("sw-cage3") {
				t.Fatalf("halted %v, forgets %d, state kept %v: want stopped, nothing forgotten, the state kept", r.halted(0), len(r.forgets), r.stateExists("sw-cage3"))
			}
			if !strings.Contains(r.log.String(), tc.want) {
				t.Errorf("log lacks %q:\n%s", tc.want, r.log)
			}
			r.read()
			if len(*r.started) != 1 {
				t.Errorf("started again while joined (%d)", len(*r.started))
			}
			r.speed(25000)
			r.read()
			if len(*r.started) != 2 || r.child(3) != "sw-cage3" {
				t.Errorf("after the split: %d started, child %q (want it resumed from its state, attached)", len(*r.started), r.child(3))
			}
		})
	}
}

// A declared lane switch (driver: breakout) is stopped while its cage is
// joined, stays attached, keeps its state and its record, and is started
// again when the cage is split.
func TestRetireDeclaredStopsOnly(t *testing.T) {
	declared := config.Device{Name: "rack-lanes", Driver: config.LaneDriver, Options: map[string]string{"parent": "sw", "cage": "3"}}
	r := newRetireRig(t, config.Device{}, declared)
	adopt(t, r.f, "rack-lanes")
	hooks := declaredLanes(r.f)
	d := hooks["rack-lanes"]
	halt, exited := d.arm()
	go func() { <-halt; close(exited) }()
	respawns := 0
	d.respawn = func() { respawns++ }
	attachLaneSwitches(r.al.log, r.c, r.snap, r.f.LaneSwitchesOf("sw"), nil)
	r.al.bindDeclared(r.f.LaneSwitchesOf("sw"), hooks)
	r.al.startAdopted()
	if len(*r.started) != 0 || r.child(3) != "rack-lanes" {
		t.Fatalf("%d automatic started, child %q", len(*r.started), r.child(3))
	}
	r.speed(100000)
	for i := 0; i < 3; i++ {
		r.read()
	}
	select {
	case <-halt:
		t.Fatal("stopped after three reads")
	default:
	}
	r.read()
	select {
	case <-halt:
	default:
		t.Fatalf("a declared lane switch on a joined cage was not stopped; log:\n%s", r.log)
	}
	if len(r.forgets) != 0 || !r.stateExists("rack-lanes") {
		t.Errorf("forgets %d, state kept %v: a declared one's record and state are the operator's", len(r.forgets), r.stateExists("rack-lanes"))
	}
	if !strings.Contains(r.log.String(), "declares it, so it stays attached") {
		t.Errorf("log:\n%s", r.log)
	}
	for i := 0; i < 3; i++ {
		r.read()
	}
	if respawns != 0 || len(*r.started) != 0 {
		t.Errorf("started again while joined (%d respawns, %d automatic)", respawns, len(*r.started))
	}
	r.speed(25000)
	r.read()
	if respawns != 1 || r.child(3) != "rack-lanes" || len(*r.started) != 0 {
		t.Errorf("after the split: %d respawns, child %q, %d automatic", respawns, r.child(3), len(*r.started))
	}
}

// The start-up case: a bridge started with an adopted lane switch's state
// file and its cage already joined. It is not attached or started; the
// first snapshot is its first joined read, and three more over two minutes
// retire it. A cage split at the start is resumed as before.
func TestRetireAtStart(t *testing.T) {
	r := newRetireRig(t, config.Device{})
	r.speed(100000) // joined before this "start" (the watcher is new)
	adopt(t, r.f, "sw-cage3")
	r.al.startAdopted()
	if len(*r.started) != 0 || r.child(3) != "" {
		t.Fatalf("started %d, child %q: a joined cage's lane switch is not started at the start", len(*r.started), r.child(3))
	}
	if !strings.Contains(r.log.String(), "not attached or started") {
		t.Errorf("log:\n%s", r.log)
	}
	r.tick(15 * time.Second) // no collect yet: nothing
	for i := 1; i <= 2; i++ {
		r.read()
	}
	if len(r.forgets) != 0 {
		t.Fatal("retired before the count")
	}
	r.read()
	r.read()
	if !slices.Equal(r.forgets, []string{r.mac(3)}) || r.stateExists("sw-cage3") {
		t.Errorf("forgets %q, state kept %v", r.forgets, r.stateExists("sw-cage3"))
	}
	if len(*r.started) != 0 {
		t.Errorf("started %d while joined", len(*r.started))
	}

	// Joined at the start, then split before the count: started from its
	// state file, attached.
	r2 := newRetireRig(t, config.Device{})
	r2.speed(100000)
	adopt(t, r2.f, "sw-cage3")
	r2.al.startAdopted()
	r2.read()
	r2.speed(25000)
	r2.read()
	if len(*r2.started) != 1 || r2.child(3) != "sw-cage3" || len(r2.forgets) != 0 {
		t.Errorf("split after a joined start: %d started, child %q, %d forgets", len(*r2.started), r2.child(3), len(r2.forgets))
	}
}

// The Arista parent: the same watcher over the 7160 with cage 50 split by
// the module rule and then joined by a forced 100G (module-layout-20261117/).
func TestRetireOnJoinArista(t *testing.T) {
	dir := func(state string) string {
		return filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M", "module-layout-20261117", state)
	}
	ft := aristaeos.NewFixtureTransport(dir("split-by-rule"))
	c := aristaeos.NewCollector(ft)
	c.Log = log.New(io.Discard, "", 0)
	ctx := context.Background()
	snap, err := c.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	al, started, buf, f := watcher(t, c, snap, config.Device{})
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	al.now = func() time.Time { return now }
	var forgets []string
	al.forget = func(_ context.Context, mac string) (unifiapi.ForgetResult, string, error) {
		forgets = append(forgets, mac)
		return unifiapi.Forgotten, "HTTP 200, rc ok", nil
	}
	adopt(t, f, "sw-cage50")
	al.startAdopted()
	if len(*started) != 1 {
		t.Fatalf("started %d", len(*started))
	}
	var ds []devicemodel.PortDesired
	for _, p := range snap.Ports {
		if p.Index == 50 {
			ds = append(ds, devicemodel.PortDesired{Index: 50, Enabled: p.Enabled, SpeedMbps: 100000})
		}
	}
	if n, err := c.ApplyPorts(ctx, ds); err != nil || n == 0 {
		t.Fatalf("the join: %d, %v", n, err)
	}
	al.tick() // the batch's own read: not a joined one
	ft.Dir = dir("joined")
	for i := 1; i <= 4; i++ {
		if _, err := c.Collect(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(40 * time.Second)
		al.tick()
		if i < 4 && len(forgets) != 0 {
			t.Fatalf("retired after %d reads", i)
		}
	}
	mac, _ := devicemodel.LaneSwitchMAC(snap.System.MAC, 50)
	if !slices.Equal(forgets, []string{mac}) {
		t.Errorf("forgets %q, want [%s]; log:\n%s", forgets, mac, buf)
	}
	if _, err := os.Stat(filepath.Join(f.StateDir, "state", "sw-cage50")); err == nil {
		t.Error("state kept")
	}
	select {
	case <-(*started)[0].halt:
	default:
		t.Error("not stopped")
	}
}

// Both halves of the count decide: a parent collecting every 10 s needs two
// minutes of reads, not four; one collecting every 70 s needs four reads,
// not two minutes.
func TestRetireNeedsBothTheReadsAndTheSpan(t *testing.T) {
	r := newRetireRig(t, config.Device{})
	adopt(t, r.f, "sw-cage3")
	r.al.startAdopted()
	r.speed(100000)
	for i := 1; i <= 12; i++ { // the 12th read is 110 s after the first
		r.readAfter(10 * time.Second)
	}
	if r.halted(0) {
		t.Fatal("retired on twelve reads over 110 s")
	}
	r.readAfter(10 * time.Second) // 120 s
	if !r.halted(0) {
		t.Fatal("not retired at two minutes of reads")
	}

	r = newRetireRig(t, config.Device{})
	adopt(t, r.f, "sw-cage3")
	r.al.startAdopted()
	r.speed(100000)
	for i := 1; i <= 3; i++ { // 140 s from the first to the third
		r.readAfter(70 * time.Second)
	}
	if r.halted(0) {
		t.Fatal("retired on three reads")
	}
	r.readAfter(70 * time.Second)
	if !r.halted(0) {
		t.Fatal("not retired on the fourth read")
	}
}
