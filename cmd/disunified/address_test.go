package main

import (
	"context"
	"io"
	"log"
	"testing"

	"github.com/TechBlueprints/disunified/internal/drivers/podman"
)

// The Podman host is dialled over a podman bridge network (the bridge runs
// on it, docs/drivers/podman.md §4b), so the dial target is not its address:
// it is reported at the LAN address read on the host, and `ip:` overrides
// that.
func TestReportedIPOfThePodmanHost(t *testing.T) {
	fr, err := podman.NewFixtureRunner("../../docs/fixtures/podman-5.8.2/collect-address.txt")
	if err != nil {
		t.Fatal(err)
	}
	c := podman.NewCollector(fr)
	c.Log = log.New(io.Discard, "", 0)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const dialled = "root@192.0.2.10" // podman4, the host on the bridge's network
	if got := reportedIP("", "", dialled, c, snap); got != "192.0.2.2" {
		t.Errorf("no ip: reported %q, want the host's own 192.0.2.2", got)
	}
	if got := reportedIP("192.0.2.77", "", dialled, c, snap); got != "192.0.2.77" {
		t.Errorf("ip: 192.0.2.77 reported %q, want the override", got)
	}
}
