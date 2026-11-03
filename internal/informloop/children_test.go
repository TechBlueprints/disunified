package informloop

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// fixedCollector serves one captured snapshot every cycle.
type fixedCollector struct{ snap *devicemodel.Snapshot }

func (f fixedCollector) Collect(context.Context) (*devicemodel.Snapshot, error) { return f.snap, nil }

// payloadPort is port idx's port_table entry and lldp_table entry in the
// inform the session renders now, and the switch-wide mac_table.
func payloadPort(t *testing.T, sess *device.Session, idx int) (port, lldp map[string]any, macs []any) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	for _, p := range m["port_table"].([]any) {
		if e := p.(map[string]any); e["port_idx"].(float64) == float64(idx) {
			port = e
		}
	}
	lt, _ := m["lldp_table"].([]any)
	for _, e := range lt {
		if e := e.(map[string]any); e["local_port_idx"].(float64) == float64(idx) {
			lldp = e
		}
	}
	macs, _ = m["mac_table"].([]any)
	return port, lldp, macs
}

func hasMAC(entries []any, mac string) bool {
	for _, e := range entries {
		if e.(map[string]any)["mac"] == mac {
			return true
		}
	}
	return false
}

// A child declared on the DX010's idle cage 2 is reported there: in the
// port's MAC table and the switch's, in the native VLAN, and as the port's
// LLDP neighbour; a child on the uplink cage, which has a neighbour of its
// own, is reported in the MAC table only and logged once. The uplink stays
// the real one and the driver's snapshot is left as it was.
func TestChildrenAreReportedOnTheirPorts(t *testing.T) {
	snap := dx010Snapshot(t)
	if snap.Ports[1].Neighbor != nil || len(snap.Ports[1].MACs) != 0 || snap.Ports[0].Neighbor == nil {
		t.Fatal("capture: want cage 2 idle and cage 1 with a neighbour")
	}
	macsBefore := len(snap.MACTable)
	desc, err := device.DescriptorFor("USWF07D", snap, device.Identity{MAC: snap.System.MAC, IP: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef"}, nil, time.Now())
	sess.SetSnapshot(snap)
	var buf strings.Builder
	l, err := New(desc, sess, Config{
		Logger: log.New(&buf, "", 0), Collector: fixedCollector{snap},
		Children: []Child{{Port: 2, MAC: "02:00:00:00:00:a2", Name: "lane-switch"}, {Port: 1, MAC: "02:00:00:00:00:a1", Name: "on-uplink"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		port, lldp, macs := payloadPort(t, sess, 2)
		pm, _ := port["mac_table"].([]any)
		if len(pm) != 1 || pm[0].(map[string]any)["mac"] != "02:00:00:00:00:a2" || pm[0].(map[string]any)["vlan"].(float64) != 1 {
			t.Errorf("%s: cage 2 mac_table = %v, want the child in VLAN 1", when, pm)
		}
		if lldp == nil || lldp["chassis_id"] != "02:00:00:00:00:a2" || lldp["port_id"] != "eth0" {
			t.Errorf("%s: cage 2 lldp_table entry = %v, want the child's MAC as chassis id", when, lldp)
		}
		if !hasMAC(macs, "02:00:00:00:00:a2") || !hasMAC(macs, "02:00:00:00:00:a1") {
			t.Errorf("%s: the switch's mac_table lacks a child", when)
		}
		up, lldp1, _ := payloadPort(t, sess, 1)
		if !hasMAC(up["mac_table"].([]any), "02:00:00:00:00:a1") {
			t.Errorf("%s: uplink cage's mac_table lacks its child", when)
		}
		if lldp1 == nil || lldp1["chassis_id"] == "02:00:00:00:00:a1" {
			t.Errorf("%s: uplink cage's neighbour = %v, want its own", when, lldp1)
		}
	}
	check("start snapshot")
	for i := 0; i < 2; i++ {
		l.collect(context.Background())
		check("after a collect")
	}
	if l.uplinkPort != 1 {
		t.Errorf("uplink port = %d, want 1: a child is never the uplink", l.uplinkPort)
	}
	log := buf.String()
	if strings.Count(log, "reported on port") != 2 {
		t.Errorf("want one start line per child, log:\n%s", log)
	}
	if strings.Count(log, "already has an LLDP neighbour") != 1 {
		t.Errorf("want the uplink cage's neighbour logged once, log:\n%s", log)
	}
	if snap.Ports[1].Neighbor != nil || len(snap.Ports[1].MACs) != 0 || len(snap.MACTable) != macsBefore {
		t.Error("the driver's snapshot was modified")
	}
}

// A child is never the uplink, even on a port that is up: the port its
// child hangs off is a downstream port.
func TestChildIsNeverTheUplink(t *testing.T) {
	snap := dx010Snapshot(t)
	s := *snap
	s.Ports = append([]devicemodel.Port(nil), snap.Ports...)
	s.Ports[0].Neighbor = nil
	s.Ports[1].Up = true
	l := &Loop{cfg: Config{Logger: log.New(&strings.Builder{}, "", 0), Children: []Child{{Port: 2, MAC: "02:00:00:00:00:a2", Name: "x"}}}}
	if got := l.withChildren(&s).UplinkPort(); got == 2 {
		t.Error("the child's port was taken as the uplink")
	}
}

// A child on a port the device does not have is logged once and skipped.
func TestChildOnAMissingPort(t *testing.T) {
	var buf strings.Builder
	l := &Loop{cfg: Config{Logger: log.New(&buf, "", 0), Children: []Child{{Port: 99, MAC: "02:00:00:00:00:a2", Name: "x"}}}}
	snap := dx010Snapshot(t)
	l.withChildren(snap)
	l.withChildren(snap)
	if strings.Count(buf.String(), "port 99 is not in the device's port table") != 1 {
		t.Errorf("log = %q", buf.String())
	}
}
