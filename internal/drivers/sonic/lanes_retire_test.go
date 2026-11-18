package sonic

import (
	"context"
	"testing"
)

// CageLanes, the read a lane switch's retire rests on
// (docs/drivers/breakout.md 16.13), on the DX010 captured with cage 3 split
// 4x25G[10G] (collect-split-dac.txt): split, joined through this device's
// own plan, and never "whole" on a read it cannot vouch for.
func TestCageLanesSplitJoinedAndUnknown(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureSplitDAC)
	if err != nil {
		t.Fatal(err)
	}
	if n, at := NewCollector(fr).CageLanes(3); n != 0 || !at.IsZero() {
		t.Errorf("before the first snapshot: %d at %v, want 0 and no time", n, at)
	}
	r := rig20261106(t, fixtureSplitDAC)
	n, split := r.c.CageLanes(3)
	if n != 4 || split.IsZero() {
		t.Fatalf("cage 3: %d lanes at %v, want 4", n, split)
	}
	if n, _ := r.c.CageLanes(7); n != 1 {
		t.Errorf("cage 7 (whole in the capture): %d, want 1", n)
	}
	if n, at := r.c.CageLanes(99); n != 0 || at.IsZero() {
		t.Errorf("no port 99: %d at %v, want 0 with the snapshot's time", n, at)
	}

	// The join, a forced 100G through this device's own plan: until the
	// next collect the read is the collect's (split, the same time), so the
	// plan's own batch is never a joined read.
	base := r.baseline()
	_, split = r.c.CageLanes(3)
	if _, changed := r.apply(speedOn(base, 3, 100000)); changed == 0 {
		t.Fatal("the join wrote nothing")
	}
	if n, at := r.c.CageLanes(3); n != 4 || !at.Equal(split) {
		t.Errorf("after the join's batch, before a collect: %d at %v, want the collect's 4 at %v", n, at, split)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, joined := r.c.CageLanes(3)
	if n != 1 || !joined.After(split) {
		t.Errorf("after the collect: %d at %v, want 1, later than %v", n, joined, split)
	}
	if got := r.c.SplitCages(); len(got) != 0 {
		t.Errorf("split cages after the join: %v", got)
	}
}

// A collect taken while the switch is still starting (a swss restart, a
// boot's early state: docs/drivers/sonic.md 6.4.2) says nothing about a
// cage, though CONFIG_DB already has every port.
func TestCageLanesWhileStarting(t *testing.T) {
	c, br, _ := bootCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.CageLanes(1); n != 1 {
		t.Fatalf("started: cage 1 reads %d, want 1", n)
	}
	withoutPortInit(t, br.FixtureRunner)
	br.boot = append(br.boot, br.FixtureRunner.Fixture)
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, at := c.CageLanes(1); n != 0 || at.IsZero() {
		t.Errorf("while starting: cage 1 reads %d at %v, want 0 with the snapshot's time", n, at)
	}
}
