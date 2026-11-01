package sonic

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
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

// The table, by xcvrd's compliance names (sonic-platform-common 202505).
func TestModuleFECTable(t *testing.T) {
	rs, none, fc := devicemodel.FECRS, devicemodel.FECDisabled, devicemodel.FECFC
	for _, tc := range []struct {
		name  string
		speed int
		want  devicemodel.FEC
		ok    bool
	}{
		{"100G CWDM4", 100000, rs, true},
		{"100GBASE-SR4 or 25GBASE-SR", 100000, rs, true},
		{"100GBASE-SR4 or 25GBASE-SR", 25000, rs, true},
		{"100GBASE-LR4 or 25GBASE-LR", 100000, none, true},
		{"100G AOC (Active Optical Cable) or 25GAUI C2M AOC", 100000, rs, true},
		{"100G AOC or 25GAUI C2M AOC", 100000, none, true},
		{"25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R", 25000, fc, true},
		{"25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R", 50000, fc, true},
		{"40GBASE-SR4", 40000, none, true},
		{"100G CWDM4", 40000, none, true},
		{"100G CWDM4", 25000, "", false},
		{"100G CLR4", 100000, "", false},
		{"", 100000, "", false},
		{"", 40000, "", false},
	} {
		got, ok := moduleFEC(tc.name, tc.speed)
		if got != tc.want || ok != tc.ok {
			t.Errorf("moduleFEC(%q, %d) = %q %v, want %q %v", tc.name, tc.speed, got, ok, tc.want, tc.ok)
		}
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
