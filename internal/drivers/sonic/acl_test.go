package sonic

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// DHCP guarding and the IP ACL rules (acl.go, docs/drivers/sonic.md
// §11.7.5) against the DX010 collected with the ACL sections
// (collect-acl.txt: no DUI_ table yet, CRM's three free ingress tables)
// and its own config help, driven by the controller's real pushes to the
// ECS Aggregation (controller-10.6.109/system_cfg-ecs-dhcp-snoop.txt,
// system_cfg-ecs-acl.txt). The FixtureRunner models the CLI's refusals and
// orchagent's STATE_DB verdict.

const aclFixture = "../../../docs/fixtures/sonic-202505.1238526-ff3c0c9f6/collect-acl.txt"

func newACLRig(t *testing.T, edit func(*FixtureRunner)) *controlRig {
	t.Helper()
	fr, err := NewFixtureRunner(aclFixture)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(fr)
	}
	var buf strings.Builder
	c := NewCollector(fr)
	c.Log = log.New(&buf, "", 0)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &controlRig{t: t, c: c, fr: fr, log: &buf}
}

// ecsPush parses one of the controller's captured pushes to the ECS.
func ecsPush(t *testing.T, name string) *unificfg.Config {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "fixtures", "controller-10.6.109", name))
	if err != nil {
		t.Fatal(err)
	}
	return unificfg.Parse(string(b))
}

// The push's groups in the model, as the loop hands them over (the loop's
// own translation is tested in internal/informloop).
func guardsOf(cfg *unificfg.Config) []devicemodel.DHCPGuard {
	var out []devicemodel.DHCPGuard
	for _, g := range cfg.DHCPGuards {
		out = append(out, devicemodel.DHCPGuard{VLAN: g.VLAN, Server: g.Server})
	}
	return out
}

func ipACLsOf(cfg *unificfg.Config) []devicemodel.IPACL {
	var out []devicemodel.IPACL
	for _, a := range cfg.IPACLs {
		proto := a.Type
		if proto == "ip" {
			proto = ""
		}
		end := func(m unificfg.IPMatch) devicemodel.IPACLEnd { return devicemodel.IPACLEnd{CIDR: m.IP, Port: m.Port} }
		out = append(out, devicemodel.IPACL{Name: a.Name, Permit: a.Permit, Protocol: proto, Src: end(a.Src), Dst: end(a.Dst)})
	}
	return out
}

// expectDevice applies d, checks the commands, collects, and checks a
// second apply writes nothing.
func (r *controlRig) expectDevice(name string, d devicemodel.DeviceDesired, want []string) *swState {
	r.t.Helper()
	if got := r.deviceCmds(d); !reflect.DeepEqual(got, want) {
		r.t.Errorf("%s: commands\n  got  %q\n  want %q", name, got, want)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	if again := r.deviceCmds(d); len(again) != 0 {
		r.t.Errorf("%s: a second apply wrote %q", name, again)
	}
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.c.state
}

// dx010Ports is the -p list for the DX010's 32 unsplit cages.
func dx010Ports() string {
	var ps []string
	for i := 0; i < 32; i++ {
		ps = append(ps, fmt.Sprintf("Ethernet%d", i*4))
	}
	return strings.Join(ps, ",")
}

func TestACLClaimsAndCommandChecks(t *testing.T) {
	r := newACLRig(t, nil)
	caps := r.c.Capabilities()
	if !caps.DHCPSnooping || caps.IPACLs != ipACLRules || caps.MACACLs != 0 {
		t.Errorf("capabilities = %+v: DHCP snooping and %d IP rules claimed, no MAC rules", caps, ipACLRules)
	}
	for _, h := range []string{"sudo config acl --help", "sudo config acl add --help", "sudo config acl remove --help"} {
		if !containsString(r.fr.Helps, h) {
			t.Errorf("Start did not check %q: %v", h, r.fr.Helps)
		}
	}
	// No FEATURE|stp on the DX010: every group but spanning-tree's two.
	if len(r.fr.Helps) != len(commandChecks)-2 {
		t.Errorf("Start read %d help texts: %v", len(r.fr.Helps), r.fr.Helps)
	}
	off := false
	if got := r.deviceCmds(devicemodel.DeviceDesired{DHCPSnooping: &off, IPACLSet: true, MACACLSet: true}); len(got) != 0 {
		t.Errorf("snooping off and no rules on a switch without the tables wrote %q", got)
	}
	// An ACL write can only come after the collector read the tables.
	old := newRig(t, nil)
	if caps := old.c.Capabilities(); caps.DHCPSnooping || caps.IPACLs != 0 {
		t.Errorf("a capture without the ACL sections must claim nothing: %+v", caps)
	}
}

// The ECS's push with Rogue DHCP Server Detection on: six guards on VLANs
// 1, 69 and 2. On a switch carrying those VLANs, one FORWARD per trusted
// server and one DROP per VLAN; UniFi's VLAN 1 matches Vlan4094.
func TestDHCPGuardFromTheECSPush(t *testing.T) {
	r := newACLRig(t, nil)
	if _, err := r.c.EnsureVLANs(context.Background(), []int{1, 2, 69}); err != nil {
		t.Fatal(err)
	}
	cfg := ecsPush(t, "system_cfg-ecs-dhcp-snoop.txt")
	on := true
	d := devicemodel.DeviceDesired{DHCPSnooping: &on, DHCPGuards: guardsOf(cfg), IPACLSet: true}
	hset := func(rule, fields string) string {
		return "sudo sonic-db-cli CONFIG_DB hset 'ACL_RULE|DUI_DHCP_GUARD|" + rule + "' " + fields
	}
	fwd := func(rule, vid, src string) string {
		return hset(rule, "IP_PROTOCOL '17' L4_DST_PORT '68' L4_SRC_PORT '67' PACKET_ACTION 'FORWARD' PRIORITY '9000' SRC_IP '"+src+"/32' VLAN_ID '"+vid+"'")
	}
	drop := func(rule, vid string) string {
		return hset(rule, "IP_PROTOCOL '17' L4_DST_PORT '68' L4_SRC_PORT '67' PACKET_ACTION 'DROP' PRIORITY '8000' VLAN_ID '"+vid+"'")
	}
	st := r.expectDevice("snooping on", d, []string{
		"sudo config acl add table DUI_DHCP_GUARD L3 -p " + dx010Ports() + " -s ingress",
		fwd("V1_S1", "4094", "192.0.2.1"),
		fwd("V2_S1", "2", "192.0.2.6"),
		fwd("V2_S2", "2", "192.0.2.1"),
		fwd("V2_S3", "2", "192.0.2.7"),
		fwd("V69_S1", "69", "192.0.2.5"),
		fwd("V69_S2", "69", "192.0.2.1"),
		drop("V1_DROP", "4094"),
		drop("V2_DROP", "2"),
		drop("V69_DROP", "69"),
		save,
	})
	if s := st.aclStatus[guardTable]; s != "Active" {
		t.Errorf("table status %q", s)
	}
	if len(st.rulesOf(guardTable)) != 9 {
		t.Errorf("read back %d rules, want 9", len(st.rulesOf(guardTable)))
	}
	if !strings.Contains(r.log.String(), "acl: DUI_DHCP_GUARD 9 of 9 rules active; CRM ingress PORT tables 0 used, 3 free") {
		t.Errorf("the collect after the write logs orchagent's verdict and CRM's counters:\n%s", r.log.String())
	}
	if strings.Contains(r.log.String(), "not Active") {
		t.Errorf("every rule is valid for orchagent:\n%s", r.log.String())
	}

	// The site's setting off: the rules go, the DROPs first, then the table.
	off := false
	st = r.expectDevice("snooping off", devicemodel.DeviceDesired{DHCPSnooping: &off}, []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V69_DROP'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V2_DROP'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V1_DROP'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V69_S2'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V69_S1'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V2_S3'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V2_S2'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V2_S1'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_DHCP_GUARD|V1_S1'",
		"sudo config acl remove table DUI_DHCP_GUARD",
		save,
	})
	if len(st.aclTables) != 0 || len(st.aclRules) != 0 || len(st.aclStatus) != 0 {
		t.Errorf("left behind: tables %v rules %v status %v", st.aclTables, st.aclRules, st.aclStatus)
	}
}

// Guards for VLANs the switch does not carry write nothing (the DX010
// carries only VLAN 1 today); snooping on with no guard leaves no table; a
// key absent from the push (nil) leaves the table alone.
func TestDHCPGuardOnlyWhereItApplies(t *testing.T) {
	r := newACLRig(t, nil)
	cfg := ecsPush(t, "system_cfg-ecs-dhcp-snoop.txt")
	on := true
	var others []devicemodel.DHCPGuard
	for _, g := range guardsOf(cfg) {
		if g.VLAN != 1 {
			others = append(others, g)
		}
	}
	if got := r.deviceCmds(devicemodel.DeviceDesired{DHCPSnooping: &on, DHCPGuards: others}); len(got) != 0 {
		t.Errorf("guards on VLANs the switch lacks wrote %q", got)
	}
	if got := r.deviceCmds(devicemodel.DeviceDesired{DHCPSnooping: &on}); len(got) != 0 {
		t.Errorf("snooping with no guard wrote %q", got)
	}
	r.deviceCmds(devicemodel.DeviceDesired{DHCPSnooping: &on, DHCPGuards: guardsOf(cfg)})
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.deviceCmds(devicemodel.DeviceDesired{}); len(got) != 0 {
		t.Errorf("a push without the snooping key must leave the table alone, wrote %q", got)
	}
	// A guard naming native_vlan itself, and a server that is not IPv4.
	r.deviceCmds(devicemodel.DeviceDesired{DHCPSnooping: &on, DHCPGuards: []devicemodel.DHCPGuard{{VLAN: 4094, Server: "192.0.2.1"}, {VLAN: 1, Server: "2001:db8::1"}}})
	for _, want := range []string{"VLAN 4094 is this switch's native_vlan", `server "2001:db8::1" on VLAN 1 is not an IPv4 address`} {
		if !strings.Contains(r.log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, r.log.String())
		}
	}
}

// The ECS's ACL push: three IP rules become DUI_IP_ACL's RULE_1..3 in the
// controller's order; the two MAC rules are refused with a log (no L2 table
// on the stock image).
func TestIPACLsFromTheECSPush(t *testing.T) {
	r := newACLRig(t, nil)
	cfg := ecsPush(t, "system_cfg-ecs-acl.txt")
	ips := ipACLsOf(cfg)
	if len(ips) != 3 || len(cfg.MACACLs) != 2 {
		t.Fatalf("capture changed: %d IP rules, %d MAC rules", len(ips), len(cfg.MACACLs))
	}
	macs := []devicemodel.MACACL{{Name: cfg.MACACLs[0].Name}, {Name: cfg.MACACLs[1].Name}}
	hset := func(rule, fields string) string {
		return "sudo sonic-db-cli CONFIG_DB hset 'ACL_RULE|DUI_IP_ACL|" + rule + "' " + fields
	}
	rule1 := hset("RULE_1", "DST_IP '198.51.100.0/24' IP_TYPE 'IPV4ANY' PACKET_ACTION 'FORWARD' PRIORITY '9998' SRC_IP '192.0.2.0/24'")
	rule2 := hset("RULE_2", "DST_IP '192.0.2.0/24' IP_PROTOCOL '6' L4_DST_PORT '9' PACKET_ACTION 'DROP' PRIORITY '9997' SRC_IP '203.0.113.0/24'")
	rule3 := hset("RULE_3", "DST_IP '192.0.2.0/24' IP_PROTOCOL '6' L4_DST_PORT '1001' PACKET_ACTION 'DROP' PRIORITY '9996' SRC_IP '203.0.113.0/24'")
	st := r.expectDevice("rules", devicemodel.DeviceDesired{IPACLSet: true, IPACLs: ips, MACACLSet: true, MACACLs: macs}, []string{
		"sudo config acl add table DUI_IP_ACL L3 -p " + dx010Ports() + " -s ingress",
		rule1, rule2, rule3, save,
	})
	if s := st.aclStatus[ipACLTable+"|RULE_2"]; s != "Active" {
		t.Errorf("RULE_2 status %q", s)
	}
	if !strings.Contains(r.log.String(), "MAC ACL rules (2): community SONiC's orchagent has no L2 table type") {
		t.Errorf("MAC rules must be refused with a log:\n%s", r.log.String())
	}

	// The first rule deleted in the UI: the others move up a slot.
	r.expectDevice("first rule gone", devicemodel.DeviceDesired{IPACLSet: true, IPACLs: ips[1:]}, []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_IP_ACL|RULE_3'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_IP_ACL|RULE_1'",
		strings.Replace(strings.Replace(rule2, "RULE_2", "RULE_1", 1), "9997", "9998", 1),
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_IP_ACL|RULE_2'",
		strings.Replace(strings.Replace(rule3, "RULE_3", "RULE_2", 1), "9996", "9997", 1),
		save,
	})
	// A rule the loop could not translate holds the set: nothing touched.
	if got := r.deviceCmds(devicemodel.DeviceDesired{}); len(got) != 0 {
		t.Errorf("IPACLSet false wrote %q", got)
	}
	// No rule left: the table goes.
	st = r.expectDevice("no rules", devicemodel.DeviceDesired{IPACLSet: true}, []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_IP_ACL|RULE_2'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_IP_ACL|RULE_1'",
		"sudo config acl remove table DUI_IP_ACL",
		save,
	})
	if len(st.aclTables) != 0 || len(st.aclRules) != 0 {
		t.Errorf("left behind: %v %v", st.aclTables, st.aclRules)
	}
}

// A rule set the table cannot express is held whole, with a log.
func TestIPRulesRefusals(t *testing.T) {
	cfg := ecsPush(t, "system_cfg-ecs-acl.txt")
	ips := ipACLsOf(cfg)
	cases := map[string]func([]devicemodel.IPACL) []devicemodel.IPACL{
		"more than": func(a []devicemodel.IPACL) []devicemodel.IPACL {
			return append(a, make([]devicemodel.IPACL, ipACLRules)...)
		},
		"rule for every protocol": func(a []devicemodel.IPACL) []devicemodel.IPACL { a[0].Dst.Port = 9; return a },
		"not an IPv4 prefix":      func(a []devicemodel.IPACL) []devicemodel.IPACL { a[1].Src.CIDR = "2001:db8::/32"; return a },
		`protocol "icmp"`:         func(a []devicemodel.IPACL) []devicemodel.IPACL { a[2].Protocol = "icmp"; return a },
		"port range 9-1": func(a []devicemodel.IPACL) []devicemodel.IPACL {
			a[2].Dst.Port, a[2].Dst.PortMin, a[2].Dst.PortMax = 0, 9, 1
			return a
		},
	}
	for want, fn := range cases {
		rules, why := ipRules(fn(append([]devicemodel.IPACL(nil), ips...)))
		if rules != nil || !strings.Contains(why, want) {
			t.Errorf("%s: rules %v, why %q", want, rules, why)
		}
	}
	// A range the table takes (the UI refuses ranges; mapped all the same).
	a := append([]devicemodel.IPACL(nil), ips...)
	a[2].Dst = devicemodel.IPACLEnd{CIDR: a[2].Dst.CIDR, PortMin: 1000, PortMax: 1010}
	rules, why := ipRules(a)
	if why != "" || rules["RULE_3"]["L4_DST_PORT_RANGE"] != "1000-1010" || rules["RULE_3"]["L4_DST_PORT"] != "" {
		t.Errorf("range: %v %q", rules["RULE_3"], why)
	}
}

// The tables bind to every port and PortChannel: after an aggregate or a
// breakout the list is written again, a member out and the PortChannel in.
func TestACLTablesFollowThePorts(t *testing.T) {
	r := newACLRig(t, nil)
	cfg := ecsPush(t, "system_cfg-ecs-acl.txt")
	r.deviceCmds(devicemodel.DeviceDesired{IPACLSet: true, IPACLs: ipACLsOf(cfg)})
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := r.baseline()
	if got, _ := r.apply(base); len(got) != 0 {
		t.Fatalf("baseline wrote %q", got)
	}
	ds := edit(base, 2, func(d *devicemodel.PortDesired) { d.LAG = 2 })
	ds = edit(ds, 3, func(d *devicemodel.PortDesired) { d.LAG = 2 })
	got, _ := r.apply(ds)
	var lagged []string
	for i := 0; i < 32; i++ {
		if i != 1 && i != 2 {
			lagged = append(lagged, fmt.Sprintf("Ethernet%d", i*4))
		}
	}
	rebind := "sudo config acl add table DUI_IP_ACL L3 -p " + strings.Join(append(lagged, "PortChannel2"), ",") + " -s ingress"
	if len(got) < 2 || got[len(got)-2] != rebind || got[len(got)-1] != save {
		t.Errorf("aggregate: the ACL table must be rebound after the port batch, got %q\nwant it to end with %q", got, rebind)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.c.mu.Lock()
	st := r.c.state
	r.c.mu.Unlock()
	if st.aclStatus[ipACLTable] != "Active" || st.aclTables[ipACLTable].ports["Ethernet4"] || !st.aclTables[ipACLTable].ports["PortChannel2"] {
		t.Errorf("after the aggregate: status %q, ports %v", st.aclStatus[ipACLTable], sortIfnames(st.aclTables[ipACLTable].ports))
	}
	if again, _ := r.apply(ds); len(again) != 0 {
		t.Errorf("second apply wrote %q", again)
	}

	// Back apart, then cage 2 split 4x25G: the children are bound.
	r.apply(base)
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ = r.apply(speedOn(base, 2, 25000))
	var split []string
	for i := 0; i < 32; i++ {
		if i == 1 {
			split = append(split, "Ethernet4", "Ethernet5", "Ethernet6", "Ethernet7")
			continue
		}
		split = append(split, fmt.Sprintf("Ethernet%d", i*4))
	}
	rebind = "sudo config acl add table DUI_IP_ACL L3 -p " + strings.Join(split, ",") + " -s ingress"
	if len(got) < 2 || got[len(got)-2] != rebind {
		t.Errorf("breakout: got %q\nwant it to end with %q", got, rebind)
	}
	if !containsString(got, "sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null") {
		t.Errorf("no breakout in %q", got)
	}
}

// orchagent's verdict is read back: a rule it refused is logged.
func TestACLVerdictIsLogged(t *testing.T) {
	r := newACLRig(t, nil)
	cfg := ecsPush(t, "system_cfg-ecs-acl.txt")
	r.deviceCmds(devicemodel.DeviceDesired{IPACLSet: true, IPACLs: ipACLsOf(cfg)})
	body := r.fr.section("state_acl")
	r.fr.UseSection("state_acl", strings.Replace(body, `"ACL_RULE_TABLE|DUI_IP_ACL|RULE_2": {"status": "Active"}`, `"ACL_RULE_TABLE|DUI_IP_ACL|RULE_2": {"status": "Inactive"}`, 1))
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ACL rule DUI_IP_ACL|RULE_2 is Inactive in orchagent", "DUI_IP_ACL 2 of 3 rules active"} {
		if !strings.Contains(r.log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, r.log.String())
		}
	}
}
