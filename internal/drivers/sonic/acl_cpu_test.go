package sonic

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The kernel mirror of the driver's L3 tables (acl_cpu.go,
// docs/drivers/sonic.md §11.32), driven by the controller's captured pushes
// to the ECS (DHCP guarding on, the IP rules) against the DX010 collected
// with the ACL sections, the marker section added as collect.py prints it
// when no mirror was written yet.

func newCPURig(t *testing.T) *controlRig {
	t.Helper()
	return newACLRig(t, func(fr *FixtureRunner) { fr.UseSection("acl_cpu", "") })
}

// cpuApply applies d and returns the mirror scripts it sent.
func (r *controlRig) cpuApply(d devicemodel.DeviceDesired) []string {
	r.t.Helper()
	n := len(r.fr.CPUScripts)
	r.deviceCmds(d)
	return append([]string(nil), r.fr.CPUScripts[n:]...)
}

func (r *controlRig) collect() {
	r.t.Helper()
	if _, err := r.c.Collect(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func TestCPUMirrorFollowsTheTables(t *testing.T) {
	r := newCPURig(t)
	if _, err := r.c.EnsureVLANs(context.Background(), []int{1, 2, 69}); err != nil {
		t.Fatal(err)
	}
	r.collect()
	on := true
	d := devicemodel.DeviceDesired{
		DHCPSnooping: &on, DHCPGuards: guardsOf(ecsPush(t, "system_cfg-ecs-dhcp-snoop.txt")),
		IPACLSet: true, IPACLs: ipACLsOf(ecsPush(t, "system_cfg-ecs-acl.txt")),
	}
	if got := r.cpuApply(d); len(got) != 1 {
		t.Fatalf("mirror scripts %d, want one", len(got))
	}
	// The guard's counted per-port DROPs are left out (the VLAN's own DROP
	// below covers them); FORWARD is RETURN, so a guard FORWARD still meets
	// the IP rules, and a DROP in either chain drops.
	want := []string{
		"*raw",
		":DUI_ACL_CPU - [0:0]",
		":DUI_DHCP_GUARD - [0:0]",
		":DUI_IP_ACL - [0:0]",
		"-A DUI_ACL_CPU -j DUI_DHCP_GUARD",
	}
	// The guard's rules as the chip has them, by priority then name.
	r.c.mu.Lock()
	st := r.c.state
	r.c.mu.Unlock()
	for _, n := range byPriority(st.rulesOf(guardTable)) {
		f := st.rulesOf(guardTable)[n]
		if isCountedRule(f) {
			continue
		}
		line := "-A DUI_DHCP_GUARD -i Vlan" + f["VLAN_ID"]
		if f["SRC_IP"] != "" {
			line += " -s " + f["SRC_IP"]
		}
		line += " -p udp -m udp --sport 67 --dport 68 -j "
		if f["PACKET_ACTION"] == "FORWARD" {
			line += "RETURN"
		} else {
			line += "DROP"
		}
		want = append(want, line)
	}
	want = append(want,
		"-A DUI_ACL_CPU -j DUI_IP_ACL",
		"-A DUI_IP_ACL -s 192.0.2.0/24 -d 198.51.100.0/24 -j RETURN",
		"-A DUI_IP_ACL -s 203.0.113.0/24 -d 192.0.2.0/24 -p tcp -m tcp --dport 9 -j DROP",
		"-A DUI_IP_ACL -s 203.0.113.0/24 -d 192.0.2.0/24 -p tcp -m tcp --dport 1001 -j DROP",
		"COMMIT",
	)
	if !reflect.DeepEqual(r.fr.CPURules, want) {
		t.Errorf("mirror:\n got  %q\n want %q", r.fr.CPURules, want)
	}
	// 3 VLANs: one FORWARD per trusted server (6) and one DROP each.
	if n := strings.Count(strings.Join(r.fr.CPURules, "\n"), "-A DUI_DHCP_GUARD "); n != 9 {
		t.Errorf("%d guard lines, want 9", n)
	}
	if strings.Contains(strings.Join(r.fr.CPURules, "\n"), "IN_PORTS") {
		t.Error("a counted rule reached the mirror")
	}

	// The marker reads back: nothing written again.
	r.collect()
	if got := r.cpuApply(d); len(got) != 0 {
		t.Errorf("a second apply sent %d mirror scripts", len(got))
	}

	// A reboot clears the rules and the marker together: written again.
	r.fr.UseSection("acl_cpu", "")
	r.collect()
	if got := r.cpuApply(d); len(got) != 1 {
		t.Errorf("after the marker went: %d mirror scripts, want one", len(got))
	}
	r.collect()

	// The first IP rule deleted in the UI: the mirror follows.
	d.IPACLs = d.IPACLs[1:]
	if got := r.cpuApply(d); len(got) != 1 {
		t.Fatalf("after a rule went: %d mirror scripts, want one", len(got))
	}
	if strings.Contains(strings.Join(r.fr.CPURules, "\n"), "-j RETURN\n-A DUI_IP_ACL") || strings.Contains(strings.Join(r.fr.CPURules, "\n"), "198.51.100.0/24") {
		t.Errorf("the deleted Allow is still mirrored: %q", r.fr.CPURules)
	}
	r.collect()

	// Everything off: the hooks, the chains and the marker go.
	off := false
	got := r.cpuApply(devicemodel.DeviceDesired{DHCPSnooping: &off, IPACLSet: true})
	if len(got) != 1 || r.fr.CPURules != nil {
		t.Fatalf("removal: %d scripts, rules %q", len(got), r.fr.CPURules)
	}
	for _, w := range []string{"iptables -t raw -D PREROUTING -i \"$i\" -j DUI_ACL_CPU", "iptables -t raw -X \"$c\"", "rm -f " + cpuMark} {
		if !strings.Contains(got[0], w) {
			t.Errorf("removal script lacks %q:\n%s", w, got[0])
		}
	}
	r.collect()
	if got := r.cpuApply(devicemodel.DeviceDesired{DHCPSnooping: &off, IPACLSet: true}); len(got) != 0 {
		t.Errorf("a second removal sent %d scripts", len(got))
	}
}

// The mirror runs on every apply, not only when the chip changed: a
// switch whose tables were written before the mirror existed (the marker
// empty, the tables in CONFIG_DB) gets it on the next reconcile, with no
// table write.
func TestCPUMirrorCatchesUpWithExistingTables(t *testing.T) {
	r := newCPURig(t)
	d := devicemodel.DeviceDesired{IPACLSet: true, IPACLs: ipACLsOf(ecsPush(t, "system_cfg-ecs-acl.txt"))}
	r.cpuApply(d)
	r.collect()
	r.fr.UseSection("acl_cpu", "")
	r.collect()
	n := len(r.fr.Commands)
	if got := r.cpuApply(d); len(got) != 1 {
		t.Fatalf("%d mirror scripts, want one", len(got))
	}
	if got := r.fr.Commands[n:]; len(got) != 1 || got[0] != aclCPUCommand {
		t.Errorf("commands %q, want the mirror alone", got)
	}
}

// Without the marker section (a collect.py before the mirror) nothing is
// mirrored, and the chip writes are unchanged.
func TestCPUMirrorNeedsTheMarkerSection(t *testing.T) {
	r := newACLRig(t, nil)
	d := devicemodel.DeviceDesired{IPACLSet: true, IPACLs: ipACLsOf(ecsPush(t, "system_cfg-ecs-acl.txt"))}
	if got := r.cpuApply(d); len(got) != 0 {
		t.Errorf("mirror written without the marker section: %q", got)
	}
}

func TestCPURuleShapes(t *testing.T) {
	cases := []struct {
		f    map[string]string
		args string
		skip bool
		why  string
	}{
		{map[string]string{"PRIORITY": "9993", "PACKET_ACTION": "DROP", "IP_TYPE": "IPV4ANY", "SRC_IP": "192.0.2.0/24", "DST_IP": "198.51.100.0/24"},
			"-s 192.0.2.0/24 -d 198.51.100.0/24 -j DROP", false, ""},
		{map[string]string{"PRIORITY": "9994", "PACKET_ACTION": "DROP", "IP_PROTOCOL": "6", "SRC_IP": "203.0.113.0/24", "L4_SRC_PORT": "9"},
			"-s 203.0.113.0/24 -p tcp -m tcp --sport 9 -j DROP", false, ""},
		{map[string]string{"PRIORITY": "1", "PACKET_ACTION": "FORWARD", "IP_PROTOCOL": "17", "L4_DST_PORT_RANGE": "1000-2000"},
			"-p udp -m udp --dport 1000:2000 -j RETURN", false, ""},
		{map[string]string{"PRIORITY": "8500", "PACKET_ACTION": "DROP", "IN_PORTS": "Ethernet4", "VLAN_ID": "4094", "IP_PROTOCOL": "17", "L4_SRC_PORT": "67", "L4_DST_PORT": "68"},
			"-i Vlan4094 -p udp -m udp --sport 67 --dport 68 -j DROP", true, ""},
		{map[string]string{"PRIORITY": "1", "PACKET_ACTION": "FORWARD", "IN_PORTS": "Ethernet4"}, "", false, "IN_PORTS on a rule that does not drop"},
		{map[string]string{"PRIORITY": "1", "PACKET_ACTION": "DROP", "TCP_FLAGS": "0x02/0x02"}, "", false, "field TCP_FLAGS"},
		{map[string]string{"PRIORITY": "1", "PACKET_ACTION": "REDIRECT:Ethernet4"}, "", false, "PACKET_ACTION REDIRECT:Ethernet4"},
		{map[string]string{"PRIORITY": "1", "PACKET_ACTION": "DROP", "L4_DST_PORT": "9"}, "", false, "L4_DST_PORT without TCP or UDP"},
	}
	for _, c := range cases {
		args, skip, why := cpuRule(c.f)
		if args != c.args || skip != c.skip || why != c.why {
			t.Errorf("%v:\n got  %q %v %q\n want %q %v %q", c.f, args, skip, why, c.args, c.skip, c.why)
		}
	}
	// A table with a rule the mirror cannot express is left out whole, with
	// a note; the other table is still mirrored.
	st := &swState{aclRules: map[string]map[string]string{
		ipACLTable + "|RULE_1":  {"PRIORITY": "9998", "PACKET_ACTION": "FORWARD", "TCP_FLAGS": "0x02/0x02"},
		ipACLTable + "|RULE_2":  {"PRIORITY": "9997", "PACKET_ACTION": "DROP", "IP_TYPE": "IPV4ANY"},
		guardTable + "|V1_DROP": {"PRIORITY": "8000", "PACKET_ACTION": "DROP", "VLAN_ID": "4094", "IP_PROTOCOL": "17", "L4_SRC_PORT": "67", "L4_DST_PORT": "68"},
	}}
	lines, notes := cpuMirror(st)
	if len(notes) != 1 || !strings.Contains(notes[0], "DUI_IP_ACL is not mirrored") {
		t.Errorf("notes %q", notes)
	}
	if strings.Contains(strings.Join(lines, "\n"), "-A DUI_IP_ACL") || !strings.Contains(strings.Join(lines, "\n"), "-A DUI_DHCP_GUARD -i Vlan4094") {
		t.Errorf("lines %q", lines)
	}
	if l, _ := cpuMirror(&swState{}); l != nil || cpuHash(l) != "" {
		t.Errorf("no tables: %q", l)
	}
}
