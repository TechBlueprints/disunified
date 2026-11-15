package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// The DX010's storm detection (docs/drivers/sonic.md §11.21), live on
// 2026-11-14: cage 5 (Ethernet16) under a 1000 kbps broadcast and
// unknown-multicast limit took in two 15 s floods above it, and l2d counted
// the frames above the limit. An inform from the capture before
// (collect.txt, no storm row) and then from the capture after
// (collect-storm.txt) sets exactly the broadcast and multicast storm bits on
// port 5 -- the port stays up, no fault -- and nothing on any other port; a
// third inform with no further growth clears them.
func TestWireContractSonicStorm(t *testing.T) {
	snap := func(name string) (*sonic.Collector, *devicemodel.Snapshot) {
		// the 202511 capture directory carries its own `config` help texts
		fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261106-f2e25f54c", name))
		if err != nil {
			t.Fatal(err)
		}
		c := sonic.NewCollector(fr)
		s, err := c.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return c, s
	}
	c, before := snap("collect.txt")
	_, after := snap("collect-storm.txt")
	ip := before.System.Addresses[0].IP
	desc, err := DescriptorFor("USWF07D", before, Identity{MAC: before.System.MAC, IP: ip, Hostname: before.System.Hostname, UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetCapabilities(c.Capabilities())
	inform := func() map[string]any {
		var m map[string]any
		if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	sess.SetSnapshot(before)
	inform()
	sess.SetSnapshot(after)
	m := inform()
	for _, e := range m["port_table"].([]any) {
		p := e.(map[string]any)
		idx, _ := p["port_idx"].(float64)
		a, _ := p["anomalies"].(float64)
		want := 0
		if idx == 5 {
			want = portAnomalyBroadcastStorm | portAnomalyMulticastStorm
		}
		if int(a) != want {
			t.Errorf("port %v anomalies = %v, want %d", idx, p["anomalies"], want)
		}
	}
	if after.Ports[4].Fault != "" || after.Ports[4].Health.StormDropsUnicast != 0 {
		t.Errorf("port 5: fault %q, unicast storm drops %d; a storm is a detection, not a hold, and this run had no unknown-unicast limit",
			after.Ports[4].Fault, after.Ports[4].Health.StormDropsUnicast)
	}
	sess.SetSnapshot(after)
	if a, _ := portEntry(t, inform(), 5)["anomalies"].(float64); a != 0 {
		t.Errorf("no further growth: port 5 anomalies = %v, want 0", a)
	}
}

// The DX010's unknown-unicast storm detection (docs/drivers/sonic.md
// §11.24), live on 2026-11-15 on igmp-202511.20261115-6c8eb1cf7: cage 15
// (Ethernet56) under a 1000 kbps unknown-unicast limit took in a 15 s flood
// to an unknown MAC at twice the limit, and l2d counted the frames above it
// from the port's sFlow samples. An inform from the capture before
// (collect.txt) and then from the capture during the run
// (collect-ucast-storm.txt) sets exactly the unicast storm bit on port 15,
// no fault; a further inform with no growth clears it.
func TestWireContractSonicUnicastStorm(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7")
	snap := func(name string) (*sonic.Collector, *devicemodel.Snapshot) {
		fr, err := sonic.NewFixtureRunner(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		c := sonic.NewCollector(fr)
		s, err := c.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return c, s
	}
	c, before := snap("collect.txt")
	_, after := snap("collect-ucast-storm.txt")
	ip := before.System.Addresses[0].IP
	desc, err := DescriptorFor("USWF07D", before, Identity{MAC: before.System.MAC, IP: ip, Hostname: before.System.Hostname, UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetCapabilities(c.Capabilities())
	inform := func() map[string]any {
		var m map[string]any
		if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	sess.SetSnapshot(before)
	inform()
	sess.SetSnapshot(after)
	for _, e := range inform()["port_table"].([]any) {
		p := e.(map[string]any)
		idx, _ := p["port_idx"].(float64)
		a, _ := p["anomalies"].(float64)
		want := 0
		if idx == 15 {
			want = portAnomalyUnicastStorm
		}
		if int(a) != want {
			t.Errorf("port %v anomalies = %v, want %d", idx, p["anomalies"], want)
		}
	}
	if after.Ports[14].Fault != "" || after.Ports[14].Health.StormDropsUnicast != 15745 {
		t.Errorf("port 15: fault %q, unicast storm drops %d; want no fault and 15745", after.Ports[14].Fault, after.Ports[14].Health.StormDropsUnicast)
	}
	sess.SetSnapshot(after)
	if a, _ := portEntry(t, inform(), 15)["anomalies"].(float64); a != 0 {
		t.Errorf("no further growth: port 15 anomalies = %v, want 0", a)
	}
}
