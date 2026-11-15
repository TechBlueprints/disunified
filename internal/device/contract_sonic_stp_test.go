package device

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// The DX010 with disunified-l2d running (collect-l2-running.txt): the
// spanning-tree fields as the ECS Aggregation's own inform carries them --
// stp_priority as the step, the root's MAC, the bridge's topology-change
// count and the last change's time -- and a port held by loop protection
// with the loop anomaly.
func TestWireContractSonicSpanningTree(t *testing.T) {
	m, _ := sonicCapturePayload(t, "sonic-202505.1238526-ff3c0c9f6/collect-l2-running.txt", "USWF07D")
	if p, _ := m["stp_priority"].(float64); p != 8 {
		t.Errorf("stp_priority = %v, want 8 (32768 as the ECS line's step)", m["stp_priority"])
	}
	if r, _ := m["root_switch"].(string); r != "02:00:00:00:00:52" {
		t.Errorf("root_switch = %v", m["root_switch"])
	}
	if n, _ := m["stp_topology_change_count"].(float64); n != 2 {
		t.Errorf("stp_topology_change_count = %v, want mstpd's 2", m["stp_topology_change_count"])
	}
	if ts, _ := m["stp_last_topology_change_timestamp"].(string); len(ts) != len("2006-01-02T15:04:05") {
		t.Errorf("stp_last_topology_change_timestamp = %v", m["stp_last_topology_change_timestamp"])
	}
	up := portEntry(t, m, 1)
	if up["stp_state"] != "forwarding" || up["stp_pathcost"] != float64(200) || up["stp_edge_port"] != false {
		t.Errorf("port 1 = stp_state %v stp_pathcost %v stp_edge_port %v", up["stp_state"], up["stp_pathcost"], up["stp_edge_port"])
	}
	down := portEntry(t, m, 2)
	if down["stp_state"] != "disabled" || down["stp_pathcost"] != float64(0) {
		t.Errorf("port 2 = stp_state %v stp_pathcost %v; the ECS sends a down port disabled at cost 0", down["stp_state"], down["stp_pathcost"])
	}
	held := portEntry(t, m, 3)
	if a, _ := held["anomalies"].(float64); int(a) != portAnomalyLoopProtection {
		t.Errorf("port 3 (held by loop protection) anomalies = %v, want exactly the loop-protection bit (512)", held["anomalies"])
	}
	// The controller's "disabled by Loop Protection" alert follows
	// error_disabled with reason bit 1, not anomalies (§11.23).
	if held["error_disabled"] != true || held["error_disabled_reason"] != float64(errDisabledLoopProtection) {
		t.Errorf("port 3 error_disabled %v reason %v, want true and %d", held["error_disabled"], held["error_disabled_reason"], errDisabledLoopProtection)
	}
	for _, i := range []int{1, 2} {
		if v, ok := portEntry(t, m, i)["error_disabled"]; ok {
			t.Errorf("port %d (not held) sends error_disabled %v", i, v)
		}
	}
}

// The DX010 with BPDU guard on port 17 from the controller and the empty
// cage looped by the chip's MAC loopback (collect-bpduguard.txt,
// 2026-11-15): the port heard its own BPDU and mstpd shut it (bpdu guard
// error yes). The controller's BPDU-guard alert ("UniFi disabled <port>
// because it detected a network loop") follows error_disabled with reason
// bit 2 (docs/drivers/sonic.md §11.23); the port carries no anomalies bit
// for it, and no other port is error-disabled.
func TestWireContractSonicBPDUGuard(t *testing.T) {
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261114-42cccc87a", "collect-bpduguard.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := sonic.NewCollector(fr)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := sonicRender(t, c, snap, "USWF07D")
	p := portEntry(t, m, 17)
	if p["error_disabled"] != true || p["error_disabled_reason"] != float64(errDisabledBPDUGuard) {
		t.Errorf("port 17 error_disabled %v reason %v, want true and %d", p["error_disabled"], p["error_disabled_reason"], errDisabledBPDUGuard)
	}
	if a, _ := p["anomalies"].(float64); a != 0 {
		t.Errorf("port 17 anomalies = %v, want 0 (no anomalies bit is BPDU guard)", a)
	}
	for _, e := range m["port_table"].([]any) {
		q := e.(map[string]any)
		if q["port_idx"] != float64(17) && q["error_disabled"] != nil {
			t.Errorf("port %v error_disabled %v", q["port_idx"], q["error_disabled"])
		}
	}
	t.Logf("port 17: up %v enable %v stp_state %v stp_role %v", p["up"], p["enable"], p["stp_state"], p["stp_role"])
}

// The same trip with l2d holding the port down in the chip
// (collect-bpduguard-held.txt, 2026-11-15, §11.28): APPL_DB admin_status
// down under CONFIG_DB's up, and l2d's verdict bpdu_guard_held yes. The port
// stays enabled (the operator's setting) and error-disabled for BPDU guard.
// The capture's oper_status still reads up: the MAC loopback keeps the
// chip's link up inside a disabled port, which a cabled peer would not.
// With l2d's verdict "no" (a port released by the operator while mstpd's old
// error stands, as it does until the link comes back) nothing is
// error-disabled.
func TestWireContractSonicBPDUGuardHeld(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7", "collect-bpduguard-held.txt")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, held string
		want       bool
	}{{"held", "yes", true}, {"released", "no", false}} {
		t.Run(c.name, func(t *testing.T) {
			text := strings.Replace(string(raw), `"bpdu_guard_held": "yes"`, `"bpdu_guard_held": "`+c.held+`"`, 1)
			fr, err := sonic.NewFixtureRunner(path)
			if err != nil {
				t.Fatal(err)
			}
			fr.Fixture = text
			c2 := sonic.NewCollector(fr)
			snap, err := c2.Start(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			m := sonicRender(t, c2, snap, "USWF07D")
			p := portEntry(t, m, 17)
			if p["enable"] != true {
				t.Errorf("port 17 enable = %v: the hold is not the operator's setting", p["enable"])
			}
			got := p["error_disabled"] == true && p["error_disabled_reason"] == float64(errDisabledBPDUGuard)
			if got != c.want {
				t.Errorf("port 17 error_disabled %v reason %v, want %v", p["error_disabled"], p["error_disabled_reason"], c.want)
			}
		})
	}
}
