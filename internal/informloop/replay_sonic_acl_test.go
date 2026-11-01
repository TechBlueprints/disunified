package informloop

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The DX010's own push (system_cfg-dx010-held.txt) with the snooping and
// ACL lines the controller sent the ECS, through the loop into the driver
// over the DX010 collected with the ACL sections: VLAN 1's guard on
// Vlan4094 (the DX010 carries no other guarded VLAN), the three IP rules,
// the MAC rules refused, and no "unsupported" line for snooping.
func TestReplaySonicDHCPGuardAndACL(t *testing.T) {
	var extra []string
	for name, prefix := range map[string]string{"system_cfg-ecs-dhcp-snoop.txt": "switch.dhcp_", "system_cfg-ecs-acl.txt": "switch.acl."} {
		for _, line := range strings.Split(readECSPush(t, name), "\n") {
			if strings.HasPrefix(line, prefix) {
				extra = append(extra, line)
			}
		}
	}
	text := strings.TrimRight(readECSPush(t, "system_cfg-dx010-held.txt"), "\n") + "\n" + strings.Join(extra, "\n") + "\n"

	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238526-ff3c0c9f6", "collect-acl.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	coll.L2 = false // spanning tree is TestReplaySonicSpanningTree's
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps := coll.Capabilities(); !caps.DHCPSnooping || caps.IPACLs == 0 {
		t.Fatalf("the controller pushes these keys only to a device that claims them: %+v", caps)
	}
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = coll.DefaultPortNames(p)
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
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
		"sudo config acl add table DUI_DHCP_GUARD L3 -p Ethernet0,",
		"'ACL_RULE|DUI_DHCP_GUARD|V1_S1' IP_PROTOCOL '17' L4_DST_PORT '68' L4_SRC_PORT '67' PACKET_ACTION 'FORWARD' PRIORITY '9000' SRC_IP '192.0.2.1/32' VLAN_ID '4094'",
		"'ACL_RULE|DUI_DHCP_GUARD|V1_DROP' IP_PROTOCOL '17' L4_DST_PORT '68' L4_SRC_PORT '67' PACKET_ACTION 'DROP' PRIORITY '8000' VLAN_ID '4094'",
		"sudo config acl add table DUI_IP_ACL L3 -p Ethernet0,",
		"'ACL_RULE|DUI_IP_ACL|RULE_1' DST_IP '198.51.100.0/24' IP_TYPE 'IPV4ANY' PACKET_ACTION 'FORWARD' PRIORITY '9998' SRC_IP '192.0.2.0/24'",
		"'ACL_RULE|DUI_IP_ACL|RULE_3' DST_IP '192.0.2.0/24' IP_PROTOCOL '6' L4_DST_PORT '1001' PACKET_ACTION 'DROP' PRIORITY '9996' SRC_IP '203.0.113.0/24'",
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("commands lack %q:\n%s", want, cmds)
		}
	}
	if strings.Contains(cmds, "V2_") || strings.Contains(cmds, "V69_") {
		t.Errorf("guards for VLANs the DX010 does not carry were written:\n%s", cmds)
	}
	logText := buf.String()
	if strings.Contains(logText, "UNSUPPORTED by this switch: DHCP snooping") {
		t.Errorf("snooping is honoured; the unsupported line must not be logged:\n%s", logText)
	}
	if !strings.Contains(logText, "MAC ACL rules (2)") {
		t.Errorf("MAC rules refused without a log:\n%s", logText)
	}
	// The next collect reads orchagent's verdict; a reconcile writes nothing.
	if _, err := coll.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "DUI_DHCP_GUARD 2 of 2 rules active, DUI_IP_ACL 3 of 3 rules active") {
		t.Errorf("verdict not logged:\n%s", buf.String())
	}
	n := len(fr.Recorded())
	l.reconcile(ctx)
	if got := fr.Recorded()[n:]; len(got) != 0 {
		t.Errorf("reconcile wrote %q", got)
	}
}

// The DX010's own push with the six test rules (2026-11-01, one of each
// shape the ECS round did not cover) through the loop into the driver:
// DUI_IP_ACL with RULE_1..6, no guard table (the site's snooping off), and
// a reconcile that writes nothing.
func TestReplaySonicDX010ACLPush(t *testing.T) {
	text := readECSPush(t, "system_cfg-dx010-acl.txt")
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238526-ff3c0c9f6", "collect-acl.txt"))
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
	desc := inform.Descriptor{MAC: snap.System.MAC}
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
	var acl []string
	for _, c := range fr.Recorded() {
		if strings.Contains(c, "acl") || strings.Contains(c, "ACL_") {
			acl = append(acl, c)
		}
	}
	if len(acl) != 7 || !strings.HasPrefix(acl[0], "sudo config acl add table DUI_IP_ACL L3 -p Ethernet0,") {
		t.Fatalf("want the table and six rules, got %q\nlog:\n%s", acl, buf.String())
	}
	for i, want := range []string{
		"'ACL_RULE|DUI_IP_ACL|RULE_1' DST_IP '192.0.2.0/24' IP_PROTOCOL '17' L4_DST_PORT '9' PACKET_ACTION 'DROP' PRIORITY '9998' SRC_IP '203.0.113.0/24'",
		"'ACL_RULE|DUI_IP_ACL|RULE_2' DST_IP '192.0.2.0/24' IP_PROTOCOL '6' L4_DST_PORT '9' PACKET_ACTION 'DROP' PRIORITY '9997' SRC_IP '203.0.113.0/24'",
		"'ACL_RULE|DUI_IP_ACL|RULE_3' DST_IP '192.0.2.0/24' IP_PROTOCOL '17' L4_DST_PORT '9' PACKET_ACTION 'DROP' PRIORITY '9996' SRC_IP '203.0.113.0/24'",
		"'ACL_RULE|DUI_IP_ACL|RULE_4' DST_IP '198.51.100.0/24' IP_TYPE 'IPV4ANY' PACKET_ACTION 'DROP' PRIORITY '9995'",
		"'ACL_RULE|DUI_IP_ACL|RULE_5' DST_IP '192.0.2.0/24' IP_PROTOCOL '6' L4_SRC_PORT '9' PACKET_ACTION 'DROP' PRIORITY '9994' SRC_IP '203.0.113.0/24'",
		"'ACL_RULE|DUI_IP_ACL|RULE_6' DST_IP '198.51.100.0/24' IP_TYPE 'IPV4ANY' PACKET_ACTION 'DROP' PRIORITY '9993' SRC_IP '192.0.2.0/24'",
	} {
		if !strings.HasSuffix(acl[i+1], want) {
			t.Errorf("rule %d:\n got  %s\n want ...%s", i+1, acl[i+1], want)
		}
	}
	if strings.Contains(buf.String(), "HELD") || strings.Contains(buf.String(), "DUI_DHCP_GUARD") {
		t.Errorf("nothing is held and no guard table is made with the site's snooping off:\n%s", buf.String())
	}
	if _, err := coll.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "DUI_IP_ACL 6 of 6 rules active") {
		t.Errorf("verdict not logged:\n%s", buf.String())
	}
	n := len(fr.Recorded())
	l.reconcile(ctx)
	if got := fr.Recorded()[n:]; len(got) != 0 {
		t.Errorf("reconcile wrote %q", got)
	}
}
