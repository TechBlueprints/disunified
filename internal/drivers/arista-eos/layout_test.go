package aristaeos

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// module-layout-20261117/ is the switch on 2026-11-17 with cage 50 (a
// CA-25G-S breakout cable, no far end) in each state, one sitting each:
// joined (`speed forced 100gfull` by hand), nospeed (no speed line),
// split-by-rule (after this rule's split), split-10g (every lane at 10G),
// split-2x50 (`50gfull` on /1 and /3), 50g-lane1-only (`50gfull` on /1
// alone: /2 gone, /3 and /4 errdisabled speed-misconfigured). Every other
// cage as in fec-auto/: 49 CWDM4 whole, 51-53 CA-25G-L whole.
func layoutDir(state string) string {
	return filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M", "module-layout-20261117", state)
}

func layoutCollector(t *testing.T, state string) (*Collector, *FixtureTransport, *bytes.Buffer, *devicemodel.Snapshot) {
	t.Helper()
	ft := NewFixtureTransport(layoutDir(state))
	c := NewCollector(ft)
	var buf bytes.Buffer
	c.Log = log.New(&buf, "", 0)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, ft, &buf, snap
}

func cage(t *testing.T, snap *devicemodel.Snapshot, idx int) devicemodel.Port {
	t.Helper()
	i := slices.IndexFunc(snap.Ports, func(p devicemodel.Port) bool { return p.Index == idx })
	if i < 0 {
		t.Fatalf("no port %d", idx)
	}
	return snap.Ports[i]
}

// batches is every config batch, its commands between configure and end.
func batches(ft *FixtureTransport) []string {
	var out []string
	for _, b := range ft.Configured {
		out = append(out, strings.Join(b, " | "))
	}
	return out
}

func speedLines(ft *FixtureTransport) []string {
	var out []string
	for _, b := range ft.Configured {
		iface := ""
		for _, c := range b {
			if strings.HasPrefix(c, "interface ") {
				iface = strings.TrimPrefix(c, "interface ")
			}
			if strings.HasPrefix(c, "speed ") {
				out = append(out, iface+": "+c)
			}
		}
	}
	return out
}

// The switch as fec-auto/ captured it: every cage runs a layout its module
// runs. No speed for the device plan, a lane switch's plan or PlanLayout,
// and no layout note.
func TestModuleLayoutWritesNothingOnTheSwitchAsItIs(t *testing.T) {
	c, ft, buf, snap := fecAutoCollector(t, "")
	ds := auto(snap.Ports)
	if got := c.PlanLayout(ds); len(got) != 0 {
		t.Errorf("PlanLayout = %v", got)
	}
	if _, err := c.ApplyPorts(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	v := lanePorts(t, c, 50)
	if _, err := c.ApplyLanes(context.Background(), 50, auto(v.Lanes)); err != nil {
		t.Fatal(err)
	}
	if got := speedLines(ft); len(got) != 0 {
		t.Errorf("speed written: %q", got)
	}
	if strings.Contains(buf.String(), "layout") {
		t.Errorf("a layout note: %s", buf.String())
	}
}

// Cage 50 joined at 100G with its CA-S cable: the module runs 4x25G or
// 2x50G, so the rule splits it on lane 1 -- and nothing else is written,
// the FEC least of all (it waits for the lanes). The controller-only plan
// counts none of it; PlanLayout names the cage. After the split (the
// capture taken after the rule's own write) nothing more is written.
func TestModuleLayoutSplitsAJoinedBreakoutCable(t *testing.T) {
	c, ft, buf, snap := layoutCollector(t, "joined")
	p := cage(t, snap, 50)
	if p.Lanes != 1 || p.AdminSpeed != 100000 {
		t.Fatalf("cage 50 joined: %d lanes at %d", p.Lanes, p.AdminSpeed)
	}
	ds := auto(snap.Ports)
	if got := c.PlanPorts(ds); len(got) != 0 {
		t.Errorf("PlanPorts = %v: the module's layout is not the controller's change", got)
	}
	if got := c.PlanLayout(ds); !slices.Equal(got, []int{50}) {
		t.Errorf("PlanLayout = %v, want [50]", got)
	}
	n, err := c.ApplyPorts(context.Background(), ds)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got, want := batches(ft), []string{"enable | configure | interface Ethernet50/1 | speed forced 25gfull | end | write memory"}; !slices.Equal(got, want) {
		t.Fatalf("wrote %q, want %q", got, want)
	}
	if strings.Contains(buf.String(), "FEC") {
		t.Errorf("a FEC note in the cycle of the split: %s", buf.String())
	}
	// A second apply before the collect writes nothing.
	if n, _ := c.ApplyPorts(context.Background(), ds); n != 0 || len(ft.Configured) != 1 {
		t.Errorf("second apply: n=%d, %d batches", n, len(ft.Configured))
	}
	// The switch after the split.
	ft.Dir = layoutDir("split-by-rule")
	snap, err = c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := cage(t, snap, 50); p.Lanes != 4 || p.AdminSpeed != 25000 {
		t.Fatalf("after the split: %d lanes at %d", p.Lanes, p.AdminSpeed)
	}
	if !slices.Contains(c.SplitCages(), 50) {
		t.Errorf("split cages %v: a lane switch is due for cage 50", c.SplitCages())
	}
	ds = auto(snap.Ports)
	if got := c.PlanLayout(ds); len(got) != 0 {
		t.Errorf("PlanLayout after the split = %v", got)
	}
	if _, err := c.ApplyPorts(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	if got := speedLines(ft); len(got) != 1 {
		t.Errorf("speed written after the split: %q", got)
	}
}

// A cage with no speed line reads as one port at 100G (EOS's adminSpeed
// says 100Gbps, not auto): the CA-S cable splits it as a joined one.
func TestModuleLayoutNoSpeedLine(t *testing.T) {
	c, ft, _, snap := layoutCollector(t, "nospeed")
	if p := cage(t, snap, 50); p.Lanes != 1 || p.AdminSpeed != 100000 {
		t.Fatalf("cage 50 with no speed line: %d lanes at %d", p.Lanes, p.AdminSpeed)
	}
	if _, err := c.ApplyPorts(context.Background(), auto(snap.Ports)); err != nil {
		t.Fatal(err)
	}
	if got := speedLines(ft); !slices.Equal(got, []string{"Ethernet50/1: speed forced 25gfull"}) {
		t.Errorf("wrote %q", got)
	}
}

// Every lane at 10G: the same port count, so the rule writes the native
// 25G to every lane (one alone would errdisable the others), the FEC
// waits, PlanLayout names nothing (the lane count stays) and the remembered
// write leaves nothing for the next reconcile.
func TestModuleLayoutSetsTheLaneSpeedOnEveryLane(t *testing.T) {
	c, ft, _, snap := layoutCollector(t, "split-10g")
	if p := cage(t, snap, 50); p.Lanes != 4 || p.AdminSpeed != 10000 {
		t.Fatalf("cage 50: %d lanes at %d", p.Lanes, p.AdminSpeed)
	}
	ds := auto(snap.Ports)
	if got := c.PlanPorts(ds); len(got) != 0 {
		t.Errorf("PlanPorts = %v", got)
	}
	if got := c.PlanLayout(ds); len(got) != 0 {
		t.Errorf("PlanLayout = %v: the lane count does not change", got)
	}
	if _, err := c.ApplyPorts(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	want := []string{"enable | configure | interface Ethernet50/1 | speed forced 25gfull | interface Ethernet50/2 | speed forced 25gfull | interface Ethernet50/3 | speed forced 25gfull | interface Ethernet50/4 | speed forced 25gfull | end | write memory"}
	if got := batches(ft); !slices.Equal(got, want) {
		t.Fatalf("wrote %q, want %q", got, want)
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("unmodelled: %q", ft.Unmodelled)
	}
	if n, _ := c.ApplyPorts(context.Background(), ds); n != 0 {
		t.Errorf("second apply before the collect: %d", n)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := cage(t, snap, 50); p.AdminSpeed != 25000 {
		t.Errorf("read back at %d", p.AdminSpeed)
	}
	if _, err := c.ApplyPorts(context.Background(), auto(snap.Ports)); err != nil {
		t.Fatal(err)
	}
	if len(speedLines(ft)) != 4 {
		t.Errorf("a reconcile wrote speed again: %q", batches(ft))
	}
}

// 2x50G (/1 and /3 at 50G): the fold reads two ports at 50G, a layout the
// CA-S cable runs, kept. 50G on /1 alone (the other lanes 25G, /3 and /4
// errdisabled): three lanes apart, which the cable does not run; lane 1
// goes back to 25G, a split to four lanes.
func TestModuleLayoutFiftyGig(t *testing.T) {
	c, ft, _, snap := layoutCollector(t, "split-2x50")
	if p := cage(t, snap, 50); p.Lanes != 2 || p.AdminSpeed != 50000 || !slices.Equal(p.Interfaces, []string{"Ethernet50/1", "Ethernet50/3"}) {
		t.Fatalf("2x50G: %d lanes at %d, %v", p.Lanes, p.AdminSpeed, p.Interfaces)
	}
	if _, err := c.ApplyPorts(context.Background(), auto(snap.Ports)); err != nil {
		t.Fatal(err)
	}
	if got := speedLines(ft); len(got) != 0 {
		t.Errorf("2x50G: wrote %q", got)
	}

	c, ft, _, snap = layoutCollector(t, "50g-lane1-only")
	if p := cage(t, snap, 50); p.Lanes != 3 {
		t.Fatalf("50G on /1 alone: %d lanes %v", p.Lanes, p.Interfaces)
	}
	ds := auto(snap.Ports)
	if got := c.PlanLayout(ds); !slices.Equal(got, []int{50}) {
		t.Errorf("PlanLayout = %v", got)
	}
	if _, err := c.ApplyPorts(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	if got := speedLines(ft); !slices.Equal(got, []string{"Ethernet50/1: speed forced 25gfull"}) {
		t.Errorf("50G on /1 alone: wrote %q", got)
	}
}

// The rule never splits or joins a cage in an aggregate or a mirror
// session: the split would take it out.
func TestModuleLayoutRefusals(t *testing.T) {
	for name, edit := range map[string]func(ds []devicemodel.PortDesired){
		"aggregate":          func(ds []devicemodel.PortDesired) { ds[49].LAG = 7 },
		"mirror destination": func(ds []devicemodel.PortDesired) { ds[49].MirrorSource = 2 },
		"mirror source":      func(ds []devicemodel.PortDesired) { ds[1].MirrorSource = 50 },
	} {
		t.Run(name, func(t *testing.T) {
			c, ft, buf, snap := layoutCollector(t, "joined")
			ds := auto(snap.Ports)
			if ds[49].Index != 50 || ds[1].Index != 2 {
				t.Fatalf("port order %d %d", ds[1].Index, ds[49].Index)
			}
			edit(ds)
			if got := c.PlanLayout(ds); len(got) != 0 {
				t.Errorf("PlanLayout = %v", got)
			}
			if _, err := c.ApplyPorts(context.Background(), ds); err != nil {
				t.Fatal(err)
			}
			if got := speedLines(ft); len(got) != 0 {
				t.Errorf("wrote %q", got)
			}
			if !strings.Contains(buf.String(), "its layout is left as it is") {
				t.Errorf("no note: %s", buf.String())
			}
		})
	}
}

// A cage with a lane switch attached: the module decides the port count
// only, so four lanes at 10G stay (the lane switch's, which may differ),
// where the cage of the switch's own takes 25G.
func TestModuleLayoutOwnedCageKeepsItsLanes(t *testing.T) {
	c, ft, _, snap := layoutCollector(t, "split-10g")
	if err := c.AttachLaneSwitch(50, devicemodel.Attachment{MAC: childMAC, Name: "lane-switch"}); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyPorts(context.Background(), auto(snap.Ports)); err != nil {
		t.Fatal(err)
	}
	if got := speedLines(ft); len(got) != 0 {
		t.Errorf("wrote %q", got)
	}
}

// Stale codes (a module swapped since they were read): the media type
// speaks, and `100GBASE-CR4` stands for the code that runs every copper
// layout, so the joined cage stays joined until the codes are read again.
func TestModuleLayoutStaleCodesFallBackToTheMediaType(t *testing.T) {
	c, ft, buf, snap := layoutCollector(t, "joined")
	c.mu.Lock()
	c.eepromStale = true
	c.mu.Unlock()
	ds := auto(snap.Ports)
	if got := c.PlanLayout(ds); len(got) != 0 {
		t.Errorf("PlanLayout = %v", got)
	}
	if _, err := c.ApplyPorts(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	if got := speedLines(ft); len(got) != 0 {
		t.Errorf("wrote %q", got)
	}
	if strings.Contains(buf.String(), "layout table") {
		t.Errorf("a note: %s", buf.String())
	}
}

// A pushed speed is the controller's: PlanLayout names a cage it splits or
// joins, the rule stays out of the way.
func TestPlanLayoutNamesAPushedSplit(t *testing.T) {
	c, _, _, snap := layoutCollector(t, "joined")
	ds := auto(snap.Ports)
	ds[49].SpeedMbps = 10000
	if got := c.PlanLayout(ds); !slices.Equal(got, []int{50}) {
		t.Errorf("PlanLayout = %v", got)
	}
	if got := c.PlanPorts(ds); !slices.Equal(got, []int{50}) {
		t.Errorf("PlanPorts = %v: a pushed speed is the controller's", got)
	}
}
