package informloop

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
)

// fan_mode on the Arista driver end to end, over the 4.26.14M capture (the
// switch under auto): the inform offers both modes and reports Default;
// fan_mode default writes nothing; fan_mode quiet writes the 30 % override
// in one batch ending in write memory.
func TestFanModeArista(t *testing.T) {
	ctx := context.Background()
	start := func(t *testing.T) (*aristaeos.Collector, *aristaeos.FixtureTransport) {
		t.Helper()
		ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
		coll := aristaeos.NewCollector(ft)
		if _, err := coll.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return coll, ft
	}

	coll, ft := start(t)
	l, sess, _ := fanLoop(t, coll, devicemodel.FanModeDefault)
	l.collect(ctx)
	if len(ft.Configured) != 0 {
		t.Errorf("fan_mode default on an auto switch wrote %q", ft.Configured)
	}
	if fm := sysFanModeOf(t, sess); fm["caps"] != float64(3) || fm["mode"] != "default" {
		t.Errorf("sys_fan_mode %v, want caps 3 mode default", fm)
	}

	coll, ft = start(t)
	l, _, buf := fanLoop(t, coll, devicemodel.FanModeQuiet)
	l.collect(ctx)
	want := [][]string{{"enable", "configure", "environment fan-speed override 30", "end", "write memory"}}
	if !reflect.DeepEqual(ft.Configured, want) {
		t.Errorf("fan_mode quiet wrote %q, want %q (log: %s)", ft.Configured, want, buf)
	}
}
