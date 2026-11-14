package sonic

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// DHCP guarding's count per port (acl_count.go, docs/drivers/sonic.md
// §11.7.9). collect-rdtest.txt is the DX010 on igmp-202511.20261106-f2e25f54c
// captured during the 2026-11-14 hand test: a table DUI_RDTEST of a CONFIG_DB
// type with IN_PORTS, bound to Ethernet0, Ethernet4 and Ethernet8, its rules
// in the guard's shape (V1_S1 FORWARD, V1_IN_Ethernet0 and V1_IN_Ethernet4
// counted DROPs, V1_DROP) but matching only the test host's own UDP 67 -> 68
// to the switch; ACL counter polling on; four replies sent from that host,
// which the switch's uplink Ethernet0 counted. The driver leaves DUI_RDTEST
// alone (it is not one of its tables) and reads its counters like the
// guard's.

const fixtureRDTest = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-rdtest.txt"

// guardTypeWrite and flexEnable are the guard's two writes besides its
// table and rules.
const (
	guardTypeWrite = "sudo sonic-db-cli CONFIG_DB hset 'ACL_TABLE_TYPE|DUI_DHCP_GUARD' 'ACTIONS@' 'PACKET_ACTION,COUNTER' 'BIND_POINTS@' 'PORT,PORTCHANNEL' 'MATCHES@' 'IN_PORTS,VLAN_ID,IP_PROTOCOL,L4_SRC_PORT,L4_DST_PORT,SRC_IP'"
	flexEnable     = "sudo sonic-db-cli CONFIG_DB hset 'FLEX_COUNTER_TABLE|ACL' FLEX_COUNTER_STATUS enable"
	guardMatch     = "IP_PROTOCOL '17' L4_DST_PORT '68' L4_SRC_PORT '67'"
)

// sortedNames is byPriority's tie order (by name) for one priority.
func sortedNames(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func guardHset(rule, fields string) string {
	return "sudo sonic-db-cli CONFIG_DB hset 'ACL_RULE|DUI_DHCP_GUARD|" + rule + "' " + fields
}

func countedHset(vlan, sw, port string) string {
	return guardHset("V"+vlan+"_IN_"+port, "IN_PORTS '"+port+"' "+guardMatch+" PACKET_ACTION 'DROP' PRIORITY '8500' VLAN_ID '"+sw+"'")
}

// guardOnVLAN1 is the commands for the guard on VLAN 1 trusting server,
// on a switch whose ports orchagent knows are ports; polling as given.
func guardOnVLAN1(server string, ports []string, polling bool) []string {
	out := []string{
		guardTypeWrite,
		"sudo config acl add table DUI_DHCP_GUARD DUI_DHCP_GUARD -p " + dx010Ports() + " -s ingress",
		guardHset("V1_S1", guardMatch+" PACKET_ACTION 'FORWARD' PRIORITY '9000' SRC_IP '"+server+"/32' VLAN_ID '4094'"),
	}
	for _, p := range sortedNames(ports) {
		out = append(out, countedHset("1", "4094", p))
	}
	out = append(out, guardHset("V1_DROP", guardMatch+" PACKET_ACTION 'DROP' PRIORITY '8000' VLAN_ID '4094'"))
	if !polling {
		out = append(out, flexEnable)
	}
	return append(out, save)
}

func vlan1Guard(t *testing.T) devicemodel.DeviceDesired {
	t.Helper()
	on := true
	var g []devicemodel.DHCPGuard
	for _, x := range guardsOf(ecsPush(t, "system_cfg-ecs-dhcp-snoop.txt")) {
		if x.VLAN == 1 {
			g = append(g, x)
		}
	}
	if len(g) != 1 || g[0].Server != "192.0.2.1" {
		t.Fatalf("capture changed: VLAN 1 guards %v", g)
	}
	return devicemodel.DeviceDesired{DHCPSnooping: &on, DHCPGuards: g}
}

func snapPort(t *testing.T, snap *devicemodel.Snapshot, idx int) devicemodel.Port {
	t.Helper()
	for _, p := range snap.Ports {
		if p.Index == idx {
			return p
		}
	}
	t.Fatalf("no port %d", idx)
	return devicemodel.Port{}
}

// The live capture: the type, polling and the counted rules' packets are
// read; nothing of DUI_RDTEST becomes a port's count (it is not the guard),
// and a push without the snooping key writes nothing.
func TestGuardCountReadsTheLiveCapture(t *testing.T) {
	r := rig20261106(t, fixtureRDTest)
	r.c.mu.Lock()
	st := r.c.state
	r.c.mu.Unlock()
	if got := st.gc.types["DUI_RDTEST"]["MATCHES@"]; got != guardType["MATCHES@"] {
		t.Errorf("the test table's type: MATCHES@ %q", got)
	}
	if st.gc.flexACL != "enable" {
		t.Errorf("polling %q, captured on", st.gc.flexACL)
	}
	if st.gc.packets["DUI_RDTEST|V1_IN_Ethernet0"] != "4" || st.gc.packets["DUI_RDTEST|V1_IN_Ethernet4"] != "0" {
		t.Errorf("packets %v: the uplink counted the four replies, the empty cage none", st.gc.packets)
	}
	if !st.gc.orch["Ethernet0"] || !st.gc.orch["Ethernet124"] || len(st.gc.orch) != 32 {
		t.Errorf("orchagent's ports: %d", len(st.gc.orch))
	}
	if s := st.aclStatus["DUI_RDTEST|V1_IN_Ethernet0"]; s != "Active" {
		t.Errorf("the counted rule on the custom type is %q in the capture", s)
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports {
		if p.Health.RogueDHCPDrops != 0 {
			t.Errorf("port %d: %d rogue DHCP drops from a table that is not the guard", p.Index, p.Health.RogueDHCPDrops)
		}
	}
	if got := r.deviceCmds(devicemodel.DeviceDesired{}); len(got) != 0 {
		t.Errorf("a push without the snooping key wrote %q", got)
	}
}

// The guard on the live capture: its type, the table, the trusted server's
// FORWARD, one counted DROP per port orchagent has, the shared DROP; polling
// is already on. Replies hitting cage 3's rule are cage 3's count, nobody
// else's; the trusted server's FORWARD has no counted rule. Snooping off
// takes all of it away again, the type after the table.
func TestGuardCountsPerPortOnTheDX010(t *testing.T) {
	r := rig20261106(t, fixtureRDTest)
	d := vlan1Guard(t)
	ports := strings.Split(dx010Ports(), ",")
	st := r.expectDevice("snooping on", d, guardOnVLAN1("192.0.2.1", ports, true))
	if s := st.aclStatus[guardTable]; s != "Active" {
		t.Errorf("guard table %q", s)
	}
	if n := len(st.rulesOf(guardTable)); n != 34 {
		t.Errorf("%d guard rules, want 34", n)
	}
	if !strings.Contains(r.log.String(), "acl: DUI_DHCP_GUARD 34 of 34 rules active") {
		t.Errorf("log:\n%s", r.log.String())
	}
	if _, ok := st.gc.packets[guardTable+"|V1_S1"]; ok {
		t.Errorf("the FORWARD is not a counted rule")
	}
	if err := r.fr.ACLHits("DUI_DHCP_GUARD:V1_IN_Ethernet8", 5); err != nil {
		t.Fatal(err)
	}
	if err := r.fr.ACLHits("DUI_RDTEST:V1_IN_Ethernet4", 7); err != nil {
		t.Fatal(err)
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports {
		want := uint64(0)
		if p.Index == 3 {
			want = 5
		}
		if p.Health.RogueDHCPDrops != want {
			t.Errorf("port %d: %d rogue DHCP drops, want %d", p.Index, p.Health.RogueDHCPDrops, want)
		}
	}
	off := false
	got := r.deviceCmds(devicemodel.DeviceDesired{DHCPSnooping: &off})
	if n := len(got); n != 34+3 || got[n-3] != "sudo config acl remove table DUI_DHCP_GUARD" || got[n-2] != "sudo sonic-db-cli CONFIG_DB del 'ACL_TABLE_TYPE|DUI_DHCP_GUARD'" {
		t.Errorf("snooping off: %q", got)
	}
	if got[0] != "sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V1_DROP'" {
		t.Errorf("the shared DROP goes first, the trusted FORWARD last: %q", got[0])
	}
}

// A guard written by an earlier build (type L3, no counted rules) is
// replaced: its rules and table go, the type comes, the new table and
// rules, and ACL counter polling is turned on (the 202505 capture predates
// the count and has it off by its absence).
func TestGuardCountReplacesAnL3Guard(t *testing.T) {
	r := newACLRig(t, nil)
	old := strings.Join([]string{
		"set -e",
		"sudo config acl add table DUI_DHCP_GUARD L3 -p " + dx010Ports() + " -s ingress",
		strings.TrimPrefix(guardHset("V1_S1", guardMatch+" PACKET_ACTION 'FORWARD' PRIORITY '9000' SRC_IP '192.0.2.1/32' VLAN_ID '4094'"), ""),
		guardHset("V1_DROP", guardMatch+" PACKET_ACTION 'DROP' PRIORITY '8000' VLAN_ID '4094'"),
	}, "\n") + "\n"
	if _, err := r.fr.Run(context.Background(), batchCommand, old); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V1_DROP'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V1_S1'",
		"sudo config acl remove table DUI_DHCP_GUARD",
	}
	want = append(want, guardOnVLAN1("192.0.2.1", strings.Split(dx010Ports(), ","), false)...)
	st := r.expectDevice("replace", vlan1Guard(t), want)
	if st.aclTables[guardTable].typ != guardTable || st.gc.flexACL != "enable" {
		t.Errorf("table type %q, polling %q", st.aclTables[guardTable].typ, st.gc.flexACL)
	}
	if !strings.Contains(r.log.String(), "acl: DUI_DHCP_GUARD 34 of 34 rules active") {
		t.Errorf("log:\n%s", r.log.String())
	}
}

// A counted rule only for a port orchagent has: one missing from STATE_DB
// gets none (aclorch would mark the rule Inactive and never retry); once
// STATE_DB has it, the next apply adds its rule. A counted rule that is
// Inactive for its port is written again.
func TestGuardCountWaitsForOrchagentsPorts(t *testing.T) {
	r := rig20261106(t, fixtureRDTest)
	hide := func(p string) {
		if err := r.fr.update("state_port", func(tb table) error { delete(tb, "PORT_TABLE|"+p); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	saved := r.fr.table("state_port")["PORT_TABLE|Ethernet12"]
	hide("Ethernet12")
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	var ports []string
	for _, p := range strings.Split(dx010Ports(), ",") {
		if p != "Ethernet12" {
			ports = append(ports, p)
		}
	}
	r.expectDevice("without Ethernet12", vlan1Guard(t), guardOnVLAN1("192.0.2.1", ports, true))
	if err := r.fr.update("state_port", func(tb table) error { tb["PORT_TABLE|Ethernet12"] = saved; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.expectDevice("Ethernet12 known", vlan1Guard(t), []string{countedHset("1", "4094", "Ethernet12"), save})

	// orchagent refused Ethernet16's rule (here: the port vanished from
	// STATE_DB under the rule and came back, which the fixture turns into
	// Inactive): the rule is written again.
	saved = r.fr.table("state_port")["PORT_TABLE|Ethernet16"]
	hide("Ethernet16")
	if err := r.fr.orchACL(); err != nil {
		t.Fatal(err)
	}
	if err := r.fr.update("state_port", func(tb table) error { tb["PORT_TABLE|Ethernet16"] = saved; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.c.mu.Lock()
	s := r.c.state.aclStatus[guardTable+"|V1_IN_Ethernet16"]
	r.c.mu.Unlock()
	if s != "Inactive" {
		t.Fatalf("setup: Ethernet16's rule is %q", s)
	}
	r.expectDevice("Inactive rewritten", vlan1Guard(t), []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V1_IN_Ethernet16'",
		countedHset("1", "4094", "Ethernet16"), save,
	})
}

// A breakout removes the cage's lane, which a counted rule names: the rule
// goes inside the port batch before `config interface breakout` (the shared
// DROP guards the cage meanwhile), no rule is written for the new lanes
// until STATE_DB has them, and then each lane gets its own; the cage's
// count is the sum of its lanes'.
func TestGuardCountAcrossABreakout(t *testing.T) {
	r := rig20261106(t, fixtureRDTest)
	r.expectDevice("guard", vlan1Guard(t), guardOnVLAN1("192.0.2.1", strings.Split(dx010Ports(), ","), true))
	base := r.baseline()
	got, _ := r.apply(speedOn(base, 2, 25000))
	brk := -1
	del := -1
	for i, c := range got {
		if strings.Contains(c, "config interface breakout") {
			brk = i
		}
		if c == "sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V1_IN_Ethernet4'" {
			del = i
		}
		if strings.Contains(c, "V1_IN_Ethernet5") || (strings.Contains(c, "hset 'ACL_RULE|DUI_DHCP_GUARD|V1_IN_Ethernet4'")) {
			t.Errorf("a rule for a lane orchagent does not have yet: %q", c)
		}
	}
	if brk < 0 || del < 0 || del > brk {
		t.Fatalf("the counted rule must go before the breakout: %q", got)
	}
	// orchagent creates the lanes: STATE_DB has them at the next collect.
	if err := r.fr.update("state_port", func(tb table) error {
		for _, l := range []string{"Ethernet4", "Ethernet5", "Ethernet6", "Ethernet7"} {
			tb["PORT_TABLE|"+l] = map[string]string{"state": "ok", "supported_speeds": "1000,10000,25000"}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.expectDevice("lanes known", vlan1Guard(t), []string{
		countedHset("1", "4094", "Ethernet4"), countedHset("1", "4094", "Ethernet5"),
		countedHset("1", "4094", "Ethernet6"), countedHset("1", "4094", "Ethernet7"), save,
	})
	for _, l := range []string{"Ethernet5", "Ethernet7"} {
		if err := r.fr.ACLHits("DUI_DHCP_GUARD:V1_IN_"+l, 2); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := snapPort(t, snap, 2); p.Lanes != 4 || p.Health.RogueDHCPDrops != 4 {
		t.Errorf("cage 2: %d lanes, %d drops (two lanes' 2 each)", p.Lanes, p.Health.RogueDHCPDrops)
	}
}

// Counted rules stop at guardEntries: VLANs in number order get them while
// the table stays within it; a VLAN past it keeps its FORWARDs and shared
// DROP and is noted.
func TestGuardCountBudget(t *testing.T) {
	r := rig20261106(t, fixtureRDTest)
	var ids []int
	var guards []devicemodel.DHCPGuard
	for v := 2; v <= 10; v++ {
		ids = append(ids, v)
		guards = append(guards, devicemodel.DHCPGuard{VLAN: v, Server: "192.0.2.1"})
	}
	if _, err := r.c.EnsureVLANs(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	on := true
	r.deviceCmds(devicemodel.DeviceDesired{DHCPSnooping: &on, DHCPGuards: guards})
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.c.mu.Lock()
	rules := r.c.state.rulesOf(guardTable)
	r.c.mu.Unlock()
	// 9 VLANs x (FORWARD + DROP) = 18, then 32 counted rules per VLAN:
	// 18 + 7 x 32 = 242 <= 256; the eighth would make 274.
	if len(rules) != 18+7*32 {
		t.Errorf("%d rules, want %d", len(rules), 18+7*32)
	}
	for v := 2; v <= 10; v++ {
		_, has := rules[fmt.Sprintf("V%d_IN_Ethernet0", v)]
		if has != (v <= 8) {
			t.Errorf("VLAN %d counted: %v", v, has)
		}
		if _, ok := rules[fmt.Sprintf("V%d_DROP", v)]; !ok {
			t.Errorf("VLAN %d lost its shared DROP", v)
		}
	}
	if !strings.Contains(r.log.String(), "VLAN 9's per-port counts would take DUI_DHCP_GUARD past 256 entries") {
		t.Errorf("log:\n%s", r.log.String())
	}
}

// Polling off on a switch whose guard exists: logged, no count.
func TestGuardCountPollingOff(t *testing.T) {
	r := rig20261106(t, fixtureRDTest)
	r.expectDevice("guard", vlan1Guard(t), guardOnVLAN1("192.0.2.1", strings.Split(dx010Ports(), ","), true))
	if _, err := r.fr.Run(context.Background(), batchCommand, "set -e\nsudo sonic-db-cli CONFIG_DB hset 'FLEX_COUNTER_TABLE|ACL' FLEX_COUNTER_STATUS disable\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.log.String(), "orchagent does not poll ACL counters") {
		t.Errorf("log:\n%s", r.log.String())
	}
	if got := r.deviceCmds(vlan1Guard(t)); len(got) != 2 || got[0] != flexEnable {
		t.Errorf("the next apply turns polling on again: %q", got)
	}
}
