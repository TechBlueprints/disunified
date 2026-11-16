package aristaeos

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Automatic lane switches (docs/drivers/breakout.md 16) on the captured
// 7160 with cage 54 split (split-cage54/).

func TestSplitCages(t *testing.T) {
	c, _, _ := splitCollector(t, "split-cage54", false)
	got := c.SplitCages()
	if !slices.Contains(got, 54) || !slices.IsSorted(got) {
		t.Errorf("split cages %v: want 54 among them, ascending", got)
	}
	for _, cage := range got {
		if _, err := c.Lanes(cage); err != nil {
			t.Errorf("cage %d is listed as split but Lanes says %v", cage, err)
		}
	}
	if got := NewCollector(nil).SplitCages(); got != nil {
		t.Errorf("before the first snapshot: %v, want nil", got)
	}
}

// Detach is the exact inverse of attach: the cage's report and the plan for
// it are, after a detach, what they were before the attach.
func TestDetachLaneSwitchIsTheInverseOfAttach(t *testing.T) {
	ctx := context.Background()
	c, _, _ := splitCollector(t, "split-cage54", false)
	fold := func() string {
		snap, err := c.Collect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(snap.Ports, func(p devicemodel.Port) bool { return p.Index == 54 })
		p := snap.Ports[i]
		b, err := json.MarshalIndent(p, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	plan := func() []int {
		snap, err := c.Collect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ds := desiredFrom(snap.Ports)
		for i := range ds {
			if ds[i].Index == 54 {
				ds[i].Description = "to-the-rack"
			}
		}
		return c.PlanPorts(ds)
	}

	before := fold()
	if got := plan(); !reflect.DeepEqual(got, []int{54}) {
		t.Fatalf("unattached: the plan changes %v, want [54]", got)
	}
	if err := c.AttachLaneSwitch(54, devicemodel.Attachment{MAC: childMAC, Name: "sw-cage54"}); err != nil {
		t.Fatal(err)
	}
	if owned := fold(); owned == before {
		t.Error("attached: the cage reports as the fold it was (no child reported)")
	}
	if got := plan(); len(got) != 0 {
		t.Errorf("owned cage: the device's plan changes %v, want nothing (the lanes are the lane switch's)", got)
	}
	c.DetachLaneSwitch(54)
	if after := fold(); after != before {
		t.Errorf("the cage after a detach is not the fold it was:\n before %s\n after  %s", before, after)
	}
	if v, err := c.Lanes(54); err != nil || v.Child.MAC != "" {
		t.Errorf("Lanes after a detach: child %+v, %v", v.Child, err)
	}
	if got := plan(); !reflect.DeepEqual(got, []int{54}) {
		t.Errorf("after a detach: the plan changes %v, want [54]", got)
	}
	c.DetachLaneSwitch(54)
	c.DetachLaneSwitch(7)
}
