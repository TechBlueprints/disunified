package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	apcups "github.com/TechBlueprints/disunified/internal/drivers/apc-ups"
)

// The UPS's payload (built by the real driver from the real register capture,
// docs/fixtures/apc-smtl-15.5) is held to the closest captured cousin: the
// USP-PDU-Pro inform, a one-port `usw`-typed power device. No real UniFi UPS
// inform exists on site, so the battery tables themselves are checked
// against unifi-emu's PROTOCOL.md spellings rather than a capture -- below
// the bar the rest of the payload is held to, and said so in
// docs/drivers/apc-ups.md.
var upsContractOmissions = map[string]string{
	"outlet_table":                     "this version presents no outlets (switching waits on a discussion)",
	"outlet_enabled":                   "no outlet table",
	"hw_caps":                          "no outlet table, so no outlet bit; the model's flags make it a UPS",
	"outlet_ac_power_budget":           "PDU-family aggregate key; a UPS reports capacity in battpool",
	"outlet_ac_power_consumption":      "PDU-family aggregate key; a UPS reports output in battpool",
	"outlet_usb_power_budget":          "no USB outlets",
	"outlet_ac_energy_7":               "no energy counter",
	"outlet_ac_energy_30":              "no energy counter",
	"outlet_ac_energy_account_time_7":  "no energy counter",
	"outlet_ac_energy_account_time_30": "no energy counter",
}

func buildUPSContractPayload(t *testing.T) map[string]any {
	t.Helper()
	fr, err := apcups.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "apc-smtl-15.5"))
	if err != nil {
		t.Fatal(err)
	}
	c := apcups.NewCollector(fr)
	c.Addr = "192.0.2.30:502"
	c.MAC = "02:00:00:00:00:02"
	c.Netmask = "255.255.255.0"
	c.GatewayMAC = "02:00:00:00:00:fe"
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := DescriptorFor("USWDA25", snap, Identity{MAC: snap.System.MAC, Serial: snap.System.Serial, IP: "192.0.2.30", Hostname: "ups-1", UDAPIVersion: "1.0.0"})
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

func TestWireContractUPS(t *testing.T) {
	ours := buildUPSContractPayload(t)
	real := loadReference(t, "inform-usp-pdu-pro-usppdup.json")

	omit := map[string]string{}
	for k, v := range contractOmissions {
		omit[k] = v
	}
	for k, v := range upsContractOmissions {
		omit[k] = v
	}
	compareKeys(t, "apc-ups device", real, ours, omit)
	compareKeys(t, "apc-ups uplink port", uplinkPort(t, real), uplinkPort(t, ours), portOmissions)

	if u, ok := ours["uplink"].(string); !ok || u == "" {
		t.Errorf("uplink must be the management interface name (a string), got %v", ours["uplink"])
	}
	if ours["model"] != "USWDA25" {
		t.Errorf("model = %v", ours["model"])
	}
	// The discriminator: what makes the controller run its battery pipeline.
	tbl, ok := ours["vbms_table"].(map[string]any)
	if !ok {
		t.Fatalf("vbms_table is %T; a UPS must send it as an object", ours["vbms_table"])
	}
	pool, ok := tbl["battpool"].(map[string]any)
	if !ok {
		t.Fatalf("battpool is %T", tbl["battpool"])
	}
	if tbl["is_battery_mode"] != false {
		t.Errorf("is_battery_mode = %v for a unit captured on mains", tbl["is_battery_mode"])
	}
	// Spellings the controller reads (PROTOCOL.md "vbms_table.battpool").
	for _, k := range []string{"batteryLevel", "timeToRemain", "ischarging", "device_total_power_budget", "device_total_power_output", "device_output_voltage", "device_input_voltage"} {
		if _, ok := pool[k]; !ok {
			t.Errorf("battpool lacks %q", k)
		}
	}
	if pool["batteryLevel"].(float64) != 100 || pool["device_total_power_budget"].(float64) != 1350 {
		t.Errorf("charge/budget = %v/%v", pool["batteryLevel"], pool["device_total_power_budget"])
	}
	if out := pool["device_total_power_output"].(float64); out < 500 || out > 1350 {
		t.Errorf("device_total_power_output = %v W, implausible for the captured ~80%% load", out)
	}
	if ours["smart_power_caps"].(float64) != 0 {
		t.Errorf("smart_power_caps = %v, want 0: nothing writable is honoured", ours["smart_power_caps"])
	}
	for _, k := range []string{"outlet_table", "hw_caps"} {
		if _, ok := ours[k]; ok {
			t.Errorf("payload carries %q; this version presents no outlets", k)
		}
	}
	if n := len(ours["port_table"].([]any)); n != 1 {
		t.Errorf("port_table has %d rows, want the one SmartConnect port", n)
	}
}
