package device

import (
	"encoding/json"
	"reflect"
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

func upsSnapshotWithGroups() *devicemodel.Snapshot {
	snap := upsSnapshot()
	snap.Outlets = []devicemodel.Outlet{
		{Index: 1, On: true, Switchable: false}, // the unswitched main group
		{Index: 2, On: true, Switchable: true},  // Outlet Group 1
	}
	return snap
}

// Two group rows, then the six slots the UPS 2U draws that the unit does not
// have, reported present-but-off with no relay so nothing phantom is offered.
func TestUPSReportsGroupRowsAndPadsTheModelsSlots(t *testing.T) {
	m := deviceTables(upsDesc(), upsSnapshotWithGroups())
	rows := outletRows(t, m)
	if len(rows) != 8 {
		t.Fatalf("outlet_table has %d rows, want 8 (2 groups + 6 placeholders for the UPS 2U's picture)", len(rows))
	}
	if m["hw_caps"] != HWCapsOutlet || m["outlet_enabled"] != true {
		t.Errorf("hw_caps=%v outlet_enabled=%v", m["hw_caps"], m["outlet_enabled"])
	}
	main, g1 := rows[0], rows[1]
	if main["index"] != 1 || main["relay_state"] != true || main["outlet_caps"] != 0 {
		t.Errorf("main group row = %v, want index 1, on, no relay bit", main)
	}
	if g1["index"] != 2 || g1["relay_state"] != true || g1["outlet_caps"] != outletCapHasRelay {
		t.Errorf("group 1 row = %v, want index 2, on, relay bit", g1)
	}
	for i, r := range rows[2:] {
		if r["index"] != i+3 || r["relay_state"] != false || r["outlet_caps"] != 0 || r["outlet_type"] != outletTypeAC {
			t.Errorf("placeholder row %v, want index %d, off, no relay, AC", r, i+3)
		}
		if _, has := r["relay_group"]; has {
			t.Errorf("AC placeholder %v carries relay_group; only USB rows do", r["index"])
		}
	}
	for _, r := range rows {
		for _, banned := range []string{"name", "cycle_enabled", "outlet_voltage", "outlet_current", "outlet_power"} {
			if _, present := r[banned]; present {
				t.Errorf("row %v reports %q", r["index"], banned)
			}
		}
	}
}

// A model the count table does not know is not padded.
func TestOutletCountUnknownModelPadsNothing(t *testing.T) {
	if OutletCount("SOME-OTHER-MODEL") != 0 {
		t.Error("unknown model got a slot count")
	}
	desc := upsDesc()
	desc.Model = "SOME-OTHER-MODEL"
	if n := len(outletRows(t, deviceTables(desc, upsSnapshotWithGroups()))); n != 2 {
		t.Errorf("%d rows for an unknown model, want the 2 real ones", n)
	}
}

// A UPS 2U draws eight slots; a two-group UPS reporting rows 1 and 2 leaves
// 3-8 as placeholders. A rack PDU claimed as a USP-PDU-Pro reports its AC
// outlets at 5..20, leaving the USB slots 1-4. A model with no picture of
// its own, or a device without outlets, has none.
func TestPlaceholderOutletsAreTheSlotsTheDeviceLacks(t *testing.T) {
	two := &devicemodel.Snapshot{Outlets: []devicemodel.Outlet{{Index: 1, On: true}, {Index: 2, On: true, Switchable: true}}}
	if got := PlaceholderOutlets("USWDA25", two); !reflect.DeepEqual(got, []int{3, 4, 5, 6, 7, 8}) {
		t.Errorf("USWDA25 with two rows: %v, want 3..8", got)
	}
	var sixteen []devicemodel.Outlet
	for i := 1; i <= 16; i++ {
		sixteen = append(sixteen, devicemodel.Outlet{Index: i, On: true, Switchable: true})
	}
	if got := PlaceholderOutlets("USPPDUP", &devicemodel.Snapshot{Outlets: sixteen}); !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
		t.Errorf("USPPDUP with sixteen rows: %v, want 1..4", got)
	}
	if got := PlaceholderOutlets("SOME-OTHER-MODEL", two); got != nil {
		t.Errorf("unknown model: %v, want nil", got)
	}
	if got := PlaceholderOutlets("USWDA25", &devicemodel.Snapshot{}); got != nil {
		t.Errorf("no outlets: %v, want nil", got)
	}
	if got := PlaceholderOutlets("USWDA25", nil); got != nil {
		t.Errorf("nil snapshot: %v, want nil", got)
	}
}

// The UPS 2U Pro is a "usp" in the catalogue -- the controller's power
// path -- and must build a descriptor like the "usw" battery models do;
// anything else (an access point) is still refused.
func TestDescriptorAcceptsThePowerDeviceType(t *testing.T) {
	snap := &devicemodel.Snapshot{System: devicemodel.System{MAC: "02:00:00:00:00:01", Serial: "SSJ00000000"}}
	desc, err := DescriptorFor("USPDA2B", snap, Identity{MAC: snap.System.MAC, Serial: snap.System.Serial, IP: "192.0.2.10", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatalf("USPDA2B: %v", err)
	}
	if desc.Type != "usp" {
		t.Errorf("type = %q, want usp on the wire", desc.Type)
	}
	if _, err := DescriptorFor("U6LR", snap, Identity{MAC: snap.System.MAC}); err == nil {
		t.Errorf("an access point model must still be refused")
	}
}
