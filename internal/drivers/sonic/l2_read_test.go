package sonic

import (
	"context"
	"strings"
	"testing"
	"time"
)

// l2RunningFixture is the DX010 collected while disunified-l2d ran it
// (docs/drivers/sonic.md §11.5, 2026-11-01): mstpd on Bridge with the ECS
// Aggregation as root (8192) and Ethernet0 the root port, two topology
// changes, and loop protection holding Ethernet8 after a test MAC flapped
// between the two empty cages Ethernet4 and Ethernet8.
const l2RunningFixture = "../../../docs/fixtures/sonic-202505.1238526-ff3c0c9f6/collect-l2-running.txt"

func TestL2ReadRunning(t *testing.T) {
	fr, err := NewFixtureRunner(l2RunningFixture)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fr.Help, fr.Zones = vs.Help, vs.Zones
	c := NewCollector(fr)
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	c.L2 = true
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sys := snap.System
	if sys.STPMode != "rstp" || sys.STPPriority != 32768 || sys.STPRoot != "02:00:00:00:00:52" {
		t.Errorf("bridge: mode %q priority %d root %q", sys.STPMode, sys.STPPriority, sys.STPRoot)
	}
	if sys.STPTopologyChanges != 2 {
		t.Errorf("topology changes = %d, want mstpd's 2", sys.STPTopologyChanges)
	}
	if d := snap.TakenAt.Sub(sys.STPLastTopologyChange); d < 286*time.Second || d > 288*time.Second {
		t.Errorf("last topology change %s before the collect, want 287 s", d)
	}
	up := port(snap, 1)
	if up.STPState != "forwarding" || up.STPRole != "root" || up.STPPathCost != 200 || up.STPOperEdge || up.BPDUGuard || up.Fault != "" {
		t.Errorf("port 1 (Ethernet0) = state %q role %q cost %d edge %v guard %v fault %q", up.STPState, up.STPRole, up.STPPathCost, up.STPOperEdge, up.BPDUGuard, up.Fault)
	}
	if up.Health.STPChanges != 2 {
		t.Errorf("port 1 STP changes = %d, want mstpd's one forwarding and one blocking transition", up.Health.STPChanges)
	}
	down := port(snap, 2)
	if down.STPState != "disabled" || down.STPRole != "disabled" || down.STPPathCost != 0 || down.Fault != "" {
		t.Errorf("port 2 (an empty cage) = state %q role %q cost %d fault %q; the ECS reports a down port disabled at cost 0", down.STPState, down.STPRole, down.STPPathCost, down.Fault)
	}
	held := port(snap, 3)
	if !strings.HasPrefix(held.Fault, "errdisabled: loop-protect") || !strings.Contains(held.Fault, "flapping with Ethernet4") {
		t.Errorf("port 3 (Ethernet8, held by loop protection) fault = %q", held.Fault)
	}
	if caps := c.Capabilities(); !caps.STP || !caps.STPAutoEdge || !caps.LoopProtection {
		t.Errorf("capabilities = %+v", caps)
	}
	in := parseL2Inst(sections(fr.Fixture)["l2d"])
	if in.Mstpd != "0.2.0-" || in.STPState != "2" || in.Conf == nil || !in.Conf.STP.Enabled || len(in.Conf.LoopGuard.Ports) != 2 {
		t.Errorf("l2d section = %+v", in)
	}
}
