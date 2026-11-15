package informloop

import (
	"strings"
	"testing"
)

// The DX010's routed-port and fixed-IP captures of 2026-11-08
// (docs/drivers/sonic.md §11.16), replayed against the switch as captured
// after the L3 run (collect-l3.txt): the port editor's Routing on port 31
// (Ethernet120), the routed network removed, Routing + Aggregating on port
// 30 (Ethernet116; the controller pushes only the aggregate's first
// member), removed; then a routed network with its DHCP server and one,
// then two clients with a Fixed IP. Every push is followed by a cycle that
// must write nothing.
func TestReplaySonicDX010RoutedPortsAndFixedIPs(t *testing.T) {
	r, _ := newDX010Rig(t, "collect-l3.txt")
	r.settle("the capture under its own push")

	got := r.push("system_cfg-dx010-routed-1-port.txt", "acdbe0037c34b77b")
	if !containsInOrder(got,
		"sudo config interface ip add Vlan4040 198.18.253.2/24", // the transit, in the device-wide apply
		brkSave,
		"sudo config vlan member del 4094 Ethernet120", // the port apply: the routed port leaves its VLAN...
		brkSave,
		"sudo config interface ip add Ethernet120 198.18.4.1/30", // ...and the L3 apply after it gives it the address
		brkSave,
	) {
		t.Errorf("the routed port wrote\n  %s", strings.Join(got, "\n  "))
	}
	if s := r.log.String(); !strings.Contains(s, `network "dui-routed-31" (port 31): Ethernet120 is still a VLAN member (the port apply takes it out); its address waits`) {
		t.Errorf("the address waiting for the port apply was not logged:\n%s", s)
	}
	r.cycle("the routed port")

	// The network removed: the address goes in the device-wide apply,
	// before the port apply puts the port back in its VLAN (SONiC refuses
	// a VLAN member that is a router interface).
	got = r.push("system_cfg-dx010-routed-2-removed.txt", "e5dddab73b7cd6da")
	if !containsInOrder(got,
		sonicRIFWait("Ethernet120"),
		"sudo config interface ip remove Ethernet120 198.18.4.1/30 </dev/null",
		brkSave,
		"sudo config vlan member add -u 4094 Ethernet120",
		brkSave,
	) {
		t.Errorf("the routed port removed wrote\n  %s", strings.Join(got, "\n  "))
	}
	r.cycle("the routed port removed")

	got = r.push("system_cfg-dx010-routed-3-aggregate.txt", "aa2920c8bb3e517e")
	if !containsInOrder(got,
		"sudo config portchannel add PortChannel1 --fast-rate true --min-links 1",
		"sudo config vlan member del 4094 Ethernet116",
		"sudo config portchannel member add PortChannel1 Ethernet116",
		brkSave,
		"sudo config interface ip add PortChannel1 198.18.4.5/30",
		brkSave,
	) {
		t.Errorf("the routed aggregate wrote\n  %s", strings.Join(got, "\n  "))
	}
	for _, w := range got {
		if strings.HasPrefix(w, "sudo config vlan member add") && strings.Contains(w, "PortChannel1") {
			t.Errorf("the routed aggregate was put in a VLAN: %s", w)
		}
	}
	r.cycle("the routed aggregate")

	got = r.push("system_cfg-dx010-routed-2-removed.txt", "3e6af602ad11dfa4")
	if !containsInOrder(got,
		sonicRIFWait("PortChannel1"),
		"sudo config interface ip remove PortChannel1 198.18.4.5/30 </dev/null",
		brkSave,
		"sudo config portchannel member del PortChannel1 Ethernet116",
		"sudo config vlan member add -u 4094 Ethernet116",
		"sudo config portchannel del PortChannel1",
		brkSave,
	) {
		t.Errorf("the routed aggregate removed wrote\n  %s", strings.Join(got, "\n  "))
	}
	r.cycle("the routed aggregate removed")

	r.push("system_cfg-dx010-fixedip-1-network.txt", "13f1e2863c5360fb")
	if conf := r.fr.DHCPdConf["Vlan3999"]; !strings.Contains(conf, "start 198.18.3.100\nend 198.18.3.199\n") || strings.Contains(conf, "static_lease") {
		t.Errorf("the DHCP network's udhcpd config:\n%s", conf)
	}
	r.cycle("the DHCP network")

	got = r.push("system_cfg-dx010-fixedip-2-one.txt", "a1e3b25ffd501c05")
	if len(got) != 1 || !strings.Contains(got[0], "disunified-dhcpd") {
		t.Errorf("one fixed IP wrote %q, want the DHCP server restarted alone", got)
	}
	if conf := r.fr.DHCPdConf["Vlan3999"]; !strings.HasSuffix(conf, "option lease 86400\nstatic_lease 02:00:00:00:00:01 198.18.3.50\n") {
		t.Errorf("one fixed IP's udhcpd config:\n%s", conf)
	}
	r.cycle("one fixed IP")

	// The first client moved, a second inside the range (udhcpd never
	// leases a static address to another client).
	r.push("system_cfg-dx010-fixedip-3-two.txt", "db3c86ad06ce186a")
	if conf := r.fr.DHCPdConf["Vlan3999"]; !strings.HasSuffix(conf, "static_lease 02:00:00:00:00:01 198.18.3.51\nstatic_lease 02:00:00:00:00:02 198.18.3.120\n") {
		t.Errorf("two fixed IPs' udhcpd config:\n%s", conf)
	}
	r.cycle("two fixed IPs")

	if s := r.log.String(); strings.Contains(s, "UNSUPPORTED") || strings.Contains(s, "not translated") {
		t.Errorf("a routing key was left unhandled:\n%s", s)
	}
}
