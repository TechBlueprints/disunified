package proxmox

import (
	"context"
	"slices"
	"testing"
)

// The device half of the repeated seed (2026-11-17): a guest NIC with
// link_down=1 (VM 999's net0 in the capture) is a present, disabled port
// whose name is one of the driver's own defaults. With the controller
// holding the free-slot combination for it, that is exactly what the
// provisioner's free-slot test matches (unifiapi
// TestSeedLeavesADisabledGuestAlone holds the other half).
func TestLinkDownGuestIsADisabledPortWithADefaultName(t *testing.T) {
	c, _ := newTestCollector(t, "collect-node1-carrier.txt")
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := portByIf(t, snap, "vm999-net0")
	if !p.Present || p.Enabled {
		t.Fatalf("vm999-net0: present=%v enabled=%v, want present and disabled", p.Present, p.Enabled)
	}
	name := c.PortName(p)
	if !slices.Contains(c.DefaultPortNames(p), name) {
		t.Errorf("name %q is not among the driver's defaults %v", name, c.DefaultPortNames(p))
	}
}
