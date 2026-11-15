package aristaeos

import (
	"context"
	"testing"
)

// afterClear serves the live capture of 2026-11-15
// (docs/fixtures/arista-eos-4.26.14M/link-counters/): `show interfaces`,
// `show version` and the STP topology status 14 h after a `clear counters`,
// with Ethernet1 and Ethernet53/1 up, everything else down.
var afterClear = map[string]string{
	"show interfaces": "link-counters/show-interfaces-after-clear.json",
	"show version":    "link-counters/show-version-after-clear.json",
	"show spanning-tree topology status detail": "link-counters/show-spanning-tree-topology-status-detail-after-clear.json",
}

// TestLinkDropsCountedFromBoot: the 2026-09-19 capture counts from boot
// (lastClear within a second of bootupTimestamp), so every interface carries
// the one transition EOS counts at the start: 1 on a port down since boot,
// and Ethernet49/1's 18 are 8 drops, Ethernet53/1's 4 one drop. The
// presentation sends LinkChanges/2 as link_down_count.
func TestLinkDropsCountedFromBoot(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		idx          int
		up           bool
		drops, trans uint64
	}{
		{49, true, 8, 17},
		{53, true, 1, 3},
		{2, false, 0, 0},
		{52, false, 0, 0},
		{50, false, 0, 0}, // four lanes, each at 1: the cage never had a link
	} {
		p := portByIndex(t, snap, tc.idx)
		if p.Up != tc.up || p.Health.LinkChanges != tc.trans || p.Health.LinkChanges/2 != tc.drops {
			t.Errorf("port %d: up %v link changes %d (drops %d), want up %v, %d (%d)",
				tc.idx, p.Up, p.Health.LinkChanges, p.Health.LinkChanges/2, tc.up, tc.trans, tc.drops)
		}
	}
}

// TestLinkDropsAfterClear: after a `clear counters` the count starts at 0
// from whatever the link was, with no start-up transition: the two up ports
// and every down one read 0 drops.
func TestLinkDropsAfterClear(t *testing.T) {
	c, ft := newFixtureCollector(t)
	ft.Overrides = afterClear
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports {
		if p.Health.LinkChanges/2 != 0 {
			t.Errorf("port %d (up %v): %d drops after the clear, want 0", p.Index, p.Up, p.Health.LinkChanges/2)
		}
	}
	if p := portByIndex(t, snap, 1); !p.Up || p.Health.LinkChanges != 1 || p.Health.STPChanges != 3 {
		t.Errorf("port 1 = up %v link changes %d stp %d, want up, 1, 3", p.Up, p.Health.LinkChanges, p.Health.STPChanges)
	}
	if p := portByIndex(t, snap, 2); p.Up || p.Health.STPChanges != 0 {
		t.Errorf("port 2 = up %v stp %d (a down port is not in the topology)", p.Up, p.Health.STPChanges)
	}
}

// TestLinkDropsCases: the derivation's cases, as the captures show them,
// plus the drop and rise that follow each.
func TestLinkDropsCases(t *testing.T) {
	const boot = 1000.0
	for _, tc := range []struct {
		name      string
		n         uint64
		lastClear float64
		up        bool
		want      uint64
	}{
		{"down since boot", 1, boot + 0.5, false, 0},
		{"up since boot", 2, boot + 0.5, true, 0},
		{"down after a drop, from boot", 3, boot + 0.5, false, 1},
		{"up after a drop, from boot", 4, boot + 0.5, true, 1},
		{"Ethernet49/1, from boot", 18, boot + 0.5, true, 8},
		{"up, cleared", 0, boot + 3600, true, 0},
		{"down, cleared", 0, boot + 3600, false, 0},
		{"dropped after the clear", 1, boot + 3600, false, 1},
		{"back up after the clear", 2, boot + 3600, true, 1},
		{"came up after the clear", 1, boot + 3600, true, 0},
		{"came up, dropped after the clear", 2, boot + 3600, false, 1},
	} {
		got := linkDrops(&eosCounters{LinkStatusChanges: tc.n, LastClear: tc.lastClear}, tc.up, boot)
		if got != tc.want {
			t.Errorf("%s: %d drops, want %d", tc.name, got, tc.want)
		}
		if lc := linkChanges(got, tc.up); lc/2 != got {
			t.Errorf("%s: LinkChanges %d halves to %d, not %d", tc.name, lc, lc/2, got)
		}
	}
	if linkDrops(nil, true, boot) != 0 {
		t.Error("no counters: want 0")
	}
}

// TestAdminShutdownCountsNoLinkChange: Ethernet2 shut by hand while it had
// no link (live 2026-11-15, inside a commit-timer session then aborted):
// disabled, and still 0 transitions.
func TestAdminShutdownCountsNoLinkChange(t *testing.T) {
	c, ft := newFixtureCollector(t)
	ft.Overrides = map[string]string{
		"show interfaces": "link-counters/show-interfaces-Ethernet2-shutdown.json",
		"show version":    "link-counters/show-version-after-clear.json",
	}
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := portByIndex(t, snap, 2); p.Enabled || p.Up || p.Health.LinkChanges != 0 {
		t.Errorf("port 2 = enabled %v up %v link changes %d, want disabled, down, 0", p.Enabled, p.Up, p.Health.LinkChanges)
	}
}

// TestSTPChangesHeldAcrossLinkUps: EOS lists an interface's STP state changes
// only while its link is up, counted from that link-up. Across the two real
// captures -- 2026-09-19 (Ethernet49/1 up, 3 changes, 8 drops) and
// 2026-11-15 (Ethernet49/1 down, not listed) -- the port's count is held
// while it is down and a later link-up adds its own count: it never falls.
func TestSTPChangesHeldAcrossLinkUps(t *testing.T) {
	c, ft := newFixtureCollector(t)
	ctx := context.Background()
	step := func(overrides map[string]string) (p49, p53 int) {
		t.Helper()
		ft.Overrides = overrides
		snap, err := c.Collect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return portByIndex(t, snap, 49).Health.STPChanges, portByIndex(t, snap, 53).Health.STPChanges
	}
	if _, err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if a, b := step(nil); a != 3 || b != 3 {
		t.Fatalf("first collect: 49 %d 53 %d, want 3 3", a, b)
	}
	// 49/1 down: held. 53/1 unchanged at 3 (its drop count fell with the
	// clear, which is no new link-up).
	if a, b := step(afterClear); a != 3 || b != 3 {
		t.Errorf("49/1 down: 49 %d 53 %d, want 3 3 (held)", a, b)
	}
	// Both listed again with drops past the last reading: a new link-up
	// each, whose 3 changes add to what was held.
	if a, b := step(nil); a != 6 || b != 6 {
		t.Errorf("back up: 49 %d 53 %d, want 6 6", a, b)
	}
	// The same reading again: no growth.
	if a, b := step(nil); a != 6 || b != 6 {
		t.Errorf("unchanged: 49 %d 53 %d, want 6 6", a, b)
	}
}

// TestHoldSTPChanges: growth within a link-up adds its growth; a count that
// fell, or a link that dropped in between, is a new link-up.
func TestHoldSTPChanges(t *testing.T) {
	c := NewCollector(nil)
	seq := []struct {
		raw   int
		drops uint64
		want  int
	}{
		{3, 0, 3},
		{5, 0, 5},  // grew within the link-up
		{0, 1, 5},  // down: not listed
		{2, 1, 7},  // up again, below the last reading
		{3, 2, 10}, // dropped and rose between two collects, above the last reading
		{3, 2, 10},
	}
	for i, s := range seq {
		raw := map[string]int{}
		if s.raw > 0 {
			raw["Ethernet2"] = s.raw
		}
		got := c.holdSTPChanges(raw, map[string]uint64{"Ethernet2": s.drops})
		if got["Ethernet2"] != s.want {
			t.Errorf("step %d: %d, want %d", i, got["Ethernet2"], s.want)
		}
	}
}
