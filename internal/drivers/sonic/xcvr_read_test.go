package sonic

import (
	"context"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The driver's own read of a module xcvrd has not read
// (docs/drivers/sonic.md §11.36), against the DX010 of 2026-11-16: cage 2
// holds a 100G DAC put in after boot (present, no TRANSCEIVER_INFO), cage 3
// a breakout DAC xcvrd has read. sfp-read-unread.txt is `sfputil show
// presence` and `sfputil show eeprom -p Ethernet4` of that switch in the
// read script's layout; sfp-read-cage3.txt the same for Ethernet8, a
// module xcvrd's record of which is in the collect capture.

const fixtureDir20261115 = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/"

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(fixtureDir20261115 + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sfputil's answer for a module gives the optic xcvrd's record of the same
// module gives.
func TestSfputilEEPROMReadsAsXcvrdsRecord(t *testing.T) {
	f, ok := parseSfputilEEPROM(sections(readFixture(t, "sfp-read-cage3.txt"))["eeprom Ethernet8"])
	if !ok {
		t.Fatal("the EEPROM answer was not read")
	}
	fr, err := NewFixtureRunner(fixtureSplitDAC)
	if err != nil {
		t.Fatal(err)
	}
	xcvr, err := parseTable("xcvr", sections(fr.Fixture)["xcvr"])
	if err != nil {
		t.Fatal(err)
	}
	rec := xcvr["TRANSCEIVER_INFO|Ethernet8"]
	if len(rec) == 0 {
		t.Fatal("the capture has no xcvrd record for Ethernet8")
	}
	if got, want := opticOf(f, nil), opticOf(rec, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("optic from sfputil %+v, from xcvrd %+v", got, want)
	}
	if name := complianceName(f["specification_compliance"]); name != "25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R" {
		t.Errorf("compliance name %q", name)
	}
	for _, k := range []string{"type", "manufacturer", "model", "serial", "vendor_rev", "vendor_oui", "vendor_date", "cable_length", "cable_type", "connector", "encoding", "ext_identifier", "nominal_bit_rate", "specification_compliance"} {
		if k == "specification_compliance" {
			a, _ := parsePyDict(f[k])
			b, _ := parsePyDict(rec[k])
			if !reflect.DeepEqual(a.m, b.m) {
				t.Errorf("%s: sfputil %v, xcvrd %v", k, a.m, b.m)
			}
			continue
		}
		if f[k] != rec[k] {
			t.Errorf("%s: sfputil %q, xcvrd %q", k, f[k], rec[k])
		}
	}
	if _, ok := parseSfputilEEPROM("Ethernet12: SFP EEPROM not detected\n"); ok {
		t.Error("an undetected EEPROM read as a module")
	}
}

func TestUnreadModuleIsReadThroughSfputil(t *testing.T) {
	ctx := context.Background()

	// Without sfputil the cage reads empty, and the driver says so once.
	r := rig20261106(t, fixtureSplitDAC)
	snap, err := r.c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p := foldOf(t, snap, 2); p.Optic != nil {
		t.Fatalf("cage 2 without the sfputil read: optic %+v", p.Optic)
	}
	if n := strings.Count(r.log.String(), "sfputil could not be run"); n != 1 {
		t.Errorf("%d notes for a switch without sfputil, want 1", n)
	}
	if n := len(r.fr.SfpReads); n != 1 {
		t.Errorf("%d reads within the idle interval, want 1", n)
	}

	fr, err := NewFixtureRunner(fixtureSplitDAC)
	if err != nil {
		t.Fatal(err)
	}
	fr.SfpRead = readFixture(t, "sfp-read-unread.txt")
	var buf strings.Builder
	c := NewCollector(fr)
	c.Log = log.New(&buf, "", 0)
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	snap, err = c.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := foldOf(t, snap, 2)
	if !p.Present || p.Optic == nil || p.Optic.MediaType != "100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS" || p.Optic.Part != "MCP1600-C002" {
		t.Fatalf("cage 2 through sfputil: present %v optic %+v", p.Present, p.Optic)
	}
	// The read asked about the cages xcvrd has no record of, and only those.
	if len(fr.SfpReads) != 1 {
		t.Fatalf("%d reads at start, want 1", len(fr.SfpReads))
	}
	script := fr.SfpReads[0]
	if !strings.Contains(script, " Ethernet4 Ethernet12 ") || strings.Contains(script, " Ethernet0 ") || strings.Contains(script, " Ethernet8 ") {
		t.Errorf("the read's cages: %q", script)
	}
	// xcvrd's record is used as it is where it exists.
	if p3 := foldOf(t, snap, 3); p3.Optic == nil || p3.Optic.MediaType != "25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R" {
		t.Errorf("cage 3 (xcvrd's record): %+v", p3.Optic)
	}
	// One line for the cage, and no second read within the minute.
	for i := 0; i < 3; i++ {
		if snap, err = c.Collect(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if p := foldOf(t, snap, 2); p.Optic == nil {
		t.Error("the read was not kept between reads")
	}
	if n := strings.Count(buf.String(), "port 2 (Ethernet4): module present but not read by xcvrd; read through sfputil"); n != 1 {
		t.Errorf("%d lines for cage 2, want 1:\n%s", n, buf.String())
	}
	if len(fr.SfpReads) != 1 {
		t.Errorf("%d reads in four collects within a minute, want 1", len(fr.SfpReads))
	}
}
