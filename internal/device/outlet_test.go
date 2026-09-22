package device

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

func pduDesc() inform.Descriptor {
	return inform.Descriptor{
		MAC: "02:00:00:00:00:01", Serial: "SSJ00000000", Model: "USPPDUP",
		ModelDisplay: "UniFi Smart Power Power Distribution Unit Pro",
		Version:      "3.9.2", IP: "192.0.2.20", Hostname: "pdu-1", Type: "usw",
		FWCaps: inform.PlaceholderFWCaps,
		Ports:  []inform.Port{{IfName: "eth0", Name: "Port 1", PortIdx: 1, Media: "FE", IsUplink: true}},
	}
}

func pduSnapshot() *devicemodel.Snapshot {
	snap := &devicemodel.Snapshot{TakenAt: time.Now()}
	snap.System = devicemodel.System{Vendor: "APC", Model: "AP7931", Version: "3.9.2", Uptime: 4707 * time.Second}
	snap.Ports = []devicemodel.Port{{Index: 1, IfName: "eth0", Name: "Network", Media: devicemodel.MediaCopper1G,
		Present: true, Enabled: true, Up: true, SpeedMbps: 100, FullDuplex: true}}
	snap.UplinkHint = 1
	for i := 1; i <= 16; i++ {
		snap.Outlets = append(snap.Outlets, devicemodel.Outlet{Index: i, Name: "Outlet " + itoa(i), On: i != 8, Switchable: true})
	}
	return snap
}

// realOutletRows drops the leading rows for outlets the claimed model has but
// the device does not, so a test can index the device's own outlets from 0.
func realOutletRows(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	rows := outletRows(t, m)
	base := OutletIndexBase(pduDesc().Model)
	return rows[base-1:]
}

func outletRows(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["outlet_table"]
	if !ok {
		t.Fatal("payload carries no outlet_table")
	}
	rows, ok := raw.([]map[string]any)
	if !ok {
		t.Fatalf("outlet_table is %T, want a list of rows", raw)
	}
	return rows
}

func TestPowerDeviceReportsItsOutlets(t *testing.T) {
	m := deviceTables(pduDesc(), pduSnapshot())
	rows := outletRows(t, m)
	if len(rows) != 20 {
		t.Fatalf("outlet_table has %d rows, want 20 (4 the model has and the device has not, plus 16 real)", len(rows))
	}
	real := realOutletRows(t, deviceTables(pduDesc(), pduSnapshot()))
	if m["outlet_enabled"] != true {
		t.Errorf("outlet_enabled = %v, want true", m["outlet_enabled"])
	}
	// hw_caps is load-bearing: without the outlet bit the controller accepts
	// the inform, stores no outlet table and logs nothing.
	if m["hw_caps"] != HWCapsOutlet {
		t.Errorf("hw_caps = %v, want %d (the outlet bit)", m["hw_caps"], HWCapsOutlet)
	}
	if real[7]["relay_state"] != false {
		t.Errorf("outlet 8 relay_state = %v, want false", real[7]["relay_state"])
	}
	if real[0]["relay_state"] != true {
		t.Errorf("outlet 1 relay_state = %v, want true", real[0]["relay_state"])
	}
}

// The controller merges its own outlet names onto the rows it is sent, by
// index, and the USP-PDU-Pro's first four outlets are USB. A 16-outlet rack
// PDU reporting 1..16 is therefore labelled "USB Outlet 1" through 4; its AC
// outlets have to be reported at the model's AC positions, 5..20.
func TestOutletsAreReportedAtTheModelsACPositions(t *testing.T) {
	rows := outletRows(t, deviceTables(pduDesc(), pduSnapshot()))
	rows = realOutletRows(t, deviceTables(pduDesc(), pduSnapshot()))
	if got := rows[0]["index"]; got != 5 {
		t.Errorf("first outlet reported at index %v, want 5 (the model's first AC outlet)", got)
	}
	if got := rows[15]["index"]; got != 20 {
		t.Errorf("last outlet reported at index %v, want 20", got)
	}
}

// A model whose outlets start at 1 must not be shifted.
func TestOutletIndexBaseDefaultsToOne(t *testing.T) {
	if got := OutletIndexBase("SOME-OTHER-MODEL"); got != 1 {
		t.Errorf("OutletIndexBase = %d, want 1 for an unknown model", got)
	}
	desc := pduDesc()
	desc.Model = "SOME-OTHER-MODEL"
	rows := outletRows(t, deviceTables(desc, pduSnapshot()))
	if got := rows[0]["index"]; got != 1 {
		t.Errorf("first outlet reported at index %v, want 1", got)
	}
}

// The controller owns an outlet's name and merges its own onto the row it
// stores. That merge is also its change test, so a device that reports the
// name back reports nothing new and has its whole table dropped -- silently.
func TestOutletTableNeverReportsNamesOrCyclePolicy(t *testing.T) {
	rows := outletRows(t, deviceTables(pduDesc(), pduSnapshot()))
	for _, r := range rows {
		for _, banned := range []string{"name", "cycle_enabled"} {
			if _, present := r[banned]; present {
				t.Errorf("outlet_table row %v reports %q, which drops the whole table", r["index"], banned)
			}
		}
	}
}

// A rack PDU uses the legacy encoding: a small outlet_caps beside an
// outlet_type. A value at or above the AC class bit would send the controller
// looking for the other family's keys.
func TestOutletTableUsesTheRackPDUEncoding(t *testing.T) {
	rows := realOutletRows(t, deviceTables(pduDesc(), pduSnapshot()))
	for _, r := range rows {
		caps, ok := r["outlet_caps"].(int)
		if !ok {
			t.Fatalf("outlet_caps is %T, want int", r["outlet_caps"])
		}
		if caps != outletCapHasRelay {
			t.Errorf("outlet %v caps = %d, want %d (relay, no metering)", r["index"], caps, outletCapHasRelay)
		}
		if r["outlet_type"] != outletTypeAC {
			t.Errorf("outlet %v type = %v, want AC", r["index"], r["outlet_type"])
		}
		// An outlet that does not meter omits the measurements rather than
		// reporting zeros: a zero reads as a real measurement of no load.
		for _, k := range []string{"outlet_voltage", "outlet_current", "outlet_power"} {
			if _, present := r[k]; present {
				t.Errorf("outlet %v reports %s though the device does not meter", r["index"], k)
			}
		}
	}
}

func TestMeteredOutletReportsDecimalStrings(t *testing.T) {
	snap := pduSnapshot()
	snap.Outlets = []devicemodel.Outlet{{Index: 1, On: true, Switchable: true, HasMetering: true, VoltageV: 120, CurrentA: 1.5, PowerW: 180}}
	rows := realOutletRows(t, deviceTables(pduDesc(), snap))
	if rows[0]["outlet_voltage"] != "120.000" {
		t.Errorf("outlet_voltage = %v, want the decimal string this family sends", rows[0]["outlet_voltage"])
	}
	if rows[0]["outlet_current"] != "1.500" {
		t.Errorf("outlet_current = %v", rows[0]["outlet_current"])
	}
	caps := rows[0]["outlet_caps"].(int)
	if caps != outletCapHasRelay|outletCapPowerMeter {
		t.Errorf("caps = %d, want relay|meter", caps)
	}
}

// A switch must not grow an outlet table or the outlet hardware bit.
func TestSwitchReportsNoOutletTable(t *testing.T) {
	m := deviceTables(testDesc(), testSnapshot())
	if _, present := m["outlet_table"]; present {
		t.Error("a switch reported an outlet_table")
	}
	if _, present := m["hw_caps"]; present {
		t.Error("a switch claimed hw_caps")
	}
}

// The whole thing has to survive the wire.
func TestOutletTableMarshals(t *testing.T) {
	b, err := json.Marshal(deviceTables(pduDesc(), pduSnapshot()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rows, ok := back["outlet_table"].([]any)
	if !ok || len(rows) != 20 {
		t.Fatalf("outlet_table did not survive the round trip: %T", back["outlet_table"])
	}
}

// Outlets the claimed model has but the device does not must be reported
// present-but-off with no relay, on every inform. Otherwise the controller
// shows four USB outlets that look enabled and switchable and are not there,
// and a push enabling one would never be contradicted.
func TestOutletsTheDeviceDoesNotHaveAreReportedOffAndUnswitchable(t *testing.T) {
	rows := outletRows(t, deviceTables(pduDesc(), pduSnapshot()))
	for i := 0; i < OutletIndexBase(pduDesc().Model)-1; i++ {
		r := rows[i]
		if r["index"] != i+1 {
			t.Errorf("row %d has index %v, want %d", i, r["index"], i+1)
		}
		if r["relay_state"] != false {
			t.Errorf("absent outlet %v reported on", r["index"])
		}
		if r["outlet_caps"] != 0 {
			t.Errorf("absent outlet %v claims caps %v, want 0 (no relay)", r["index"], r["outlet_caps"])
		}
		if r["outlet_type"] != outletTypeUSB {
			t.Errorf("absent outlet %v type = %v, want USB", r["index"], r["outlet_type"])
		}
	}
}
