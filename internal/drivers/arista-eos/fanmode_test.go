package aristaeos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The 4.26.14M capture is the switch under auto: Default, no override.
func TestFanModeFromCapture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M", "show-system-environment-cooling.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c showSystemEnvCooling
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.OverrideFanSpeed != 0 {
		t.Fatalf("overrideFanSpeed = %d under auto", c.OverrideFanSpeed)
	}
	if got := fanModeOf(c); got != fanModeDefault {
		t.Errorf("mode = %q, want default", got)
	}
}

func TestFanModeCommands(t *testing.T) {
	for _, tc := range []struct {
		override int
		want     string
		cmds     []string
	}{
		{0, fanModeDefault, nil},
		{0, fanModeQuiet, []string{"environment fan-speed override 30"}},
		{30, fanModeQuiet, nil},
		{30, fanModeDefault, []string{"no environment fan-speed"}},
		// An override set by hand at another speed is replaced by either mode.
		{60, fanModeQuiet, []string{"environment fan-speed override 30"}},
		{60, fanModeDefault, []string{"no environment fan-speed"}},
	} {
		got, err := fanModeCommands(tc.override, tc.want)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.cmds) {
			t.Errorf("override %d -> %s: %q, want %q", tc.override, tc.want, got, tc.cmds)
		}
	}
	if _, err := fanModeCommands(0, "silent"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
