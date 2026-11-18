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

// The DX010's cage 26 live, 2026-11-17 (docs/drivers/sonic.md 11.40), every
// push of controller-11.0.99/replies-dx010-cage26-breakout-secured.ndjson:
// a MAC filter with two made-up MACs on the empty cage (record 0), a forced
// 25G that splits it (record 1, which failed live: the filter's static FDB
// entries hold the bridge port, sairedis refused its removal and the CLI
// bailed out with CONFIG_DB holding no port for the cage; record 2 is the
// controller's re-send of the same cfgversion once the port reported
// nothing, without the speed), a name change and back (3, 4), then with the
// fix the split and the join (5, 6) and the filter off (7); 802.1X on for
// the device (8), Credentials on the cage (9), its split and join (10, 11),
// force-authorised again (12); 802.1X off and isolation on cage 27 (13),
// cage 26 split and joined beside it (14, 15), and isolation off (16). The
// switch is the capture of the same DX010 with cage 26 whole and empty
// (collect-split-dac.txt); cage 3 carries its adopted lane switch, as live.
func TestReplaySonicDX010Cage26BreakoutSecured(t *testing.T) {
	recs := loadReplies(t, "controller-11.0.99/replies-dx010-cage26-breakout-secured.ndjson")
	if len(recs) != 17 {
		t.Fatalf("%d records, want 17", len(recs))
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
	coll.L2 = true
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
	start, ver := cfg(4)
	desc := inform.Descriptor{MAC: snap.System.MAC}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: ver, SystemCfg: start}, nil, time.Now())
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
	// layoutCycle: the cycle after a breakout tells l2d the new interfaces
	// (l2.json), as live one cycle later, and writes nothing else.
	layoutCycle := func(after string) {
		t.Helper()
		l.collect(context.Background())
		l.reconcile(context.Background())
		got := writes()
		if len(got) != 1 || got[0] != "bash -s -- disunified-l2" {
			t.Errorf("the cycle after %s wrote %q, want l2d's config alone", after, got)
		}
	}
	fdbDel := func(mac string) string { return `{"op":"del","table":"FDB_TABLE","key":"Vlan4094:` + mac + `"}` }
	fdbSet := func(mac, port string) string {
		return `{"op":"set","table":"FDB_TABLE","key":"Vlan4094:` + mac + `","fv":["port","` + port + `","type","static"]}`
	}
	macs := []string{"02:00:00:00:26:01", "02:00:00:00:26:02"}
	drop := func(ln string) string { return "sudo sonic-db-cli CONFIG_DB hset 'PORT|" + ln + "' learn_mode drop" }
	brk := func(mode string) string {
		return "sudo config interface breakout -f -y Ethernet100 '" + mode + "' </dev/null"
	}
	// before: every one of want is in got before the breakout, in this order.
	before := func(step, got string, want ...string) {
		t.Helper()
		b := strings.Index(got, "config interface breakout")
		if b < 0 {
			t.Fatalf("%s: no breakout in%s", step, got)
		}
		at := 0
		for _, w := range want {
			i := strings.Index(got[at:b], w)
			if i < 0 {
				t.Errorf("%s: no %q (in order) before the breakout in%s", step, w, got)
				return
			}
			at += i + len(w)
		}
	}
	after := func(step, got string, want ...string) {
		t.Helper()
		b := strings.Index(got, "config interface breakout")
		for _, w := range want {
			if !strings.Contains(got[b:], w) {
				t.Errorf("%s: no %q after the breakout in%s", step, w, got)
			}
		}
	}

	// The switch under record 4 (the filter on the whole cage): written
	// once, then nothing.
	l.collect(context.Background())
	l.reconcile(context.Background())
	first := "\n" + strings.Join(writes(), "\n") + "\n"
	in("the filter", first, drop("Ethernet100"))
	for _, m := range macs {
		if !strings.Contains(first, fdbSet(m, "Ethernet100")) {
			t.Errorf("the filter: no static entry for %s in%s", m, first)
		}
	}
	cycle("the filter")

	// The split with the filter: the entries leave APPL_DB and the chip
	// before the breakout, and come back on lane 1; every lane is closed.
	split := push(5)
	before("split", split, fdbDel(macs[0]), fdbDel(macs[1]), "SAI_FDB_ENTRY_ATTR_BRIDGE_PORT_ID")
	after("split", split, drop("Ethernet100"), drop("Ethernet101"), drop("Ethernet102"), drop("Ethernet103"),
		fdbSet(macs[0], "Ethernet100"), fdbSet(macs[1], "Ethernet100"))
	layoutCycle("the split")
	cycle("the split, then")
	join := push(6)
	before("join", join, fdbDel(macs[0]), fdbDel(macs[1]), "SAI_FDB_ENTRY_ATTR_BRIDGE_PORT_ID")
	if !strings.Contains(join, brk("1x100G[40G]")) {
		t.Errorf("join: not joined:%s", join)
	}
	after("join", join, drop("Ethernet100"), fdbSet(macs[0], "Ethernet100"), fdbSet(macs[1], "Ethernet100"))
	layoutCycle("the join")
	cycle("the join, then")
	off := push(7)
	in("filter off", off, "sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet100' learn_mode hardware")
	if !strings.Contains(off, fdbDel(macs[0])) || !strings.Contains(off, fdbDel(macs[1])) {
		t.Errorf("filter off: the entries stay:%s", off)
	}
	cycle("the filter off")

	// 802.1X: Credentials closes the cage; a split and a join hold its
	// learn mode out and close every lane (no admitted host, so no entry).
	push(8)
	cycle("802.1X on")
	cred := push(9)
	in("Credentials", cred, drop("Ethernet100"))
	cycle("Credentials")
	split = push(10)
	before("802.1X split", split, "sudo sonic-db-cli CONFIG_DB hdel 'PORT|Ethernet100' learn_mode")
	after("802.1X split", split, drop("Ethernet100"), drop("Ethernet101"), drop("Ethernet102"), drop("Ethernet103"))
	layoutCycle("the 802.1X split")
	cycle("the 802.1X split, then")
	join = push(11)
	after("802.1X join", join, drop("Ethernet100"))
	layoutCycle("the 802.1X join")
	cycle("the 802.1X join, then")
	open := push(12)
	in("force-authorised", open, "sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet100' learn_mode hardware")
	cycle("force-authorised")

	// Isolation on cage 27 beside a split and a join of cage 26: the group
	// is never rewritten (live, its ASIC_DB member stayed throughout).
	iso := push(13)
	if !strings.Contains(iso, `"fv":["TYPE","port","PORTS","Ethernet104","MEMBERS","Ethernet104"]`) {
		t.Errorf("isolation: cage 27 is not isolated:%s", iso)
	}
	cycle("isolation")
	for i, step := range []string{"isolated split", "isolated join"} {
		got := push(14 + i)
		if !strings.Contains(got, "config interface breakout") {
			t.Errorf("%s: no breakout in%s", step, got)
		}
		if strings.Contains(got, "ISOLATION_GROUP_TABLE") || strings.Contains(got, "SAI_OBJECT_TYPE_ISOLATION_GROUP_MEMBER") {
			t.Errorf("%s: the isolation group was touched:%s", step, got)
		}
		layoutCycle("the " + step)
		cycle("the " + step + ", then")
	}
	isoOff := push(16)
	if !strings.Contains(isoOff, `{"op":"del","table":"ISOLATION_GROUP_TABLE","key":"DUI_ISOLATION"}`) {
		t.Errorf("isolation off: the group stays:%s", isoOff)
	}
	cycle("isolation off")
	if s := buf.String(); strings.Contains(s, "is new since the controller's config") {
		t.Errorf("a breakout was taken for a new port:\n%s", s)
	}
}
