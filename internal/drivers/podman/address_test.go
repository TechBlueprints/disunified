package podman

import (
	"context"
	"log"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// collect-address.txt is the host captured 2026-11-15 while the bridge on it
// dials it over a podman bridge network (podman4, scrubbed to 192.0.2.10):
// the uplink enp6s18 carries the LAN address (192.0.2.2/16) that
// NetworkManager holds static from the controller's IP Settings.
const addressFixture = "../../../docs/fixtures/podman-5.8.2/collect-address.txt"

func startAddressFixture(t *testing.T) (*Collector, *FixtureRunner, *devicemodel.Snapshot) {
	t.Helper()
	fr, err := NewFixtureRunner(addressFixture)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	if a := c.CurrentAddress(); a != "" {
		t.Fatalf("before the first collect: %q, want none", a)
	}
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, fr, snap
}

// The reported address is the host's own: the uplink's, as NetworkManager
// holds it -- never the podman bridges' gateway addresses, one of which is
// where the bridge dials the host.
func TestCurrentAddressIsTheHostsLANAddress(t *testing.T) {
	c, fr, snap := startAddressFixture(t)
	if got := c.CurrentAddress(); got != "192.0.2.2" {
		t.Errorf("CurrentAddress = %q, want the uplink's 192.0.2.2", got)
	}
	if !strings.Contains(fr.section("nmipv4"), "manual\n192.0.2.2/16\n") {
		t.Fatalf("the capture's profile is not the static one this test is about: %q", fr.section("nmipv4"))
	}
	if snap.System.DHCP {
		t.Error("the uplink's address is static in the capture")
	}
	if !strings.Contains(fr.section("addr"), `"local":"192.0.2.10"`) {
		t.Fatal("the capture has lost the podman bridge address the bridge dials")
	}
}

// A host on a lease (the 2026-09-27 capture, ipv4.method auto) reports the
// uplink's address too.
func TestCurrentAddressOnALease(t *testing.T) {
	c, _, snap := startFixture(t)
	if !snap.System.DHCP || len(snap.System.Addresses) == 0 {
		t.Fatalf("collect.txt's uplink should be on a lease: %+v", snap.System.Addresses)
	}
	if got, want := c.CurrentAddress(), snap.System.Addresses[0].IP; got != want || want == "" {
		t.Errorf("CurrentAddress = %q, want the uplink's %q", got, want)
	}
}

// The address follows the host: moved on the host (or by NetworkManager),
// the next poll reports the new one.
func TestCurrentAddressFollowsTheHost(t *testing.T) {
	c, fr, _ := startAddressFixture(t)
	fr.SetSection("addr", strings.ReplaceAll(fr.section("addr"), `"local":"192.0.2.2"`, `"local":"192.0.2.3"`))
	fr.SetSection("nmipv4", strings.Replace(fr.section("nmipv4"), "192.0.2.2/16", "192.0.2.3/16", 1))
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.CurrentAddress(); got != "192.0.2.3" {
		t.Errorf("after the move: %q, want 192.0.2.3", got)
	}
}

// With several addresses on the uplink, the static profile's is the host's.
func TestHostAddressPrefersTheStaticProfile(t *testing.T) {
	addrs := []devicemodel.IfAddress{
		{Iface: "podman4", IP: "192.0.2.10", PrefixLen: 24},
		{Iface: "enp6s18", IP: "192.0.2.20", PrefixLen: 24},
		{Iface: "enp6s18", IP: "192.0.2.2", PrefixLen: 16},
	}
	if got := hostAddress(addrs, "enp6s18", nmState{Method: "manual", Addresses: "192.0.2.2/16"}); got != "192.0.2.2" {
		t.Errorf("static profile: %q", got)
	}
	if got := hostAddress(addrs, "enp6s18", nmState{Method: "auto"}); got != "192.0.2.20" {
		t.Errorf("lease: %q, want the uplink's first", got)
	}
	if got := hostAddress(addrs, "enp6s18", nmState{Method: "manual", Addresses: "192.0.2.99/24"}); got != "192.0.2.20" {
		t.Errorf("a profile address the uplink lacks: %q, want the uplink's first", got)
	}
	if got := hostAddress(addrs, "eth9", nmState{}); got != "" {
		t.Errorf("no such NIC: %q", got)
	}
}
