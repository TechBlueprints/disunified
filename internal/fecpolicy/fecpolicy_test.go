package fecpolicy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

type fecs = []devicemodel.FEC

// The rows of docs/fec-by-standard.md §1, the specified mode first.
func TestAllowedFollowsTheStandard(t *testing.T) {
	for _, tc := range []struct {
		row   string
		m     Medium
		speed int
		want  fecs
	}{
		{"10G: none defined", Other, 10000, fecs{none}},
		{"10G on a 25G-rated DAC", CopperCAS, 10000, fecs{none}},
		{"40G optics: none", Other, 40000, fecs{none}},
		{"40G on a 100G optic", CWDM4, 40000, fecs{none}},
		{"40G CR4: FC optional", Copper40G, 40000, fecs{none, fc}},
		{"40G over a CA-L assembly", CopperCAL, 40000, fecs{none, fc}},
		{"25G CA-N: RS, FC or none", CopperCAN, 25000, fecs{none, fc, rs}},
		{"25G CA-S: RS or FC", CopperCAS, 25000, fecs{fc, rs}},
		{"25G CA-L: RS only", CopperCAL, 25000, fecs{rs}},
		{"25G SR: RS required", SR, 25000, fecs{rs}},
		{"25G LR: RS required", LR, 25000, fecs{rs}},
		{"25G ER: RS required", ER, 25000, fecs{rs}},
		{"25G AOC 5e-5: RS required", ActiveCable5e5, 25000, fecs{rs}},
		{"25G AOC 1e-12: none", ActiveCable1e12, 25000, fecs{none, rs}},
		{"50G-CR2 with no FEC", CopperCAN, 50000, fecs{none, fc, rs}},
		{"50G-CR2 with BASE-R", CopperCAS, 50000, fecs{fc, rs}},
		{"50G-CR2 with RS", CopperCAL, 50000, fecs{rs}},
		{"100G CR4: RS required", CopperCAL, 100000, fecs{rs}},
		{"100G CR4, class unknown", CopperCR4, 100000, fecs{rs}},
		{"100G SR4: RS required", SR, 100000, fecs{rs}},
		{"100G CWDM4", CWDM4, 100000, fecs{rs}},
		{"100G PSM4", PSM4, 100000, fecs{rs}},
		{"100G SWDM4", SWDM4, 100000, fecs{rs}},
		{"100G PAM4 BiDi", PAM4BiDi, 100000, fecs{rs}},
		{"100G LR4: none", LR, 100000, fecs{none}},
		{"100G ER4: none", ER, 100000, fecs{none}},
		{"100G AOC 5e-5", ActiveCable5e5, 100000, fecs{rs}},
		{"100G AOC 1e-12", ActiveCable1e12, 100000, fecs{none, rs}},
		// No answer: the medium is not rated there, or nothing is known.
		{"a 100G optic on a 25G lane", CWDM4, 25000, nil},
		{"a 100G optic at 50G", LR, 50000, nil},
		{"a CR4 of unknown class on a 25G lane", CopperCR4, 25000, nil},
		{"a CA-S breakout at 100G", CopperCAS, 100000, nil},
		{"CLR4 (optional) at 100G", Other, 100000, nil},
		{"1G", CopperCAS, 1000, nil},
		{"unknown at 10G", Unknown, 10000, nil},
		{"unknown at 100G", Unknown, 100000, nil},
	} {
		if got := Allowed(tc.m, tc.speed); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Allowed(%v, %d) = %v, want %v", tc.row, tc.m, tc.speed, got, tc.want)
		}
	}
}

func TestChoose(t *testing.T) {
	for _, tc := range []struct {
		row       string
		m         Medium
		speed     int
		supported fecs
		want      devicemodel.FEC
		ok        bool
		reason    string
	}{
		{"CA-S on a lane without RS (a CR-S host): FC", CopperCAS, 25000, fecs{none, fc}, fc, true, ""},
		{"CA-S on a lane with both: FC, what it is specified for", CopperCAS, 25000, fecs{none, fc, rs}, fc, true, ""},
		{"CA-S on a two-lane port without FC: RS", CopperCAS, 50000, fecs{none, rs}, rs, true, ""},
		{"CA-N anywhere: none", CopperCAN, 25000, fecs{none, fc}, none, true, ""},
		{"CA-L on a full host: RS", CopperCAL, 25000, fecs{none, fc, rs}, rs, true, ""},
		{"CA-L on a CR-S host: cannot link", CopperCAL, 25000, fecs{none, fc}, "", false,
			"copper CA-25G-L / 100GBASE-CR4 at 25000 Mb/s runs only with FEC rs by the standard and the port supports none, fc: the two cannot link within the standard"},
		{"a 25G LR optic on a CR-S host: cannot link", LR, 25000, fecs{none, fc}, "", false, "runs only with FEC rs"},
		{"a 25G SR optic on a full host: RS", SR, 25000, fecs{none, fc, rs}, rs, true, ""},
		{"LR4 at 100G: none", LR, 100000, fecs{none, rs}, none, true, ""},
		{"the port's list unknown: the first allowed", CopperCAS, 25000, nil, fc, true, ""},
		{"a port that lists nothing", CWDM4, 100000, fecs{}, "", false, "the port supports nothing"},
		{"no answer", CWDM4, 25000, fecs{none, fc}, "", false, "no FEC is known for 100G CWDM4 at 25000 Mb/s"},
		{"unknown medium", Unknown, 100000, fecs{none, rs}, "", false, "no FEC is known for unknown at 100000 Mb/s"},
	} {
		got, ok, reason := Choose(tc.m, tc.speed, tc.supported)
		if got != tc.want || ok != tc.ok || !strings.Contains(reason, tc.reason) || (ok && reason != "") {
			t.Errorf("%s: Choose = %q %v %q, want %q %v %q", tc.row, got, ok, reason, tc.want, tc.ok, tc.reason)
		}
	}
}

func TestPermits(t *testing.T) {
	if !Permits(CopperCAS, 25000, rs) || Permits(CopperCAS, 25000, none) || Permits(Unknown, 25000, none) {
		t.Error("Permits disagrees with Allowed")
	}
}

// The SFF-8024 extended compliance codes docs/fec-by-standard.md names.
func TestFromSFF8024(t *testing.T) {
	for code, want := range map[int]Medium{
		0x00: Unknown, 0x01: ActiveCable5e5, 0x02: SR, 0x03: LR, 0x04: ER, 0x06: CWDM4, 0x07: PSM4,
		0x08: ActiveCable5e5, 0x0B: CopperCAL, 0x0C: CopperCAS, 0x0D: CopperCAN, 0x18: ActiveCable1e12,
		0x19: ActiveCable1e12, 0x20: SWDM4, 0x21: PAM4BiDi, 0x17: Other, 0x10: Other,
	} {
		if got := FromSFF8024(code); got != want {
			t.Errorf("FromSFF8024(%#x) = %v, want %v", code, got, want)
		}
	}
}
