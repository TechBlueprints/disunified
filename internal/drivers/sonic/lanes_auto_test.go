package sonic

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Automatic lane switches (docs/drivers/breakout.md 16), against the DX010
// captured on 2026-11-16 with a QSFP28-to-4xSFP28 DAC in cage 3, the cage
// split 4x25G[10G] (collect-split-dac.txt): the four lanes are admin up,
// forced 25G with fec fc, without link (nothing on the far ends), and
// xcvrd has the module under every lane's key. Cage 2 holds a 100G DAC
// that xcvrd has not read.

const fixtureSplitDAC = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/collect-split-dac.txt"

func foldOf(t *testing.T, snap *devicemodel.Snapshot, cage int) devicemodel.Port {
	t.Helper()
	i := slices.IndexFunc(snap.Ports, func(p devicemodel.Port) bool { return p.Index == cage })
	if i < 0 {
		t.Fatalf("no port %d", cage)
	}
	return snap.Ports[i]
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSplitCages(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureSplitDAC)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	if got := c.SplitCages(); got != nil {
		t.Errorf("before the first snapshot: %v, want nil", got)
	}
	r := rig20261106(t, fixtureSplitDAC)
	if got := r.c.SplitCages(); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("split cages %v, want [3]", got)
	}
	// The 2026-11-07 capture's split cage is 2.
	if got := rig20261106(t, fixture20261106Split).c.SplitCages(); !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("split cages %v, want [2]", got)
	}
}

// Detach is the exact inverse of attach: the cage's report and the plan for
// it are, after a detach, what they were before the attach.
func TestDetachLaneSwitchIsTheInverseOfAttach(t *testing.T) {
	ctx := context.Background()
	r := rig20261106(t, fixtureSplitDAC)
	collect := func() devicemodel.Port {
		snap, err := r.c.Collect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return foldOf(t, snap, 3)
	}
	// The cage's push: a name on it. Unattached the plan puts it on every
	// lane; attached, the lanes are the lane switch's and nothing changes.
	cage := collect()
	plan := func() []int {
		return r.c.PlanPorts([]devicemodel.PortDesired{{Index: 3, Enabled: cage.Enabled, Description: "to-the-rack", VLANSet: true, NativeVLAN: 1}})
	}
	if got := plan(); !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("unattached: the plan changes %v, want [3]", got)
	}

	before := collect()
	if before.Media != "SFP28" || before.Neighbor != nil {
		t.Fatalf("the fold: media %q neighbour %+v, want SFP28 and none", before.Media, before.Neighbor)
	}
	if v, err := r.c.Lanes(3); err != nil || v.Child.MAC != "" || len(v.Lanes) != 4 {
		t.Fatalf("Lanes unattached: %d lanes, child %+v, %v", len(v.Lanes), v.Child, err)
	}

	if err := r.c.AttachLaneSwitch(3, devicemodel.Attachment{MAC: laneMAC, Name: "sw-cage3"}); err != nil {
		t.Fatal(err)
	}
	owned := collect()
	if owned.Media != "QSFP28" || owned.Neighbor == nil || owned.Neighbor.ChassisID != laneMAC {
		t.Errorf("owned: media %q neighbour %+v, want QSFP28 and the child", owned.Media, owned.Neighbor)
	}
	if got := plan(); len(got) != 0 {
		t.Errorf("owned cage: the device's plan changes %v, want nothing (the lanes are the lane switch's)", got)
	}

	r.c.DetachLaneSwitch(3)
	after := collect()
	if a, b := asJSON(t, before), asJSON(t, after); a != b {
		t.Errorf("the cage after a detach is not the fold it was:\n before %s\n after  %s", a, b)
	}
	if v, err := r.c.Lanes(3); err != nil || v.Child.MAC != "" {
		t.Errorf("Lanes after a detach: child %+v, %v", v.Child, err)
	}
	if got := plan(); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("after a detach: the plan changes %v, want [3] (the cage's own settings go back on the lanes)", got)
	}
	// Detaching again, or a cage that never had one, does nothing.
	r.c.DetachLaneSwitch(3)
	r.c.DetachLaneSwitch(7)
}
