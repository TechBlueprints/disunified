package informloop

import (
	"context"
	"strings"
	"testing"
)

// The DX010's L3 captures of 2026-11-07 (docs/drivers/sonic.md §11.13),
// replayed against the port-feature capture with its dhcpd section empty
// (no DHCP server of the driver's): the UI routed a new network
// (dui-l3-test, VLAN 3999, DHCP server) on the switch, and the controller
// created "Inter-VLAN routing" (VLAN 4040) with the switch's default route
// through the gateway; then three switch routes (a next hop, an interface,
// a black hole); then DHCP relay to two servers with Isolate Network on;
// then DHCP none, isolation off.

func TestReplaySonicDX010L3(t *testing.T) {
	r, _ := newPortFeatRig(t)
	r.fr.UseSection("dhcpd", "")
	r.l.collect(context.Background())
	r.l.reconcile(context.Background())
	r.writes() // the first reconcile's LLDP-MED, TestReplaySonicDX010PortFeatures's

	got := r.push("system_cfg-dx010-l3-1-dhcp-server.txt", "c62e71b7cbac2668")
	if !containsInOrder(got,
		"sudo config vlan add 4040", // the port plan: VLAN 4040 is in the push's VLAN list
		brkSave,
		"sudo config interface ip add Vlan4040 198.18.253.2/24",
		"sudo config vlan add 3999", // the L3 plan: no port carries the routed network yet
		"sudo config interface ip add Vlan3999 198.18.3.1/24",
		brkSave,
	) {
		t.Errorf("the routed network wrote\n  %s", strings.Join(got, "\n  "))
	}
	for _, w := range got {
		if strings.Contains(w, "STATIC_ROUTE") {
			t.Errorf("the transit's default route was written with VLAN 4040 on no port with link: %s", w)
		}
	}
	if s := r.log.String(); !strings.Contains(s, "default route via 198.18.253.1: Vlan4040 reaches no port with link") {
		t.Errorf("the held default route was not logged:\n%s", s)
	}
	conf := r.fr.DHCPdConf["Vlan3999"]
	for _, line := range []string{"interface Vlan3999", "start 198.18.3.6", "end 198.18.3.254", "max_leases 249",
		"option subnet 255.255.255.0", "option router 198.18.3.1", "option dns 198.18.253.1", "option lease 86400"} {
		if !strings.Contains(conf, line+"\n") {
			t.Errorf("udhcpd config lacks %q:\n%s", line, conf)
		}
	}
	r.cycle("the routed network")

	got = r.push("system_cfg-dx010-l3-2-routes.txt", "b95c4ff6efea2eab")
	wantWrites(t, "the three switch routes", got, []string{
		"sudo sonic-db-cli CONFIG_DB hset 'STATIC_ROUTE|default|198.51.100.0/24' nexthop '198.18.3.10' ifname '' distance '0' blackhole 'false' nexthop-vrf ''",
		"sudo sonic-db-cli CONFIG_DB hset 'STATIC_ROUTE|default|203.0.113.0/25' nexthop '' ifname 'Vlan3999' distance '0' blackhole 'false' nexthop-vrf ''",
		"sudo sonic-db-cli CONFIG_DB hset 'STATIC_ROUTE|default|203.0.113.128/25' nexthop '' ifname '' distance '0' blackhole 'true' nexthop-vrf ''",
		brkSave,
	})
	r.cycle("the routes")

	got = r.push("system_cfg-dx010-l3-3-relay-isolated.txt", "80cdf51bad9b9fcc")
	joined := strings.Join(got, "\n")
	if !containsInOrder(got,
		"sudo config vlan dhcp_relay add 3999 192.0.2.2 198.18.253.1",
		brkSave,
		"sudo config acl add table DUI_IP_ACL L3 -p "+ports32()+" -s ingress",
	) || !strings.Contains(joined, `{"iface":"Vlan3999"}`) {
		t.Errorf("relay and isolation wrote\n  %s", strings.Join(got, "\n  "))
	}
	for _, rule := range []string{
		`'ACL_RULE|DUI_IP_ACL|ISO_1' DST_IP '198.18.3.1/32' IP_TYPE 'IPV4ANY' PACKET_ACTION 'FORWARD' PRIORITY '9870'`,
		`'ACL_RULE|DUI_IP_ACL|ISO_2' DST_IP '192.0.0.0/16' IP_TYPE 'IPV4ANY' PACKET_ACTION 'DROP' PRIORITY '9869' SRC_IP '198.18.3.0/24'`,
		`'ACL_RULE|DUI_IP_ACL|ISO_3' DST_IP '198.18.3.0/24' IP_TYPE 'IPV4ANY' PACKET_ACTION 'DROP' PRIORITY '9868' SRC_IP '192.0.0.0/16'`,
	} {
		if !strings.Contains(joined, rule) {
			t.Errorf("isolation rule %s missing:\n  %s", rule, strings.Join(got, "\n  "))
		}
	}
	if strings.Contains(joined, "198.18.253.0/24") {
		t.Errorf("the transit to the gateway was isolated:\n  %s", strings.Join(got, "\n  "))
	}
	r.cycle("relay and isolation")

	got = r.push("system_cfg-dx010-l3-4-none.txt", "dc8ba61700d24aaa")
	joined = strings.Join(got, "\n")
	if !containsInOrder(got,
		"sudo config vlan dhcp_relay del 3999 192.0.2.2 198.18.253.1",
		brkSave,
		"sudo config acl remove table DUI_IP_ACL",
	) || strings.Contains(joined, "disunified-dhcpd") {
		t.Errorf("DHCP none, isolation off wrote\n  %s", strings.Join(got, "\n  "))
	}
	r.cycle("DHCP none")

	// The network removed in the UI: the controller removed its transit
	// with it and pushed routing off. The routes go, then -- once FRR lists
	// nothing through them -- the two addresses (the CLI refuses the last
	// address of an interface a static route still uses; live, 2026-11-07).
	got = r.push("system_cfg-dx010-l3-5-off.txt", "08a61d94a987a7ca")
	wantWrites(t, "routing off", got, []string{
		"sudo sonic-db-cli CONFIG_DB del 'STATIC_ROUTE|default|198.51.100.0/24'",
		"sudo sonic-db-cli CONFIG_DB del 'STATIC_ROUTE|default|203.0.113.0/25'",
		"sudo sonic-db-cli CONFIG_DB del 'STATIC_ROUTE|default|203.0.113.128/25'",
		sonicRIFWait("Vlan3999"),
		sonicRIFWait("Vlan4040"),
		"sudo config interface ip remove Vlan3999 198.18.3.1/24 </dev/null",
		"sudo config interface ip remove Vlan4040 198.18.253.2/24 </dev/null",
		brkSave,
		// The VLAN the network left behind (the driver never deletes a
		// VLAN) takes the ports' MTU, as every VLAN on the switch does
		// (vlanMTUs; the same write followed live).
		"sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan3999' mtu 1500",
		brkSave,
	})
	r.cycle("routing off")

	if s := r.log.String(); strings.Contains(s, "UNSUPPORTED") || strings.Contains(s, "not translated") {
		t.Errorf("a routing key was left unhandled:\n%s", s)
	}
}

func sonicRIFWait(iface string) string {
	return `sh -c 'for i in $(seq 1 60); do vtysh -c "show ip route vrf all static" </dev/null | grep -qw ` + iface + ` || exit 0; sleep 0.5; done; exit 0' disunified-rif-wait`
}
