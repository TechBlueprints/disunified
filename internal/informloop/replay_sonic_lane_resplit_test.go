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

// The parent's side of the automatic lane switch's live run, step 6
// (docs/drivers/breakout.md 16.10, the DX010 2026-11-16): with the lane
// switch of cage 3 adopted, a forced 100G on the parent's port 3 joined the
// cage (record 3 of controller-11.0.99/replies-dx010-lane-handover.ndjson),
// and Auto split it again by the module rule (record 4: the cage holds a
// CA-25G-S DAC, which runs only split layouts). The switch is the capture of
// that cage split for the DAC (sonic-igmp-202511.20261115-6c8eb1cf7/
// collect-split-dac.txt).
//
// Live, the split's batch put LLDP-MED on lanes 2-4 only (the plan's copy
// kept the joined port's TLV for Ethernet8, which lldpd had dropped with the
// port), and the next collect took port 3 for a new port, since the split was
// the driver's own write, which PlanPorts leaves out: "port 3 ... is new
// since the controller's config and would be changed by it; not written",
// and Ethernet8's TLV was written only at the next restart. Here the split's
// batch carries every lane's TLV, the layout is the controller's, and the
// cycle after writes nothing.
func TestReplaySonicDX010LaneCageResplit(t *testing.T) {
	recs := loadReplies(t, "controller-11.0.99/replies-dx010-lane-handover.ndjson")
	if len(recs) != 7 {
		t.Fatalf("%d records, want 7", len(recs))
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
	push := func(i int) []string {
		t.Helper()
		sc, v := cfg(i)
		body, _ := json.Marshal(map[string]string{"_type": "setparam", "system_cfg": sc, "mgmt_cfg": "cfgversion=" + v + "\n"})
		sess.Apply(time.Now(), body)
		if !l.applyPending(context.Background()) {
			t.Fatalf("record %d was not consumed", i)
		}
		return writes()
	}
	med := func(ln string) string {
		return "sudo docker exec lldp lldpcli configure ports " + ln + " lldp custom-tlv replace oui 00,12,bb subtype 1 oui-info 00,01,04"
	}
	has := func(step string, got []string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains("\n"+strings.Join(got, "\n")+"\n", "\n"+w+"\n") {
				t.Errorf("%s: no %q in\n  %s", step, w, strings.Join(got, "\n  "))
			}
		}
	}

	// The switch under the push in force before the join (whatever the
	// capture differs by is written once), then nothing.
	l.collect(context.Background())
	l.reconcile(context.Background())
	writes()
	cycle("the first reconcile")

	join := push(3)
	has("join", join, "sudo config interface breakout -f -y Ethernet8 '1x100G[40G]' </dev/null", med("Ethernet8"))
	// The push's RS waits for the joined port's supported_fecs (sonic.md
	// §11.37): written the cycle after the join, then nothing.
	for _, c := range join {
		if strings.Contains(c, " fec ") {
			t.Errorf("join: FEC written before the joined port's list was read: %q", c)
		}
	}
	l.collect(context.Background())
	l.reconcile(context.Background())
	has("the cycle after the join", writes(), "sudo config interface fec Ethernet8 rs")
	cycle("the join's FEC")

	has("split", push(4), "sudo config interface breakout -f -y Ethernet8 '4x25G[10G]' </dev/null",
		med("Ethernet8"), med("Ethernet9"), med("Ethernet10"), med("Ethernet11"))
	cycle("the split")
	cycle("the split, again")
	s := buf.String()
	if strings.Contains(s, "is new since the controller's config") {
		t.Errorf("the module rule's split was taken for a new port:\n%s", s)
	}
	if !strings.Contains(s, "port layout changed as the controller's config asked: [3(Ethernet8/Ethernet8,Ethernet9,Ethernet10,Ethernet11)]") {
		t.Errorf("the split is not logged as the controller's:\n%s", s)
	}
}
