package informloop

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/podman"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The Podman host's address comes from the host (podman.Collector is an
// AddressReporter): an explicit `ip:` is kept while the host's address
// stays put, and once the host's address moves the informs follow it.
func TestPodmanHostAddressIsFollowed(t *testing.T) {
	fr, err := podman.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "podman-5.8.2", "collect-address.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	coll := podman.NewCollector(fr)
	coll.Log = logger
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if coll.CurrentAddress() != "192.0.2.2" {
		t.Fatalf("host address %q", coll.CurrentAddress())
	}
	const explicit = "192.0.2.77"
	desc := inform.Descriptor{MAC: snap.System.MAC, IP: explicit}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true}, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll})
	if err != nil {
		t.Fatal(err)
	}
	l.collect(context.Background())
	l.collect(context.Background())
	if ip, _ := reported(t, sess); ip != explicit {
		t.Errorf("ip: was overridden by an unchanged host address: %q", ip)
	}
	// The host's address moves (by hand on the host, or NetworkManager).
	fr.SetSection("addr", strings.ReplaceAll(fr.Section("addr"), `"local":"192.0.2.2"`, `"local":"192.0.2.3"`))
	fr.SetSection("nmipv4", strings.Replace(fr.Section("nmipv4"), "192.0.2.2/16", "192.0.2.3/16", 1))
	l.collect(context.Background())
	if ip, connect := reported(t, sess); ip != "192.0.2.3" || connect != "192.0.2.3" {
		t.Errorf("after the host moved: ip %q connect_request_ip %q, want 192.0.2.3", ip, connect)
	}
}
