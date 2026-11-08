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
