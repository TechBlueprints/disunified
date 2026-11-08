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
