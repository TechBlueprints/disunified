package aristaeos

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

const aclFixtureDir = "../../../docs/fixtures/arista-eos-4.26.14M"

// The captured switch has an IPv4 ACL of its own (mDNS-block, bound
// nowhere) and none of the driver's.
func TestACLStateFromTheCapturedRunningConfig(t *testing.T) {
	c, _ := startedCollector(t)
	st := c.acl
	if len(st.lists) != 0 || len(st.bound) != 0 || len(st.foreign) != 0 {
		t.Errorf("the capture holds no list of the driver's and binds nothing: %+v", st)
	}
}

// dui-acl is the captured switch with the driver's lists on the two test
// ports, as the live run of 2026-11-14 left them before its abort
// (show-running-config-dui-acl.json: the session body
// session-dui-acl-test-ports.txt, rendered by this driver, committed under
// a timer).
func duiACLCollector(t *testing.T) (*Collector, *FixtureTransport) {
	t.Helper()
	ft := NewFixtureTransport(aclFixtureDir)
	ft.Overrides = map[string]string{
		"show running-config":         "show-running-config-dui-acl.json",
		"show ip access-lists summary": "show-ip-access-lists-summary-dui-acl.json",
	}
	c := NewCollector(ft)
	c.host = fixtureHost
	c.aclSettle = 0
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, ft
}

func TestACLStateFromTheDriversLists(t *testing.T) {
	c, _ := duiACLCollector(t)
	st := c.acl
	if len(st.lists) != 2 || st.bound["Ethernet2"] != "DUI_IP_ACL_Ethernet2" || st.bound["Ethernet54/1"] != "DUI_IP_ACL_Ethernet54-1" {
		t.Fatalf("state %+v", st)
	}
	l := st.lists["DUI_IP_ACL_Ethernet2"]
	g := l.blocks[blockDHCPGuard]
	if !l.counters || l.stray != 0 || g.hash != "53d29e982de7" || len(g.lines) != 3 || len(l.seqs) != 6 {
		t.Errorf("list %+v", l)
	}
	if d := l.blocks[blockDefault]; len(d.lines) != 1 || d.lines[0] != aclDefaultAllow {
		t.Errorf("default block %+v", d)
	}
}

// Every front-panel lane is a target, a split cage's four lanes included;
// the management port and VLAN interfaces are not.
func TestACLTargetsAreEveryFrontPanelLane(t *testing.T) {
	c, _ := startedCollector(t)
	targets := aclTargets(c.last.Ports)
	set := map[string]int{}
	for _, tg := range targets {
		set[tg.iface] = tg.port.Index
	}
	for _, want := range []string{"Ethernet1", "Ethernet48", "Ethernet49/1", "Ethernet50/1", "Ethernet50/4", "Ethernet54/1"} {
		if _, ok := set[want]; !ok {
			t.Errorf("%s is not a target", want)
		}
	}
	if set["Ethernet50/4"] != set["Ethernet50/1"] {
		t.Errorf("a split cage's lanes belong to one port: %v", set)
	}
	for _, not := range []string{"Management1", "Vlan1", "Port-Channel1"} {
		if _, ok := set[not]; ok {
			t.Errorf("%s must not be bound (no aggregate has members in the capture)", not)
		}
	}
	if len(targets) != 48+6+3 {
		t.Errorf("%d targets, want 57 (48 copper, 6 cages, 3 more lanes on the split one)", len(targets))
	}
}

// capturedSnoopGuards and capturedIPRules are the controller's real pushes to
// the ECS Aggregation (docs/fixtures/controller-10.6.109/system_cfg-ecs-dhcp-snoop.txt
// and system_cfg-ecs-acl.txt) as the loop hands them over; informloop's
// TestDesiredDeviceGuardsAndACLs checks that translation against the pushes.
var (
	capturedSnoopGuards = []devicemodel.DHCPGuard{
		{VLAN: 1, Server: "192.0.2.1"}, {VLAN: 69, Server: "192.0.2.5"}, {VLAN: 69, Server: "192.0.2.1"},
		{VLAN: 2, Server: "192.0.2.6"}, {VLAN: 2, Server: "192.0.2.1"}, {VLAN: 2, Server: "192.0.2.7"},
	}
	capturedIPRules = []devicemodel.IPACL{
		{Name: "dui-test-allow-ip-1", Permit: true, Src: devicemodel.IPACLEnd{CIDR: "192.0.2.0/24"}, Dst: devicemodel.IPACLEnd{CIDR: "198.51.100.0/24"}},
		{Name: "dui-test-block-ip-1", Protocol: "tcp", Src: devicemodel.IPACLEnd{CIDR: "203.0.113.0/24"}, Dst: devicemodel.IPACLEnd{CIDR: "192.0.2.0/24", Port: 9}},
		{Name: "dui-test-block-ip-2", Protocol: "tcp", Src: devicemodel.IPACLEnd{CIDR: "203.0.113.0/24"}, Dst: devicemodel.IPACLEnd{CIDR: "192.0.2.0/24", Port: 1001}},
	}
	capturedMACRules = []devicemodel.MACACL{
		{Name: "dui-test-allow-mac-1", Permit: true, VLAN: 4010,
			Src: devicemodel.MACACLEnd{MAC: "02:00:00:00:00:01", Mask: "ff:ff:ff:ff:ff:ff"}, Dst: devicemodel.MACACLEnd{MAC: "02:00:00:00:00:02", Mask: "ff:ff:ff:ff:ff:ff"}},
	}
)

func wantGuards(t *testing.T, guards []devicemodel.DHCPGuard, vlans []int, mode string) aclWant {
	t.Helper()
	on := true
	w, err := aclWantFrom(devicemodel.DeviceDesired{DHCPSnooping: &on, DHCPGuards: guards}, vlans, mode)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// withAccess is the captured snapshot with the named ports made access ports
// of a VLAN (the capture's ports are all trunks carrying every VLAN).
func withAccess(snap *devicemodel.Snapshot, vlan int, ifnames ...string) *devicemodel.Snapshot {
	s := *snap
	s.Ports = append([]devicemodel.Port(nil), snap.Ports...)
	for i, p := range s.Ports {
		for _, n := range ifnames {
			if p.IfName == n {
				s.Ports[i].VLAN = devicemodel.PortVLAN{Mode: "access", NativeVLAN: vlan}
			}
		}
	}
	return &s
}

// The captured push's guards on the captured switch's VLANs (1, 2, 3, 8, 9,
// 10, 1000, 2000, 3000; no 69). Exact: an access port of VLAN 1 is guarded
// with VLAN 1's server; a trunk carrying every VLAN is not (VLAN 3 and
// others have no server list), and says so. Edge: the trunk is guarded
// with the servers of every guarded VLAN it carries, VLAN 69's dropped as
// not carried; an exempt port is not.
func TestPortGuardByMode(t *testing.T) {
	c, _ := startedCollector(t)
	snap := withAccess(c.last, 1, "Ethernet2")
	var access, trunk devicemodel.Port
	for _, p := range snap.Ports {
		switch p.IfName {
		case "Ethernet2":
			access = p
		case "Ethernet3":
			trunk = p
		}
	}
	exact := wantGuards(t, capturedSnoopGuards, snap.VLANs, guardExact)
	if _, ok := exact.guards[69]; ok {
		t.Error("VLAN 69 is not on the switch and must be skipped")
	}
	lines, why := portGuard(exact, access, snap.VLANs, nil)
	if strings.Join(lines, "\n") != "permit udp host 192.0.2.1 eq 67 any eq 68\n"+guardDrop || why != "" {
		t.Errorf("access port: %q (%s)", lines, why)
	}
	if lines, why := portGuard(exact, trunk, snap.VLANs, nil); lines != nil || !strings.Contains(why, "unguarded VLAN 3,") {
		t.Errorf("trunk, exact: %q (%s)", lines, why)
	}
	edge := wantGuards(t, capturedSnoopGuards, snap.VLANs, guardEdge)
	lines, _ = portGuard(edge, trunk, snap.VLANs, map[int]bool{})
	want := []string{
		"permit udp host 192.0.2.1 eq 67 any eq 68", // VLAN 1, then 2's in push order, 192.0.2.1 once
		"permit udp host 192.0.2.6 eq 67 any eq 68",
		"permit udp host 192.0.2.7 eq 67 any eq 68",
		guardDrop,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("trunk, edge:\n%s", strings.Join(lines, "\n"))
	}
	if lines, why := portGuard(edge, trunk, snap.VLANs, map[int]bool{trunk.Index: true}); lines != nil || why == "" {
		t.Errorf("exempt trunk, edge: %q (%s)", lines, why)
	}
	if _, err := aclWantFrom(devicemodel.DeviceDesired{DHCPSnooping: &[]bool{true}[0], DHCPGuards: []devicemodel.DHCPGuard{{VLAN: 1, Server: "2001:db8::1"}}}, snap.VLANs, guardExact); err == nil {
		t.Error("an IPv6 server must be refused: the guard is an IPv4 list")
	}
}

// The edge mode's exemptions on the captured switch: the ports with a
// bridge as LLDP neighbour (the uplink among them) and the gateway's port.
func TestGuardExemptFromTheCapture(t *testing.T) {
	c, _ := startedCollector(t)
	ex := guardExempt(c.last)
	up := c.last.UplinkPort()
	if up == 0 || !ex[up] {
		t.Fatalf("uplink %d not exempt: %v", up, ex)
	}
	for idx := range ex {
		p := portByIndex(t, c.last, idx)
		if p.Neighbor == nil || !p.Neighbor.IsBridge {
			mac := c.last.System.ARP[c.last.System.Gateway]
			found := false
			for _, e := range c.last.MACTable {
				found = found || (e.MAC == mac && e.PortIndex == idx)
			}
			if !found {
				t.Errorf("port %d exempt with no bridge neighbour and no gateway MAC", idx)
			}
		}
	}
}

// The plan for VLAN 1 guarded (servers 192.0.2.1 and 192.0.2.5), the test
// ports access ports of VLAN 1, every other port a trunk carrying every
// VLAN: a list each for the two test ports, none elsewhere. This is the
// body run live on 2026-11-14 (session-dui-acl-test-ports.txt). Planning
// again from the running config it left (captured) writes nothing.
func TestACLPlanIsTheBodyRunLive(t *testing.T) {
	c, _ := startedCollector(t)
	snap := withAccess(c.last, 1, "Ethernet2", "Ethernet54/1")
	w := aclWant{guards: map[int][]string{1: {"192.0.2.1", "192.0.2.5"}}, keepRules: true, mode: guardExact}
	body, next, skipped, unguarded := aclPlan(c.acl, w, aclTargets(snap.Ports), snap.VLANs, nil)
	live, err := os.ReadFile(filepath.Join(aclFixtureDir, "session-dui-acl-test-ports.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(body, "\n"); got != strings.TrimSpace(string(live)) {
		t.Errorf("body:\n%s\nlive run:\n%s", got, live)
	}
	if len(skipped) != 0 || len(unguarded) != 55 {
		t.Errorf("skipped %v, %d unguarded", skipped, len(unguarded))
	}
	if again, _, _, _ := aclPlan(next, w, aclTargets(snap.Ports), snap.VLANs, nil); len(again) != 0 {
		t.Errorf("planning from its own result: %v", again)
	}

	d, _ := duiACLCollector(t)
	if again, _, _, _ := aclPlan(d.acl, w, aclTargets(snap.Ports), snap.VLANs, nil); len(again) != 0 {
		t.Errorf("planning from the captured result: %v", again)
	}
}

// From the captured lists: a changed server list rewrites the port's list in
// place (old sequence numbers removed); snooping off unbinds both lists and
// deletes them; a hand-made list on an interface is left alone.
func TestACLPlanRewritesAndRemoves(t *testing.T) {
	d, _ := duiACLCollector(t)
	snap := withAccess(d.last, 1, "Ethernet2", "Ethernet54/1")
	targets := aclTargets(snap.Ports)
	w := aclWant{guards: map[int][]string{1: {"192.0.2.1"}}, keepRules: true, mode: guardExact}
	body, _, _, _ := aclPlan(d.acl, w, targets, snap.VLANs, nil)
	s := strings.Join(body, "\n")
	for _, want := range []string{"ip access-list DUI_IP_ACL_Ethernet2\ncounters per-entry\nno 10\nno 20\nno 30\nno 40\nno 50\nno 60\n10 remark disunified dhcp-guard ",
		"20 permit udp host 192.0.2.1 eq 67 any eq 68\n30 deny udp any eq 67 any eq 68\n40 remark disunified default\n50 permit ip any any\nexit"} {
		if !strings.Contains(s, want) {
			t.Errorf("rewrite lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "ip access-group") {
		t.Errorf("a list already bound is not bound again:\n%s", s)
	}

	off := aclWant{keepRules: true}
	body, next, _, _ := aclPlan(d.acl, off, targets, snap.VLANs, nil)
	want := "interface Ethernet2\nno ip access-group DUI_IP_ACL_Ethernet2 in\nexit\n" +
		"interface Ethernet54/1\nno ip access-group DUI_IP_ACL_Ethernet54-1 in\nexit\n" +
		"no ip access-list DUI_IP_ACL_Ethernet2\nno ip access-list DUI_IP_ACL_Ethernet54-1"
	if got := strings.Join(body, "\n"); got != want {
		t.Errorf("removal:\n%s\nwant:\n%s", got, want)
	}
	if len(next.lists) != 0 || len(next.bound) != 0 {
		t.Errorf("after removal: %+v", next)
	}

	c, _ := startedCollector(t)
	have := c.acl
	have.foreign = map[string]string{"Ethernet2": "mDNS-block"}
	body, _, skipped, _ := aclPlan(have, w, targets, snap.VLANs, nil)
	if len(skipped) != 1 || strings.Contains(strings.Join(body, "\n"), "DUI_IP_ACL_Ethernet2") {
		t.Errorf("an interface with its own ACL: skipped %v, body %v", skipped, body)
	}
}

// The captured IP rules: an Allow for every protocol, and a TCP Block whose
// source side carries the any-port range 1-65535 and whose destination
// names one port.
func TestIPRuleLinesFromTheCapturedPush(t *testing.T) {
	for _, tc := range []struct {
		permit   bool
		proto    string
		src, dst ipSide
		want     string
	}{
		{true, "", ipSide{CIDR: "192.0.2.0/24"}, ipSide{CIDR: "198.51.100.0/24"}, "permit ip 192.0.2.0/24 198.51.100.0/24"},
		{false, "tcp", ipSide{CIDR: "203.0.113.0/24", PortMin: 1, PortMax: 65535}, ipSide{CIDR: "192.0.2.0/24", Port: 9}, "deny tcp 203.0.113.0/24 192.0.2.0/24 eq 9"},
		{false, "tcp", ipSide{CIDR: "203.0.113.0/24", PortMin: 1, PortMax: 65535}, ipSide{CIDR: "192.0.2.0/24", Port: 1001}, "deny tcp 203.0.113.0/24 192.0.2.0/24 eq 1001"},
		{true, "udp", ipSide{}, ipSide{CIDR: "192.0.2.7/32", PortMin: 1000, PortMax: 2000}, "permit udp any host 192.0.2.7 range 1000 2000"},
		{false, "tcp", ipSide{CIDR: "192.0.2.9/24"}, ipSide{PortMin: 22, PortMax: 22}, "deny tcp 192.0.2.0/24 any eq 22"},
	} {
		got, err := ipRuleLine(tc.permit, tc.proto, tc.src, tc.dst)
		if err != nil || got != tc.want {
			t.Errorf("rule %+v: %q, %v; want %q", tc, got, err, tc.want)
		}
	}
	for _, bad := range []struct {
		proto    string
		src, dst ipSide
	}{
		{"", ipSide{}, ipSide{Port: 53}},
		{"icmp", ipSide{}, ipSide{}},
		{"tcp", ipSide{CIDR: "2001:db8::/32"}, ipSide{}},
		{"tcp", ipSide{}, ipSide{Port: 80, PortMin: 1, PortMax: 2}},
		{"udp", ipSide{}, ipSide{PortMin: 9, PortMax: 3}},
	} {
		if got, err := ipRuleLine(true, bad.proto, bad.src, bad.dst); err == nil {
			t.Errorf("%+v rendered as %q, want a refusal", bad, got)
		}
	}
}

// IP rules go in every interface's list, after the guard where there is one.
func TestApplyDeviceWritesTheCapturedRulesOnEveryInterface(t *testing.T) {
	c, ft := aclCollector(t, nil)
	before := len(ft.Configured)
	n, err := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{IPACLSet: true, IPACLs: capturedIPRules, MACACLSet: true})
	if err != nil || n == 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := ft.Configured[before:]
	if len(got) != 2 {
		t.Fatalf("batches %d, want the session and its confirm", len(got))
	}
	s := strings.Join(got[0], "\n")
	if strings.Count(s, "\nip access-list DUI_IP_ACL_") != 57 || strings.Count(s, "\nip access-group DUI_IP_ACL_") != 57 {
		t.Errorf("want 57 lists, each bound once:\n%s", s)
	}
	one := "ip access-list DUI_IP_ACL_Ethernet50-3\ncounters per-entry\n10 remark disunified ip-rules " +
		blockHash([]string{"permit ip 192.0.2.0/24 198.51.100.0/24", "deny tcp 203.0.113.0/24 192.0.2.0/24 eq 9", "deny tcp 203.0.113.0/24 192.0.2.0/24 eq 1001"}) +
		"\n20 permit ip 192.0.2.0/24 198.51.100.0/24\n30 deny tcp 203.0.113.0/24 192.0.2.0/24 eq 9\n40 deny tcp 203.0.113.0/24 192.0.2.0/24 eq 1001\n50 remark disunified default\n60 permit ip any any\nexit"
	if !strings.Contains(s, one) {
		t.Errorf("a split cage's lane list:\n%s", s)
	}
	if again, err := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{IPACLSet: true, IPACLs: capturedIPRules, MACACLSet: true}); again != 0 || err != nil {
		t.Errorf("second apply: n=%d err=%v (want idempotent)", again, err)
	}
}

// The captured guard push on the captured switch, exact: every port is a
// trunk carrying unguarded VLANs, so nothing is written. Edge: every port
// but the exempt ones gets a guard.
func TestApplyDeviceGuardsByMode(t *testing.T) {
	on := true
	d := devicemodel.DeviceDesired{DHCPSnooping: &on, DHCPGuards: capturedSnoopGuards, MACACLSet: true}
	c, ft := aclCollector(t, nil)
	before := len(ft.Configured)
	if n, err := c.ApplyDevice(context.Background(), d); n != 0 || err != nil || len(ft.Configured) != before {
		t.Errorf("exact: n=%d err=%v batches %v", n, err, ft.Configured[before:])
	}
	c.dhcpGuard = guardEdge
	if n, err := c.ApplyDevice(context.Background(), d); n == 0 || err != nil {
		t.Fatalf("edge: n=%d err=%v", n, err)
	}
	s := strings.Join(ft.Configured[before], "\n")
	ex := guardExempt(c.last)
	lists := strings.Count(s, "\nip access-list ")
	exemptIfaces := 0
	for _, tg := range aclTargets(c.last.Ports) {
		if ex[tg.port.Index] {
			exemptIfaces++
			if strings.Contains(s, "ip access-list "+aclListName(tg.iface)+"\n") {
				t.Errorf("exempt %s guarded", tg.iface)
			}
		}
	}
	if exemptIfaces == 0 || lists != 57-exemptIfaces {
		t.Errorf("%d lists, %d exempt interfaces", lists, exemptIfaces)
	}
}

// What every push to this driver carries while nothing is claimed, and
// snooping off, write nothing on a switch without the driver's lists.
func TestApplyDeviceWritesNoACLWithoutRules(t *testing.T) {
	c, ft := aclCollector(t, nil)
	off := false
	for _, d := range []devicemodel.DeviceDesired{
		{IPACLSet: true, MACACLSet: true},
		{DHCPSnooping: &off, IPACLSet: true, MACACLSet: true},
		{DHCPSnooping: &off},
	} {
		before := len(ft.Configured)
		if n, err := c.ApplyDevice(context.Background(), d); n != 0 || err != nil || len(ft.Configured) != before {
			t.Errorf("%+v: n=%d err=%v batches %v", d, n, err, ft.Configured[before:])
		}
	}
}

// MAC rules are refused with the reason; the IP rules in the same push still
// go in.
func TestApplyDeviceRefusesMACRules(t *testing.T) {
	c, ft := aclCollector(t, nil)
	before := len(ft.Configured)
	_, err := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{
		IPACLSet: true, IPACLs: capturedIPRules, MACACLSet: true, MACACLs: capturedMACRules,
	})
	if err == nil || !strings.Contains(err.Error(), "1 MAC rules not applied") {
		t.Errorf("err = %v", err)
	}
	if len(ft.Configured) != before+2 || strings.Contains(strings.Join(ft.Configured[before], "\n"), "mac access") {
		t.Errorf("batches: %v", ft.Configured[before:])
	}
}

// A rule EOS cannot write holds every IP rule: the guard is still written.
func TestApplyDeviceHoldsIPRulesWhenOneCannotBeWritten(t *testing.T) {
	c, ft := aclCollector(t, nil)
	c.last = withAccess(c.last, 1, "Ethernet2")
	on := true
	bad := append([]devicemodel.IPACL(nil), capturedIPRules...)
	bad = append(bad, devicemodel.IPACL{Name: "all-with-port", Dst: devicemodel.IPACLEnd{Port: 53}})
	before := len(ft.Configured)
	_, err := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{
		DHCPSnooping: &on, DHCPGuards: capturedSnoopGuards[:1], IPACLSet: true, IPACLs: bad,
	})
	if err == nil || !strings.Contains(err.Error(), `rule "all-with-port"`) {
		t.Errorf("err = %v", err)
	}
	session := strings.Join(ft.Configured[before], "\n")
	if !strings.Contains(session, "ip access-list DUI_IP_ACL_Ethernet2\ncounters per-entry\n10 remark disunified dhcp-guard") || strings.Contains(session, "ip-rules") {
		t.Errorf("session:\n%s", session)
	}
}

func aclCollector(t *testing.T, redial func(string) Transport) (*Collector, *FixtureTransport) {
	t.Helper()
	c, ft := addressCollector(t, redial)
	c.aclSettle = 0
	return c, ft
}

func sampleWant() aclWant {
	return aclWant{keepGuard: true, rules: []string{"deny tcp 203.0.113.0/24 192.0.2.0/24 eq 9"}}
}

// The change goes in a timed session; a fresh connection that answers
// confirms it, and the same rules are not written again.
func TestApplyACLConfirmsOverAFreshConnection(t *testing.T) {
	fresh := &answering{FixtureTransport: NewFixtureTransport(aclFixtureDir)}
	var dialled string
	c, ft := aclCollector(t, func(h string) Transport { dialled = h; return fresh })
	before := len(ft.Configured)
	n, err := c.applyACL(context.Background(), c.last, sampleWant())
	if err != nil || n == 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := ft.Configured[before:]
	if len(got) != 1 {
		t.Fatalf("batches on the switch's transport: %v", got)
	}
	session := got[0]
	if session[0] != "enable" || session[1] != "configure session "+aclSession || session[len(session)-1] != "commit timer "+commitTimer {
		t.Errorf("session batch = %v ... %v", session[:2], session[len(session)-1])
	}
	if dialled != fixtureHost || !fresh.closed {
		t.Errorf("confirmed via %q (closed %v), want a fresh dial to the switch", dialled, fresh.closed)
	}
	if len(fresh.Configured) != 1 || strings.Join(fresh.Configured[0], " / ") != "enable / configure session "+aclSession+" commit / write memory" {
		t.Errorf("confirm = %v", fresh.Configured)
	}
	if n, err := c.applyACL(context.Background(), c.last, sampleWant()); n != 0 || err != nil || len(ft.Configured) != before+1 {
		t.Errorf("second apply: n=%d err=%v batches=%d (want idempotent)", n, err, len(ft.Configured)-before)
	}
}

// A change after which the switch stops answering is left to the timer, and
// the same rules are held, not sent again every cycle.
func TestApplyACLHoldsAChangeThatCutsTheBridgeOff(t *testing.T) {
	fresh := &answering{FixtureTransport: NewFixtureTransport(aclFixtureDir), fail: true}
	c, ft := aclCollector(t, func(string) Transport { return fresh })
	before := len(ft.Configured)
	if _, err := c.applyACL(context.Background(), c.last, sampleWant()); err == nil || !strings.Contains(err.Error(), "revert") {
		t.Fatalf("err = %v, want a failure that says the timer reverts", err)
	}
	if len(fresh.Configured) != 0 {
		t.Errorf("confirmed over a connection that did not answer: %v", fresh.Configured)
	}
	if _, err := c.applyACL(context.Background(), c.last, sampleWant()); err == nil || !strings.Contains(err.Error(), "holding") {
		t.Errorf("second apply: err = %v, want the hold", err)
	}
	if len(ft.Configured) != before+1 {
		t.Errorf("the held change was sent again: %v", ft.Configured[before:])
	}
	other := sampleWant()
	other.rules = append(other.rules, "permit ip 192.0.2.0/24 198.51.100.0/24")
	fresh.fail = false
	if _, err := c.applyACL(context.Background(), c.last, other); err != nil {
		t.Errorf("different rules are sent: %v", err)
	}
}

// rejecting stands in for a switch that refuses the session batch.
type rejecting struct{ *FixtureTransport }

func (r *rejecting) Configure(ctx context.Context, cmds []string) error {
	_ = r.FixtureTransport.Configure(ctx, cmds)
	if strings.Contains(strings.Join(cmds, "\n"), "commit timer") {
		return errors.New("% Invalid input")
	}
	return nil
}

// A batch EOS rejects is aborted, so the next attempt does not resume its
// leftovers, and it is not held.
func TestApplyACLAbortsARejectedSession(t *testing.T) {
	ft := &rejecting{NewFixtureTransport(aclFixtureDir)}
	c := NewCollector(ft)
	c.aclSettle = 0
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.applyACL(context.Background(), c.last, sampleWant()); err == nil {
		t.Fatal("a rejected batch reported success")
	}
	last := ft.Configured[len(ft.Configured)-1]
	if strings.Join(last, " / ") != "enable / configure session "+aclSession+" abort" {
		t.Errorf("last batch = %v, want the abort", last)
	}
	if c.aclHeld != "" {
		t.Error("a rejected batch is not a reason to hold the rules")
	}
}

// timingOut stands in for a dial that outlives the write budget.
type timingOut struct {
	*FixtureTransport
	cancel context.CancelFunc
}

func (s *timingOut) Run(ctx context.Context, _ []string) ([]json.RawMessage, error) {
	s.cancel()
	return nil, ctx.Err()
}

// A confirm the write budget cut short says nothing about the rules: the
// timer reverts, and the same rules are tried again, not held.
func TestApplyACLDoesNotHoldWhenTheBudgetRunsOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := &timingOut{NewFixtureTransport(aclFixtureDir), cancel}
	c, _ := aclCollector(t, func(string) Transport { return slow })
	if _, err := c.applyACL(ctx, c.last, sampleWant()); err == nil || !strings.Contains(err.Error(), "reverts") {
		t.Fatalf("err = %v", err)
	}
	if c.aclHeld != "" {
		t.Error("held after a budget timeout")
	}
	if got := c.ApplyTimeout(); got <= 0 || got >= 2*time.Minute {
		t.Errorf("write budget %v: it must hold an ACL confirm and end inside the 2-minute commit timer", got)
	}
}
