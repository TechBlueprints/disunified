package sonic

import (
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Speed "auto" follows the module (docs/drivers/sonic.md §10.9), on the
// DX010's capture: cage 1 holds a 100G CWDM4 (Ethernet0), the other 31
// are empty, platform.json offers 1x100G[40G], 2x50G and 4x25G[10G].

// withModule puts another compliance code in place of the capture's CWDM4,
// in the capture's own compliance string (sonic-platform-common 202505's
// names): the same module record, as xcvrd would print it for that code.
func withModule(name string) func(*FixtureRunner) {
	return func(fr *FixtureRunner) {
		_ = fr.update("xcvr", func(t table) error {
			x := t["TRANSCEIVER_INFO|Ethernet0"]
			x["specification_compliance"] = strings.Replace(x["specification_compliance"], "'100G CWDM4'", "'"+name+"'", 1)
			return nil
		})
	}
}

func speedOf(ds []devicemodel.PortDesired, idx, mbps int) []devicemodel.PortDesired {
	return edit(ds, idx, func(d *devicemodel.PortDesired) { d.SpeedMbps = mbps })
}

// The night's case with a module in the cage: split to 4x25G through
// UniFi, then back to auto. A CWDM4 runs 1x100G only, so the cage joins --
// the driver's own write, which the hold's plan does not see -- and the
// rejoined port gets the cage's intent and the module's RS.
func TestSpeedAutoJoinsASplitTheModuleCannotRun(t *testing.T) {
	r := newHardwareRig(t, nil)
	base := r.baseline()
	r.expect("split", speedOf(base, 1, 25000), []string{
		"sudo config interface breakout -f -y Ethernet0 '4x25G[10G]' </dev/null",
		"sudo config interface startup Ethernet0", "sudo config interface startup Ethernet1",
		"sudo config interface startup Ethernet2", "sudo config interface startup Ethernet3",
		"sudo config vlan member add -u 4094 Ethernet0", "sudo config vlan member add -u 4094 Ethernet1",
		"sudo config vlan member add -u 4094 Ethernet2", "sudo config vlan member add -u 4094 Ethernet3",
		save,
	})
	if got := r.c.PlanPorts(base); len(got) != 0 {
		t.Errorf("PlanPorts(auto) = %v: the module's join is the driver's own", got)
	}
	snap := r.expect("auto", base, []string{
		"sudo config interface breakout -f -y Ethernet0 '1x100G[40G]' </dev/null",
		"sudo config interface startup Ethernet0",
		"sudo config interface fec Ethernet0 rs",
		"sudo config vlan member add -u 4094 Ethernet0",
		save,
	})
	if p := port(snap, 1); len(p.Interfaces) != 1 || p.AdminSpeed != 100000 || p.FECConfig != devicemodel.FECRS {
		t.Errorf("cage 1 after: interfaces %v speed %d FEC %q", p.Interfaces, p.AdminSpeed, p.FECConfig)
	}
}

// A forced 40G (the command verified live on the DX010) back to auto: the
// CWDM4's 100G, by lane speed, not counted by the hold's plan; the
// module's FEC follows the speed (none at 40G, RS at 100G, §10.3).
func TestSpeedAutoRestoresTheModulesSpeed(t *testing.T) {
	r := newHardwareRig(t, nil)
	base := r.baseline()
	r.expect("40G", speedOf(base, 1, 40000), []string{"sudo config interface speed Ethernet0 40000", "sudo config interface fec Ethernet0 none", save})
	if got := r.c.PlanPorts(base); len(got) != 0 {
		t.Errorf("PlanPorts(auto) = %v", got)
	}
	r.expect("auto", base, []string{"sudo config interface speed Ethernet0 100000", "sudo config interface fec Ethernet0 rs", save})
}

// A cable that can be a 100G cable or a 4x25G breakout keeps a split made
// for it: auto changes nothing.
func TestSpeedAutoKeepsASplitTheModuleRuns(t *testing.T) {
	r := newHardwareRig(t, withModule("100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS"))
	base := r.baseline()
	got, _ := r.apply(speedOf(base, 1, 25000))
	if len(got) == 0 || !strings.Contains(got[0], "'4x25G[10G]'") {
		t.Fatalf("split: %q", got)
	}
	r.system2()
	r.expect("auto", base, nil)
}

// A 25G-only code in a QSFP28 cage is a breakout cable: auto splits the
// cage for it (the driver's own write). Its lanes get CL74, which the old
// 100G interface's supported_fecs (none,rs) no longer speaks for.
func TestSpeedAutoSplitsForABreakoutCable(t *testing.T) {
	r := newHardwareRig(t, withModule("25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R"))
	base := r.baseline()
	if got := r.c.PlanPorts(base); len(got) != 0 {
		t.Errorf("PlanPorts(auto) = %v", got)
	}
	got, n := r.apply(base)
	if n != 1 || len(got) == 0 || got[0] != "sudo config interface breakout -f -y Ethernet0 '4x25G[10G]' </dev/null" || got[len(got)-1] != save {
		t.Fatalf("auto with a breakout cable (%d ports): %q", n, got)
	}
	for _, ln := range []string{"Ethernet0", "Ethernet1", "Ethernet2", "Ethernet3"} {
		for _, c := range []string{"sudo config interface startup " + ln, "sudo config interface fec " + ln + " fc", "sudo config vlan member add -u 4094 " + ln} {
			if !containsStr(got, c) {
				t.Errorf("missing %q in %q", c, got)
			}
		}
	}
}

// The night's case as it was, an empty cage: no module, no opinion -- the
// split stays (the rule from before; a manual 100G joins it).
func TestSpeedAutoLeavesAnEmptySplitCage(t *testing.T) {
	r := newHardwareRig(t, nil)
	base := r.baseline()
	got, _ := r.apply(speedOf(base, 2, 25000))
	if len(got) == 0 || got[0] != "sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null" {
		t.Fatalf("split: %q", got)
	}
	r.system2()
	r.expect("auto", base, nil)
	r.expect("manual 100G", speedOf(base, 2, 100000), []string{
		"sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null",
		"sudo config interface startup Ethernet4",
		"sudo config vlan member add -u 4094 Ethernet4",
		save,
	})
}

// A module the table does not know: logged, and the cage keeps the rule
// for an empty cage (an unsplit one at its native speed, which the hold
// counts as before).
func TestSpeedAutoUnknownModule(t *testing.T) {
	r := newHardwareRig(t, withModule("10 Mb/s Single Pair Ethernet"))
	base := r.baseline()
	r.expect("40G", speedOf(base, 1, 40000), []string{"sudo config interface speed Ethernet0 40000", "sudo config interface fec Ethernet0 none", save})
	if got := r.c.PlanPorts(base); !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("PlanPorts(auto) = %v, want [1] (the old rule's write)", got)
	}
	got, _ := r.apply(base)
	if !containsStr(got, "sudo config interface speed Ethernet0 100000") {
		t.Errorf("auto: %q", got)
	}
	if !strings.Contains(r.log.String(), `the module "10 Mb/s Single Pair Ethernet" is not in the speed table`) {
		t.Errorf("no note: %s", r.log.String())
	}
}

func TestModuleRunsTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []laneCfg
	}{
		{"100G CWDM4", []laneCfg{cfg1x100}},
		{"100GBASE-SR4 or 25GBASE-SR", []laneCfg{cfg1x100, cfg4x25}},
		{"100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS", []laneCfg{cfg1x100, cfg4x25, cfg2x50}},
		{"25GBASE-CR CA-25G-N or 50GBASE-CR2 with no FEC", []laneCfg{cfg4x25, cfg2x50}},
		{"40GBASE-LR4", []laneCfg{cfg1x40}},
		{"40GBASE-CR4", []laneCfg{cfg1x40, cfg4x10}},
		{"4 x 10GBASE-SR", []laneCfg{cfg4x10}},
	} {
		got, ok := moduleRuns(tc.name)
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("moduleRuns(%q) = %v %v, want %v", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := moduleRuns("QSFP28 or later"); ok {
		t.Error("a form factor is not a module the table knows")
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
