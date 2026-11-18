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
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The DX010's cage 26 live, 2026-11-17 (docs/drivers/sonic.md 11.38): an
// egress limit and isolation set on the empty cage through the controller
// (record 0 of controller-11.0.99/replies-dx010-cage26-breakout-portfeat.ndjson),
// then a forced 25G that splits it (records 1 and 6), a forced 100G that
// joins it again (record 7), and the override as it was before (record 8).
// Live, before the fixes, the split was refused every cycle by sonic-yang
// (the shaper's pir without a cir), then, once the shaper was held out, the
// breakout failed half done: Broadcom's SAI keeps an isolated port's bridge
// port, and the CLI bailed out with CONFIG_DB holding neither the old port
// nor the new ones. The batch the plan made also left lane 1's PORT_QOS_MAP
// out (the plan's copy kept the joined port's row under lane 1's name, as
// it had kept its LLDP-MED TLV, breakout.md 16.10). The switch is the
// capture of the same DX010 with cage 26 whole and empty
// (collect-split-dac.txt); cage 3 carries its adopted lane switch, as live.
func TestReplaySonicDX010Cage26BreakoutPortFeatures(t *testing.T) {
	recs := loadReplies(t, "controller-11.0.99/replies-dx010-cage26-breakout-portfeat.ndjson")
	if len(recs) != 9 {
		t.Fatalf("%d records, want 9", len(recs))
	}
	cfg := func(i int) (string, string) {
		var p struct {
			SystemCfg string `json:"system_cfg"`
			MgmtCfg   string `json:"mgmt_cfg"`
		}
		if err := json.Unmarshal(recs[i].Payload, &p); err != nil || p.SystemCfg == "" {
			t.Fatalf("record %d carries no system_cfg: %v", i, err)
		}
		for _, l := range strings.Split(p.MgmtCfg, "\n") {
			if v, ok := strings.CutPrefix(l, "cfgversion="); ok {
				return p.SystemCfg, v
			}
		}
		t.Fatalf("record %d has no cfgversion", i)
		return "", ""
	}

	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7", "collect-split-dac.txt"))
	if err != nil {
		t.Fatal(err)
	}
	buf := &strings.Builder{}
	logger := log.New(buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	coll.L2 = false
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mac, err := devicemodel.LaneSwitchMAC(snap.System.MAC, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := coll.AttachLaneSwitch(3, devicemodel.Attachment{MAC: mac, Name: "sw-cage3"}); err != nil {
		t.Fatal(err)
	}
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = coll.DefaultPortNames(p)
	}
	before, ver := cfg(2)
	desc := inform.Descriptor{MAC: snap.System.MAC}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: ver, SystemCfg: before}, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, DefaultPortNames: defaults})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	writes := func() []string {
		c := fr.Recorded()
		out := c[seen:]
		seen = len(c)
		return out
	}
	cycle := func(after string) {
		t.Helper()
		l.collect(context.Background())
		l.reconcile(context.Background())
		if got := writes(); len(got) != 0 {
			t.Errorf("the cycle after %s wrote %q", after, got)
		}
	}
	push := func(i int) string {
		t.Helper()
		sc, v := cfg(i)
		body, _ := json.Marshal(map[string]string{"_type": "setparam", "system_cfg": sc, "mgmt_cfg": "cfgversion=" + v + "\n"})
		sess.Apply(time.Now(), body)
		if !l.applyPending(context.Background()) {
			t.Fatalf("record %d was not consumed", i)
		}
		return "\n" + strings.Join(writes(), "\n") + "\n"
	}
	// in: every want is a line of got, in this order.
	in := func(step, got string, want ...string) {
		t.Helper()
		at := 0
		for _, w := range want {
			i := strings.Index(got[at:], "\n"+w+"\n")
			if i < 0 {
				t.Errorf("%s: no %q (in order) in%s", step, w, got)
				return
			}
			at += i + 1
		}
	}
	qos := func(ln string) string {
		return "sudo sonic-db-cli CONFIG_DB hset 'PORT_QOS_MAP|" + ln + "' scheduler DUI_EGRESS_100000"
	}
	sched := "sudo sonic-db-cli CONFIG_DB hset 'SCHEDULER|DUI_EGRESS_100000' meter_type bytes pir 12500000 pbs 125000"
	iso := func(list string) string {
		return `"fv":["TYPE","port","PORTS","` + list + `","MEMBERS","` + list + `"]`
	}

	// The switch under record 2 (the limit and isolation on the whole cage):
	// written once, then nothing.
	l.collect(context.Background())
	l.reconcile(context.Background())
	first := "\n" + strings.Join(writes(), "\n") + "\n"
	in("the features", first, sched, qos("Ethernet100"))
	if !strings.Contains(first, iso("Ethernet100")) {
		t.Errorf("the features: no isolation group in%s", first)
	}
	cycle("the features")

	split := push(6)
	in("split", split,
		"sudo sonic-db-cli CONFIG_DB hdel 'PORT_QOS_MAP|Ethernet100' scheduler",
		"sudo sonic-db-cli CONFIG_DB del 'SCHEDULER|DUI_EGRESS_100000'",
		"sudo config interface breakout -f -y Ethernet100 '4x25G[10G]' </dev/null",
		sched, qos("Ethernet100"), qos("Ethernet101"), qos("Ethernet102"), qos("Ethernet103"))
	// The group without the cage (no other port is isolated: deleted), then
	// the wait for the chip, then the breakout.
	i, w, j := strings.Index(split, `"op":"del","table":"ISOLATION_GROUP_TABLE"`), strings.Index(split, "SAI_OBJECT_TYPE_ISOLATION_GROUP_MEMBER"), strings.Index(split, "config interface breakout")
	if i < 0 || w < i || j < w {
		t.Errorf("split: the cage does not leave the isolation group before the breakout:%s", split)
	}
	if !strings.Contains(split, iso("Ethernet100,Ethernet101,Ethernet102,Ethernet103")) {
		t.Errorf("split: the lanes are not isolated:%s", split)
	}
	cycle("the split")
	if s := buf.String(); strings.Contains(s, "is new since the controller's config") {
		t.Errorf("the split was taken for a new port:\n%s", s)
	}

	join := push(7)
	in("join", join,
		"sudo sonic-db-cli CONFIG_DB hdel 'PORT_QOS_MAP|Ethernet103' scheduler",
		"sudo config interface breakout -f -y Ethernet100 '1x100G[40G]' </dev/null",
		sched, qos("Ethernet100"))
	if !strings.Contains(join, iso("Ethernet100")) {
		t.Errorf("join: lane 1 is not isolated:%s", join)
	}
	cycle("the join")

	off := push(8)
	in("off", off,
		"sudo sonic-db-cli CONFIG_DB hdel 'PORT_QOS_MAP|Ethernet100' scheduler",
		"sudo sonic-db-cli CONFIG_DB del 'SCHEDULER|DUI_EGRESS_100000'")
	if !strings.Contains(off, `{"op":"del","table":"ISOLATION_GROUP_TABLE","key":"DUI_ISOLATION"}`) {
		t.Errorf("off: the isolation group stays:%s", off)
	}
	cycle("the removal")
}
