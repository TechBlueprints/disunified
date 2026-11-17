package sonic

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/fecpolicy"
)

// presetFEC puts PORT|Ethernet0 back as the L2 preset generated it on the
// day -- no fec field, as on every other port of the capture (and of the VS
// capture) -- before `config interface fec Ethernet0 rs` was typed by hand
// to bring the link up.
func presetFEC(fr *FixtureRunner) {
	_ = fr.update("cfg_port", func(t table) error {
		delete(t["PORT|Ethernet0"], "fec")
		return nil
	})
}

// §10.3, as seen live: the CWDM4 module in cage 1 needs RS-FEC and UniFi
// pushes none. The push that leaves FEC at auto gets the module's FEC; the
// plan the first-push hold reads does not count it; then nothing more.
func TestFECFollowsTheModule(t *testing.T) {
	r := newHardwareRig(t, presetFEC)
	base := r.baseline()
	if p := port(r.system2(), 1); p.FECConfig != devicemodel.FECUnknown || p.Optic == nil || p.Optic.MediaType != "100G CWDM4" {
		t.Fatalf("port 1 before: FEC config %q optic %+v", p.FECConfig, p.Optic)
	}
	if got := r.c.PlanPorts(base); len(got) != 0 {
		t.Errorf("PlanPorts = %v: the module's FEC is not the controller's change", got)
	}
	snap := r.expect("auto", base, []string{"sudo config interface fec Ethernet0 rs", save})
	if p := port(snap, 1); p.FECConfig != devicemodel.FECRS {
		t.Errorf("port 1 FEC config after = %q", p.FECConfig)
	}
	// The other 31 cages are empty: no module, no opinion, no note.
	if strings.Contains(r.log.String(), "no FEC known") {
		t.Errorf("an empty cage logged a note: %s", r.log.String())
	}
}

// A FEC the controller pushes wins over the module's, and auto brings the
// module's back.
func TestPushedFECWinsOverTheModule(t *testing.T) {
	r := newHardwareRig(t, nil)
	base := r.baseline()
	if got, n := r.apply(base); len(got) != 0 || n != 0 {
		t.Fatalf("the unit as captured (RS on cage 1) needs no write: %q", got)
	}
	off := devicemodel.FECDisabled
	r.expect("pushed none", edit(base, 1, func(d *devicemodel.PortDesired) { d.FEC = &off }), []string{"sudo config interface fec Ethernet0 none", save})
	r.expect("auto again", base, []string{"sudo config interface fec Ethernet0 rs", save})
}

// A module the table does not know (the capture's, with CLR4's code in
// place of CWDM4's -- that MSA leaves FEC optional): nothing written, one
// note.
func TestModuleFECUnknownModule(t *testing.T) {
	r := newHardwareRig(t, func(fr *FixtureRunner) {
		presetFEC(fr)
		_ = fr.update("xcvr", func(t table) error {
			x := t["TRANSCEIVER_INFO|Ethernet0"]
			x["specification_compliance"] = strings.Replace(x["specification_compliance"], "'100G CWDM4'", "'100G CLR4'", 1)
			return nil
		})
	})
	if got, _ := r.apply(r.baseline()); len(got) != 0 {
		t.Errorf("wrote %q", got)
	}
	if !strings.Contains(r.log.String(), `no FEC known for the module "100G CLR4" at 100000 Mb/s on Ethernet0`) {
		t.Errorf("no note: %s", r.log.String())
	}
}

// The names xcvrd prints (sonic-platform-common 202505) to the policy's
// medium, and the choice within what a DX010 port lists: a whole cage
// none,rs; a 25G lane none,fc (docs/fec-by-standard.md §3).
func TestModuleMediumAndChoice(t *testing.T) {
	rs, none, fc := devicemodel.FECRS, devicemodel.FECDisabled, devicemodel.FECFC
	cage := []devicemodel.FEC{none, rs}
	lane := []devicemodel.FEC{none, fc}
	full := []devicemodel.FEC{none, fc, rs}
	for _, tc := range []struct {
		name  string
		speed int
		sup   []devicemodel.FEC
		want  devicemodel.FEC
		ok    bool
	}{
		{"100G CWDM4", 100000, cage, rs, true},
		{"100GBASE-SR4 or 25GBASE-SR", 100000, cage, rs, true},
		{"100GBASE-SR4 or 25GBASE-SR", 25000, full, rs, true},
		{"100GBASE-SR4 or 25GBASE-SR", 25000, lane, "", false}, // RS required; the lane has none
		{"100GBASE-LR4 or 25GBASE-LR", 100000, cage, none, true},
		{"100GBASE-LR4 or 25GBASE-LR", 25000, full, rs, true}, // was "disabled": the standard requires RS
		{"100GBASE-ER4 or 25GBASE-ER", 25000, full, rs, true}, // likewise
		{"100GBASE-LR4 or 25GBASE-LR", 25000, lane, "", false},
		{"100G AOC (Active Optical Cable) or 25GAUI C2M AOC", 100000, cage, rs, true},
		{"100G AOC (Active Optical Cable) or 25GAUI C2M AOC", 25000, lane, "", false},
		{"100G AOC or 25GAUI C2M AOC", 100000, cage, none, true},
		{"100G AOC or 25GAUI C2M AOC", 25000, lane, none, true},
		{"100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS", 100000, cage, rs, true},
		{"100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS", 50000, cage, rs, true},
		{"100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS", 25000, lane, "", false}, // CA-L on a CR-S host
		{"25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R", 25000, lane, fc, true},
		{"25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R", 25000, full, fc, true},
		{"25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R", 50000, cage, rs, true}, // was left alone: FC is not offered, RS is allowed
		{"25GBASE-CR CA-25G-N or 50GBASE-CR2 with no FEC", 25000, lane, none, true},
		{"25GBASE-CR CA-25G-N or 50GBASE-CR2 with no FEC", 50000, cage, none, true},
		{"40GBASE-SR4", 40000, cage, none, true},
		{"40GBASE-CR4", 40000, cage, none, true},
		{"100G CWDM4", 40000, cage, none, true},
		{"100G CLR4", 40000, cage, none, true},
		{"4 x 10GBASE-SR", 10000, lane, none, true},
		{"100G CWDM4", 25000, lane, "", false},
		{"100G CLR4", 100000, cage, "", false},
		{"", 100000, cage, "", false},
		{"", 40000, cage, "", false},
	} {
		got, ok, _ := fecpolicy.Choose(moduleMedium(tc.name), tc.speed, tc.sup)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q at %d within %v = %q %v, want %q %v", tc.name, tc.speed, tc.sup, got, ok, tc.want, tc.ok)
		}
	}
}

// settled is the split-DAC capture's rig with its baseline applied (the
// baseline takes the capture's test aggregate apart) and collected again.
func settled(t *testing.T, edits ...func(*FixtureRunner)) (*controlRig, []devicemodel.PortDesired, []string) {
	t.Helper()
	r := rig20261106(t, fixtureSplitDAC, edits...)
	base := r.baseline()
	first, _ := r.apply(base)
	r.collect()
	return r, base, first
}

// Cage 3 as captured: split 4x25G with a CA-S breakout cable, its lanes on
// fc (none,fc supported). The standard's choice is what is there: no FEC
// write and no FEC note, in the device's plan and in a lane switch's.
func TestSplitDACKeepsFC(t *testing.T) {
	r, base, first := settled(t)
	for _, c := range first {
		if strings.Contains(c, " fec ") {
			t.Errorf("wrote %q", c)
		}
	}
	if got, n := r.apply(base); len(got) != 0 || n != 0 {
		t.Errorf("the settled unit needs no write: %q", got)
	}
	v, err := r.c.Lanes(3)
	if err != nil {
		t.Fatal(err)
	}
	var lanes []devicemodel.PortDesired
	for _, l := range v.Lanes {
		lanes = append(lanes, devicemodel.PortDesired{Index: l.Index, Enabled: l.Enabled, Description: l.Description})
	}
	n0 := len(r.fr.Commands)
	if _, err := r.c.ApplyLanes(context.Background(), 3, lanes); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.fr.Commands[n0:] {
		if strings.Contains(c, " fec ") {
			t.Errorf("the lane switch's plan wrote %q", c)
		}
	}
	if strings.Contains(r.log.String(), "FEC") {
		t.Errorf("a FEC note for a cage that follows the standard: %s", r.log.String())
	}
}

// The same cage holding what a 25G lane of this switch cannot run by the
// standard: a CA-25G-L cable or a standard 25G optic call for RS and the
// lanes list none,fc. Nothing is written and the note says so outright.
// (These modules also run the cage whole, so the capture's split is kept
// by forcing the lane speed, as the UI's drawer does.)
func TestLaneCannotMeetTheModule(t *testing.T) {
	for name, medium := range map[string]string{
		"100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS": "copper CA-25G-L / 100GBASE-CR4",
		"100GBASE-SR4 or 25GBASE-SR":                               "100GBASE-SR4 / 25GBASE-SR",
		"100GBASE-LR4 or 25GBASE-LR":                               "100GBASE-LR4 / 25GBASE-LR",
		"100GBASE-ER4 or 25GBASE-ER":                               "100GBASE-ER4 / 25GBASE-ER",
	} {
		r := rig20261106(t, fixtureSplitDAC, func(fr *FixtureRunner) {
			_ = fr.update("xcvr", func(t table) error {
				x := t["TRANSCEIVER_INFO|Ethernet8"]
				x["specification_compliance"] = strings.Replace(x["specification_compliance"], "25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R", name, 1)
				return nil
			})
		})
		got, _ := r.apply(speedOf(r.baseline(), 3, 25000))
		for _, c := range got {
			if strings.Contains(c, " fec ") || strings.Contains(c, "breakout") {
				t.Errorf("%s: wrote %q", name, c)
			}
		}
		want := `port 3: the module "` + name + `" on Ethernet8: ` + medium + ` at 25000 Mb/s runs only with FEC rs by the standard and the port supports none, fc: the two cannot link within the standard; FEC left as it is`
		if !strings.Contains(r.log.String(), want) {
			t.Errorf("%s: no note %q in: %s", name, want, r.log.String())
		}
	}
}

// A "with BASE-R" (CA-25G-S) cable on a cage split 2x50G: the halves list
// none,rs, the cable's set is FC or RS, so RS -- where the one-mode table
// asked for FC and left the port alone. Written the cycle after the split,
// once the halves' own list is read.
func TestBaseRCableOnTwoLanePortsGetsRS(t *testing.T) {
	r := newHardwareRig(t, withModule("25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R"))
	ds := speedOf(r.baseline(), 1, 50000)
	got, _ := r.apply(ds)
	if len(got) == 0 || got[0] != "sudo config interface breakout -f -y Ethernet0 '2x50G' </dev/null" {
		t.Fatalf("split: %q", got)
	}
	for _, c := range got {
		if strings.Contains(c, " fec ") {
			t.Errorf("FEC written before the halves' supported_fecs was read: %q", c)
		}
	}
	r.system2()
	r.expect("the cycle after", ds, []string{"sudo config interface fec Ethernet0 rs", "sudo config interface fec Ethernet2 rs", save})
}

// A port that is up on a mode the standard permits for its module is not
// flapped for the preferred one: cage 1 of the capture, a linked CA-L DAC
// on rs, read as a BER 1e-12 active cable (none preferred, RS permitted).
func TestLinkedPortKeepsAPermittedFEC(t *testing.T) {
	r, base, _ := settled(t, func(fr *FixtureRunner) {
		_ = fr.update("xcvr", func(t table) error {
			x := t["TRANSCEIVER_INFO|Ethernet0"]
			x["specification_compliance"] = strings.Replace(x["specification_compliance"], "100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS", "100G AOC or 25GAUI C2M AOC", 1)
			return nil
		})
	})
	if p := port(r.system2(), 1); !p.Up || p.FECConfig != devicemodel.FECRS || p.Optic == nil || p.Optic.MediaType != "100G AOC or 25GAUI C2M AOC" {
		t.Fatalf("cage 1: up %v FEC %q optic %+v", p.Up, p.FECConfig, p.Optic)
	}
	if got, _ := r.apply(base); len(got) != 0 {
		t.Errorf("wrote %q", got)
	}
}

// supported_fecs as STATE_DB lists it on the DX010.
func TestSupportedFECsRead(t *testing.T) {
	r := newHardwareRig(t, nil)
	r.c.mu.Lock()
	got := r.c.state.fecs["Ethernet0"]
	r.c.mu.Unlock()
	if !reflect.DeepEqual(got, map[string]bool{"none": true, "rs": true}) {
		t.Errorf("supported FECs = %v", got)
	}
}

// system2 collects and returns the snapshot.
func (r *controlRig) system2() *devicemodel.Snapshot {
	r.t.Helper()
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return snap
}

// §11.8: FC-FEC pushed on a QSFP28 cage of the DX010, whose supported_fecs
// is none,rs. portconfig refuses it on stdout and exits 1, which under set
// -e failed the whole batch live and left the rest of the push unapplied
// every retry; the driver now skips the mode with a note and the rest of
// the port's push (here its name) goes through.
func TestPushedFECTheCageCannotRun(t *testing.T) {
	r := newHardwareRig(t, nil)
	if _, err := r.fr.Run(context.Background(), batchCommand, "set -e\nsudo config interface fec Ethernet0 fc\n"); err == nil || !strings.Contains(err.Error(), "fec fc is not in ['none', 'rs']") {
		t.Fatalf("the capture must refuse FC as portconfig does: %v", err)
	}
	base := r.baseline()
	fc := devicemodel.FECFC
	ds := edit(base, 1, func(d *devicemodel.PortDesired) { d.FEC = &fc; d.Description = "fec test" })
	r.expect("FC on a QSFP28 cage", ds, []string{"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet0' description 'fec test'", save})
	if !strings.Contains(r.log.String(), "port 1: the controller asks for FEC fc, which Ethernet0 does not support (none,rs); FEC left as it is") {
		t.Errorf("no note: %s", r.log.String())
	}
}
