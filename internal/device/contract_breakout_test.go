package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/breakout"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// buildBreakoutContractPayload is a lane switch's inform (docs/drivers/
// breakout.md): the DX010 captured with cage 2 split to 4x25G
// (sonic-igmp-202511.20261106-f2e25f54c/collect-4x25g.txt) as the parent,
// the breakout driver on that cage, claimed as the Flex XG (USFXG) the
// controller placed and configured in the 2026-11-02 capture
// (docs/breakout-lag.md 6.6).
func buildBreakoutContractPayload(t *testing.T) map[string]any {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261106-f2e25f54c", "collect-4x25g.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := sonic.NewCollector(fr)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.AttachLaneSwitch(2, devicemodel.Attachment{MAC: "02:00:00:00:00:aa", Name: "lane-switch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := breakout.New(breakout.Options{Parent: "core", Cage: 2, MAC: "02:00:00:00:00:aa", Name: "lane-switch", MaxAge: 1000 * time.Hour}, c, c)
	snap, err := d.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := DescriptorFor("USFXG", snap, Identity{MAC: snap.System.MAC, IP: snap.System.Addresses[0].IP, Hostname: snap.System.Hostname, UDAPIVersion: "1.0.0", UplinkPort: snap.UplinkPort()})
	if err != nil {
		t.Fatal(err)
	}
	desc.FWCaps = FWCapsFor(d.Capabilities())
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetCapabilities(d.Capabilities())
	sess.SetSnapshot(snap)
	sess.SetGatewayIP("192.0.2.2")
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The lane switch's inform has the shape the controller placed and drew in
// the stub capture: five ports, the lanes as SFP28 at their speed, port 5
// the uplink as QSFP28 with the parent as its LLDP neighbour, the MAC's
// digits as the serial.
func TestLaneSwitchPayload(t *testing.T) {
	m := buildBreakoutContractPayload(t)
	pt, _ := m["port_table"].([]any)
	if len(pt) != 5 {
		t.Fatalf("port_table has %d ports", len(pt))
	}
	for _, x := range pt {
		p := x.(map[string]any)
		idx := int(p["port_idx"].(float64))
		up, _ := p["is_uplink"].(bool)
		media, _ := p["media"].(string)
		switch {
		case idx == 5 && (!up || media != "QSFP28"):
			t.Errorf("port 5: is_uplink %v media %q", up, media)
		case idx < 5 && (up || media != "SFP28"):
			t.Errorf("lane %d: is_uplink %v media %q", idx, up, media)
		}
	}
	if s, _ := m["serial"].(string); s != "0200000000AA" {
		t.Errorf("serial %q", s)
	}
	if u, _ := m["uplink"].(string); u != "eth0" {
		t.Errorf("uplink %v", m["uplink"])
	}
	lt, _ := m["lldp_table"].([]any)
	found := false
	for _, x := range lt {
		e := x.(map[string]any)
		if int(e["local_port_idx"].(float64)) == 5 && e["chassis_id"] == "02:00:00:00:00:01" {
			found = true
		}
	}
	if !found {
		t.Errorf("no LLDP entry for the parent on port 5: %v", lt)
	}
}
