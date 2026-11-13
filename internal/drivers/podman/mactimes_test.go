package podman

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// fixtureMACVLAN is a capture of the same host taken 2026-11-13, once the
// bridge's own container had joined the host's macvlan network (scrubbed
// to net-2): the one capture with a running LAN endpoint, so the one that
// yields a MAC entry.
const fixtureMACVLAN = "../../../docs/fixtures/podman-5.8.2/collect-macvlan.txt"

// lanEndpoint is a running container's endpoint on a macvlan/ipvlan
// network, read from the capture's own sections, not through the driver.
type lanEndpoint struct {
	container, network, mac, started string
}

// captureLANEndpoints lists the capture's running LAN endpoints and every
// other endpoint's MAC (bridge networks: never a client).
func captureLANEndpoints(t *testing.T, fr *FixtureRunner) (lan []lanEndpoint, other map[string]bool) {
	t.Helper()
	var nets []struct {
		Name   string `json:"name"`
		Driver string `json:"driver"`
	}
	if err := json.Unmarshal([]byte(fr.section("networks")), &nets); err != nil {
		t.Fatal(err)
	}
	driver := map[string]string{}
	for _, n := range nets {
		driver[n.Name] = n.Driver
	}
	var ins []struct {
		Name     string `json:"name"`
		Started  string `json:"started"`
		Running  bool   `json:"running"`
		Networks map[string]struct {
			MAC string `json:"mac"`
		} `json:"networks"`
	}
	if err := json.Unmarshal([]byte(fr.section("inspect")), &ins); err != nil {
		t.Fatal(err)
	}
	other = map[string]bool{}
	for _, c := range ins {
		for n, e := range c.Networks {
			if d := driver[n]; c.Running && e.MAC != "" && (d == "macvlan" || d == "ipvlan") {
				lan = append(lan, lanEndpoint{c.Name, n, strings.ToLower(e.MAC), c.Started})
			} else if e.MAC != "" {
				other[strings.ToLower(e.MAC)] = true
			}
		}
	}
	return lan, other
}

// A running macvlan endpoint is the one client: its entry was learned when
// the container started (the reduced inspect's `started`) and last seen
// now (the collect), on the container's port, in both the port's table and
// the switch's. No bridge-network endpoint carries an entry.
func TestLANEndpointLearnedAtContainerStart(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureMACVLAN)
	if err != nil {
		t.Fatal(err)
	}
	lan, other := captureLANEndpoints(t, fr)
	if len(lan) == 0 {
		t.Fatal("the capture has no running macvlan/ipvlan endpoint; it no longer exercises the learn time")
	}
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.MACTable) != len(lan) {
		t.Errorf("MAC table has %d entries, the capture %d running LAN endpoints: %+v", len(snap.MACTable), len(lan), snap.MACTable)
	}
	byMAC := map[string]devicemodel.MACEntry{}
	for _, m := range snap.MACTable {
		byMAC[m.MAC] = m
		if other[m.MAC] {
			t.Errorf("%s is a bridge-network endpoint's MAC; no client may be reported for it", m.MAC)
		}
	}
	for _, e := range lan {
		started, err := time.Parse(time.RFC3339Nano, e.started)
		if err != nil {
			t.Fatalf("%s: the capture's started %q does not parse: %v", e.container, e.started, err)
		}
		m, ok := byMAC[e.mac]
		if !ok {
			t.Errorf("%s on %s (%s): no MAC entry", e.container, e.network, e.mac)
			continue
		}
		if !m.LearnedAt.Equal(started) {
			t.Errorf("%s: LearnedAt %v, want the container's start %v", e.container, m.LearnedAt, started)
		}
		if !m.LastSeen.Equal(snap.TakenAt) {
			t.Errorf("%s: LastSeen %v, want the snapshot time %v", e.container, m.LastSeen, snap.TakenAt)
		}
		if m.VLAN != 1 {
			t.Errorf("%s: VLAN %d, want 1", e.container, m.VLAN)
		}
		// The container's port carries the same entry. The bridge's own
		// container has two endpoints, so its port label names the network.
		label := e.container
		if p, ok := portByName(snap, e.container+" "+e.network); ok {
			label = p.Name
		}
		p, ok := portByName(snap, label)
		if !ok {
			t.Errorf("%s: no port", label)
			continue
		}
		if p.Index != m.PortIndex || len(p.MACs) != 1 || p.MACs[0] != m {
			t.Errorf("%s: port %d carries %+v; the table has %+v", label, p.Index, p.MACs, m)
		}
	}
	// Every other port is clientless.
	for _, p := range snap.Ports {
		for _, m := range p.MACs {
			if _, ok := byMAC[m.MAC]; !ok || m.PortIndex != p.Index {
				t.Errorf("port %d (%s) carries %+v, not in the switch's table", p.Index, p.Name, m)
			}
		}
	}
}

// What the capture has for a learn time the driver cannot read: nothing.
// Every running container in it has a `started` that parses, so the case
// of an absent or unparsed one (LearnedAt zero, the loop's tracker then
// supplies the time) has no capture and is not tested here.
func TestEveryRunningContainerInTheCaptureHasAStart(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureMACVLAN)
	if err != nil {
		t.Fatal(err)
	}
	var ins []inspected
	if err := json.Unmarshal([]byte(fr.section("inspect")), &ins); err != nil {
		t.Fatal(err)
	}
	for _, c := range ins {
		if !c.Running {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, c.Started); err != nil {
			t.Errorf("%s: started %q does not parse (%v): the capture now has the unparsed case; test it", c.Name, c.Started, err)
		}
	}
}
