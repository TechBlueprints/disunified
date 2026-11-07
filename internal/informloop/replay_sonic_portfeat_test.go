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

// The DX010's port-feature captures of 2026-11-07 (docs/drivers/sonic.md
// 11.11), replayed: the switch as captured before the claims (no isolation
// group, no provisioned FDB entry, no shaper, lldpd sending no LLDP-MED TLV
// anywhere) under the push it was following, then the controller's push
// after the UI set port 30 to isolation, a two-MAC allow list, a 100 Mb/s
// egress limit and LLDP-MED off, and port 31 to isolation.

func newPortFeatRig(t *testing.T) (*breakoutRig, *sonic.Collector) {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261106-f2e25f54c", "collect-portfeat.txt"))
	if err != nil {
		t.Fatal(err)
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
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = coll.DefaultPortNames(p)
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: "1fb5ffd3beeffcd8", SystemCfg: readECSPush(t, "system_cfg-dx010-portfeat-0-baseline.txt")}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, DefaultPortNames: defaults, ControlIGMP: true})
	if err != nil {
		t.Fatal(err)
	}
	return &breakoutRig{t: t, fr: fr, l: l, sess: sess, log: buf}, coll
}

func medOn(ln string) string {
	return "sudo docker exec lldp lldpcli configure ports " + ln + " lldp custom-tlv replace oui 00,12,bb subtype 1 oui-info 00,01,04"
}

func TestReplaySonicDX010PortFeatures(t *testing.T) {
	r, coll := newPortFeatRig(t)
	ifaces := strings.Split(ports32(), ",")

	// The first reconcile with the claims: the baseline push predates them
	// and carries no lldpmed keys, which is UniFi's default, LLDP-MED on --
	// the deployed bridge's first apply did the same on all 32 ports.
	r.l.collect(context.Background())
	r.l.reconcile(context.Background())
	got := r.writes()
	var want []string
	for _, ln := range ifaces {
		want = append(want, medOn(ln))
	}
	want = append(want, brkSave)
	wantWrites(t, "first reconcile", got, want)

	got = r.push("system_cfg-dx010-portfeat-1-set.txt", "84b57a6092b23683")
	type applOp struct {
		Op    string   `json:"op"`
		Table string   `json:"table"`
		Key   string   `json:"key"`
		FV    []string `json:"fv,omitempty"`
	}
	appl, _ := json.Marshal([]applOp{
		{"set", "ISOLATION_GROUP_TABLE", "DUI_ISOLATION", []string{"TYPE", "port", "PORTS", "Ethernet116,Ethernet120", "MEMBERS", "Ethernet116,Ethernet120"}},
		{"set", "FDB_TABLE", "Vlan4094:02:00:00:00:00:01", []string{"port", "Ethernet116", "type", "static"}},
		{"set", "FDB_TABLE", "Vlan4094:02:00:00:00:00:02", []string{"port", "Ethernet116", "type", "static"}},
	})
	if !containsInOrder(got,
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet116' learn_mode drop",
		"sudo sonic-db-cli CONFIG_DB hset 'SCHEDULER|DUI_EGRESS_100000' meter_type bytes pir 12500000 pbs 125000",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT_QOS_MAP|Ethernet116' scheduler DUI_EGRESS_100000",
		"sudo docker exec lldp lldpcli unconfigure ports Ethernet116 lldp custom-tlv oui 00,12,bb subtype 1",
	) {
		t.Errorf("the push wrote %q", got)
	}
	if n := len(got); n < 2 || got[n-1] != brkSave || !strings.Contains(got[n-2], " disunified-appl ") || !strings.Contains(got[n-2], string(appl)) {
		t.Errorf("the APPL_DB writes are not one ProducerStateTable call before the save:\n%s", strings.Join(got, "\n"))
	}
	r.cycle("the port features")

	// What the bridge now reports for the two ports.
	snap, err := coll.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports {
		switch p.Index {
		case 30:
			if !p.Isolated || !p.PortSecurity || strings.Join(p.AllowedMACs, ",") != "02:00:00:00:00:01,02:00:00:00:00:02" || p.EgressKbps != 100000 || !p.LLDPMEDOff {
				t.Errorf("port 30 reads back as %+v", p)
			}
		case 31:
			if !p.Isolated || p.PortSecurity || p.EgressKbps != 0 || p.LLDPMEDOff {
				t.Errorf("port 31 reads back as isolated=%v security=%v egress=%d medOff=%v", p.Isolated, p.PortSecurity, p.EgressKbps, p.LLDPMEDOff)
			}
		}
	}
	if s := r.log.String(); strings.Contains(s, "UNSUPPORTED") {
		t.Errorf("a claimed feature was logged as unsupported:\n%s", s)
	}
}
