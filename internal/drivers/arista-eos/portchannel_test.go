package aristaeos

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The captures of 2026-11-15 (docs/drivers/arista-eos.md §4, "Aggregates"),
// one sanitizer run, the switch as it was each time:
//
//	empty-po-leftovers          Port-Channel1..4 left empty by earlier tests
//	empty-po-lag5               + Ethernet2 and Ethernet54/1 in Port-Channel5 (the driver's build batch)
//	empty-po-lag5-removed       the deployed bridge's removal: members out, Port-Channel5 left
//	empty-po-lag5-acl           Port-Channel5 with members and a list of its own (PO_TEST) bound
//	empty-po-lag5-removed-fixed after the fixed batch: members out, then `no interface Port-Channel5`
//	empty-po-none               after the leftovers were removed by hand: no Port-Channel at all

func emptyPOCollector(t *testing.T, dir string) (*Collector, *FixtureTransport) {
	t.Helper()
	ft := NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M", "empty-po-"+dir))
	c := NewCollector(ft)
	c.Log = log.New(io.Discard, "", 0)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, ft
}

// without is desired with ports idx out of every aggregate.
func without(desired []devicemodel.PortDesired, idx ...int) []devicemodel.PortDesired {
	for i := range desired {
		for _, n := range idx {
			if desired[i].Index == n {
				desired[i].LAG = 0
			}
		}
	}
	return desired
}

func TestTheCapturesShowTheLeftover(t *testing.T) {
	c, _ := emptyPOCollector(t, "lag5")
	for _, idx := range []int{2, 54} {
		if p := portByIndex(t, c.last, idx); p.LAG != "Port-Channel5" || p.LAGID != 5 {
			t.Errorf("port %d in %q/%d, want Port-Channel5", idx, p.LAG, p.LAGID)
		}
	}
	// What the driver's old removal left: the interface, VLANs and all.
	c, _ = emptyPOCollector(t, "lag5-removed")
	if got := c.channelCfg[5]; len(got) != 1 || got[0] != "switchport mode trunk" {
		t.Errorf("Port-Channel5 after the old removal: %q", got)
	}
	for _, idx := range []int{2, 54} {
		if p := portByIndex(t, c.last, idx); p.LAG != "" {
			t.Errorf("port %d still in %s", idx, p.LAG)
		}
	}
	c, _ = emptyPOCollector(t, "lag5-removed-fixed")
	if _, ok := c.channelCfg[5]; ok {
		t.Error("Port-Channel5 after the fixed batch")
	}
}

// The controller removes the aggregate: the members leave, then the
// Port-Channel goes in the same batch (with the capture's own leftovers).
func TestRemovingAnAggregateRemovesItsPortChannel(t *testing.T) {
	c, ft := emptyPOCollector(t, "lag5")
	desired := without(desiredFrom(c.last.Ports), 2, 54)
	n, err := c.ApplyPorts(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	want := "enable | configure | interface Ethernet2 | no channel-group | interface Ethernet54/1 | no channel-group | " +
		"no interface Port-Channel1 | no interface Port-Channel2 | no interface Port-Channel3 | no interface Port-Channel4 | no interface Port-Channel5 | end | write memory"
	if got := lastBatch(t, ft); got != want {
		t.Errorf("removal\n%s\nwant\n%s", got, want)
	}
	if n != 7 {
		t.Errorf("changed %d, want 7 (two ports, five Port-Channels)", n)
	}
	// Idempotent until the next collect, and forgotten for the next diff.
	if n, _ := c.ApplyPorts(context.Background(), desired); n != 0 {
		t.Errorf("second apply changed %d: %s", n, lastBatch(t, ft))
	}
	if _, ok := c.portChannelVLAN("Port-Channel5"); ok {
		t.Error("Port-Channel5's VLANs still known")
	}
	// The switch after that batch: nothing left to do.
	c, ft = emptyPOCollector(t, "none")
	if n, _ := c.ApplyPorts(context.Background(), without(desiredFrom(c.last.Ports), 2, 54)); n != 0 || len(ft.Configured) != 0 {
		t.Errorf("on the clean switch: %d %v", n, ft.Configured)
	}
}

// A kept aggregate keeps its Port-Channel; an empty one left by the old
// removal goes at the next apply, and changes no port (the first-push hold
// does not wait on it).
func TestLeftoverEmptyPortChannelsGo(t *testing.T) {
	c, ft := emptyPOCollector(t, "lag5")
	desired := desiredFrom(c.last.Ports)
	if got := c.PlanPorts(desired); len(got) != 0 {
		t.Errorf("PlanPorts = %v", got)
	}
	if _, err := c.ApplyPorts(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	if got := lastBatch(t, ft); got != fixtureLeftovers {
		t.Errorf("kept aggregate\n%s\nwant\n%s", got, fixtureLeftovers)
	}

	c, ft = emptyPOCollector(t, "lag5-removed")
	desired = desiredFrom(c.last.Ports)
	if got := c.PlanPorts(desired); len(got) != 0 {
		t.Errorf("PlanPorts = %v: removing an empty Port-Channel changes no port", got)
	}
	if _, err := c.ApplyPorts(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(fixtureLeftovers, "Port-Channel4 |", "Port-Channel4 | no interface Port-Channel5 |", 1)
	if got := lastBatch(t, ft); got != want {
		t.Errorf("leftovers\n%s\nwant\n%s", got, want)
	}

	c, ft = emptyPOCollector(t, "lag5-removed-fixed")
	if _, err := c.ApplyPorts(context.Background(), desiredFrom(c.last.Ports)); err != nil {
		t.Fatal(err)
	}
	if got := lastBatch(t, ft); got != fixtureLeftovers {
		t.Errorf("after the fixed batch\n%s\nwant\n%s", got, fixtureLeftovers)
	}
}

// A Port-Channel carrying what the driver never writes (here a list of
// someone else's) was made by hand: its members follow the controller,
// the interface stays, and the plan says why.
func TestHandMadePortChannelStays(t *testing.T) {
	c, ft := emptyPOCollector(t, "lag5-acl")
	buf := &strings.Builder{}
	c.Log.SetOutput(buf)
	if _, err := c.ApplyPorts(context.Background(), without(desiredFrom(c.last.Ports), 2, 54)); err != nil {
		t.Fatal(err)
	}
	got := lastBatch(t, ft)
	if strings.Contains(got, "no interface Port-Channel5") {
		t.Errorf("a hand-made Port-Channel removed: %s", got)
	}
	if !strings.Contains(got, "interface Ethernet2 | no channel-group | interface Ethernet54/1 | no channel-group") {
		t.Errorf("the members did not leave: %s", got)
	}
	if !strings.Contains(buf.String(), `Port-Channel5 has no member and carries "ip access-group PO_TEST in"`) {
		t.Errorf("no note: %s", buf.String())
	}
}

// A build on the clean switch writes the members and the Port-Channel's
// VLANs, and removes nothing.
func TestBuildOnTheCleanSwitch(t *testing.T) {
	c, ft := emptyPOCollector(t, "none")
	desired := desiredFrom(c.last.Ports)
	for i := range desired {
		if desired[i].Index == 2 || desired[i].Index == 54 {
			desired[i].LAG = 5
			desired[i].VLANSet, desired[i].NativeVLAN, desired[i].TaggedAll = true, 1, true
		}
	}
	if _, err := c.ApplyPorts(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	want := "enable | configure | interface Ethernet2 | channel-group 5 mode active | interface Ethernet54/1 | channel-group 5 mode active | " +
		"interface Port-Channel5 | switchport mode trunk | no switchport trunk native vlan | switchport trunk allowed vlan all | end | write memory"
	if got := lastBatch(t, ft); got != want {
		t.Errorf("build\n%s\nwant\n%s", got, want)
	}
}

// The range: the device's plan owns 1..its last port; a lane switch's
// groups (1000+) and anything above the ports are another's or hand-made.
func TestStaleChannelsRange(t *testing.T) {
	c, _ := emptyPOCollector(t, "leftovers")
	remove, kept := staleChannels(c.channelCfg, map[string]bool{"Port-Channel2": true}, 1, 3)
	if len(remove) != 2 || remove[0] != 1 || remove[1] != 3 || len(kept) != 0 {
		t.Errorf("remove %v kept %v", remove, kept)
	}
	if remove, _ := staleChannels(c.channelCfg, nil, laneBase(54)+1, laneBase(54)+laneGroupSpan-1); len(remove) != 0 {
		t.Errorf("lane range removed %v", remove)
	}
}
