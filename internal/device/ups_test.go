package device

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

func upsDesc() inform.Descriptor {
	return inform.Descriptor{
		MAC: "02:00:00:00:00:02", Serial: "SSJ00000000", Model: "USWDA25", ModelDisplay: "UPS 2U",
		Version: "15.5", IP: "192.0.2.30", Hostname: "ups-1", Type: "usw", FWCaps: inform.PlaceholderFWCaps,
		Ports: []inform.Port{{IfName: "eth0", Name: "Port 1", PortIdx: 1, Media: "FE", IsUplink: true}},
	}
}

func upsSnapshot() *devicemodel.Snapshot {
	snap := &devicemodel.Snapshot{TakenAt: time.Now()}
	snap.System = devicemodel.System{Vendor: "APC", Model: "Smart-UPS 1500", Version: "15.5"}
	snap.System.Battery = &devicemodel.Battery{
		ChargePct: 100, Runtime: 374 * time.Second, VoltageV: 26.72, TemperatureC: 22.34, HasTemperature: true,
		LoadPct: 80.97, RealPowerW: 1093.08, ApparentPowerVA: 1135.07, RealPowerRatingW: 1350, ApparentRatingVA: 1440,
		OutputVoltageV: 115.77, OutputCurrentA: 9.78, OutputFrequencyHz: 60, HasOutput: true,
		InputVoltageV: 115.77, HasInput: true,
	}
	snap.Ports = []devicemodel.Port{{Index: 1, IfName: "eth0", Name: "SmartConnect", Media: devicemodel.MediaCopper1G,
		Present: true, Enabled: true, Up: true, SpeedMbps: 100, FullDuplex: true}}
	snap.UplinkHint = 1
	return snap
}

func vbms(t *testing.T, m map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	tbl, ok := m["vbms_table"].(map[string]any)
	if !ok {
		t.Fatalf("vbms_table is %T, want an object (it is an object despite the name)", m["vbms_table"])
	}
	pool, ok := tbl["battpool"].(map[string]any)
	if !ok {
		t.Fatalf("battpool is %T, want an object", tbl["battpool"])
	}
	return tbl, pool
}

// vbms_table is what makes a device a UPS to the controller, and its keys
// are read with these exact spellings and no others.
func TestUPSReportsTheBatteryTablesWithTheControllersSpellings(t *testing.T) {
	m := upsTables(upsDesc(), upsSnapshot())
	tbl, pool := vbms(t, m)
	for _, k := range []string{"is_battery_mode", "battpool", "bms_run_anomaly", "battery_table"} {
		if _, ok := tbl[k]; !ok {
			t.Errorf("vbms_table lacks %q", k)
		}
	}
	for _, k := range []string{"batteryLevel", "timeToRemain", "ischarging", "batt_available_cnt", "readycnt",
		"device_total_power_budget", "device_total_power_output", "device_output_voltage", "device_output_current",
		"device_input_voltage", "device_total_power_factor"} {
		if _, ok := pool[k]; !ok {
			t.Errorf("battpool lacks %q", k)
		}
	}
	// The controller's own republished spellings must never be sent.
	for _, k := range []string{"batteryCurrentLevel", "timeToRemaining", "deviceTotalPowerOutput", "load_percentage", "battery_charge", "on_battery"} {
		if _, ok := pool[k]; ok {
			t.Errorf("battpool carries %q, a spelling the controller does not read", k)
		}
	}
	if pool["batteryLevel"] != 100 || pool["timeToRemain"] != 374 {
		t.Errorf("charge/runtime = %v/%v, want 100/374", pool["batteryLevel"], pool["timeToRemain"])
	}
	if pool["device_total_power_budget"] != 1350 || pool["device_total_power_output"] != 1093.08 {
		t.Errorf("budget/output = %v/%v", pool["device_total_power_budget"], pool["device_total_power_output"])
	}
	if m["smart_power_caps"] != SmartPowerCapsNone {
		t.Errorf("smart_power_caps = %v, want 0: the driver honours no power control", m["smart_power_caps"])
	}
	if m["total_max_power"] != 1350 {
		t.Errorf("total_max_power = %v, want the real-power rating", m["total_max_power"])
	}
}

func TestUPSOnBatteryAndLowBatteryAreSignalled(t *testing.T) {
	snap := upsSnapshot()
	snap.System.Battery.OnBattery = true
	snap.System.Battery.LowBattery = true
	tbl, pool := vbms(t, upsTables(upsDesc(), snap))
	if tbl["is_battery_mode"] != true {
		t.Error("is_battery_mode not set while on battery")
	}
	if pool["ischarging"] != false {
		t.Error("ischarging set while on battery")
	}
	if tbl["bms_run_anomaly"].(int)&bmsAnomalyBatteryLow == 0 {
		t.Error("battery-low anomaly bit not set")
	}
}

// Overload bands come from the measured load, in the two bands the controller alerts on.
func TestUPSOverloadBandsFollowTheMeasuredLoad(t *testing.T) {
	for _, tc := range []struct {
		load float64
		want int
	}{{80.97, 0}, {105, bmsAnomalyOverload100to120}, {130, bmsAnomalyOverloadOver120}} {
		snap := upsSnapshot()
		snap.System.Battery.LoadPct = tc.load
		tbl, _ := vbms(t, upsTables(upsDesc(), snap))
		if got := tbl["bms_run_anomaly"].(int) &^ bmsAnomalyBatteryLow; got != tc.want {
			t.Errorf("load %.0f%%: anomaly %d, want %d", tc.load, got, tc.want)
		}
	}
}

// A measurement the unit did not make is omitted, never zeroed.
func TestUPSOmitsUnmeasuredValues(t *testing.T) {
	snap := upsSnapshot()
	snap.System.Battery.HasInput = false
	snap.System.Battery.HasOutput = false
	_, pool := vbms(t, upsTables(upsDesc(), snap))
	for _, k := range []string{"device_input_voltage", "device_total_power_output", "device_output_voltage", "device_output_current"} {
		if _, ok := pool[k]; ok {
			t.Errorf("battpool reports %q though it was not measured", k)
		}
	}
}

// This version presents no outlet table: reporting one would offer the
// controller a relay the driver does not switch yet.
func TestUPSReportsNoOutletTable(t *testing.T) {
	m := deviceTables(upsDesc(), upsSnapshot())
	for _, k := range []string{"outlet_table", "hw_caps", "outlet_enabled"} {
		if _, ok := m[k]; ok {
			t.Errorf("a UPS with no presented outlets reported %q", k)
		}
	}
}

// A switch must not grow battery tables.
func TestSwitchReportsNoBatteryTables(t *testing.T) {
	m := upsTables(testDesc(), testSnapshot())
	if len(m) != 0 {
		t.Errorf("a switch reported UPS keys %v", m)
	}
}

func TestUPSTablesMarshal(t *testing.T) {
	b, err := json.Marshal(upsTables(upsDesc(), upsSnapshot()))
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["vbms_table"].(map[string]any); !ok {
		t.Fatalf("vbms_table did not survive the round trip: %T", back["vbms_table"])
	}
}
