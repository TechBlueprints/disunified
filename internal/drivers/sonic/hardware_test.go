package sonic

import (
	"context"
	"log"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// hwFixture is the DX010's collector run (docs/drivers/sonic.md §10.7):
// build 1238526, the VS guest's commit two builds later, on the L2 preset,
// one 100G CWDM4 module in cage 1 linked to a UniFi device.
const hwFixture = "../../../docs/fixtures/sonic-202505.1238526-ff3c0c9f6/collect.txt"

// startHardware runs the collector over the DX010 capture, dialled on its
// Vlan4094 address. Both builds are commit ff3c0c9f6, so the VS capture's
// help texts and zone list stand in for the hardware's.
func startHardware(t *testing.T) (*Collector, *FixtureRunner, *devicemodel.Snapshot) {
	t.Helper()
	fr, err := NewFixtureRunner(hwFixture)
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
	return c, fr, snap
}

func TestHardwareSectionsParse(t *testing.T) {
	_, fr, _ := startHardware(t)
	sec := sections(fr.Fixture)
	want := map[string]int{
		"cfg_port": 32, "state_port": 32, "counters": 32, "cfg_breakout": 32,
		"fan": 12, "psu": 2, "temp": 5, "chassis": 1, "asic_temp": 1,
		"xcvr": 1, "dom": 1, "lldp": 1, "cfg_snmp": 0,
	}
	for name, n := range want {
		tb, err := parseTable(name, sec[name])
		if err != nil {
			t.Fatal(err)
		}
		if len(tb) != n {
			t.Errorf("section %s: %d entries, want %d", name, len(tb), n)
		}
	}
	if got := strings.TrimSpace(sec["snmp_yml"]); got != "{}" {
		t.Errorf("snmp_yml = %q; the unit's file holds no community since it was fixed by hand", got)
	}
}

// The pmon tables, which the VS leaves empty: five fan drawers of two
// rotors each (the PSU fans belong to the supplies), two PSUs with their
// power, ceiling and temperature, five board sensors (the ASIC table reads
// all zeros on this platform).
func TestHardwarePlatform(t *testing.T) {
	_, _, snap := startHardware(t)
	sys := snap.System
	if sys.Serial != "SSJ00000000" || sys.Model != "Seastone-DX010" || sys.Version != "202505.1238526-ff3c0c9f6" {
		t.Errorf("identity: serial %q model %q version %q", sys.Serial, sys.Model, sys.Version)
	}
	if len(sys.Fans) != 5 {
		t.Fatalf("fans: %d, want the 5 drawers", len(sys.Fans))
	}
	for i, f := range sys.Fans {
		want := devicemodel.Fan{Label: "Fan" + strconv.Itoa(i+1), SpeedPct: 38, TargetPct: 37, OK: true, Direction: "intake"}
		if f != want {
			t.Errorf("fan %d: %+v, want %+v", i+1, f, want)
		}
	}
	wantPSU := []devicemodel.PSU{
		{Slot: "PSU-1", Model: "DPS-800AB-16 K", Serial: "SSJ00000000", Present: true, OK: true, HotSwap: true, OutputW: 67, CapacityW: 900, TempsC: []float64{49}},
		{Slot: "PSU-2", Model: "DPS-800AB-16 K", Serial: "SSJ00000000", Present: true, OK: true, HotSwap: true, OutputW: 62.125, CapacityW: 900, TempsC: []float64{38}},
	}
	if !reflect.DeepEqual(sys.PSUs, wantPSU) {
		t.Errorf("PSUs:\n got %+v\nwant %+v", sys.PSUs, wantPSU)
	}
	if !sys.HasTemperature || sys.TemperatureC != 45.125 || sys.Overheating {
		t.Errorf("temperature %v (has %v, overheating %v), want the ASIC sensor's 45.125", sys.TemperatureC, sys.HasTemperature, sys.Overheating)
	}
}

// Cage 1: the CWDM4 module, its DOM, RS-FEC configured and running, the
// FEC codeword counters, and the speeds the cage runs.
func TestHardwarePortOne(t *testing.T) {
	_, _, snap := startHardware(t)
	p := snap.Ports[0]
	if p.Index != 1 || !p.Up || p.SpeedMbps != 100000 || p.Media != devicemodel.MediaQSFP28 {
		t.Fatalf("port 1: %+v", p)
	}
	o := p.Optic
	if o == nil {
		t.Fatal("port 1: no optic")
	}
	if o.Vendor != "AOI" || o.Part != "AQPLACQ4EDOA1294" || o.Serial != "SSJ00000000" || o.MediaType != "100G CWDM4" {
		t.Errorf("optic identity %+v", o)
	}
	if !o.HasDOM || o.TempC != 26.867 || o.VoltageV != 3.318 || o.TxBiasMA != 39.54 || o.TxPowerDBm != 0.697 || o.RxPowerDBm != -3.134 {
		t.Errorf("optic DOM %+v", o)
	}
	if p.FECConfig != devicemodel.FECRS || p.FEC != devicemodel.FECRS {
		t.Errorf("FEC config %q oper %q, want rs-fec both", p.FECConfig, p.FEC)
	}
	if !p.Health.HasFECCounters || p.Health.FECCorrected != 7 || p.Health.FECUncorrected != 0 {
		t.Errorf("FEC counters: has %v corrected %d uncorrected %d", p.Health.HasFECCounters, p.Health.FECCorrected, p.Health.FECUncorrected)
	}
	// STATE_DB's supported_speeds (40000,100000) plus what platform.json's
	// breakout modes run (4x25G[10G], 2x50G): a split is a speed choice.
	if !reflect.DeepEqual(p.SpeedCaps, []int{10000, 25000, 40000, 50000, 100000}) {
		t.Errorf("speed caps %v", p.SpeedCaps)
	}
	if p.MTU != 1500 {
		t.Errorf("MTU %d, want 1500 (set by hand to match UniFi's jumbo off)", p.MTU)
	}
	for _, q := range snap.Ports[1:] {
		if q.Optic != nil || q.Up {
			t.Errorf("port %d: empty cage reads up %v optic %+v", q.Index, q.Up, q.Optic)
		}
	}
}

// LLDP on hardware: the UniFi device behind cage 1 announces bridge and
// router, so it is the uplink without the VS's gateway-MAC fallback.
func TestHardwareLLDPUplink(t *testing.T) {
	_, _, snap := startHardware(t)
	n := snap.Ports[0].Neighbor
	if n == nil || n.SystemName != "neighbor-1" || !n.IsBridge || !n.IsRouter || n.PortDescription != "eth26" {
		t.Fatalf("port 1 neighbour %+v", n)
	}
	if snap.UplinkHint != 1 {
		t.Errorf("uplink hint %d, want 1", snap.UplinkHint)
	}
}

// eth0 on the unit is cabled but taken down on purpose (no UP flag) and
// keeps its last DHCP lease: reported admin down, without carrier.
func TestHardwareOOBAdminDown(t *testing.T) {
	_, _, snap := startHardware(t)
	oob := snap.System.OOBInterfaces
	if len(oob) != 1 || oob[0].Name != "eth0" || oob[0].Up || !oob[0].AdminDown || oob[0].IP == "" {
		t.Errorf("OOB interfaces %+v, want eth0 admin down with its stale address", oob)
	}
}

// The module's name as UniFi's own SONiC switch reports it: the capture's
// compliance dict, and the same dict with the other codes xcvrd prints
// (sonic-platform-common 202505 sff8636.py / sff8024.py) in its place.
func TestComplianceName(t *testing.T) {
	sec := sections(mustRead(t, hwFixture))
	x := parseTableMust(t, sec, "xcvr")["TRANSCEIVER_INFO|Ethernet0"]
	raw := x["specification_compliance"]
	if got := complianceName(raw); got != "100G CWDM4" {
		t.Fatalf("complianceName(capture) = %q", got)
	}
	for _, tc := range []struct{ from, to, want string }{
		{"'10/40G Ethernet Compliance Code': 'Extended'", "'10/40G Ethernet Compliance Code': '40GBASE-SR4'", "40GBASE-SR4"},
		{"'10/40G Ethernet Compliance Code': 'Extended'", "'10/40G Ethernet Compliance Code': '40GBASE-CR4,Extended'", "100G CWDM4"},
		{"'Extended Specification Compliance': '100G CWDM4'", "'Extended Specification Compliance': '100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS'", "100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS"},
		{"'Extended Specification Compliance': '100G CWDM4'", "'Extended Specification Compliance': 'Unspecified'", ""},
	} {
		s := strings.Replace(raw, tc.from, tc.to, 1)
		if s == raw {
			t.Fatalf("%q not in the capture's compliance string", tc.from)
		}
		if got := complianceName(s); got != tc.want {
			t.Errorf("complianceName with %s = %q, want %q", tc.to, got, tc.want)
		}
	}
	// Not a dict: the string itself, unless it names nothing.
	if got := complianceName(x["type"]); got != "QSFP28 or later" {
		t.Errorf("complianceName(type) = %q", got)
	}
	for _, s := range []string{"N/A", "", "Unknown", "{'a': 'b'", "{'a' 'b'}"} {
		if got := complianceName(s); got != "" && got != s {
			t.Errorf("complianceName(%q) = %q", s, got)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	fr, err := NewFixtureRunner(path)
	if err != nil {
		t.Fatal(err)
	}
	return fr.Fixture
}
