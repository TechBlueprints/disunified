package informloop

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The DX010's fan mode through the loop, on its capture (Quiet in all three
// files): fan_mode default writes once and the collects after it write
// nothing; a bridge restart (a new collector and loop on the same switch)
// reads the mode back and writes nothing; fan_mode quiet puts 20 % back.
// The inform carries sys_fan_mode in the gateway's shape throughout.
func TestReplaySonicFanMode(t *testing.T) {
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261106-f2e25f54c", "collect-fan.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	ctx := context.Background()
	start := func(want devicemodel.FanMode) (*Loop, *device.Session) {
		t.Helper()
		coll := sonic.NewCollector(fr)
		coll.Log = logger
		snap, err := coll.Start(ctx)
		if err != nil {
			t.Fatal(err)
		}
		desc := inform.Descriptor{MAC: snap.System.MAC, Model: "USWF07D"}
		sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef"}, nil, time.Now())
		l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, CollectTimeout: 5 * time.Second, FanMode: want})
		if err != nil {
			t.Fatal(err)
		}
		return l, sess
	}
	writes := func() int { return strings.Count(strings.Join(fr.Recorded(), "\n"), "disunified-fancontrol") }

	l, sess := start(devicemodel.FanModeDefault)
	l.collect(ctx)
	if writes() != 1 || !strings.Contains(buf.String(), "fan mode quiet -> default") {
		t.Fatalf("writes %d; log: %s", writes(), buf.String())
	}
	if fm := sysFanModeOf(t, sess); fm["caps"] != float64(3) || fm["mode"] != "quiet" {
		t.Errorf("the writing cycle reports what it read: %v", fm)
	}
	l.collect(ctx)
	l.collect(ctx)
	if writes() != 1 {
		t.Fatalf("a converged switch was written again (%d writes)", writes())
	}
	if fm := sysFanModeOf(t, sess); fm["mode"] != "default" {
		t.Errorf("sys_fan_mode %v after Default", fm)
	}

	// Bridge restart, same setting: nothing to write.
	l, _ = start(devicemodel.FanModeDefault)
	l.collect(ctx)
	if writes() != 1 {
		t.Fatalf("a restarted bridge rewrote a converged switch (%d writes)", writes())
	}

	// Back to Quiet, Clint's standing choice.
	l, sess = start(devicemodel.FanModeQuiet)
	l.collect(ctx)
	l.collect(ctx)
	if writes() != 2 {
		t.Fatalf("Quiet: %d writes, want 2 in all", writes())
	}
	if fm := sysFanModeOf(t, sess); fm["mode"] != "quiet" {
		t.Errorf("sys_fan_mode %v after Quiet", fm)
	}
}
