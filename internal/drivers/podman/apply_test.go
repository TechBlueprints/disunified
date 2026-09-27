package podman

import (
	"context"
	"log"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The controller's static IP Settings for the address the uplink already
// carries: the profile is modified and reapplied without bouncing the
// interface, read back, and a second push is a no-op. The capture's host
// is on a lease ("auto"), which is the case that matters: making it
// permanent.
func TestApplyAddressMakesTheLeaseStatic(t *testing.T) {
	c, fr, snap := startFixture(t)
	fr.Commands = nil
	cur := snap.System.Addresses[0]
	if !snap.System.DHCP {
		t.Fatalf("the capture's uplink must be on a lease; got static %v", cur)
	}
	d := devicemodel.AddressDesired{IP: cur.IP, PrefixLen: cur.PrefixLen, Gateway: snap.System.Gateway, DNS: []string{snap.System.Gateway}}
	changed, err := c.ApplyAddress(context.Background(), d)
	if err != nil || !changed {
		t.Fatalf("ApplyAddress = %v, %v; want changed", changed, err)
	}
	if len(fr.Commands) != 3 {
		t.Fatalf("commands = %q, want mod, reapply, read-back", fr.Commands)
	}
	if !strings.HasPrefix(fr.Commands[0], "nmcli con mod 'Wired connection 1' ipv4.method manual ipv4.addresses '"+cur.IP+"/") || !strings.Contains(fr.Commands[0], "ipv4.gateway '"+snap.System.Gateway+"'") {
		t.Errorf("mod = %q", fr.Commands[0])
	}
	if fr.Commands[1] != "nmcli device reapply 'enp6s18'" {
		t.Errorf("reapply = %q", fr.Commands[1])
	}
	fr.Commands = nil
	if changed, err := c.ApplyAddress(context.Background(), d); err != nil || changed {
		t.Errorf("second push: changed %v err %v; want a no-op", changed, err)
	}
	if len(fr.Commands) != 0 {
		t.Errorf("a no-op must run nothing: %q", fr.Commands)
	}
	// A fresh poll reads the same state back from the (fixture's) host.
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fr.Commands = nil
	if changed, _ := c.ApplyAddress(context.Background(), d); changed {
		t.Errorf("after a re-poll the profile is still manual; nothing to write")
	}
}

// What the driver will not do to a host it reaches by that very address.
func TestApplyAddressRefusesMovesAndDHCP(t *testing.T) {
	c, fr, snap := startFixture(t)
	fr.Commands = nil
	if _, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{DHCP: true}); err == nil || !strings.Contains(err.Error(), "refusing DHCP") {
		t.Errorf("DHCP: %v", err)
	}
	if _, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.250", PrefixLen: 24, Gateway: snap.System.Gateway}); err == nil || !strings.Contains(err.Error(), "refusing to move") {
		t.Errorf("foreign address: %v", err)
	}
	if len(fr.Commands) != 0 {
		t.Errorf("a refusal must run nothing: %q", fr.Commands)
	}
	// No NetworkManager profile known: nothing is attempted.
	fr2, _ := NewFixtureRunner(fixture)
	fr2.SetSection("nmconn", "")
	c2 := NewCollector(fr2)
	c2.Log = log.New(testWriter{t}, "", 0)
	if _, err := c2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	cur := snap.System.Addresses[0]
	if _, err := c2.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: cur.IP, PrefixLen: cur.PrefixLen}); err == nil || !strings.Contains(err.Error(), "profile is unknown") {
		t.Errorf("no profile: %v", err)
	}
}
