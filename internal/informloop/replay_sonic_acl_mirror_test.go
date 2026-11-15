package informloop

import (
	"reflect"
	"strings"
	"testing"
)

// The DX010's kernel ACL mirror through the driver (2026-11-15, Network
// 11.0.99, docs/drivers/sonic.md §11.32.5): an IP Block rule saved in the
// UI and scoped to the DX010 alone -- UDP from a documentation address to
// the switch's own address (192.0.2.1 after the scrub), port 9999 --,
// then edited to ports 9998 and 9999 (two groups), then removed. The
// capture is the DX010 collected just before the run (no ACL table, an
// empty acl_cpu marker; collected over SSH from a LAN host, so its ssh
// section names that host). The pushes are the bridge's reply log; the
// one before the run and the removal's are the same but for the
// cfgversion. Live, each push wrote the chip table and then the mirror
// (the journal's lines are the commands and restore inputs below), every
// later cycle wrote nothing, a deleted marker was written again on the
// next cycle with the same rules, and the removal took the dispatcher
// with it.
func TestReplaySonicDX010ACLMirror(t *testing.T) {
	r, _ := newNet11RigFrom(t, "collect-acl-mirror.txt", "system_cfg-dx010-acl-mirror-3-removed.txt", "e180da302351583c")
	r.l.collect(t.Context())
	r.l.reconcile(t.Context())
	r.writes() // the capture's own drift under the push before is not this test's
	r.cycle("the push before")

	rule := func(name, port, prio string) string {
		return "sudo sonic-db-cli CONFIG_DB hset 'ACL_RULE|DUI_IP_ACL|" + name + "' DST_IP '192.0.2.1/32' IP_PROTOCOL '17' L4_DST_PORT '" + port + "' PACKET_ACTION 'DROP' PRIORITY '" + prio + "' SRC_IP '198.51.100.7/32'"
	}
	drop := func(port string) string {
		return "-A DUI_IP_ACL -s 198.51.100.7/32 -d 192.0.2.1/32 -p udp -m udp --dport " + port + " -j DROP"
	}
	mirror := func(rules ...string) []string {
		out := []string{"*raw", ":DUI_ACL_CPU - [0:0]", ":DUI_DHCP_GUARD - [0:0]", ":DUI_IP_ACL - [0:0]", "-A DUI_ACL_CPU -j DUI_IP_ACL"}
		return append(append(out, rules...), "COMMIT")
	}
	const cpu = "sudo bash -s -- disunified-acl-cpu"
	tableAdd := func(got []string) string {
		for _, g := range got {
			if strings.HasPrefix(g, "sudo config acl add table DUI_IP_ACL L3 -p ") {
				return g
			}
		}
		return ""
	}

	// The rule: the table bound and its one rule, saved, then the mirror.
	got := r.push11("system_cfg-dx010-acl-mirror-1-add.txt", "fcdff3206db281b3")
	add := tableAdd(got)
	if add == "" || !strings.HasSuffix(add, ",PortChannel1 -s ingress") || strings.Contains(add, "Ethernet0,") || strings.Contains(add, "Ethernet4,") {
		t.Errorf("the table is not bound to the uplink's PortChannel and the free ports, without its members: %q", add)
	}
	if !containsInOrder(got, add, rule("RULE_1", "9999", "9998"), brkSave, cpu) {
		t.Errorf("the rule wrote\n  %s", strings.Join(got, "\n  "))
	}
	if want := mirror(drop("9999")); !reflect.DeepEqual(r.fr.CPURules, want) {
		t.Errorf("the mirror is\n  %s\nwant\n  %s", strings.Join(r.fr.CPURules, "\n  "), strings.Join(want, "\n  "))
	}
	r.cycle("the rule")

	// Edited to two ports: two groups, RULE_1 rewritten and RULE_2 added
	// below it; the mirror's DROPs in the same order.
	got = r.push11("system_cfg-dx010-acl-mirror-2-edit.txt", "9908cf107acc2cb7")
	if !containsInOrder(got,
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_IP_ACL|RULE_1'",
		rule("RULE_1", "9998", "9998"),
		rule("RULE_2", "9999", "9997"),
		brkSave, cpu,
	) || tableAdd(got) != "" {
		t.Errorf("the edit wrote\n  %s", strings.Join(got, "\n  "))
	}
	edited := mirror(drop("9998"), drop("9999"))
	if !reflect.DeepEqual(r.fr.CPURules, edited) {
		t.Errorf("the edited mirror is\n  %s", strings.Join(r.fr.CPURules, "\n  "))
	}
	r.cycle("the edit")

	// The marker lost (live: deleted by hand, as a reboot clears /run with
	// the rules): the next cycle writes the same mirror again, and only it.
	r.fr.UseSection("acl_cpu", "")
	r.l.collect(t.Context())
	r.l.reconcile(t.Context())
	if got := r.writes(); !reflect.DeepEqual(got, []string{cpu}) || !reflect.DeepEqual(r.fr.CPURules, edited) {
		t.Errorf("a lost marker wrote %q, mirror\n  %s", got, strings.Join(r.fr.CPURules, "\n  "))
	}
	r.cycle("the mirror written again")

	// Removed: the rules lowest first, the table, the save, then the
	// mirror's removal (hooks, chains and the dispatcher, the marker).
	got = r.push11("system_cfg-dx010-acl-mirror-3-removed.txt", "e180da302351583c")
	if !containsInOrder(got,
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_IP_ACL|RULE_2'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_IP_ACL|RULE_1'",
		"sudo config acl remove table DUI_IP_ACL",
		brkSave, cpu,
	) {
		t.Errorf("the removal wrote\n  %s", strings.Join(got, "\n  "))
	}
	if r.fr.CPURules != nil {
		t.Errorf("the mirror outlived its table:\n  %s", strings.Join(r.fr.CPURules, "\n  "))
	}
	r.cycle("the removal")

	if s := r.log.String(); !strings.Contains(s, "sonic apply acl cpu mirror (iptables raw DUI_ACL_CPU, removed)") {
		t.Errorf("the removal was not logged:\n%s", s)
	}
}
