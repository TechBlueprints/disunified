package informloop

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The DX010's breakout run of 2026-11-07 (docs/drivers/sonic.md 11.11),
// replayed: the capture is the switch on image igmp-202511.20261106-f2e25f54c
// once eth0's address row was gone (sonic-yang then accepts a breakout), and
// the pushes are the controller's own, in the order the run sent them --
// cage 2 to 4x25G, the lanes to 10G, 2x50G, the join; then one IP ACL rule
// scoped to the switch, a split and a join while its table is bound. Each
// push must write what the bridge wrote live, and a reconcile in between
// nothing.

type breakoutRig struct {
	t    *testing.T
	fr   *sonic.FixtureRunner
	l    *Loop
	sess *device.Session
	log  *strings.Builder
	seen int
}

func newBreakoutRig(t *testing.T) *breakoutRig {
	t.Helper()
	return newBreakoutRigFrom(t, "collect.txt", "system_cfg-dx010-brk-0-baseline.txt", "13da591370c88d31")
}

// newBreakoutRigFrom: the capture in the 20261106 fixture dir, following
// the given push.
func newBreakoutRigFrom(t *testing.T, capture, applied, ver string) *breakoutRig {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261106-f2e25f54c", capture))
	if err != nil {
		t.Fatal(err)
	}
	buf := &strings.Builder{}
	logger := log.New(buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	coll.L2 = false // spanning tree is TestReplaySonicSpanningTree's
	coll.IGMP = true
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = coll.DefaultPortNames(p)
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
	// Adopted and following the controller's baseline (cage 2 on auto), as
	// the switch was before the run: no first-push hold, and the fresh-port
	// withholding live (AllowInitialChanges off, as deployed).
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: ver, SystemCfg: readECSPush(t, applied)}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, DefaultPortNames: defaults, ControlIGMP: true})
	if err != nil {
		t.Fatal(err)
	}
	r := &breakoutRig{t: t, fr: fr, l: l, sess: sess, log: buf}
	r.l.collect(context.Background())
	r.l.reconcile(context.Background())
	if got := r.writes(); len(got) != 0 {
		t.Fatalf("%s is the switch as captured, but the reconcile wrote %q", applied, got)
	}
	return r
}

// push hands the session the controller's setparam with the recorded
// system_cfg, as an inform reply would, and applies it.
func (r *breakoutRig) push(file, ver string) []string {
	r.t.Helper()
	body, _ := json.Marshal(map[string]string{"_type": "setparam", "system_cfg": readECSPush(r.t, file), "mgmt_cfg": "cfgversion=" + ver + "\n"})
	r.sess.Apply(time.Now(), body)
	if !r.l.applyPending(context.Background()) {
		r.t.Fatalf("%s was not consumed", file)
	}
	return r.writes()
}

// cycle is the next inform cycle without a push: collect, then reconcile,
// which must write nothing.
func (r *breakoutRig) cycle(after string) {
	r.t.Helper()
	r.l.collect(context.Background())
	r.l.reconcile(context.Background())
	if got := r.writes(); len(got) != 0 {
		r.t.Errorf("reconcile after %s wrote %q", after, got)
	}
}

func (r *breakoutRig) writes() []string {
	c := r.fr.Recorded()
	out := c[r.seen:]
	r.seen = len(c)
	return out
}

func wantWrites(t *testing.T, step string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s wrote\n  %s\nwant\n  %s", step, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// The commands each push ran on the DX010 (the bridge's journal, 2026-11-07),
// in order; the IGMP apply's own script is checked through L2MCScripts.
const brkSave = "sudo config save -y"

func childIntent(names ...string) []string {
	var out []string
	for _, n := range names {
		out = append(out, "sudo config interface startup "+n, "sudo config interface mtu "+n+" 1500")
	}
	for _, n := range names {
		out = append(out, "sudo config vlan member add -u 4094 "+n)
	}
	return out
}

func TestReplaySonicDX010BreakoutSeries(t *testing.T) {
	r := newBreakoutRig(t)

	got := r.push("system_cfg-dx010-brk-1-4x25g.txt", "81fdb670c3d4164c")
	want := append(append([]string{"sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null"}, childIntent("Ethernet4", "Ethernet5", "Ethernet6", "Ethernet7")...), brkSave)
	if len(got) < len(want) {
		t.Fatalf("4x25G wrote %q", got)
	}
	wantWrites(t, "4x25G", got[:len(want)], want)
	if l2mc := strings.Join(r.fr.L2MCScripts, "\n"); !strings.Contains(l2mc, "L2MC_STATIC_MEMBER|Vlan4094|239.255.255.250|Ethernet7") {
		t.Errorf("the reserved groups were not extended to the new lanes:\n%s", l2mc)
	}
	r.cycle("4x25G")

	wantWrites(t, "lanes at 10G", r.push("system_cfg-dx010-brk-2-lanes-10g.txt", "644f4572a59be3bd"), []string{
		"sudo config interface speed Ethernet4 10000", "sudo config interface speed Ethernet5 10000",
		"sudo config interface speed Ethernet6 10000", "sudo config interface speed Ethernet7 10000", brkSave})
	r.cycle("the lane speed")

	n := len(r.fr.L2MCScripts)
	got = r.push("system_cfg-dx010-brk-3-2x50g.txt", "118b8151afe96f16")
	want = append(append([]string{"sudo config interface breakout -f -y Ethernet4 '2x50G' </dev/null"}, childIntent("Ethernet4", "Ethernet6")...), brkSave)
	if len(got) < len(want) {
		t.Fatalf("2x50G wrote %q", got)
	}
	wantWrites(t, "2x50G", got[:len(want)], want)
	if l2mc := strings.Join(r.fr.L2MCScripts[n:], "\n"); !strings.Contains(l2mc, `L2MC_STATIC_MEMBER|Vlan4094|239.255.255.250|Ethernet5\"}`) {
		t.Errorf("the gone lanes' reserved-group rows were not removed:\n%s", l2mc)
	}
	// The join is pushed in the cycle whose collect first sees the 2x50G
	// layout: no reconcile in between.
	r.l.collect(context.Background())
	got = r.push("system_cfg-dx010-brk-4-join.txt", "f49b55fc0441cb65")
	want = append(append([]string{"sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null"}, childIntent("Ethernet4")...), brkSave)
	if len(got) < len(want) {
		t.Fatalf("join wrote %q\nlog:\n%s", got, r.log.String())
	}
	wantWrites(t, "join", got[:len(want)], want)
	r.cycle("the join")
	if s := r.log.String(); strings.Contains(s, "is new since the controller's config") || !strings.Contains(s, "port layout changed as the controller's config asked: [2(Ethernet4/Ethernet4,Ethernet6)]") {
		t.Errorf("a layout the controller asked for was treated as a new port:\n%s", s)
	}
}

// The live failure (2026-11-07 12:05): with the ACL table bound, cage 2 was
// split and the join pushed in the cycle whose collect first saw the split.
// The loop marked the cage "fresh" (its interfaces had changed) and withheld
// the join, for good -- the controller wanted one port and the switch had
// four, so the two never matched. Now the split's own push makes the layout
// the controller's, the join runs, and each breakout rebinds the table.
func TestReplaySonicDX010BreakoutUnderACL(t *testing.T) {
	r := newBreakoutRig(t)

	got := r.push("system_cfg-dx010-brk-acl-0-rule.txt", "15a7093b59d0b06b")
	if len(got) != 3 || !strings.HasPrefix(got[0], "sudo config acl add table DUI_IP_ACL L3 -p Ethernet0,Ethernet4,Ethernet8,") || got[0] != "sudo config acl add table DUI_IP_ACL L3 -p "+ports32()+" -s ingress" ||
		got[1] != "sudo sonic-db-cli CONFIG_DB hset 'ACL_RULE|DUI_IP_ACL|RULE_1' DST_IP '198.51.100.0/24' IP_TYPE 'IPV4ANY' PACKET_ACTION 'FORWARD' PRIORITY '9998' SRC_IP '192.0.2.0/24'" || got[2] != brkSave {
		t.Fatalf("the rule's push wrote %q", got)
	}
	r.cycle("the rule")

	got = r.push("system_cfg-dx010-brk-acl-1-split.txt", "e891cd306938e61b")
	split := strings.Replace(ports32(), "Ethernet4,", "Ethernet4,Ethernet5,Ethernet6,Ethernet7,", 1)
	if !containsInOrder(got, "sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null", brkSave, "sudo config acl add table DUI_IP_ACL L3 -p "+split+" -s ingress", brkSave) {
		t.Errorf("split under the table wrote %q", got)
	}
	r.l.collect(context.Background())
	got = r.push("system_cfg-dx010-brk-acl-2-join.txt", "55e5ea66a184c830")
	if !containsInOrder(got, "sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null", brkSave, "sudo config acl add table DUI_IP_ACL L3 -p "+ports32()+" -s ingress", brkSave) {
		t.Errorf("join under the table wrote %q\nlog:\n%s", got, r.log.String())
	}
	r.cycle("the join")
	if strings.Contains(r.log.String(), "is new since the controller's config") {
		t.Errorf("the join was withheld:\n%s", r.log.String())
	}
}

func ports32() string {
	var p []string
	for i := 0; i < 32; i++ {
		p = append(p, "Ethernet"+itoa(i*4))
	}
	return strings.Join(p, ",")
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// containsInOrder: each of want appears in got, in that order.
func containsInOrder(got []string, want ...string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// 1G on the split cage (2026-11-07 12:35, after the deploy of the fix): the
// controller took the speed only once an inform had carried the split
// cage's caps, which include 1G from the lanes' supported_speeds (before
// that: api.err.InvalidInterfaceSpeedDuplex). The bridge wrote it as a lane
// speed, no breakout; APPL_DB followed on all four lanes.
func TestReplaySonicDX010LanesAt1G(t *testing.T) {
	r := newBreakoutRigFrom(t, "collect-4x25g.txt", "system_cfg-dx010-brk-1-4x25g.txt", "9dd96649cb883082")
	wantWrites(t, "1G", r.push("system_cfg-dx010-brk-5-lanes-1g.txt", "661258697247e150"), []string{
		"sudo config interface speed Ethernet4 1000", "sudo config interface speed Ethernet5 1000",
		"sudo config interface speed Ethernet6 1000", "sudo config interface speed Ethernet7 1000", brkSave})
	r.cycle("1G")
	wantWrites(t, "join from 1G", r.push("system_cfg-dx010-brk-4-join.txt", "7e75422bf845c58e")[:1], []string{"sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null"})
}
