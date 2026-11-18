package aristaeos

import (
	"context"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// CageLanes, the read a lane switch's retire rests on
// (docs/drivers/breakout.md 16.13), on the 7160 captured with cage 50 split
// by the module rule and joined by hand (module-layout-20261117/): split,
// joined through this device's own plan, and never "whole" before the next
// collect has read the join back.
func TestCageLanesSplitThenJoined(t *testing.T) {
	if n, at := NewCollector(nil).CageLanes(50); n != 0 || !at.IsZero() {
		t.Errorf("before the first snapshot: %d at %v", n, at)
	}
	c, ft, _, snap := layoutCollector(t, "split-by-rule")
	n, split := c.CageLanes(50)
	if n != 4 || split.IsZero() {
		t.Fatalf("cage 50 split: %d at %v, want 4", n, split)
	}
	if n, _ := c.CageLanes(49); n != 1 {
		t.Errorf("cage 49 (whole): %d, want 1", n)
	}
	if n, _ := c.CageLanes(99); n != 0 {
		t.Errorf("no port 99: %d, want 0", n)
	}
	if err := c.AttachLaneSwitch(50, devicemodel.Attachment{MAC: childMAC, Name: "sw-cage50"}); err != nil {
		t.Fatal(err)
	}
	// The join: a forced 100G on the cage through this device's own plan.
	ds := auto(snap.Ports)
	for i := range ds {
		if ds[i].Index == 50 {
			ds[i].SpeedMbps = 100000
		}
	}
	if n, err := c.ApplyPorts(context.Background(), ds); err != nil || n == 0 {
		t.Fatalf("the join: n=%d err=%v", n, err)
	}
	if n, at := c.CageLanes(50); n == 1 {
		t.Errorf("after the join's batch, before a collect: reads whole (%d at %v); only a collect may say so", n, at)
	}
	ft.Dir = layoutDir("joined")
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, joined := c.CageLanes(50)
	if n != 1 || !joined.After(split) {
		t.Errorf("after the collect: %d at %v, want 1, later than %v", n, joined, split)
	}
}
