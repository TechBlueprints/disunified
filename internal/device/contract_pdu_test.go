package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	apcpdu "github.com/TechBlueprints/disunified/internal/drivers/apc-pdu"
)

// The PDU's payload (built by the real driver from the real card capture,
// docs/fixtures/apc-aos-3.9.2) is held to a real USP-PDU-Pro's inform --
// inform-usp-pdu-pro-usppdup.json, one of eleven decrypted last.inform files
// from the UniFi OS support bundle of 2026-09-20, scrubbed -- rather than to
// the switch references: a power device is its own class. Each gap is
// listed with its reason.
var pduContractOmissions = map[string]string{
	"outlet_usb_power_budget":          "the AP7931 has no USB outlets",
	"outlet_ac_energy_7":               "no energy counter on this hardware (1G rPDU MIB; rPDU2 is where energy lives)",
	"outlet_ac_energy_30":              "no energy counter on this hardware",
	"outlet_ac_energy_account_time_7":  "no energy counter on this hardware",
	"outlet_ac_energy_account_time_30": "no energy counter on this hardware",
}

// A real AC outlet row carries the names and measurements a bridged one must
// not, or cannot, send.
var pduOutletOmissions = map[string]string{
	"name":                "controller-owned: reporting it back drops the whole outlet table",
	"cycle_enabled":       "controller-owned, same merge rule as name",
	"outlet_voltage":      "no per-outlet metering on an AP79xx (AP84xx/AP86xx only)",
	"outlet_current":      "no per-outlet metering on an AP79xx",
	"outlet_power":        "no per-outlet metering on an AP79xx",
	"outlet_power_factor": "no per-outlet metering on an AP79xx",
}

func buildPDUContractPayload(t *testing.T) map[string]any {
	t.Helper()
	fr, err := apcpdu.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "apc-aos-3.9.2"))
	if err != nil {
		t.Fatal(err)
	}
	c := apcpdu.NewCollector(fr)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ip := snap.System.Addresses[0].IP
	desc, err := DescriptorFor("USPPDUP", snap, Identity{MAC: snap.System.MAC, Serial: snap.System.Serial, IP: ip, Hostname: snap.System.Hostname, UDAPIVersion: "1.0.0"})
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
	return m
}

func outletRow(t *testing.T, m map[string]any, index int) map[string]any {
	t.Helper()
	for _, e := range m["outlet_table"].([]any) {
		r := e.(map[string]any)
		if int(r["index"].(float64)) == index {
			return r
		}
	}
	t.Fatalf("no outlet row with index %d", index)
	return nil
}

func TestWireContractPDU(t *testing.T) {
	ours := buildPDUContractPayload(t)
	real := loadReference(t, "inform-usp-pdu-pro-usppdup.json")

	omit := map[string]string{}
	for k, v := range contractOmissions {
		omit[k] = v
	}
	for k, v := range pduContractOmissions {
		omit[k] = v
	}
	compareKeys(t, "apc-pdu device", real, ours, omit)
	// The one network port is the uplink on both.
	compareKeys(t, "apc-pdu uplink port", uplinkPort(t, real), uplinkPort(t, ours), portOmissions)
	// An AC outlet against an AC outlet (index 5 is the first on both), and
	// the model's USB outlet 1 against the placeholder we report for it.
	compareKeys(t, "apc-pdu AC outlet row", outletRow(t, real, 5), outletRow(t, ours, 5), pduOutletOmissions)
	compareKeys(t, "apc-pdu USB outlet row", outletRow(t, real, 1), outletRow(t, ours, 1), pduOutletOmissions)

	if u, ok := ours["uplink"].(string); !ok || u == "" {
		t.Errorf("uplink must be the management interface name (a string), got %v", ours["uplink"])
	}
	// The claim that makes the controller keep an outlet table at all. A real
	// USP-PDU-Pro sends 136 (outlets + LCM); this device has the outlets and
	// not the touchscreen, so it claims exactly the outlet bit.
	if hw, _ := ours["hw_caps"].(float64); int(hw) != HWCapsOutlet {
		t.Errorf("hw_caps = %v, want %d", ours["hw_caps"], HWCapsOutlet)
	}
	if n := len(ours["outlet_table"].([]any)); n != len(real["outlet_table"].([]any)) {
		t.Errorf("outlet_table has %d rows, a real USP-PDU-Pro has %d", n, len(real["outlet_table"].([]any)))
	}
}
