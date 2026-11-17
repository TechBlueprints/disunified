package sonic

import (
	"context"
	"reflect"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The first-push hold plans before EnsureVLANs has run
// (docs/drivers/breakout.md §16.10): PlanPortsWithVLANs and
// PlanLanesWithVLANs plan with the push's VLANs as if they existed. The
// capture is the DX010 with cage 3 split (collect-split-dac.txt), which has
// no VLAN but the native stand-in Vlan4094: every port and lane is untagged
// in it and nothing else, as on adoption day, when the site's ten networks
// had been pruned from the switch. The VLANs are the ten the lane switch's
// first push named (controller-11.0.99/replies-lane-switch-dx010-cage3.ndjson).

var prunedSiteVLANs = []int{1, 2, 8, 9, 10, 69, 1000, 2000, 3000, 4000, 4010}

// allowAll is the controller's default for a port it has no override for.
func allowAll(idxs ...int) []devicemodel.PortDesired {
	var ds []devicemodel.PortDesired
	for _, i := range idxs {
		ds = append(ds, devicemodel.PortDesired{Index: i, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true})
	}
	return ds
}

// blockAll is the seeded push for a port untagged in VLAN 1 alone.
func blockAll(idxs ...int) []devicemodel.PortDesired {
	var ds []devicemodel.PortDesired
	for _, i := range idxs {
		ds = append(ds, devicemodel.PortDesired{Index: i, Enabled: true, VLANSet: true, NativeVLAN: 1})
	}
	return ds
}

func TestPlanWithThePushsVLANs(t *testing.T) {
	r := rig20261106(t, fixtureSplitDAC)
	// The device: ports 6 and 7, admin up, untagged in the stand-in alone
	// (reported as VLAN 1).
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{6, 7} {
		if p := foldOf(t, snap, i); !p.Enabled || p.VLAN.Mode != "access" || p.VLAN.NativeVLAN != 1 || len(p.VLAN.Allowed) != 0 {
			t.Fatalf("port %d is not as the test expects: %+v", i, p.VLAN)
		}
	}
	// Planned against the VLANs the switch has, Allow All is no change:
	// the gap of §16.10.
	if got := r.c.PlanPorts(allowAll(6, 7)); len(got) != 0 {
		t.Errorf("PlanPorts over the existing VLANs = %v; the test's premise is that it sees none", got)
	}
	if got := r.c.PlanPortsWithVLANs(allowAll(6, 7), prunedSiteVLANs); !reflect.DeepEqual(got, []int{6, 7}) {
		t.Errorf("PlanPortsWithVLANs(Allow All) = %v, want [6 7]", got)
	}
	if got := r.c.PlanPortsWithVLANs(blockAll(6, 7), prunedSiteVLANs); len(got) != 0 {
		t.Errorf("PlanPortsWithVLANs(the seeded push) = %v, want none", got)
	}
	// A push that names VLAN 1 alone is a list too: Allow All tags nothing.
	if got := r.c.PlanPortsWithVLANs(allowAll(6, 7), nil); len(got) != 0 {
		t.Errorf("PlanPortsWithVLANs(no VLAN but 1) = %v, want none", got)
	}

	// The lanes of cage 3, with a lane switch attached.
	if err := r.c.AttachLaneSwitch(3, devicemodel.Attachment{MAC: laneMAC, Name: "lane-switch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.c.PlanLanes(3, allowAll(1, 2, 3, 4)); len(got) != 0 {
		t.Errorf("PlanLanes over the existing VLANs = %v; the test's premise is that it sees none", got)
	}
	if got := r.c.PlanLanesWithVLANs(3, allowAll(1, 2, 3, 4), prunedSiteVLANs); !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
		t.Errorf("PlanLanesWithVLANs(Allow All) = %v, want [1 2 3 4]", got)
	}
	if got := r.c.PlanLanesWithVLANs(3, blockAll(1, 2, 3, 4), prunedSiteVLANs); len(got) != 0 {
		t.Errorf("PlanLanesWithVLANs(the seeded push) = %v, want none", got)
	}
	// Planning creates nothing and leaves the collector's state alone.
	if n := len(r.fr.Commands); n != 0 {
		t.Errorf("planning wrote %q", r.fr.Commands)
	}
	if got := r.c.PlanLanes(3, allowAll(1, 2, 3, 4)); len(got) != 0 {
		t.Errorf("PlanLanes after the planning = %v: the plan's VLANs leaked into the state", got)
	}
}
