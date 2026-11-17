package aristaeos

import (
	"context"
	"reflect"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/breakout"
)

// The first-push hold plans before EnsureVLANs has run
// (docs/drivers/breakout.md §16.10). On SONiC that hid a push whose only
// change was tagging VLANs the switch did not have yet; on EOS UniFi's
// Allow All is the trunk's own mode (`switchport trunk allowed vlan all`),
// diffed against the port's mode and never against the switch's VLANs, so
// the plan sees the change whatever VLANs exist, and the driver needs no
// VLANPlanner. The lane switch over this parent plans with PlanLanes
// (breakout.Device.PlanPortsWithVLANs falls back to it). On the capture
// (split-cage54) every lane is an Allow All trunk already; lane 1 is made
// an access port in VLAN 1 (Block All) first, as a seeded lane switch
// would leave it.
func TestLanePlanSeesAllowAllWhateverVLANsExist(t *testing.T) {
	c, ft, _ := splitCollector(t, "split-cage54", true)
	ctx := context.Background()
	allowAll := func() []devicemodel.PortDesired {
		var ds []devicemodel.PortDesired
		for i := 1; i <= 4; i++ {
			ds = append(ds, devicemodel.PortDesired{Index: i, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true})
		}
		return ds
	}
	if got := c.PlanLanes(54, allowAll()); len(got) != 0 {
		t.Fatalf("Allow All on the captured lanes (Allow All trunks) plans %v, want none", got)
	}
	block := allowAll()
	block[0].TaggedAll = false
	if _, err := c.ApplyLanes(ctx, 54, block); err != nil {
		t.Fatal(err)
	}
	if got, want := lastBatch(t, ft), "enable | configure | interface Ethernet54/1 | switchport mode access | switchport access vlan 1 | end | write memory"; got != want {
		t.Fatalf("Block All on lane 1:\n%s\nwant\n%s", got, want)
	}
	if _, err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if v := lanePorts(t, c, 54).Lanes[0].VLAN; v.Mode != "access" || v.NativeVLAN != 1 {
		t.Fatalf("lane 1 after Block All: %+v", v)
	}

	o, err := breakout.ParseOptions(map[string]string{"parent": "eos", "cage": "54", "mac": childMAC, "name": "lane-switch"})
	if err != nil {
		t.Fatal(err)
	}
	lanes := breakout.New(o, c, c)
	if _, err := lanes.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// A VLAN the switch does not have (4040) in the push's list changes
	// nothing in the plan: the lane's mode is what differs.
	ids := []int{1, 2, 8, 9, 10, 69, 1000, 2000, 3000, 4000, 4010, 4040}
	for name, got := range map[string][]int{
		"PlanLanes":                     c.PlanLanes(54, allowAll()),
		"PlanPorts (lane switch)":       lanes.PlanPorts(allowAll()),
		"PlanPortsWithVLANs (lane sw.)": lanes.PlanPortsWithVLANs(allowAll(), ids),
	} {
		if !reflect.DeepEqual(got, []int{1}) {
			t.Errorf("%s: the unseeded push plans %v, want [1]", name, got)
		}
	}
	if got := lanes.PlanPortsWithVLANs(block, ids); len(got) != 0 {
		t.Errorf("the seeded push plans %v, want none", got)
	}
}
