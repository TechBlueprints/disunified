package unificfg

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func readL3(t *testing.T, name string) *Config {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", name))
	if err != nil {
		t.Fatal(err)
	}
	return Parse(string(b))
}

// The DX010's L3 pushes of 2026-11-07: a network routed on the switch with
// its DHCP server, the transit the controller made with it, three routes,
// then relay with Isolate Network, then no DHCP.
func TestParseRouting(t *testing.T) {
	c := readL3(t, "system_cfg-dx010-portfeat-0-baseline.txt")
	if c.Routing == nil || c.Routing.Enabled || len(c.Routing.Interfaces) != 0 {
		t.Errorf("a push routing nothing parses as %+v", c.Routing)
	}

	c = readL3(t, "system_cfg-dx010-l3-2-routes.txt")
	r := c.Routing
	if r == nil || !r.Enabled || !r.DHCPServer || r.DHCPRelay || len(r.Interfaces) != 2 {
		t.Fatalf("routing = %+v", r)
	}
	transit := RoutingInterface{ID: 1, Name: "Inter-VLAN routing", VLAN: 4040, OpMode: "manual", IP: "198.18.253.2", Mask: "255.255.255.0", DefaultRoute: "198.18.253.1",
		Routes: []Route{{ID: 1, Name: "Inter-VLAN routing", Prefix: "0.0.0.0", Mask: "0.0.0.0", Gateway: "198.18.253.1"}}}
	if !reflect.DeepEqual(r.Interfaces[0], transit) {
		t.Errorf("transit = %+v\nwant      %+v", r.Interfaces[0], transit)
	}
	net := RoutingInterface{ID: 2, Name: "dui-l3-test", VLAN: 3999, Type: "vlan", OpMode: "dhcp_server", IP: "198.18.3.1", Mask: "255.255.255.0", DefaultRoute: "198.18.253.1", Purpose: "corporate",
		DHCPServer: RoutingDHCPServer{Start: "198.18.3.6", End: "198.18.3.254", Gateway: "198.18.3.1", LeaseTime: 86400, DNS: []string{"198.18.253.1"}},
		Routes: []Route{
			{ID: 1, Name: "dui-l3-blackhole", Prefix: "203.0.113.128", Mask: "255.255.255.128", Gateway: "Null0"},
			{ID: 2, Name: "dui-l3-route", Prefix: "198.51.100.0", Mask: "255.255.255.0", Gateway: "198.18.3.10"},
			{ID: 3, Name: "dui-l3-ifroute", Prefix: "203.0.113.0", Mask: "255.255.255.128", Interface: 2},
		}}
	if !reflect.DeepEqual(r.Interfaces[1], net) {
		t.Errorf("network = %+v\nwant      %+v", r.Interfaces[1], net)
	}

	c = readL3(t, "system_cfg-dx010-l3-3-relay-isolated.txt")
	ri := c.Routing.Interfaces[1]
	if ri.OpMode != "dhcp_relay" || ri.Purpose != "guest" || !reflect.DeepEqual(ri.Relays, []string{"192.0.2.2", "198.18.253.1"}) || !c.Routing.DHCPRelay || c.Routing.DHCPServer {
		t.Errorf("relay, isolated = %+v (relay %v server %v)", ri, c.Routing.DHCPRelay, c.Routing.DHCPServer)
	}

	c = readL3(t, "system_cfg-dx010-l3-4-none.txt")
	ri = c.Routing.Interfaces[1]
	if ri.OpMode != "manual" || ri.Purpose != "corporate" || len(ri.Relays) != 0 || ri.DHCPServer.Start != "" {
		t.Errorf("no DHCP = %+v", ri)
	}
	for _, f := range []string{"system_cfg-dx010-l3-1-dhcp-server.txt", "system_cfg-dx010-l3-2-routes.txt", "system_cfg-dx010-l3-3-relay-isolated.txt", "system_cfg-dx010-l3-4-none.txt"} {
		for _, ri := range readL3(t, f).Routing.Interfaces {
			if len(ri.Unmodelled) > 0 {
				t.Errorf("%s: %s left %v", f, ri.Name, ri.Unmodelled)
			}
			for _, rt := range ri.Routes {
				if len(rt.Unmodelled) > 0 {
					t.Errorf("%s: route %s left %v", f, rt.Name, rt.Unmodelled)
				}
			}
		}
	}
}

// The DX010's routed-port and fixed-IP pushes of 2026-11-08: the port
// editor's Routing on port 31, Routing + Aggregating on ports 30-31, and a
// routed network's clients with a Fixed IP (one, then two with the first
// moved).
func TestParseRoutedPortsAndFixedIPs(t *testing.T) {
	c := readL3(t, "system_cfg-dx010-routed-1-port.txt")
	ri := c.Routing.Interfaces[1]
	want := RoutingInterface{ID: 2, Name: "dui-routed-31", Type: "port", Port: 31, OpMode: "manual", IP: "198.18.4.1", Mask: "255.255.255.252", DefaultRoute: "198.18.253.1", Purpose: "corporate"}
	if !reflect.DeepEqual(ri, want) {
		t.Errorf("routed port = %+v\nwant          %+v", ri, want)
	}
	// The controller still pushes the routed port as a switching port in
	// its native VLAN.
	if p := c.Ports[31]; p.OpMode != "switch" || !p.VLANExplicit || p.NativeVLAN != 1 {
		t.Errorf("port 31 = %+v", p)
	}

	c = readL3(t, "system_cfg-dx010-routed-3-aggregate.txt")
	ri = c.Routing.Interfaces[1]
	want = RoutingInterface{ID: 2, Name: "dui-routed-lag-30", Type: "lag", LAG: 30, OpMode: "manual", IP: "198.18.4.5", Mask: "255.255.255.252", DefaultRoute: "198.18.253.1", Purpose: "corporate"}
	if !reflect.DeepEqual(ri, want) {
		t.Errorf("routed aggregate = %+v\nwant               %+v", ri, want)
	}
	// Only the first member is pushed, with no VLAN; the second member
	// the operator aggregated (31) is in no key at all.
	if p := c.Ports[30]; p.OpMode != "aggregate" || p.LAG != 1 || p.VLANExplicit {
		t.Errorf("port 30 = %+v", p)
	}
	if _, ok := c.Ports[31]; ok {
		t.Errorf("port 31 is in the routed aggregate's push: %+v", c.Ports[31])
	}

	if c := readL3(t, "system_cfg-dx010-routed-2-removed.txt"); c.Routing == nil || c.Routing.Enabled {
		t.Errorf("the routed port removed = %+v", c.Routing)
	}

	c = readL3(t, "system_cfg-dx010-fixedip-1-network.txt")
	if s := c.Routing.Interfaces[1].DHCPServer; s.Start != "198.18.3.100" || s.End != "198.18.3.199" || len(s.Static) != 0 {
		t.Errorf("the DHCP network = %+v", s)
	}
	c = readL3(t, "system_cfg-dx010-fixedip-2-one.txt")
	if got, w := c.Routing.Interfaces[1].DHCPServer.Static, []StaticLease{{ID: 1, MAC: "02:00:00:00:00:01", IP: "198.18.3.50"}}; !reflect.DeepEqual(got, w) {
		t.Errorf("one fixed IP = %+v, want %+v", got, w)
	}
	c = readL3(t, "system_cfg-dx010-fixedip-3-two.txt")
	if got, w := c.Routing.Interfaces[1].DHCPServer.Static, []StaticLease{{ID: 1, MAC: "02:00:00:00:00:01", IP: "198.18.3.51"}, {ID: 2, MAC: "02:00:00:00:00:02", IP: "198.18.3.120"}}; !reflect.DeepEqual(got, w) {
		t.Errorf("two fixed IPs = %+v, want %+v", got, w)
	}
	for _, f := range []string{"system_cfg-dx010-routed-1-port.txt", "system_cfg-dx010-routed-3-aggregate.txt", "system_cfg-dx010-fixedip-2-one.txt", "system_cfg-dx010-fixedip-3-two.txt"} {
		for _, ri := range readL3(t, f).Routing.Interfaces {
			if len(ri.Unmodelled) > 0 {
				t.Errorf("%s: %s left %v", f, ri.Name, ri.Unmodelled)
			}
		}
	}
}
