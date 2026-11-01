package device

import "testing"

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
	if a, _ := held["anomalies"].(float64); int(a)&anomLoopKeepalive == 0 {
		t.Errorf("port 3 (held by loop protection) anomalies = %v, want the loop bit (512)", held["anomalies"])
	}
}
