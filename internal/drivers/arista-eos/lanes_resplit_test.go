package aristaeos

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// A lane switch moves its cage between 4x25G and 2x50G by a lane's forced
// speed (docs/drivers/breakout.md 16.15), over the 7160's own states of
// cage 50 (module-layout-20261117/: split-by-rule is 4x25G, split-2x50 the
// cage after `speed forced 50gfull` on /1 and /3; the cage holds a CA-25G-S
// DAC, which runs both). The batch is the captured commands, the switch's
// read after it is the next state's capture.
func TestLaneResplitBetweenSplitModes(t *testing.T) {
	ctx := context.Background()
	c, ft, buf, snap := layoutCollector(t, "split-by-rule")
	if err := c.AttachLaneSwitch(50, devicemodel.Attachment{MAC: childMAC, Name: "sw-cage50"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	v := lanePorts(t, c, 50)
	for _, l := range v.Lanes {
		if !slices.Equal(l.SpeedCaps, []int{25000, 50000}) || l.Media != devicemodel.MediaSFP28 {
			t.Errorf("4x25G lane %d: caps %v media %s", l.Index, l.SpeedCaps, l.Media)
		}
	}
	push := func(speeds map[int]int) []devicemodel.PortDesired {
		ds := auto(lanePorts(t, c, 50).Lanes)
		for i := range ds {
			ds[i].SpeedMbps = speeds[ds[i].Index]
		}
		return ds
	}
	apply := func(ds []devicemodel.PortDesired) string {
		t.Helper()
		before := len(ft.Configured)
		if _, err := c.ApplyLanes(ctx, 50, ds); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range ft.Configured[before:] {
			out = append(out, strings.Join(b, " | "))
		}
		return strings.Join(out, "\n")
	}

	// The cycle before: the lanes as they are write nothing (the push as
	// the controller has them seeded).
	base := apply(push(nil))
	if plan := c.PlanLanes(50, push(map[int]int{1: 50000})); len(plan) != 4 {
		t.Errorf("PlanLanes of a re-split names lanes %v, want every lane slot", plan)
	}
	got := apply(push(map[int]int{1: 50000}))
	want := "interface Ethernet50/1 | speed forced 50gfull | interface Ethernet50/3 | speed forced 50gfull"
	if !strings.Contains(got, want) {
		t.Fatalf("the 2x50G re-split wrote %q (the cycle before: %q), want %q", got, base, want)
	}
	// Before the switch is read again the lane plan waits.
	if got := apply(push(map[int]int{1: 50000})); got != "" {
		t.Errorf("planned before the switch was read again: %s", got)
	}
	ft.Dir = layoutDir("split-2x50")
	ps, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ps.LayoutWritten, []int{50}) {
		t.Errorf("the snapshot names %v as written through a lane switch, want [50]", ps.LayoutWritten)
	}
	if ps2, _ := c.Collect(ctx); len(ps2.LayoutWritten) != 0 {
		t.Errorf("told again: %v", ps2.LayoutWritten)
	}
	v = lanePorts(t, c, 50)
	if len(v.Lanes) != 2 || v.LaneLayouts != 1 {
		t.Fatalf("2x50G: %d lanes, re-splits %d", len(v.Lanes), v.LaneLayouts)
	}
	for i, l := range v.Lanes {
		name := []string{"Ethernet50/1", "Ethernet50/3"}[i]
		if l.IfName != name || l.Media != devicemodel.MediaSFP28 || !slices.Contains(l.SpeedCaps, 25000) || !slices.Contains(l.SpeedCaps, 50000) {
			t.Errorf("2x50G lane %d: %s media %s caps %v", l.Index, l.IfName, l.Media, l.SpeedCaps)
		}
	}
	// The next cycle writes nothing; nor does this device's own push with a
	// forced 25G on the cage (between split modes the lane switch decides).
	if got := apply(push(map[int]int{1: 50000})); got != "" {
		t.Errorf("the cycle after wrote %s", got)
	}
	own := auto(ps.Ports)
	for i := range own {
		if own[i].Index == 50 {
			own[i].SpeedMbps = 25000
		}
	}
	before := len(ft.Configured)
	if _, err := c.ApplyPorts(ctx, own); err != nil {
		t.Fatal(err)
	}
	for _, b := range ft.Configured[before:] {
		if j := strings.Join(b, " | "); strings.Contains(j, "Ethernet50/") && strings.Contains(j, "speed") {
			t.Errorf("this device's push wrote a speed on the cage: %s", j)
		}
	}
	if !strings.Contains(buf.String(), "only a join is taken from the cage's speed") {
		t.Errorf("no note for the cage's 25G: %s", buf.String())
	}

	// Back: a forced 25G on lane 2.
	got = apply(push(map[int]int{2: 25000}))
	want = "interface Ethernet50/1 | speed forced 25gfull | interface Ethernet50/3 | speed forced 25gfull"
	if !strings.Contains(got, want) {
		t.Fatalf("the 4x25G re-split wrote %q, want %q", got, want)
	}
	ft.Dir = layoutDir("split-by-rule")
	if ps, err = c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ps.LayoutWritten, []int{50}) || len(lanePorts(t, c, 50).Lanes) != 4 {
		t.Errorf("after the way back: written %v, %d lanes", ps.LayoutWritten, len(lanePorts(t, c, 50).Lanes))
	}

	// Speeds of both modes at once: nothing, one note.
	if got := apply(push(map[int]int{1: 50000, 2: 25000})); strings.Contains(got, "speed") {
		t.Errorf("speeds of two modes wrote %s", got)
	}
	if !strings.Contains(buf.String(), "lane 1 50000 Mb/s, lane 2 25000 Mb/s, which no one split mode") {
		t.Errorf("no note for two modes: %s", buf.String())
	}
	_ = snap
}
