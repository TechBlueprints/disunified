package informloop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/breakout"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// A lane switch end to end (docs/drivers/breakout.md): the parent is the
// DX010 as captured on 2026-11-07 with cage 2 split to 4x25G
// (collect-4x25g.txt, following the push that split it), the child a
// breakout device on that cage in its own loop, and the child's push the
// controller's own for a Flex XG whose ports 2-4 were marked Aggregate in
// the UI (system_cfg-flexxg-aggregate.txt, captured from the stub on
// 2026-11-02, docs/breakout-lag.md 6.6). The push must become the parent's
// PortChannel on lanes 2-4 -- nothing else, nothing on the parent's own
// ports -- and the next cycle of both loops must write nothing.

const laneSwitchMAC = "02:00:00:00:00:aa"

type laneRig struct {
	t      *testing.T
	fr     *sonic.FixtureRunner
	coll   *sonic.Collector
	parent *Loop
	child  *Loop
	csess  *device.Session
	lanes  *breakout.Device
	log    *strings.Builder
	seen   int
}

func newLaneRig(t *testing.T) *laneRig {
	t.Helper()
	return newLaneRigWith(t, nil)
}

// newLaneRigWith: edit runs on the capture's runner before the parent
// starts; what it writes is not the loops'.
func newLaneRigWith(t *testing.T, edit func(*sonic.FixtureRunner)) *laneRig {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261106-f2e25f54c", "collect-4x25g.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(fr)
	}
	buf := &strings.Builder{}
	logger := log.New(buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	coll.L2 = false
	coll.IGMP = true
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// main attaches the lane switch before the parent's loop starts.
	if err := coll.AttachLaneSwitch(2, devicemodel.Attachment{MAC: laneSwitchMAC, Name: "lane-switch"}); err != nil {
		t.Fatal(err)
	}
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = coll.DefaultPortNames(p)
	}
	pdesc := inform.Descriptor{MAC: snap.System.MAC}
	pst := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: "81fdb670c3d4164c", SystemCfg: readECSPush(t, "system_cfg-dx010-brk-1-4x25g.txt")}
	psess := device.NewSession(pdesc, "http://192.0.2.1:8080/inform", pst, nil, time.Now())
	parent, err := New(pdesc, psess, Config{Logger: logger, Collector: coll, Controller: coll, DefaultPortNames: defaults, ControlIGMP: true})
	if err != nil {
		t.Fatal(err)
	}
	r := &laneRig{t: t, fr: fr, coll: coll, parent: parent, log: buf, seen: len(fr.Recorded())}
	r.cycleParent("the capture")

	o, err := breakout.ParseOptions(map[string]string{"parent": "core", "cage": "2", "mac": laneSwitchMAC, "name": "lane-switch"})
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
	// Adopted and following a push that matches the lanes as they are (the
	// controller seeded from them): the push of interest is the next one.
	cst := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: "0000000000000001"}
	r.csess = device.NewSession(cdesc, "http://192.0.2.1:8080/inform", cst, nil, time.Now())
	r.child, err = New(cdesc, r.csess, Config{Logger: logger, Collector: lanes, Controller: lanes, DefaultPortNames: cdefaults, AllowInitialChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	r.lanes = lanes
	return r
}

func (r *laneRig) writes() []string {
	c := r.fr.Recorded()
	out := c[r.seen:]
	r.seen = len(c)
	return out
}

func (r *laneRig) cycleParent(after string) {
	r.t.Helper()
	r.parent.collect(context.Background())
	r.parent.reconcile(context.Background())
	if got := r.writes(); len(got) != 0 {
		r.t.Errorf("the parent's reconcile after %s wrote %q", after, got)
	}
}

func (r *laneRig) cycleChild(after string) {
	r.t.Helper()
	r.child.collect(context.Background())
	r.child.reconcile(context.Background())
	if got := r.writes(); len(got) != 0 {
		r.t.Errorf("the lane switch's reconcile after %s wrote %q", after, got)
	}
}

func (r *laneRig) pushChild(file, ver string) []string {
	r.t.Helper()
	return r.pushChildText(readECSPush(r.t, file), ver)
}

func (r *laneRig) pushChildText(text, ver string) []string {
	r.t.Helper()
	r.child.collect(context.Background())
	body, _ := json.Marshal(map[string]string{"_type": "setparam", "system_cfg": text, "mgmt_cfg": "cfgversion=" + ver + "\n"})
	r.csess.Apply(time.Now(), body)
	if !r.child.applyPending(context.Background()) {
		r.t.Fatalf("cfgversion %s was not consumed", ver)
	}
	return r.writes()
}

func TestReplayLaneSwitchAggregate(t *testing.T) {
	r := newLaneRig(t)

	// The parent reports the child on cage 2: its MAC, and as the LLDP
	// neighbour (a Child, never this device's uplink).
	snap, err := r.coll.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var cage devicemodel.Port
	for _, p := range snap.Ports {
		if p.Index == 2 {
			cage = p
		}
	}
	if n := cage.Neighbor; n == nil || !n.Child || n.ChassisID != laneSwitchMAC {
		t.Errorf("cage 2's neighbour: %+v", cage.Neighbor)
	}
	if snap.UplinkPort() == 2 {
		t.Errorf("the lane switch became the parent's uplink")
	}

	got := r.pushChild("system_cfg-flexxg-aggregate.txt", "d4a1a0cac0f5bb83")
	// lag=1 in the push: the lane switch's aggregate 1 is PortChannel201.
	// The push carries no VLAN keys for the ports (no override: "all"), so
	// lane 1 and the aggregate are Allow All, the native stand-in untagged.
	// cl-91 (RS) on the members is refused: the DX010's 25G lanes support
	// none,fc (STATE_DB supported_fecs in the capture).
	want := []string{
		"sudo config vlan member add -e -m 4094 Ethernet4",
		"sudo config portchannel add PortChannel201 --fast-rate true --min-links 1",
		"sudo sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel201' mtu 1500",
	}
	for _, ln := range []string{"Ethernet5", "Ethernet6", "Ethernet7"} {
		// The bridge-port wait of docs/drivers/sonic.md §8.15 (l2mcd on this
		// image, no sonic-swss 0004): a lane that left its last VLAN waits
		// before it joins.
		want = append(want, "sudo config vlan member del 4094 "+ln, "<bridge-port wait "+ln+">", "sudo config portchannel member add PortChannel201 "+ln)
	}
	want = append(want,
		"sudo config vlan member add -u 4094 PortChannel201",
		"sudo config vlan member add -e -m 4094 PortChannel201",
		brkSave,
		"sudo python3 - disunified-l2mc", brkSave) // the reserved groups follow the VLAN move (igmp.go)
	wantWrites(t, "the aggregate push", waitsNamed(withoutL2MC(got)), want)
	if !strings.Contains(r.log.String(), "the controller asks for FEC rs, which Ethernet5 does not support (fc,none)") {
		t.Errorf("no FEC note: %s", r.log.String())
	}
	if v, _, ok := r.csess.Applied(); !ok || v != "d4a1a0cac0f5bb83" {
		t.Errorf("the lane switch reports cfgversion %q (applied %v)", v, ok)
	}
	if strings.Contains(r.log.String(), "breakout core cage 2: port 5") {
		t.Errorf("the uplink's defaults were noted: %s", r.log.String())
	}

	// Both loops' next cycle writes nothing; the lanes report the aggregate.
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

// The parent joins the cage (100G on port 2): the lane aggregate goes first,
// then the breakout, and the lane switch stops answering.
func TestReplayLaneSwitchJoin(t *testing.T) {
	r := newLaneRig(t)
	r.pushChild("system_cfg-flexxg-aggregate.txt", "d4a1a0cac0f5bb83")
	r.cycleParent("the lane aggregate")

	body, _ := json.Marshal(map[string]string{"_type": "setparam", "system_cfg": readECSPush(t, "system_cfg-dx010-brk-4-join.txt"), "mgmt_cfg": "cfgversion=0000000000000004\n"})
	r.parent.collect(context.Background())
	sess := r.parent.session
	sess.Apply(time.Now(), body)
	if !r.parent.applyPending(context.Background()) {
		t.Fatal("the join was not consumed")
	}
	got := waitsNamed(withoutL2MC(r.writes()))
	head := []string{
		"sudo config portchannel member del PortChannel201 Ethernet5",
		"sudo config portchannel member del PortChannel201 Ethernet6",
		"sudo config portchannel member del PortChannel201 Ethernet7",
		"sudo config vlan member del -m 2,8,9,10,69,1000,2000,3000,4000,4010,4094 PortChannel201",
		"<bridge-port wait PortChannel201>",
		"sudo config portchannel del PortChannel201",
		"sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null",
	}
	if len(got) < len(head) {
		t.Fatalf("the join wrote %q", got)
	}
	wantWrites(t, "the join", got[:len(head)], head)
	r.parent.collect(context.Background())
	if _, err := r.lanes.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "not split") {
		t.Errorf("the lane switch after the join: %v", err)
	}
}

// waitsNamed shows sonic's bridge-port wait (a python3 one-liner ending in
// "<iface> <limit> </dev/null") as "<bridge-port wait <iface>>".
func waitsNamed(cmds []string) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		if strings.HasPrefix(c, "python3 -c ") && strings.Contains(c, "SAI_OBJECT_TYPE_BRIDGE_PORT") && strings.HasSuffix(c, " </dev/null") {
			f := strings.Fields(c)
			c = "<bridge-port wait " + f[len(f)-3] + ">"
		}
		out = append(out, c)
	}
	return out
}

func withoutL2MC(cmds []string) []string {
	var out []string
	for _, c := range dropPrefix(cmds, "sudo sonic-db-cli CONFIG_DB del 'L2MC_STATIC_MEMBER|") {
		if _, ok := sonic.L2MCArgOps(c); !ok {
			out = append(out, c)
		}
	}
	return out
}

// l2mcUnbound takes the breakout's multicast unbind lines (sonic's
// l2mcUnbind, docs/drivers/sonic.md 11.25.3) out of cmds and checks them:
// they come before the first breakout, take out only member and router
// rows naming one of ifaces, and touch the static groups' lists. It
// returns the rest, and the number of rows taken out.
func l2mcUnbound(t *testing.T, cmds []string, ifaces ...string) ([]string, int) {
	t.Helper()
	cage := map[string]bool{}
	for _, i := range ifaces {
		cage[i] = true
	}
	var out []string
	n, brk := 0, false
	for _, c := range cmds {
		ops, ok := sonic.L2MCArgOps(c)
		if !ok {
			brk = brk || strings.HasPrefix(c, "sudo config interface breakout ")
			out = append(out, c)
			continue
		}
		if brk {
			t.Errorf("the multicast rows were unbound after the breakout: %q", cmds)
		}
		for _, o := range ops {
			verb, key, _ := strings.Cut(o, " ")
			parts := strings.Split(key, "|")
			switch {
			case parts[0] == "L2MC_STATIC_GROUP":
			case verb == "del" && (parts[0] == "L2MC_STATIC_MEMBER" || parts[0] == "L2MC_MROUTER") && cage[parts[len(parts)-1]]:
				n++
			default:
				t.Errorf("the breakout's unbind %s, which names no interface of the cage %v", o, ifaces)
			}
		}
	}
	return out, n
}

func dropPrefix(cmds []string, prefix string) []string {
	var out []string
	for _, c := range cmds {
		if !strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// A freshly adopted lane switch holds a first push that would change its
// lanes, as every device does: the aggregate push is held, nothing written,
// and the device keeps reporting the old cfgversion.
func TestLaneSwitchFirstPushHeld(t *testing.T) {
	r := newLaneRig(t)
	r.child.cfg.AllowInitialChanges = false
	r.child.collect(context.Background())
	body, _ := json.Marshal(map[string]string{"_type": "setparam", "system_cfg": readECSPush(t, "system_cfg-flexxg-aggregate.txt"), "mgmt_cfg": "cfgversion=d4a1a0cac0f5bb83\n"})
	r.csess.Apply(time.Now(), body)
	r.child.applyPending(context.Background())
	if got := r.writes(); len(got) != 0 {
		t.Errorf("the held push wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "HOLDING the first push after adoption") || !strings.Contains(r.log.String(), "[1 2 3 4]") {
		t.Errorf("not held: %s", r.log.String())
	}
	if v, _, _ := r.csess.Applied(); v == "d4a1a0cac0f5bb83" {
		t.Errorf("a held push was reported as applied")
	}
}
