package sonic

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The DX010 on the kit's image, captured 2026-11-07 with the fancontrol
// section: F2B airflow, all three files at the Quiet floor (patch 0012).
const fixtureFan = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-fan.txt"

var both = []devicemodel.FanMode{devicemodel.FanModeDefault, devicemodel.FanModeQuiet}

func fanCollect(t *testing.T, r *controlRig) devicemodel.System {
	t.Helper()
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap.System
}

// The capture reads as Quiet with both modes offered.
func TestFanModeReadBack(t *testing.T) {
	r := rig20261106(t, fixtureFan)
	sys := fanCollect(t, r)
	if !slices.Equal(sys.FanModes, both) || sys.FanMode != devicemodel.FanModeQuiet {
		t.Fatalf("FanModes %v FanMode %q, want %v quiet", sys.FanModes, sys.FanMode, both)
	}
	if strings.Contains(r.log.String(), "fan mode not offered") {
		t.Errorf("log: %s", r.log)
	}
}

// Default writes the stock floor to the direction file, copies it over
// fancontrol and into pmon and restarts only fancontrol; the next collect
// reads Default from all three. Quiet puts 0012's floor back. Neither
// touches MINSTART, MAXTEMP or MAXPWM.
func TestFanModeApply(t *testing.T) {
	r := rig20261106(t, fixtureFan)
	fanCollect(t, r)
	ctx := context.Background()
	if err := r.c.ApplyFanMode(ctx, devicemodel.FanModeDefault); err != nil {
		t.Fatal(err)
	}
	if got := r.fr.Recorded(); len(got) != 1 || got[0] != fanCommand {
		t.Fatalf("commands %q, want one %q", got, fanCommand)
	}
	script := r.fr.FanScripts[0]
	for _, want := range []string{
		`d=/usr/share/sonic/device/x86_64-cel_seastone-r0`,
		`src="$d/fancontrol-F2B"`,
		`vals = {"MINPWM":"89","MINSTOP":"89","MINTEMP":"26"}`,
		`sudo cp "$src" "$d/fancontrol"`,
		`docker cp "$d/fancontrol" pmon:/etc/fancontrol`,
		`docker exec pmon supervisorctl restart fancontrol`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "restart pmon") || strings.Contains(script, "MINSTART") || strings.Contains(script, "MAXPWM") {
		t.Errorf("script touches more than the floor:\n%s", script)
	}
	sys := fanCollect(t, r)
	if sys.FanMode != devicemodel.FanModeDefault {
		t.Fatalf("after Default the switch reads %q", sys.FanMode)
	}
	sec := sections(r.fr.Fixture)["fancontrol"]
	if !strings.Contains(sec, `"MINSTART": "89"`) || !strings.Contains(sec, `"MAXPWM": "255"`) {
		t.Errorf("the rest of the table changed: %s", sec)
	}
	if err := r.c.ApplyFanMode(ctx, devicemodel.FanModeQuiet); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.fr.FanScripts[1], `vals = {"MINPWM":"51","MINSTOP":"51","MINTEMP":"35"}`) {
		t.Errorf("Quiet script: %s", r.fr.FanScripts[1])
	}
	if sys := fanCollect(t, r); sys.FanMode != devicemodel.FanModeQuiet {
		t.Fatalf("after Quiet the switch reads %q", sys.FanMode)
	}
}

// A file out of step -- pmon running a hand-set 25 % floor, or the boot
// file left at stock while fancontrol is quiet -- reads as no mode, so the
// loop writes the configured one again.
func TestFanModeOutOfStepReadsAsNone(t *testing.T) {
	for name, re := range map[string]*regexp.Regexp{
		"pmon at 25 %":       regexp.MustCompile(`("pmon": \{[^}]*"MINPWM": ")51`),
		"boot file at stock": regexp.MustCompile(`("fancontrol-F2B": \{[^}]*"MINPWM": ")51`),
	} {
		r := rig20261106(t, fixtureFan)
		if !re.MatchString(r.fr.Fixture) {
			t.Fatalf("%s: the capture no longer has the line", name)
		}
		r.fr.Fixture = re.ReplaceAllString(r.fr.Fixture, "${1}64")
		if sys := fanCollect(t, r); sys.FanMode != "" || !slices.Equal(sys.FanModes, both) {
			t.Errorf("%s: FanModes %v FanMode %q, want both offered and no mode", name, sys.FanModes, sys.FanMode)
		}
	}
}

// A B2F unit (nothing measured on one), the VS and an older capture without
// the section offer nothing; the B2F unit says why once.
func TestFanModeNotOffered(t *testing.T) {
	r := rig20261106(t, fixtureFan)
	r.fr.Fixture = strings.NewReplacer(`"airflow": "F2B"`, `"airflow": "B2F"`, `"fancontrol-F2B":`, `"fancontrol-B2F":`).Replace(r.fr.Fixture)
	for i := 0; i < 2; i++ {
		if sys := fanCollect(t, r); sys.FanModes != nil || sys.FanMode != "" {
			t.Fatalf("B2F: FanModes %v FanMode %q", sys.FanModes, sys.FanMode)
		}
	}
	if n := strings.Count(r.log.String(), "fan mode not offered: no measured fan floors for B2F airflow on x86_64-cel_seastone-r0"); n != 1 {
		t.Errorf("B2F refusal logged %d times: %s", n, r.log)
	}
	if err := r.c.ApplyFanMode(context.Background(), devicemodel.FanModeQuiet); err == nil || len(r.fr.Recorded()) != 0 {
		t.Errorf("ApplyFanMode on B2F: err %v, commands %q", err, r.fr.Recorded())
	}
	for _, path := range []string{fixture, fixture20261106} {
		r := rig20261106(t, path)
		if sys := fanCollect(t, r); sys.FanModes != nil || sys.FanMode != "" {
			t.Errorf("%s: FanModes %v FanMode %q", path, sys.FanModes, sys.FanMode)
		}
		if strings.Contains(r.log.String(), "fan mode") {
			t.Errorf("%s: a switch without the section logs nothing: %s", path, r.log)
		}
	}
}
