package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	apcbackups "github.com/TechBlueprints/disunified/internal/drivers/apc-backups"
)

// The Back-UPS Pro driver's payload (built by the real driver from the
// card's real pages, docs/fixtures/apc-backups-05.3) must meet the same
// wire contract as the other power devices: the captured USP-PDU-Pro inform
// is the reference, the UPS-only tables come on top.
func TestWireContractBackUPS(t *testing.T) {
	fr, err := apcbackups.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "apc-backups-05.3"))
	if err != nil {
		t.Fatal(err)
	}
	c := apcbackups.NewCollector(fr)
	c.Addr, c.MAC, c.Netmask, c.GatewayMAC = "192.0.2.30", "02:00:00:00:00:01", "255.255.255.0", "02:00:00:00:00:fe"
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := DescriptorFor("USPDA2B", snap, Identity{MAC: snap.System.MAC, Serial: snap.System.Serial, IP: snap.System.Addresses[0].IP, Hostname: "ups-2", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	desc.FWCaps = FWCapsFor(c.Capabilities())
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetCapabilities(c.Capabilities())
	sess.SetSnapshot(snap)
	sess.SetGatewayIP("192.0.2.2")
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	real := loadReference(t, "inform-usp-pdu-pro-usppdup.json")
	omit := map[string]string{}
	for k, v := range contractOmissions {
		omit[k] = v
	}
	for k, v := range upsContractOmissions {
		omit[k] = v
	}
	compareKeys(t, "apc-backups device", real, m, omit)
	compareKeys(t, "apc-backups uplink port", uplinkPort(t, real), uplinkPort(t, m), portOmissions)
	// This unit meters every outlet (the PDU's rows omit the meter keys).
	metered := map[string]string{}
	for k, v := range pduOutletOmissions {
		if k != "outlet_power" && k != "outlet_voltage" && k != "outlet_current" {
			metered[k] = v
		}
	}
	compareKeys(t, "apc-backups metered row", outletRow(t, real, 5), outletRow(t, m, 1), metered)
	compareKeys(t, "apc-backups placeholder row", outletRow(t, real, 5), outletRow(t, m, 5), pduOutletOmissions)
	if m["model"] != "USPDA2B" || m["smart_power_caps"].(float64) != 0 {
		t.Errorf("model %v smart_power_caps %v", m["model"], m["smart_power_caps"])
	}
	rows, _ := m["outlet_table"].([]any)
	if len(rows) != OutletCount("USPDA2B") {
		t.Fatalf("outlet_table has %d rows, want %d (four real, four placeholders)", len(rows), OutletCount("USPDA2B"))
	}
	for i, r := range rows[:4] {
		row := r.(map[string]any)
		caps := int(row["outlet_caps"].(float64))
		wantCaps := outletCapPowerMeter
		if i >= 2 {
			wantCaps |= outletCapHasRelay
		}
		if caps != wantCaps {
			t.Errorf("row %d outlet_caps = %d, want %d (metered; relay only on the switched groups)", i+1, caps, wantCaps)
		}
		if _, has := row["outlet_power"]; !has {
			t.Errorf("row %d carries no outlet_power although the card meters every outlet", i+1)
		}
	}
	v, _ := m["vbms_table"].(map[string]any)
	pool, _ := v["battpool"].(map[string]any)
	if pool["batteryLevel"] != float64(100) || pool["timeToRemain"] != float64(900) {
		t.Errorf("battpool = %v", pool)
	}
}
