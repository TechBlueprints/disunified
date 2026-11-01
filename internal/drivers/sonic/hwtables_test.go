package sonic

import (
	"context"
	"log"
	"os"
	"reflect"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// hwTablesFixture is the DX010's second collector run (2026-11-01), with
// collect.py's later sections: teamd's LAG members (none: one cable),
// CRM, and the hwmon chips (docs/drivers/sonic.md §11.4).
const hwTablesFixture = "../../../docs/fixtures/sonic-202505.1238526-ff3c0c9f6/collect-hwtables.txt"

func startHWTables(t *testing.T) *devicemodel.Snapshot {
	t.Helper()
	fr, err := NewFixtureRunner(hwTablesFixture)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fr.Help, fr.Zones = vs.Help, vs.Zones
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// The supplies with their PMBus chips (10-005a / 11-005b on this
// platform): three temperatures each, the fan's RPM and the input power
// beside psud's output power; the drawers; the FDB's size from CRM; the
// in-band interface's IPv6 (link-local only on site); the optic's revision.
func TestHardwareTables(t *testing.T) {
	snap := startHWTables(t)
	sys := snap.System
	wantPSU := []devicemodel.PSU{
		{Slot: "PSU-1", Model: "DPS-800AB-16 K", Serial: "SSJ00000000", Present: true, OK: true, HotSwap: true,
			OutputW: 67.5, InputW: 78.5, CapacityW: 900, TempsC: []float64{28, 48, 38}, FanRPM: []int{5952}},
		{Slot: "PSU-2", Model: "DPS-800AB-16 K", Serial: "SSJ00000000", Present: true, OK: true, HotSwap: true,
			OutputW: 63.8125, InputW: 74.5, CapacityW: 900, TempsC: []float64{27, 45, 36}, FanRPM: []int{6688}},
	}
	if !reflect.DeepEqual(sys.PSUs, wantPSU) {
		t.Errorf("PSUs:\n got %+v\nwant %+v", sys.PSUs, wantPSU)
	}
	// psud's one temperature is one of the chip's three (the in-number of
	// vout1: temp2 on PSU-1, temp3 on PSU-2, platform psu.py).
	if len(sys.Fans) != 5 || sys.Fans[4].Label != "Fan5" || sys.Fans[4].Direction != "intake" || !sys.Fans[4].OK || sys.Fans[4].Missing {
		t.Errorf("fans %+v", sys.Fans)
	}
	if sys.MACTableCapacity != 8196 || sys.MACTableUsed != 152 {
		t.Errorf("FDB capacity %d used %d, want CRM's 8044 available + 152 used", sys.MACTableCapacity, sys.MACTableUsed)
	}
	if !reflect.DeepEqual(sys.IPv6, []string{"fe80::2/64"}) {
		t.Errorf("IPv6 %v, want Vlan4094's link-local", sys.IPv6)
	}
	if o := snap.Ports[0].Optic; o == nil || o.Revision != "1" {
		t.Errorf("optic %+v, want revision 1 (xcvrd's vendor_rev, padding trimmed)", o)
	}
	for _, p := range snap.Ports {
		if p.LAG != "" || p.LACP != nil {
			t.Errorf("port %d: LAG %q LACP %+v on a switch with no PortChannel", p.Index, p.LAG, p.LACP)
		}
	}
}

// A drawer with a pulled rotor is not OK and runs at the other rotor's
// speed, and one with every rotor gone is missing: the capture's FAN_INFO
// with presence cleared.
func TestFanModulesMissing(t *testing.T) {
	db := parseFixtureDB(t, hwTablesFixture)
	fans := db["fan"].byName("|")
	fans["FAN-3R"]["presence"] = "False"
	got := fanModules(fans, db["psu"].byName("|"))
	if got[2].OK || got[2].Missing || got[2].SpeedPct != atoi(fans["FAN-3F"]["speed"]) {
		t.Errorf("drawer 3 with one rotor: %+v", got[2])
	}
	fans["FAN-3F"]["presence"] = "False"
	if got := fanModules(fans, db["psu"].byName("|")); got[2].OK || !got[2].Missing {
		t.Errorf("drawer 3 with no rotor: %+v", got[2])
	}
}

// upstreamLAGMembers is SONiC's own test data for teamd's members
// (sonic-utilities 202505, docs/fixtures/sonic-utilities-202505/): a member
// with no link, two bundled ones with a partner, and two defaulted ones
// with link but no partner.
const upstreamLAGMembers = "../../../docs/fixtures/sonic-utilities-202505/lag_member_table.json"

func upstreamMembers(t *testing.T) table {
	t.Helper()
	b, err := os.ReadFile(upstreamLAGMembers)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := parseTable("state_lag_member", string(b))
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

// lacpOf on every upstream record: bundled only with link, selection, the
// selected aggregator and the partner in sync; the partner's system id
// only once one has answered.
func TestLACPOfUpstreamRecords(t *testing.T) {
	members := upstreamMembers(t)
	want := map[string]devicemodel.LACP{
		"LAG_MEMBER_TABLE|PortChannel0001|Ethernet112": {},                                                 // link down
		"LAG_MEMBER_TABLE|PortChannel0002|Ethernet116": {Active: true, PartnerSystem: "02:00:00:00:00:02"}, // current
		"LAG_MEMBER_TABLE|PortChannel0003|Ethernet120": {Active: true, PartnerSystem: "02:00:00:00:00:03"},
		"LAG_MEMBER_TABLE|PortChannel001|Ethernet0":    {}, // defaulted: link, no partner
		"LAG_MEMBER_TABLE|PortChannel001|Ethernet4":    {},
	}
	if len(members) != len(want) {
		t.Fatalf("%d records, want %d", len(members), len(want))
	}
	for k, w := range want {
		if l := lacpOf(members[k]); l == nil || *l != w {
			t.Errorf("%s: %+v, want %+v", k, l, w)
		}
	}
	if lacpOf(nil) != nil {
		t.Error("an empty entry is no state")
	}
}

// Each condition alone keeps a bundled member out: the upstream bundled
// record with one field as teamd reports it otherwise.
func TestLACPInactive(t *testing.T) {
	real := upstreamMembers(t)["LAG_MEMBER_TABLE|PortChannel0002|Ethernet116"]
	for field, v := range map[string]string{
		"link.up": "false", "runner.selected": "false", "runner.aggregator.selected": "false",
		"runner.partner_lacpdu_info.state": "53", // 61 without SYNCHRONIZATION
	} {
		f := map[string]string{}
		for k, x := range real {
			f[k] = x
		}
		f[field] = v
		if l := lacpOf(f); l.Active {
			t.Errorf("%s=%s: still active", field, v)
		}
	}
}

// setLACP keys a cage's state by its LAG and its first interface: the
// upstream records against cages named after their members.
func TestSetLACP(t *testing.T) {
	members := upstreamMembers(t)
	ports := []cagePort{
		{Port: devicemodel.Port{Index: 1, LAG: "PortChannel0002", Interfaces: []string{"Ethernet116"}}},
		{Port: devicemodel.Port{Index: 2, LAG: "PortChannel0001", Interfaces: []string{"Ethernet112"}}},
		{Port: devicemodel.Port{Index: 3, Interfaces: []string{"Ethernet120"}}},                    // in no aggregate here
		{Port: devicemodel.Port{Index: 4, LAG: "PortChannel9", Interfaces: []string{"Ethernet0"}}}, // no teamd state
	}
	setLACP(ports, members)
	if l := ports[0].LACP; l == nil || !l.Active || l.PartnerSystem != "02:00:00:00:00:02" {
		t.Errorf("port 1: %+v", l)
	}
	if l := ports[1].LACP; l == nil || l.Active {
		t.Errorf("port 2: %+v, want read and not bundled", l)
	}
	if ports[2].LACP != nil || ports[3].LACP != nil {
		t.Errorf("ports 3/4: %+v %+v, want none", ports[2].LACP, ports[3].LACP)
	}
}

// parseFixtureDB parses every Redis-shaped section of a capture the way
// build does.
func parseFixtureDB(t *testing.T, path string) map[string]table {
	t.Helper()
	sec := sections(mustRead(t, path))
	db := map[string]table{}
	for _, name := range append(redisSections[:len(redisSections):len(redisSections)], laterSections...) {
		db[name] = parseTableMust(t, sec, name)
	}
	return db
}
