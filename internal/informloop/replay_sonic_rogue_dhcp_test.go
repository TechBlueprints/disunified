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
)

// Rogue DHCP Server Detection end to end on the DX010 (docs/drivers/sonic.md
// §11.7.9): the DX010's own push with the snooping lines the controller sent
// the ECS, through the loop into the driver over the DX010 captured on
// 2026-11-14 (igmp-202511.20261106-f2e25f54c, ACL counter polling on, a hand
// test table of the guard's shape beside it). The guard's counted DROP on
// cage 3 then counts replies (the FixtureRunner's ACLHits, as the chip
// counted the hand test's), and the inform after that carries anomalies bit
// 64 (rogue DHCP server) on port 3 alone; the inform after one with no
// further growth carries none.
func TestReplaySonicRogueDHCPPerPort(t *testing.T) {
	var extra []string
	for _, line := range strings.Split(readECSPush(t, "system_cfg-ecs-dhcp-snoop.txt"), "\n") {
		if strings.HasPrefix(line, "switch.dhcp_") {
			extra = append(extra, line)
		}
	}
	text := strings.TrimRight(readECSPush(t, "system_cfg-dx010-held.txt"), "\n") + "\n" + strings.Join(extra, "\n") + "\n"

	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261106-f2e25f54c", "collect-rdtest.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	coll.L2 = false
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = coll.DefaultPortNames(p)
	}
	desc, err := device.DescriptorFor("USWF07D", snap, device.Identity{MAC: snap.System.MAC, IP: "192.0.2.10", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: text}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, AllowInitialChanges: true, DefaultPortNames: defaults})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if !l.applyPending(ctx) {
		t.Fatal("the push was not consumed")
	}
	cmds := strings.Join(fr.Recorded(), "\n")
	for _, want := range []string{
		"sudo config acl add table DUI_DHCP_GUARD DUI_DHCP_GUARD -p Ethernet0,",
		"'ACL_RULE|DUI_DHCP_GUARD|V1_IN_Ethernet8' IN_PORTS 'Ethernet8'",
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("commands lack %q:\n%s", want, cmds)
		}
	}
	if strings.Contains(cmds, "FLEX_COUNTER_TABLE") || strings.Contains(cmds, "DUI_RDTEST") {
		t.Errorf("polling was already on, and the hand test's table is not the driver's:\n%s", cmds)
	}

	anomalies := func() map[int]float64 {
		t.Helper()
		s, err := coll.Collect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sess.SetSnapshot(s)
		var m map[string]any
		if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
			t.Fatal(err)
		}
		out := map[int]float64{}
		for _, p := range m["port_table"].([]any) {
			pe := p.(map[string]any)
			if a, _ := pe["anomalies"].(float64); a != 0 {
				out[int(pe["port_idx"].(float64))] = a
			}
		}
		return out
	}
	if a := anomalies(); len(a) != 0 {
		t.Errorf("no history yet: anomalies %v", a)
	}
	if a := anomalies(); len(a) != 0 {
		t.Errorf("no growth: anomalies %v", a)
	}
	if err := fr.ACLHits("DUI_DHCP_GUARD:V1_IN_Ethernet8", 3); err != nil {
		t.Fatal(err)
	}
	if a := anomalies(); len(a) != 1 || a[3] != 64 {
		t.Errorf("after replies dropped on cage 3: anomalies %v, want port 3 = 64", a)
	}
	if a := anomalies(); len(a) != 0 {
		t.Errorf("no further growth: anomalies %v", a)
	}
	n := len(fr.Recorded())
	l.reconcile(ctx)
	if got := fr.Recorded()[n:]; len(got) != 0 {
		t.Errorf("reconcile wrote %q", got)
	}
}
