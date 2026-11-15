package aristaeos

import (
	"context"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// What a detached lane switch leaves (docs/drivers/breakout.md §7c), on the
// captures of 2026-11-15 (one sanitizer run), the empty test cage 54 each
// time:
//
//	lane-po-empty    split to 4x25G; lanes 3-4 put in channel group 1541 and
//	                 taken out again: Port-Channel1541 left with `switchport
//	                 mode trunk` (inherited from the lanes), no member;
//	                 show-running-config-handmade.json: the same with a
//	                 `description` on it
//	lane-po-removed  after `no interface Port-Channel1541`
//	lane-po-joined   the cage joined back to 100G with an empty
//	                 Port-Channel1541 left (lane 3 in and out again)
//
// The switch's own Port-Channel1 (cages 51 and 52) is in all three.

func lanePOCollector(t *testing.T, dir string, attach bool) (*Collector, *FixtureTransport, *strings.Builder) {
	t.Helper()
	ft := NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M", "lane-po-"+dir))
	c := NewCollector(ft)
	buf := &strings.Builder{}
	c.Log = log.New(buf, "", 0)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attach {
		if err := c.AttachLaneSwitch(54, devicemodel.Attachment{MAC: childMAC, Name: "lane-switch"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return c, ft, buf
}

func TestTheCapturesShowTheDetachedLeftover(t *testing.T) {
	c, _, _ := lanePOCollector(t, "empty", false)
	if got := c.channelCfg[1541]; !slices.Equal(got, []string{"switchport mode trunk"}) {
		t.Errorf("Port-Channel1541: %q", got)
	}
	if p := portByIndex(t, c.last, 54); p.Lanes != 4 || p.LAG != "" {
		t.Errorf("cage 54: %d lanes, LAG %q", p.Lanes, p.LAG)
	}
	if p := portByIndex(t, c.last, 51); p.LAG != "Port-Channel1" || p.LAGID != 1 {
		t.Errorf("cage 51 in %q/%d", p.LAG, p.LAGID)
	}
	c, _, _ = lanePOCollector(t, "joined", false)
	if p := portByIndex(t, c.last, 54); p.Lanes > 1 {
		t.Errorf("cage 54 still split")
	}
	if _, ok := c.channelCfg[1541]; !ok {
		t.Error("no Port-Channel1541 on the joined cage")
	}
	c, _, _ = lanePOCollector(t, "removed", false)
	if _, ok := c.channelCfg[1541]; ok {
		t.Error("Port-Channel1541 after its removal")
	}
}

// With no lane switch attached to the cage, the device's own plan removes
// the empty lane Port-Channel, changing no port, once; the switch it
// leaves reads as the capture taken after the same command.
func TestDetachedLaneSwitchsEmptyPortChannelGoes(t *testing.T) {
	for _, dir := range []string{"empty", "joined"} {
		c, ft, _ := lanePOCollector(t, dir, false)
		desired := desiredFrom(c.last.Ports)
		if got := c.PlanPorts(desired); len(got) != 0 {
			t.Errorf("%s: PlanPorts = %v: removing a Port-Channel changes no port", dir, got)
		}
		n, err := c.ApplyPorts(context.Background(), desired)
		if err != nil {
			t.Fatal(err)
		}
		if want := "enable | configure | no interface Port-Channel1541 | end | write memory"; lastBatch(t, ft) != want || n != 1 {
			t.Errorf("%s: %d changed\n%s\nwant\n%s", dir, n, lastBatch(t, ft), want)
		}
		// Idempotent before the next collect, and after it.
		writes := len(ft.Configured)
		if n, _ := c.ApplyPorts(context.Background(), desired); n != 0 {
			t.Errorf("%s: second apply changed %d", dir, n)
		}
		if _, err := c.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n, _ := c.ApplyPorts(context.Background(), desiredFrom(c.last.Ports)); n != 0 || len(ft.Configured) != writes {
			t.Errorf("%s: after the collect changed %d: %v", dir, n, ft.Configured[writes:])
		}
		if len(ft.Unmodelled) != 0 {
			t.Errorf("%s: unmodelled %q", dir, ft.Unmodelled)
		}
		if _, ok := c.channelCfg[1541]; ok {
			t.Errorf("%s: the transport still shows Port-Channel1541", dir)
		}
	}
	// The capture of the switch after the command: nothing left to do.
	c, ft, _ := lanePOCollector(t, "removed", false)
	if n, _ := c.ApplyPorts(context.Background(), desiredFrom(c.last.Ports)); n != 0 || len(ft.Configured) != 0 {
		t.Errorf("after the removal: %d %v", n, ft.Configured)
	}
}

// While a lane switch is attached the range is its own: the device's plan
// leaves the Port-Channel, the lane plan removes it.
func TestAttachedLaneSwitchKeepsItsRange(t *testing.T) {
	c, ft, _ := lanePOCollector(t, "empty", true)
	if _, err := c.ApplyPorts(context.Background(), desiredFrom(c.last.Ports)); err != nil {
		t.Fatal(err)
	}
	for _, b := range ft.Configured {
		if s := strings.Join(b, " | "); strings.Contains(s, "1541") {
			t.Errorf("the device's plan wrote %q", s)
		}
	}
	if _, err := c.ApplyLanes(context.Background(), 54, desiredFrom(lanePorts(t, c, 54).Lanes)); err != nil {
		t.Fatal(err)
	}
	if want := "enable | configure | no interface Port-Channel1541 | end | write memory"; lastBatch(t, ft) != want {
		t.Errorf("lane plan\n%s\nwant\n%s", lastBatch(t, ft), want)
	}
}

// A detached lane switch's Port-Channel that someone has written to by
// hand stays, as anywhere.
func TestDetachedHandMadeLanePortChannelStays(t *testing.T) {
	ft := NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M", "lane-po-empty"))
	ft.Overrides = map[string]string{"show running-config": "show-running-config-handmade.json"}
	c := NewCollector(ft)
	buf := &strings.Builder{}
	c.Log = log.New(buf, "", 0)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, err := c.ApplyPorts(context.Background(), desiredFrom(c.last.Ports)); err != nil || n != 0 {
		t.Errorf("changed %d (%v): %v", n, err, ft.Configured)
	}
	if !strings.Contains(buf.String(), `Port-Channel1541 has no member and carries "description hand-made"`) {
		t.Errorf("no note: %s", buf.String())
	}
}

// One with members stays, with a note naming them (split-cage54-lag: lanes
// 3-4 in Port-Channel1541, nothing attached).
func TestDetachedLaneGroupWithMembersStays(t *testing.T) {
	c, ft, buf := splitCollector(t, "split-cage54-lag", false)
	if _, err := c.ApplyPorts(context.Background(), desiredFrom(c.last.Ports)); err != nil {
		t.Fatal(err)
	}
	for _, b := range ft.Configured {
		if s := strings.Join(b, " | "); strings.Contains(s, "1541") {
			t.Errorf("the device's plan wrote %q", s)
		}
	}
	if want := "Port-Channel1541 is port 54's lane switch's aggregate and no lane switch is attached to port 54: left as it is while it has members (Ethernet54/3, Ethernet54/4)"; !strings.Contains(buf.String(), want) {
		t.Errorf("no note: %s", buf.String())
	}
}

// Membership is every lane's, not the fold's one group per cage; an
// attached cage's range and a cage past EOS's 2000 are not the plan's.
func TestDetachedLaneChannelsMembership(t *testing.T) {
	c, _, _ := lanePOCollector(t, "empty", false)
	have := map[int][]string{1541: {"switchport mode trunk"}, 1542: {"switchport mode trunk"}}
	lanes := map[int][]devicemodel.Port{54: slices.Clone(c.lanes[54])}
	lanes[54][3].LAG = "Port-Channel1542" // lane 4 in a second group the fold does not show
	note := func(string, ...any) {}
	remove, _ := detachedLaneChannels(c.last.Ports, lanes, nil, have, nil, note)
	if !slices.Equal(remove, []int{1541}) {
		t.Errorf("remove %v, want [1541]", remove)
	}
	if remove, _ := detachedLaneChannels(c.last.Ports, lanes, map[int]bool{54: true}, have, nil, note); len(remove) != 0 {
		t.Errorf("attached: remove %v", remove)
	}
	far := []devicemodel.Port{{Index: 100}}
	if remove, _ := detachedLaneChannels(far, nil, nil, map[int][]string{2001: nil}, nil, note); len(remove) != 0 {
		t.Errorf("port 100: remove %v", remove)
	}
}
