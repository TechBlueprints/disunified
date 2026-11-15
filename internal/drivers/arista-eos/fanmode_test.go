package aristaeos

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The 4.26.14M capture is the switch under auto: both modes offered, the
// switch in Default (overrideFanSpeed 0).
func TestFanModeFromCapture(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(snap.System.FanModes, []devicemodel.FanMode{devicemodel.FanModeDefault, devicemodel.FanModeQuiet}) {
		t.Errorf("fan modes = %v", snap.System.FanModes)
	}
	if snap.System.FanMode != devicemodel.FanModeDefault {
		t.Errorf("fan mode = %q, want default", snap.System.FanMode)
	}
}

func TestFanModeCommands(t *testing.T) {
	const d, q = devicemodel.FanModeDefault, devicemodel.FanModeQuiet
	for _, tc := range []struct {
		have, want devicemodel.FanMode
		cmds       []string
	}{
		{d, d, nil},
		{d, q, []string{"environment fan-speed override 30"}},
		{q, q, nil},
		{q, d, []string{"no environment fan-speed"}},
		// An override set by hand at another speed reads as neither mode
		// and is replaced by either one.
		{"", q, []string{"environment fan-speed override 30"}},
		{"", d, []string{"no environment fan-speed"}},
	} {
		got, err := fanModeCommands(tc.have, tc.want)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.cmds) {
			t.Errorf("%q -> %q: %q, want %q", tc.have, tc.want, got, tc.cmds)
		}
	}
	if _, err := fanModeCommands("", "silent"); err == nil {
		t.Error("an unknown mode was accepted")
	}
	if _, err := fanModeCommands("silent", "silent"); err == nil {
		t.Error("an unknown mode was accepted when have == want")
	}
}

// Quiet on the captured (auto) switch is one batch ending in write memory;
// Quiet again writes nothing; Default puts auto back; an unknown mode is
// refused without a write.
func TestApplyFanMode(t *testing.T) {
	c, ft := newFixtureCollector(t)
	ctx := context.Background()
	if _, err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyFanMode(ctx, devicemodel.FanModeDefault); err != nil || len(ft.Configured) != 0 {
		t.Fatalf("default on an auto switch: err %v, wrote %q", err, ft.Configured)
	}
	if err := c.ApplyFanMode(ctx, devicemodel.FanModeQuiet); err != nil {
		t.Fatal(err)
	}
	want := []string{"enable", "configure", "environment fan-speed override 30", "end", "write memory"}
	if len(ft.Configured) != 1 || !reflect.DeepEqual(ft.Configured[0], want) {
		t.Fatalf("quiet wrote %q, want %q", ft.Configured, want)
	}
	if err := c.ApplyFanMode(ctx, devicemodel.FanModeQuiet); err != nil || len(ft.Configured) != 1 {
		t.Fatalf("quiet twice: err %v, batches %d", err, len(ft.Configured))
	}
	if err := c.ApplyFanMode(ctx, devicemodel.FanModeDefault); err != nil {
		t.Fatal(err)
	}
	want = []string{"enable", "configure", "no environment fan-speed", "end", "write memory"}
	if len(ft.Configured) != 2 || !reflect.DeepEqual(ft.Configured[1], want) {
		t.Fatalf("default wrote %q, want %q", ft.Configured, want)
	}
	if err := c.ApplyFanMode(ctx, "silent"); err == nil || len(ft.Configured) != 2 {
		t.Fatalf("unknown mode: err %v, batches %d", err, len(ft.Configured))
	}
}
