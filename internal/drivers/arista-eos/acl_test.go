package aristaeos

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The captured switch has an IPv4 ACL of its own (mDNS-block, bound
// nowhere) and none of the driver's: nothing is reported as the driver's,
// nothing as bound.
func TestACLStateFromTheCapturedRunningConfig(t *testing.T) {
	c, _ := startedCollector(t)
	st := c.acl
	if st.exists || len(st.seqs) != 0 || len(st.blocks) != 0 || st.stray != 0 {
		t.Errorf("the capture holds no %s, got %+v", ipACLName, st)
	}
	if len(st.bound) != 0 || len(st.foreign) != 0 {
		t.Errorf("no interface in the capture carries an ACL: bound %v, foreign %v", st.bound, st.foreign)
	}
}

// Every front-panel lane is a target, a split cage's four lanes included;
// the management port and VLAN interfaces are not.
func TestACLTargetsAreEveryFrontPanelLane(t *testing.T) {
	c, _ := startedCollector(t)
	targets := aclTargets(c.last.Ports)
	set := map[string]bool{}
	for _, tg := range targets {
		set[tg] = true
	}
	for _, want := range []string{"Ethernet1", "Ethernet48", "Ethernet49/1", "Ethernet50/1", "Ethernet50/4", "Ethernet54/1"} {
		if !set[want] {
			t.Errorf("%s is not a target: %v", want, targets)
		}
	}
	for _, not := range []string{"Management1", "Vlan1", "Port-Channel1"} {
		if set[not] {
			t.Errorf("%s must not be bound (no aggregate has members in the capture)", not)
		}
	}
	if len(targets) != 48+6+3 {
		t.Errorf("%d targets, want 57 (48 copper, 6 cages, 3 more lanes on the split one)", len(targets))
	}
}

// The DHCP guard for the captured push's VLAN 69 (two trusted servers): a
// permit per server, then the drop.
func TestGuardLinesFromTheCapturedPush(t *testing.T) {
	got, err := guardLines(69, []string{"192.0.2.5", "192.0.2.1", "192.0.2.5"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"permit vlan 69 0xfff udp host 192.0.2.5 eq 67 any eq 68",
		"permit vlan 69 0xfff udp host 192.0.2.1 eq 67 any eq 68",
		"deny vlan 69 0xfff udp any eq 67 any eq 68",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("guard:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := guardLines(69, []string{"2001:db8::1"}); err == nil {
		t.Error("an IPv6 server must be refused: the guard is an IPv4 list")
	}
}

// The captured IP rules (docs/fixtures/controller-10.6.109/system_cfg-ecs-acl.txt):
// an Allow for every protocol, and a TCP Block whose source side carries the
// any-port range 1-65535 and whose destination names one port.
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
		// Shapes EOS takes that the capture lacks: any, a host, a range.
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
		{"", ipSide{}, ipSide{Port: 53}},                 // a port needs TCP or UDP
		{"icmp", ipSide{}, ipSide{}},                     // not a UniFi protocol
		{"tcp", ipSide{CIDR: "2001:db8::/32"}, ipSide{}}, // IPv4 only
		{"tcp", ipSide{}, ipSide{Port: 80, PortMin: 1, PortMax: 2}},
		{"udp", ipSide{}, ipSide{PortMin: 9, PortMax: 3}},
	} {
		if got, err := ipRuleLine(true, bad.proto, bad.src, bad.dst); err == nil {
			t.Errorf("%+v rendered as %q, want a refusal", bad, got)
		}
	}
}

func sampleWant(t *testing.T) aclWant {
	t.Helper()
	guard, err := guardLines(1, []string{"192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := ipRuleLine(false, "tcp", ipSide{CIDR: "203.0.113.0/24"}, ipSide{CIDR: "192.0.2.0/24", Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	return aclWant{guard: guard, rules: []string{rule}}
}

// From the captured switch (no ACL of the driver's), the plan writes the
// whole list -- guard block, rules block, the closing permit -- and binds it
// to every target. Planning again from the state it leaves writes nothing.
func TestACLPlanCreatesTheListAndBindsEveryTarget(t *testing.T) {
	c, _ := startedCollector(t)
	targets := aclTargets(c.last.Ports)
	want := sampleWant(t)
	body, next, skipped := aclPlan(c.acl, want, targets)
	if len(skipped) != 0 {
		t.Errorf("skipped %v", skipped)
	}
	list := []string{
		"ip access-list " + ipACLName,
		"10 remark disunified dhcp-guard " + blockHash(want.guard),
		"20 permit vlan 1 0xfff udp host 192.0.2.1 eq 67 any eq 68",
		"30 deny vlan 1 0xfff udp any eq 67 any eq 68",
		"40 remark disunified ip-rules " + blockHash(want.rules),
		"50 deny tcp 203.0.113.0/24 192.0.2.0/24 eq 9",
		"60 remark disunified default",
		"70 permit ip any any",
		"exit",
	}
	if got := strings.Join(body[:len(list)], "\n"); got != strings.Join(list, "\n") {
		t.Fatalf("list:\n%s\nwant:\n%s", got, strings.Join(list, "\n"))
	}
	binds := body[len(list):]
	if len(binds) != 3*len(targets) {
		t.Fatalf("%d binding commands for %d targets", len(binds), len(targets))
	}
	if strings.Join(binds[:3], " / ") != "interface Ethernet1 / ip access-group "+ipACLName+" in / exit" {
		t.Errorf("first binding = %v", binds[:3])
	}
	if again, _, _ := aclPlan(next, want, targets); len(again) != 0 {
		t.Errorf("second plan from the written state: %v (want nothing)", again)
	}
}

// A block the controller leaves alone keeps the switch's lines and hash;
// only the other block is rewritten.
func TestACLPlanKeepsABlockTheControllerLeavesAlone(t *testing.T) {
	c, _ := startedCollector(t)
	targets := aclTargets(c.last.Ports)
	_, written, _ := aclPlan(c.acl, sampleWant(t), targets)
	body, next, _ := aclPlan(written, aclWant{keepGuard: true}, targets)
	got := strings.Join(body, "\n")
	for _, want := range []string{
		"no 70",
		"10 remark disunified dhcp-guard " + blockHash(sampleWant(t).guard),
		"20 permit vlan 1 0xfff udp host 192.0.2.1 eq 67 any eq 68",
		"40 remark disunified default",
		"50 permit ip any any",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("body lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ip-rules") || strings.Contains(got, "access-group") {
		t.Errorf("the rules block is gone and the bindings stay:\n%s", got)
	}
	if again, _, _ := aclPlan(next, aclWant{keepGuard: true}, targets); len(again) != 0 {
		t.Errorf("not idempotent: %v", again)
	}
}

// With no rule left the list is unbound everywhere, then deleted.
func TestACLPlanRemovesTheListWhenNoRuleIsLeft(t *testing.T) {
	c, _ := startedCollector(t)
	targets := aclTargets(c.last.Ports)
	_, written, _ := aclPlan(c.acl, sampleWant(t), targets)
	body, next, _ := aclPlan(written, aclWant{}, targets)
	if len(body) != 3*len(targets)+1 || body[len(body)-1] != "no ip access-list "+ipACLName {
		t.Fatalf("body ends %v (len %d)", body[len(body)-3:], len(body))
	}
	if strings.Join(body[:3], " / ") != "interface Ethernet1 / no ip access-group "+ipACLName+" in / exit" {
		t.Errorf("first unbind = %v", body[:3])
	}
	if next.exists || len(next.bound) != 0 {
		t.Errorf("next = %+v", next)
	}
	if again, _, _ := aclPlan(next, aclWant{}, targets); len(again) != 0 {
		t.Errorf("not idempotent: %v", again)
	}
}

// An interface that already carries another IPv4 ACL inbound is left alone
// and reported: EOS binds one per direction.
func TestACLPlanLeavesAnInterfaceWithItsOwnACL(t *testing.T) {
	c, _ := startedCollector(t)
	have := c.acl
	have.foreign = map[string]string{"Ethernet2": "mDNS-block"}
	body, next, skipped := aclPlan(have, sampleWant(t), aclTargets(c.last.Ports))
	if len(skipped) != 1 || skipped[0] != "Ethernet2 (mDNS-block)" {
		t.Errorf("skipped = %v", skipped)
	}
	if strings.Contains(strings.Join(body, "\n"), "interface Ethernet2\n") || next.bound["Ethernet2"] {
		t.Error("bound over another ACL")
	}
}

func aclCollector(t *testing.T, redial func(string) Transport) (*Collector, *FixtureTransport) {
	t.Helper()
	c, ft := addressCollector(t, redial)
	c.aclSettle = 0
	return c, ft
}

// The change goes in a timed session; a fresh connection that answers
// confirms it, and the same rules are not written again.
func TestApplyACLConfirmsOverAFreshConnection(t *testing.T) {
	fresh := &answering{FixtureTransport: NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))}
	var dialled string
	c, ft := aclCollector(t, func(h string) Transport { dialled = h; return fresh })
	before := len(ft.Configured)
	n, err := c.applyACL(context.Background(), c.last, sampleWant(t))
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
	if n, err := c.applyACL(context.Background(), c.last, sampleWant(t)); n != 0 || err != nil || len(ft.Configured) != before+1 {
		t.Errorf("second apply: n=%d err=%v batches=%d (want idempotent)", n, err, len(ft.Configured)-before)
	}
}

// A change after which the switch stops answering is left to the timer, and
// the same rules are held, not sent again every cycle.
func TestApplyACLHoldsAChangeThatCutsTheBridgeOff(t *testing.T) {
	fresh := &answering{FixtureTransport: NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M")), fail: true}
	c, ft := aclCollector(t, func(string) Transport { return fresh })
	before := len(ft.Configured)
	if _, err := c.applyACL(context.Background(), c.last, sampleWant(t)); err == nil || !strings.Contains(err.Error(), "revert") {
		t.Fatalf("err = %v, want a failure that says the timer reverts", err)
	}
	if len(fresh.Configured) != 0 {
		t.Errorf("confirmed over a connection that did not answer: %v", fresh.Configured)
	}
	if _, err := c.applyACL(context.Background(), c.last, sampleWant(t)); err == nil || !strings.Contains(err.Error(), "holding") {
		t.Errorf("second apply: err = %v, want the hold", err)
	}
	if len(ft.Configured) != before+1 {
		t.Errorf("the held change was sent again: %v", ft.Configured[before:])
	}
	other := sampleWant(t)
	other.rules = nil
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
	ft := &rejecting{NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))}
	c := NewCollector(ft)
	c.aclSettle = 0
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.applyACL(context.Background(), c.last, sampleWant(t)); err == nil {
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

// Both captured pushes at once on the captured switch: the guard for the
// VLANs it carries (1 and 2; it has no VLAN 69), servers in push order, then
// the IP rules, in one timed session.
func TestApplyDeviceWritesTheCapturedGuardsAndRules(t *testing.T) {
	c, ft := aclCollector(t, nil)
	on := true
	before := len(ft.Configured)
	n, err := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{
		DHCPSnooping: &on, DHCPGuards: capturedSnoopGuards, IPACLSet: true, IPACLs: capturedIPRules, MACACLSet: true,
	})
	if err != nil || n == 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := ft.Configured[before:]
	if len(got) != 2 {
		t.Fatalf("batches %d, want the session and its confirm", len(got))
	}
	guard := []string{
		"permit vlan 1 0xfff udp host 192.0.2.1 eq 67 any eq 68",
		"deny vlan 1 0xfff udp any eq 67 any eq 68",
		"permit vlan 2 0xfff udp host 192.0.2.6 eq 67 any eq 68",
		"permit vlan 2 0xfff udp host 192.0.2.1 eq 67 any eq 68",
		"permit vlan 2 0xfff udp host 192.0.2.7 eq 67 any eq 68",
		"deny vlan 2 0xfff udp any eq 67 any eq 68",
	}
	rules := []string{
		"permit ip 192.0.2.0/24 198.51.100.0/24",
		"deny tcp 203.0.113.0/24 192.0.2.0/24 eq 9",
		"deny tcp 203.0.113.0/24 192.0.2.0/24 eq 1001",
	}
	list := []string{"enable", "configure session " + aclSession, "ip access-list " + ipACLName,
		"10 remark disunified dhcp-guard " + blockHash(guard)}
	seq := 20
	for _, l := range guard {
		list = append(list, strconv.Itoa(seq)+" "+l)
		seq += 10
	}
	list = append(list, strconv.Itoa(seq)+" remark disunified ip-rules "+blockHash(rules))
	seq += 10
	for _, l := range rules {
		list = append(list, strconv.Itoa(seq)+" "+l)
		seq += 10
	}
	list = append(list, strconv.Itoa(seq)+" remark disunified default", strconv.Itoa(seq+10)+" permit ip any any", "exit")
	if s := strings.Join(got[0][:len(list)], "\n"); s != strings.Join(list, "\n") {
		t.Errorf("session:\n%s\nwant:\n%s", s, strings.Join(list, "\n"))
	}
	if got[0][len(got[0])-1] != "commit timer "+commitTimer {
		t.Errorf("session ends %q", got[0][len(got[0])-1])
	}
	again, err := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{
		DHCPSnooping: &on, DHCPGuards: capturedSnoopGuards, IPACLSet: true, IPACLs: capturedIPRules, MACACLSet: true,
	})
	if again != 0 || err != nil || len(ft.Configured) != before+2 {
		t.Errorf("second apply: n=%d err=%v (want idempotent)", again, err)
	}
}

// What every push to this driver carries while nothing is claimed --
// snooping absent, no rules of either kind -- writes nothing on a switch
// without the driver's ACL. Snooping off, as the captured ECS push sends
// it, writes nothing either.
func TestApplyDeviceWritesNoACLWithoutRules(t *testing.T) {
	c, ft := aclCollector(t, nil)
	off := false
	for _, d := range []devicemodel.DeviceDesired{
		{IPACLSet: true, MACACLSet: true},
		{DHCPSnooping: &off, IPACLSet: true, MACACLSet: true},
		{DHCPSnooping: &off}, // rules held by the loop
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

// A rule EOS cannot write holds every IP rule, as the loop does for a shape
// it has not seen: the switch keeps its list, the guard is still written.
func TestApplyDeviceHoldsIPRulesWhenOneCannotBeWritten(t *testing.T) {
	c, ft := aclCollector(t, nil)
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
	if !strings.Contains(session, "remark disunified dhcp-guard") || strings.Contains(session, "ip-rules") {
		t.Errorf("session:\n%s", session)
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
	slow := &timingOut{NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M")), cancel}
	c, _ := aclCollector(t, func(string) Transport { return slow })
	if _, err := c.applyACL(ctx, c.last, sampleWant(t)); err == nil || !strings.Contains(err.Error(), "reverts") {
		t.Fatalf("err = %v", err)
	}
	if c.aclHeld != "" {
		t.Error("held after a budget timeout")
	}
	if got := c.ApplyTimeout(); got <= 0 || got >= 2*time.Minute {
		t.Errorf("write budget %v: it must hold an ACL confirm and end inside the 2-minute commit timer", got)
	}
}
