package podman

import (
	"context"
	"log"
	"testing"
)

// The Podman host's uplink is a virtio_net NIC, whose driver turns the
// carrier off after registering: the host's 2026-11-15 capture
// (collect-carrier.txt, 19.5 days up) reads 1 up and 1 down while up, a
// drop that never happened, which carrier_changes / 2 reported as one.
func TestUplinkCarrierCountsDrops(t *testing.T) {
	fr, err := NewFixtureRunner("../../../docs/fixtures/podman-5.8.2/collect-carrier.txt")
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, ok := portByName(snap, "enp6s18")
	if !ok {
		t.Fatal("no enp6s18 port")
	}
	if !p.Up || p.Health.LinkChanges/2 != 0 {
		t.Errorf("uplink: up %v, %d drops, want up with 0 (LinkChanges %d)", p.Up, p.Health.LinkChanges/2, p.Health.LinkChanges)
	}
	// The older capture's one-field line keeps carrier_changes as it was.
	if got := carrierChanges(map[string]string{"enp6s18": "2"}, "enp6s18"); got != 2 {
		t.Errorf("one-field line = %d, want 2", got)
	}
}
