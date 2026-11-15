package sonic

import (
	"maps"
	"testing"
)

// The optic's fault flags (docs/drivers/sonic.md §10.10). collect-optic.txt
// is the DX010 on igmp-202511.20261114-42cccc87a (2026-11-14): one module,
// the uplink's 100G CWDM4 in cage 1, linked and healthy, its
// TRANSCEIVER_STATUS_FLAG every channel "False". No fault can be induced on
// it, and neither upstream's test data (sonic-utilities' mock state_db,
// sonic-platform-common's sff8636 tests) nor any capture holds one, so the
// fault cases below are that real entry with one field set to "True" --
// the string xcvrd writes for a Python True (it writes "False" for this
// module's False, as captured) -- and are marked as such.
const (
	fixtureOptic      = "../../../docs/fixtures/sonic-igmp-202511.20261114-42cccc87a/collect-optic.txt"
	fixtureOpticSplit = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-4x25g.txt"
)

func opticRig(t *testing.T, path string, edit func(db map[string]table)) []cagePort {
	t.Helper()
	db := parseFixtureDB(t, path)
	if edit != nil {
		edit(db)
	}
	_, plat := parsePlatform(sections(mustRead(t, path))["platform_json"])
	return buildPorts(db, 4094, plat)
}

// setFlag rewrites one field of a module's flag entry (a copy of the real one).
func setFlag(db map[string]table, iface, field, value string) {
	k := "TRANSCEIVER_STATUS_FLAG|" + iface
	f := maps.Clone(db["xcvr_flag"][k])
	f[field] = value
	db["xcvr_flag"][k] = f
}

func TestHealthyOpticReportsNoFault(t *testing.T) {
	db := parseFixtureDB(t, fixtureOptic)
	f := db["xcvr_flag"]["TRANSCEIVER_STATUS_FLAG|Ethernet0"]
	if len(db["xcvr_flag"]) != 1 || len(f) != 9 || f["rx1los"] != "False" || f["tx4fault"] != "False" {
		t.Fatalf("capture: %d entries, Ethernet0 %v", len(db["xcvr_flag"]), f)
	}
	ports := opticRig(t, fixtureOptic, nil)
	for _, p := range ports {
		if p.Health.OpticRxAlarm || p.Health.OpticTxAlarm {
			t.Errorf("port %d: rx %v tx %v, want no fault", p.Index, p.Health.OpticRxAlarm, p.Health.OpticTxAlarm)
		}
	}
	if ports[0].Optic == nil || !ports[0].Present || ports[0].Optic.MediaType != "100G CWDM4" {
		t.Errorf("port 1: optic %+v present %v", ports[0].Optic, ports[0].Present)
	}
	// A capture from before the section reads as no flags at all.
	for _, p := range opticRig(t, fixtureOpticSplit, nil) {
		if p.Health.OpticRxAlarm || p.Health.OpticTxAlarm {
			t.Errorf("older capture, port %d: a fault from no section", p.Index)
		}
	}
}

// Mutations of the real entry (see the top of the file).
func TestOpticFaultFollowsTheModulesFlags(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   func(db map[string]table)
		rx, tx bool
	}{
		{"loss of signal on one channel", func(db map[string]table) { setFlag(db, "Ethernet0", "rx3los", "True") }, true, false},
		{"transmit fault on one channel", func(db map[string]table) { setFlag(db, "Ethernet0", "tx2fault", "True") }, false, true},
		{"both", func(db map[string]table) {
			setFlag(db, "Ethernet0", "rx1los", "True")
			setFlag(db, "Ethernet0", "tx4fault", "True")
		}, true, true},
		// sff8636.py: "N/A" on every channel when the module does not
		// implement the flag (a passive DAC has no receiver to lose light).
		{"not implemented", func(db map[string]table) {
			for _, k := range []string{"rx1los", "rx2los", "rx3los", "rx4los", "tx1fault", "tx2fault", "tx3fault", "tx4fault"} {
				setFlag(db, "Ethernet0", k, "N/A")
			}
		}, false, false},
		// An empty cage's entry never counts: xcvrd drops it with the
		// module, and without TRANSCEIVER_INFO there is no module to fault.
		{"empty cage", func(db map[string]table) {
			setFlag(db, "Ethernet0", "rx1los", "True")
			db["xcvr_flag"]["TRANSCEIVER_STATUS_FLAG|Ethernet8"] = db["xcvr_flag"]["TRANSCEIVER_STATUS_FLAG|Ethernet0"]
		}, true, false},
		// The uplink's link and readings are the real ones throughout: a
		// fault comes from the flag alone, nothing from the readings.
	} {
		t.Run(tc.name, func(t *testing.T) {
			ports := opticRig(t, fixtureOptic, tc.edit)
			if h := ports[0].Health; h.OpticRxAlarm != tc.rx || h.OpticTxAlarm != tc.tx {
				t.Errorf("port 1: rx %v tx %v, want rx %v tx %v", h.OpticRxAlarm, h.OpticTxAlarm, tc.rx, tc.tx)
			}
			for _, p := range ports[1:] {
				if p.Health.OpticRxAlarm || p.Health.OpticTxAlarm || p.Present && !p.Up {
					t.Errorf("port %d: rx %v tx %v present %v", p.Index, p.Health.OpticRxAlarm, p.Health.OpticTxAlarm, p.Present)
				}
			}
		})
	}
}

// A split cage: the lanes run on the module's channels in order. The 4x25G
// capture's cage 2 is split and empty; it gets the uplink module's real
// entries (info and flags) with one channel's flag set.
func TestSplitCageLanesTakeTheirChannels(t *testing.T) {
	mod := parseFixtureDB(t, fixtureOptic)
	ports := opticRig(t, fixtureOpticSplit, func(db map[string]table) {
		db["xcvr"]["TRANSCEIVER_INFO|Ethernet4"] = mod["xcvr"]["TRANSCEIVER_INFO|Ethernet0"]
		db["xcvr_flag"] = table{"TRANSCEIVER_STATUS_FLAG|Ethernet4": mod["xcvr_flag"]["TRANSCEIVER_STATUS_FLAG|Ethernet0"]}
		setFlag(db, "Ethernet4", "rx2los", "True")
		setFlag(db, "Ethernet4", "tx4fault", "True")
	})
	cage := ports[1]
	if cage.Index != 2 || len(cage.lanes) != 4 {
		t.Fatalf("cage 2: index %d, %d lanes", cage.Index, len(cage.lanes))
	}
	if !cage.Health.OpticRxAlarm || !cage.Health.OpticTxAlarm {
		t.Errorf("cage 2: rx %v tx %v, want both", cage.Health.OpticRxAlarm, cage.Health.OpticTxAlarm)
	}
	for j, l := range cage.lanes {
		if rx, tx := l.Health.OpticRxAlarm, l.Health.OpticTxAlarm; rx != (j == 1) || tx != (j == 3) {
			t.Errorf("lane %d (%s): rx %v tx %v", l.Index, l.Interfaces[0], rx, tx)
		}
	}
	// Two lanes of two channels each (2x50G), and one lane over every
	// channel for a module with fewer channels than lanes.
	m := moduleFlags(map[string]string{"rx2los": "True", "rx1los": "False", "rx3los": "False", "rx4los": "False"})
	if !laneSet(m.rxLOS, 0, 2) || laneSet(m.rxLOS, 1, 2) {
		t.Errorf("2x50G: lane 1 %v lane 2 %v", laneSet(m.rxLOS, 0, 2), laneSet(m.rxLOS, 1, 2))
	}
	one := moduleFlags(map[string]string{"rx1los": "True"})
	for j := range 4 {
		if !laneSet(one.rxLOS, j, 4) {
			t.Errorf("one-channel module, lane %d: no fault", j+1)
		}
	}
}
