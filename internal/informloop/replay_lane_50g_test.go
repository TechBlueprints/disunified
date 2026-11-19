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

// The lane switch's own split modes (docs/drivers/breakout.md 16.15), the
// live run on the DX010's cage 26 (2026-11-18, Network 11.0.99): the switch
// is the capture of cage 26 split 4x25G with its lane switch attached
// (sonic-igmp-202511.20261116-6c8eb1cf7/collect-cage26-4x25g.txt), the
// pushes the controller's own for that lane switch
// (controller-11.0.99/replies-lane-switch-dx010-cage26-50g.ndjson):
//
//	0  lane 1 at 50 GbE, saved from the port drawer (no FEC key)
//	1  lane 1 50G and lane 2 25G with FC-FEC (the drawer's save on lane 2)
//	2  lanes 1 and 2 at 25G with FC-FEC (the drawer's save on lane 1)
//	3  lane 1 at 50G again (REST, lane 2 back on auto; slots 3-4 enabled)
//	4  lane 1 at 25G with FC-FEC (REST)
//
// Each re-split is one batch; neither loop takes the new layout for new
// ports, and the cycles after write nothing but what waits a cycle (the
// pushed FEC after a breakout, sonic.md 11.37).
func TestReplayLaneSwitchSplitModes(t *testing.T) {
	recs := loadReplies(t, "controller-11.0.99/replies-lane-switch-dx010-cage26-50g.ndjson")
	if len(recs) != 5 {
		t.Fatalf("%d records, want 5", len(recs))
	}
	push := func(i int) (string, string) {
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

	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261116-6c8eb1cf7", "collect-cage26-4x25g.txt"))
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
	if err := coll.AttachLaneSwitch(26, devicemodel.Attachment{MAC: laneSwitchMAC, Name: "lane-switch"}); err != nil {
		t.Fatal(err)
	}
	// The parent with no push of its own: only its collect and its layout
	// tracking are under test.
	pdesc := inform.Descriptor{MAC: snap.System.MAC}
	psess := device.NewSession(pdesc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef"}, nil, time.Now())
	parent, err := New(pdesc, psess, Config{Logger: logger, Collector: coll, Controller: coll})
	if err != nil {
		t.Fatal(err)
	}
	parent.collect(context.Background())

	o, err := breakout.ParseOptions(map[string]string{"parent": "core", "cage": "26", "mac": laneSwitchMAC, "name": "lane-switch"})
	if err != nil {
		t.Fatal(err)
	}
	lanes := breakout.New(o, coll, coll)
	lanes.Log = logger
	csnap, err := lanes.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u := csnap.Ports[breakout.UplinkPort-1]; !u.Up || u.SpeedMbps != 100000 {
		t.Errorf("the uplink of an attached lane switch whose lanes have no link: up %v %d Mb/s", u.Up, u.SpeedMbps)
	}
	cdefaults := map[int][]string{}
	for _, p := range csnap.Ports {
		cdefaults[p.Index] = append(lanes.DefaultPortNames(p), fmt.Sprintf("Port %d", p.Index))
	}
	cdesc := inform.Descriptor{MAC: csnap.System.MAC}
	csess := device.NewSession(cdesc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: "0000000000000001"}, nil, time.Now())
	child, err := New(cdesc, csess, Config{Logger: logger, Collector: lanes, Controller: lanes, DefaultPortNames: cdefaults, AllowInitialChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	seen := len(fr.Recorded())
	writes := func() string {
		c := fr.Recorded()
		out := strings.Join(c[seen:], "\n")
		seen = len(c)
		return out
	}
	apply := func(i int) string {
		t.Helper()
		text, ver := push(i)
		child.collect(context.Background())
		body, _ := json.Marshal(map[string]string{"_type": "setparam", "system_cfg": text, "mgmt_cfg": "cfgversion=" + ver + "\n"})
		csess.Apply(time.Now(), body)
		if !child.applyPending(context.Background()) {
			t.Fatalf("record %d (cfgversion %s) was not consumed", i, ver)
		}
		return writes()
	}
	// cycle runs the parent's collect, then the lane switch's collect and
	// reconcile, and returns what they wrote.
	cycle := func() string {
		t.Helper()
		parent.collect(context.Background())
		child.collect(context.Background())
		child.reconcile(context.Background())
		return writes()
	}
	freshNote := "are not written until the controller's config matches them"

	// 0: 50G from the drawer re-splits the cage, in one batch.
	got := apply(0)
	if !strings.Contains(got, "sudo config interface breakout -f -y Ethernet100 '2x50G' </dev/null") {
		t.Fatalf("the drawer's 50G wrote %q", got)
	}
	if got := cycle(); got != "" {
		t.Errorf("the cycle after the 2x50G re-split wrote %q", got)
	}
	if got := cycle(); got != "" {
		t.Errorf("the second cycle after the 2x50G re-split wrote %q", got)
	}
	if !strings.Contains(buf.String(), "port layout changed as the controller's config asked: [26(") || strings.Contains(buf.String(), freshNote) {
		t.Errorf("the re-split was taken for new ports:\n%s", buf.String())
	}

	// 1: the drawer's 25G on lane 2 beside lane 1's 50G: two modes, nothing.
	if got := apply(1); got != "" {
		t.Errorf("speeds of two modes wrote %q", got)
	}
	if !strings.Contains(buf.String(), "lane 1 50000 Mb/s, lane 2 25000 Mb/s, which no one breakout mode") {
		t.Errorf("no note for two modes:\n%s", buf.String())
	}

	// 2: 25G on both: back to 4x25G; the pushed FC-FEC follows a cycle on.
	got = apply(2)
	if !strings.Contains(got, "sudo config interface breakout -f -y Ethernet100 '4x25G[10G]' </dev/null") || strings.Contains(got, "fec") {
		t.Fatalf("the drawer's 25G wrote %q", got)
	}
	if got := cycle(); got != "sudo config interface fec Ethernet100 fc\nsudo config interface fec Ethernet101 fc\nsudo config save -y" {
		t.Errorf("the cycle after the 4x25G re-split wrote %q, want the pushed FEC on lanes 1-2", got)
	}
	if got := cycle(); got != "" {
		t.Errorf("the second cycle after the 4x25G re-split wrote %q", got)
	}

	// 3 and 4: the same through REST; slots 3-4 come back enabled.
	if got := apply(3); !strings.Contains(got, "'2x50G'") {
		t.Errorf("record 3 wrote %q", got)
	}
	cycle()
	got = apply(4)
	if !strings.Contains(got, "'4x25G[10G]'") || !strings.Contains(got, "sudo config interface startup Ethernet102") || !strings.Contains(got, "sudo config interface startup Ethernet103") {
		t.Errorf("record 4 wrote %q, want the re-split with lanes 3-4 started", got)
	}
	cycle()
	if got := cycle(); got != "" {
		t.Errorf("the cycles after record 4 wrote %q", got)
	}
	if strings.Contains(buf.String(), freshNote) {
		t.Errorf("a re-split was taken for new ports:\n%s", buf.String())
	}
}
