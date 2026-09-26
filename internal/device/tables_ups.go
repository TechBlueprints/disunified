package device

import (
	"github.com/jamesbraid/unifi-emu/inform"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// upsTables renders a battery-backed device's state. It is additive to the
// switch tables: a UPS is not a device type on the wire (the UPS 2U is
// `type: usw`), what makes the controller run its battery pipeline is the
// presence of `vbms_table`. Nothing here is sent unless the snapshot carries
// a Battery, so a switch or a PDU is untouched.
//
// Key spellings are reproduced exactly as unifi-emu's PROTOCOL.md records the
// controller reading them -- battpool mixes camelCase (batteryLevel,
// timeToRemain), snake_case (device_total_power_output) and neither
// (ischarging, capWh), and the controller republishes them under other
// names, so a device using those republished spellings reports nothing it
// will read. Load percent is derived by consumers from output/budget and is
// deliberately not sent.
//
// No real UniFi UPS inform has been captured (none is on site), so this
// emitter is spec-derived rather than capture-verified -- below the bar the
// rest of the payload is held to. See docs/drivers/apc-ups.md.
func upsTables(_ inform.Descriptor, snap *devicemodel.Snapshot) map[string]any {
	m := map[string]any{}
	if snap == nil || snap.System.Battery == nil {
		return m
	}
	b := snap.System.Battery

	pool := map[string]any{
		"batteryLevel":              b.ChargePct,
		"timeToRemain":              int(b.Runtime.Seconds()),
		"ischarging":                b.Charging,
		"batt_available_cnt":        1,
		"readycnt":                  1,
		"device_total_power_budget": int(b.RealPowerRatingW + 0.5),
		"batt_total_power":          int(b.RealPowerRatingW + 0.5),
		"batt_available_power":      int(b.RealPowerRatingW + 0.5),
	}
	// The measurements go out only when the device measured them: a
	// fabricated 0 W or 0 V reads as a real reading of an idle or dead rack.
	if b.HasOutput {
		pool["device_total_power_output"] = round2(b.RealPowerW)
		pool["device_output_voltage"] = round2(b.OutputVoltageV)
		pool["device_output_current"] = round2(b.OutputCurrentA)
		if b.ApparentPowerVA > 0 {
			pool["device_total_power_factor"] = round2(b.RealPowerW / b.ApparentPowerVA)
		}
	}
	if b.HasInput {
		pool["device_input_voltage"] = round2(b.InputVoltageV)
	}

	anomaly := 0
	if b.LowBattery {
		anomaly |= bmsAnomalyBatteryLow
	}
	// Overload is asserted from the measured load, not the device's own
	// overload flag alone, so the two bands the controller distinguishes are
	// reported honestly.
	if b.HasOutput {
		switch {
		case b.LoadPct > 120:
			anomaly |= bmsAnomalyOverloadOver120
		case b.LoadPct >= 100 || b.Overload:
			anomaly |= bmsAnomalyOverload100to120
		}
	}

	m["vbms_table"] = map[string]any{
		"is_battery_mode": b.OnBattery,
		"battpool":        pool,
		"bms_run_anomaly": anomaly,
		"battery_table":   []any{},
	}
	m["smart_power_caps"] = SmartPowerCapsNone
	// Keys every captured battery-backed power device sends as literals: no
	// fan, and the battery's temperature is not a chassis sensor, so no
	// temperature card is claimed. power_source "0" is the class constant the
	// captured USP-PDU-Pros send, including the one fed from a UPS.
	if _, has := m["has_fan"]; !has {
		m["has_fan"] = false
	}
	if !snap.System.HasTemperature {
		m["has_temperature"] = false
	}
	m["power_source"] = "0"
	// What a real battery-backed power device reports as its capacity; the
	// switch/PSU path never sets it for a device with no PSU table.
	if _, has := m["total_max_power"]; !has && b.RealPowerRatingW > 0 {
		m["total_max_power"] = int(b.RealPowerRatingW + 0.5)
	}
	if b.HasOutput {
		m["power_consumption"] = round1(b.RealPowerW)
	}
	return m
}
