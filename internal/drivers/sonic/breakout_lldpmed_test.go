package sonic

import (
	"strings"
	"testing"
)

// A breakout re-makes the cage's interfaces, and lldpd forgets their
// custom TLVs: the plan's LLDP-MED goes on every new interface in the same
// batch, the one that keeps the old one's name included. Before the fix the
// plan's copy kept the joined Ethernet8's TLV, so a split wrote lanes 2-4
// only and left lane 1 without it until the next plan that ran (the DX010's
// cage 3, 2026-11-16: docs/drivers/breakout.md 16.10). Against the capture
// of that cage split for a CA-25G-S DAC, every interface carrying the TLV.
func TestBreakoutPutsLLDPMEDOnEveryNewInterface(t *testing.T) {
	r := rig20261106(t, fixtureSplitDAC)
	// The baseline (every port as it is, untagged in VLAN 1) takes the
	// capture's test aggregate apart; settle it first.
	base := r.baseline()
	r.apply(base)
	r.collect()
	if got, _ := r.apply(base); len(got) != 0 {
		t.Fatalf("the baseline does not settle: %q", got)
	}
	med := func(ln string) string {
		return "sudo docker exec lldp lldpcli configure ports " + ln + " lldp custom-tlv replace oui 00,12,bb subtype 1 oui-info 00,01,04"
	}
	check := func(name string, got []string, lanes ...string) {
		t.Helper()
		if !strings.Contains(strings.Join(got, "\n"), "config interface breakout") {
			t.Fatalf("%s: no breakout in %q", name, got)
		}
		for _, ln := range lanes {
			if !containsStr(got, med(ln)) {
				t.Errorf("%s: no LLDP-MED on %s in %q", name, ln, got)
			}
		}
	}

	// The join (a forced 100G from the parent's drawer, as live).
	joined := speedOf(base, 3, 100000)
	got, _ := r.apply(joined)
	check("join", got, "Ethernet8")
	r.collect()
	if again, n := r.apply(joined); len(again) != 0 || n != 0 {
		t.Errorf("join: the next apply wrote %q (%d ports)", again, n)
	}

	// Auto: the module runs only split layouts, so the driver splits it.
	got, _ = r.apply(base)
	check("split", got, "Ethernet8", "Ethernet9", "Ethernet10", "Ethernet11")
	r.collect()
	if again, n := r.apply(base); len(again) != 0 || n != 0 {
		t.Errorf("split: the next apply wrote %q (%d ports)", again, n)
	}
}
