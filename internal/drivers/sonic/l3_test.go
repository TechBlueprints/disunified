package sonic

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

const fixtureL3 = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-l3.txt"

// transitDesired is the controller's Inter-VLAN routing network as the
// 2026-11-07 capture pushes it (system_cfg-dx010-l3-1-dhcp-server.txt):
// VLAN 4040, the switch .2, the default route via the gateway's .1.
func transitDesired() devicemodel.DeviceDesired {
	return devicemodel.DeviceDesired{
		L3Set:        true,
		L3Interfaces: []devicemodel.L3Interface{{Name: "Inter-VLAN routing", VLAN: 4040, Address: "198.18.253.2/24"}},
		StaticRoutes: []devicemodel.StaticRoute{{Name: "Inter-VLAN routing", Prefix: "0.0.0.0/0", NextHop: "198.18.253.1"}},
	}
}

// Once the transit reaches a port with link, the default route goes
// through it, and the management gateway the IP Settings own stays in
// the row behind it at distance 200: the address apply still finds its
// gateway and changes nothing, and the routing taken away hands the
// default back.
func TestTransitCarriesTheDefaultRoute(t *testing.T) {
	r := rig20261106(t, fixtureL3)
	ctx := context.Background()
	mgmt := r.c.state.defaultRoute()
	if mgmt == "" {
		t.Fatal("the capture has no static default route")
	}
	// The uplink (Ethernet0, with link in the capture) carries VLAN 4040.
	if err := r.c.runBatch(ctx, []string{"sudo config vlan add 4040", "sudo config vlan member add 4040 Ethernet0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	r.fr.Commands = nil
	if _, err := r.c.ApplyDevice(ctx, transitDesired()); err != nil {
		t.Fatal(err)
	}
	want := "sudo sonic-db-cli CONFIG_DB hset 'STATIC_ROUTE|default|0.0.0.0/0' nexthop '" + mgmt + ",198.18.253.1' ifname ',' distance '200,0' blackhole 'false,false' nexthop-vrf ','"
	if got := r.fr.Commands; !containsInOrder(got, "sudo config interface ip add Vlan4040 198.18.253.2/24", want, save) {
		t.Fatalf("the transit wrote\n  %s", strings.Join(got, "\n  "))
	}
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.c.state.defaultRoute(); got != mgmt {
		t.Errorf("the management gateway reads %q beside the transit, want %q", got, mgmt)
	}
	r.fr.Commands = nil
	if n, err := r.c.ApplyDevice(ctx, transitDesired()); n != 0 || err != nil || len(r.fr.Commands) != 0 {
		t.Errorf("the second apply wrote %q (%d, %v)", r.fr.Commands, n, err)
	}
	addr := ifaceAddr(t, r.fr.Fixture, "Vlan4094")
	if moved, err := r.c.ApplyAddress(ctx, devicemodel.AddressDesired{IP: addr, PrefixLen: 16, Gateway: mgmt}); moved || err != nil {
		t.Errorf("the IP Settings apply beside the transit: moved %v, %v; wrote %q", moved, err, r.fr.Commands)
	}

	// Routing off: the transit hop goes and the management gateway is the
	// default again at its own distance, then -- once FRR lists no route
	// through it -- the driver's address goes.
	r.fr.Commands = nil
	if _, err := r.c.ApplyDevice(ctx, devicemodel.DeviceDesired{L3Set: true}); err != nil {
		t.Fatal(err)
	}
	want = "sudo sonic-db-cli CONFIG_DB hset 'STATIC_ROUTE|default|0.0.0.0/0' nexthop '" + mgmt + "' ifname '' distance '0' blackhole 'false' nexthop-vrf ''"
	if got := r.fr.Commands; !containsInOrder(got, want, rifWait("Vlan4040"), "sudo config interface ip remove Vlan4040 198.18.253.2/24 </dev/null", save) {
		t.Errorf("routing off wrote\n  %s", strings.Join(got, "\n  "))
	}
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	r.fr.Commands = nil
	if _, err := r.c.ApplyDevice(ctx, devicemodel.DeviceDesired{L3Set: true}); err != nil || len(r.fr.Commands) != 0 {
		t.Errorf("routing off again wrote %q (%v)", r.fr.Commands, err)
	}
}

// The switch's own address is never the driver's: a routed network on
// the VLAN that carries it is refused with a note, and nothing is written
// there.
func TestRoutedNetworkLeavesTheManagementAddress(t *testing.T) {
	r := rig20261106(t, fixtureL3)
	ctx := context.Background()
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	d := devicemodel.DeviceDesired{L3Set: true, L3Interfaces: []devicemodel.L3Interface{{Name: "Default", VLAN: 1, Address: "192.0.2.254/16"}}}
	r.fr.Commands = nil
	if _, err := r.c.ApplyDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.fr.Commands {
		if strings.Contains(c, "Vlan4094") {
			t.Errorf("wrote on the management VLAN: %s", c)
		}
	}
	if !strings.Contains(r.log.String(), "which the driver did not add (the switch's own address)") {
		t.Errorf("no note:\n%s", r.log.String())
	}
}

// swallowed is a runner whose batch shell met end of input early: a
// command in the batch read the rest of it from stdin, and bash exited 0.
type swallowed struct{}

func (swallowed) Run(context.Context, string, string) (string, error) { return "", nil }
func (swallowed) Close() error                                        { return nil }

// A batch that did not reach its last line is an error, never a success
// whose remaining writes the plan then believes (live, 2026-11-07).
func TestBatchThatStoppedEarlyIsAnError(t *testing.T) {
	c := NewCollector(swallowed{})
	err := c.runBatch(context.Background(), []string{"sudo config interface ip remove Vlan3999 198.18.3.1/24"})
	if err == nil || !strings.Contains(err.Error(), "ended before its last line") {
		t.Errorf("runBatch = %v", err)
	}
}

// A fixed IP outside the network, or on the switch's own, the network or
// the broadcast address, is left out of udhcpd's config with a note; the
// rest are static leases (busybox udhcpd's static_lease).
func TestFixedIPsInUdhcpd(t *testing.T) {
	in := devicemodel.L3Interface{Name: "dui-l3-test", VLAN: 3999, Address: "198.18.3.1/24", DHCP: devicemodel.L3DHCP{
		Mode: "server", Start: "198.18.3.100", End: "198.18.3.199", Router: "198.18.3.1",
		Static: []devicemodel.DHCPStatic{
			{MAC: "02:00:00:00:00:01", IP: "198.18.3.51"},
			{MAC: "02:00:00:00:00:02", IP: "198.18.4.9"},
			{MAC: "02:00:00:00:00:03", IP: "198.18.3.1"},
			{MAC: "02:00:00:00:00:04", IP: "198.18.3.255"},
			{MAC: "02:00:00:00:00:05", IP: "198.18.3.120"},
		}}}
	conf, why, skipped := udhcpdConf("Vlan3999", in)
	if why != "" {
		t.Fatal(why)
	}
	if !strings.HasSuffix(conf, "static_lease 02:00:00:00:00:01 198.18.3.51\nstatic_lease 02:00:00:00:00:05 198.18.3.120\n") {
		t.Errorf("udhcpd config:\n%s", conf)
	}
	if len(skipped) != 3 || !strings.Contains(skipped[0], "is not inside 198.18.3.0/24") || !strings.Contains(skipped[1], "the switch's own") {
		t.Errorf("skipped = %q", skipped)
	}
}

// A routed port on a split cage has no one interface for its address: the
// L3 apply and the port apply both leave it with a note.
func TestRoutedPortOnASplitCage(t *testing.T) {
	r := rig20261106(t, fixtureL3)
	ctx := context.Background()
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	ports := map[int]devicemodel.Port{31: {Index: 31, IfName: "Ethernet120", Interfaces: []string{"Ethernet120", "Ethernet121", "Ethernet122", "Ethernet123"}}}
	if _, why := r.c.l3IfaceOf(devicemodel.L3Interface{Port: 31}, ports); why != "port 31 is a split cage (4 ports)" {
		t.Errorf("why = %q", why)
	}
	if iface, why := r.c.l3IfaceOf(devicemodel.L3Interface{LAG: 2}, ports); iface != "PortChannel2" || why != "" {
		t.Errorf("aggregate 2 = %q, %q", iface, why)
	}
}

// A port that still carries an address (a routed port returning to
// switching whose address the L3 apply has not removed) gets no VLAN:
// SONiC refuses a router interface as a VLAN member, which would fail the
// whole batch.
func TestVLANsWaitForARoutedPortsAddress(t *testing.T) {
	r := rig20261106(t, fixtureL3)
	ctx := context.Background()
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	routed := devicemodel.DeviceDesired{L3Set: true, L3Interfaces: []devicemodel.L3Interface{{Name: "dui-routed-31", Port: 31, Address: "198.18.4.1/30"}}}
	if _, err := r.c.ApplyDevice(ctx, routed); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.ApplyPorts(ctx, []devicemodel.PortDesired{{Index: 31, Enabled: true, VLANSet: true, Routed: true}}); err != nil {
		t.Fatal(err)
	}
	if got := r.fr.Commands; !containsInOrder(got, "sudo config vlan member del 4094 Ethernet120", save, "sudo config interface ip add Ethernet120 198.18.4.1/30", save) {
		t.Fatalf("routing the port wrote\n  %s", strings.Join(got, "\n  "))
	}
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	// The port asked back into its VLAN while the routed network still
	// stands (its address is still the L3 apply's).
	r.fr.Commands = nil
	if _, err := r.c.ApplyPorts(ctx, []devicemodel.PortDesired{{Index: 31, Enabled: true, VLANSet: true, NativeVLAN: 1}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.fr.Commands {
		if strings.Contains(c, "vlan member add") {
			t.Errorf("a VLAN was added to a router interface: %s", c)
		}
	}
	if !strings.Contains(r.log.String(), "port 31: Ethernet120 still carries an address (a routed port returning to switching); its VLANs wait for the address to go") {
		t.Errorf("no note:\n%s", r.log.String())
	}
}
