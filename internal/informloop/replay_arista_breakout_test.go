package informloop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
	"github.com/TechBlueprints/disunified/internal/drivers/breakout"
	"github.com/jamesbraid/unifi-emu/inform"
)

// A lane switch on the Arista (docs/drivers/breakout.md §7b): the parent is
// the 7160 as captured on 2026-11-15 with its empty test cage 54 split to
// 4x25G (arista-eos-4.26.14M/split-cage54/), the child a breakout device on
// that cage in its own loop, and the child's push the controller's own for
// a Flex XG whose ports 2-4 were marked Aggregate in the UI
// (system_cfg-flexxg-aggregate.txt, the stub, 2026-11-02). The push must
// become channel group 1541 (laneBase(54)+1) on lanes 2-4 -- nothing on the
// parent's own ports -- and, once the switch is as the batch leaves it
// (split-cage54-flexxg/, the same batch run on the switch and captured),
// neither loop writes anything.

type aristaLaneRig struct {
	t      *testing.T
	ft     *aristaeos.FixtureTransport
	coll   *aristaeos.Collector
	parent *Loop
	child  *Loop
	csess  *device.Session
	lanes  *breakout.Device
	log    *strings.Builder
	seen   int
}

func aristaFixtures(sub string) string {
	return filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M", sub)
}

func newAristaLaneRig(t *testing.T) *aristaLaneRig {
	t.Helper()
	ft := aristaeos.NewFixtureTransport(aristaFixtures("split-cage54"))
	buf := &strings.Builder{}
	logger := log.New(buf, "", 0)
	coll := aristaeos.NewCollector(ft)
	coll.Log = logger
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := coll.AttachLaneSwitch(54, devicemodel.Attachment{MAC: laneSwitchMAC, Name: "lane-switch"}); err != nil {
		t.Fatal(err)
	}
	// The parent runs the last push the controller recorded for it
	// (10.6.106): it names no speed for port 54, so its own plan has
	// nothing for the cage but what it skips.
	pdesc := inform.Descriptor{MAC: snap.System.MAC}
	pst := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: "0000000000000001", SystemCfg: lastAristaPush(t)}
	psess := device.NewSession(pdesc, "http://192.0.2.1:8080/inform", pst, nil, time.Now())
	parent, err := New(pdesc, psess, Config{Logger: logger, Collector: coll, Controller: coll})
	if err != nil {
		t.Fatal(err)
	}
	r := &aristaLaneRig{t: t, ft: ft, coll: coll, parent: parent, log: buf}
	r.cycleParent("the capture")

	o, err := breakout.ParseOptions(map[string]string{"parent": "arista", "cage": "54"})
	if err != nil {
		t.Fatal(err)
	}
	lanes := breakout.New(o, coll, coll)
	lanes.Log = logger
	csnap, err := lanes.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cdefaults := map[int][]string{}
	for _, p := range csnap.Ports {
		cdefaults[p.Index] = append(lanes.DefaultPortNames(p), fmt.Sprintf("Port %d", p.Index))
	}
	cdesc := inform.Descriptor{MAC: csnap.System.MAC}
	cst := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: "0000000000000001"}
	r.csess = device.NewSession(cdesc, "http://192.0.2.1:8080/inform", cst, nil, time.Now())
	r.child, err = New(cdesc, r.csess, Config{Logger: logger, Collector: lanes, Controller: lanes, DefaultPortNames: cdefaults, AllowInitialChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	r.lanes = lanes
	return r
}

// lastAristaPush is the last system_cfg in the Arista's recorded replies.
func lastAristaPush(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106", "replies.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec replayRecord
		if json.Unmarshal([]byte(ln), &rec) != nil {
			continue
		}
		var p map[string]any
		if json.Unmarshal(rec.Payload, &p) != nil {
			continue
		}
		if s, ok := p["system_cfg"].(string); ok && s != "" {
			last = s
		}
	}
	if last == "" {
		t.Fatal("no system_cfg in the recorded replies")
	}
	return last
}

// writes is every batch configured since the last call, one string each.
func (r *aristaLaneRig) writes() []string {
	var out []string
	for _, b := range r.ft.Configured[r.seen:] {
		out = append(out, strings.Join(b, " | "))
	}
	r.seen = len(r.ft.Configured)
	return out
}

// cycleParent runs the parent's collect and reconcile: whatever its own
// (stale) push asks of its own ports, nothing may reach cage 54's lanes.
func (r *aristaLaneRig) cycleParent(after string) {
	r.t.Helper()
	r.parent.collect(context.Background())
	r.parent.reconcile(context.Background())
	for _, w := range r.writes() {
		if strings.Contains(w, "Ethernet54/") || strings.Contains(w, "Port-Channel15") || strings.Contains(w, "monitor session 15") {
			r.t.Errorf("the parent's reconcile after %s touched the lane switch's cage: %q", after, w)
		}
	}
}

func (r *aristaLaneRig) cycleChild(after string) {
	r.t.Helper()
	r.child.collect(context.Background())
	r.child.reconcile(context.Background())
	if got := r.writes(); len(got) != 0 {
		r.t.Errorf("the lane switch's reconcile after %s wrote %q", after, got)
	}
}

func (r *aristaLaneRig) pushChild(file, ver string) []string {
	r.t.Helper()
	r.child.collect(context.Background())
	body, _ := json.Marshal(map[string]string{"_type": "setparam", "system_cfg": readECSPush(r.t, file), "mgmt_cfg": "cfgversion=" + ver + "\n"})
	r.csess.Apply(time.Now(), body)
	if !r.child.applyPending(context.Background()) {
		r.t.Fatalf("%s was not consumed", file)
	}
	return r.writes()
}

// afterFlexXG serves the switch as the aggregate batch left it.
func (r *aristaLaneRig) afterFlexXG() { r.ft.Dir = aristaFixtures("split-cage54-flexxg") }

func TestReplayAristaLaneSwitchAggregate(t *testing.T) {
	r := newAristaLaneRig(t)

	snap, err := r.coll.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var cage devicemodel.Port
	for _, p := range snap.Ports {
		if p.Index == 54 {
			cage = p
		}
	}
	if n := cage.Neighbor; n == nil || !n.Child || n.ChassisID != laneSwitchMAC {
		t.Errorf("cage 54's neighbour: %+v", cage.Neighbor)
	}
	if snap.UplinkPort() == 54 {
		t.Errorf("the lane switch became the parent's uplink")
	}

	got := r.pushChild("system_cfg-flexxg-aggregate.txt", "d4a1a0cac0f5bb83")
	want := []string{aristaFlexXGBatch}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the aggregate push wrote\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if v, _, ok := r.csess.Applied(); !ok || v != "d4a1a0cac0f5bb83" {
		t.Errorf("the lane switch reports cfgversion %q (applied %v)", v, ok)
	}
	// Before the next collect the marked lanes plan nothing.
	r.child.reconcile(context.Background())
	if got := r.writes(); len(got) != 0 {
		t.Errorf("a second apply before the collect wrote %q", got)
	}

	// The switch as the batch left it: both loops' next cycle writes
	// nothing for the lanes; the lanes report the aggregate.
	r.afterFlexXG()
	r.cycleParent("the lane aggregate")
	r.cycleChild("the lane aggregate")
	csnap, err := r.lanes.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range csnap.Ports {
		want := 0
		if p.Index >= 2 && p.Index <= 4 {
			want = 1
		}
		if p.LAGID != want {
			t.Errorf("lane switch port %d: LAGID %d, want %d", p.Index, p.LAGID, want)
		}
	}
}

// The parent joins the cage (100G on port 54): the lane aggregate's members
// leave it first, then lane 1 takes the cage speed, and the lane switch
// stops answering once the cage is whole.
func TestReplayAristaLaneSwitchJoin(t *testing.T) {
	r := newAristaLaneRig(t)
	r.afterFlexXG()
	r.parent.collect(context.Background())
	r.writes()
	n, err := r.coll.ApplyPorts(context.Background(), joinCage54(t, r))
	if err != nil {
		t.Fatal(err)
	}
	got := r.writes()
	if n == 0 || len(got) != 1 {
		t.Fatalf("the join wrote %q (%d)", got, n)
	}
	head := "enable | configure | interface Ethernet54/2 | no channel-group | interface Ethernet54/3 | no channel-group | interface Ethernet54/4 | no channel-group | interface Ethernet54/1 | speed forced 100gfull"
	if !strings.HasPrefix(got[0], head) {
		t.Errorf("the join wrote\n%s\nwant it to start\n%s", got[0], head)
	}
	r.ft.Dir = aristaFixtures("") // the cage whole again (the 2026-09-19 capture)
	r.parent.collect(context.Background())
	if _, err := r.lanes.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "not split") {
		t.Errorf("the lane switch after the join: %v", err)
	}
}

// joinCage54 is the parent's own desired ports as the snapshot has them,
// with port 54 forced to 100G: the join as the controller pushes it.
func joinCage54(t *testing.T, r *aristaLaneRig) []devicemodel.PortDesired {
	t.Helper()
	snap, err := r.coll.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []devicemodel.PortDesired
	for _, p := range snap.Ports {
		d := devicemodel.PortDesired{Index: p.Index, Enabled: p.Enabled, Description: p.Description, BPDUGuard: p.BPDUGuard, STPEdge: p.STPEdge, StormCtrl: p.StormCtrl, LAG: p.LAGID, MirrorSource: p.MirrorFrom}
		if p.AdminSpeed > 0 {
			d.SpeedMbps = p.AdminSpeed
		}
		if p.Index == 54 {
			d.SpeedMbps = 100000
		}
		out = append(out, d)
	}
	return out
}

// aristaFlexXGBatch is the aggregate push as one EOS batch: lanes 2-4 into
// channel group 1541, the push's cl-91 as RS (offered on the 7160's 25G
// lanes: `show interfaces error-correction` lists Reed-Solomon and
// Fire-code), the Port-Channel Allow All (the push names no VLANs), lane 1
// and the cable port untouched, the names the profile's defaults. The same
// lines, run on the switch after the split on 2026-11-15 in a commit-timer
// session, were all accepted; split-cage54-flexxg is the switch then.
const aristaFlexXGBatch = "enable | configure | " +
	"interface Ethernet54/2 | channel-group 1541 mode active | error-correction encoding reed-solomon | " +
	"interface Ethernet54/3 | channel-group 1541 mode active | error-correction encoding reed-solomon | " +
	"interface Ethernet54/4 | channel-group 1541 mode active | error-correction encoding reed-solomon | " +
	"interface Port-Channel1541 | switchport mode trunk | no switchport trunk native vlan | switchport trunk allowed vlan all | " +
	"end | write memory"
