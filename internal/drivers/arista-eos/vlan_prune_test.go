package aristaeos

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The VLAN delete (vlan_prune.go, docs/drivers/arista-eos.md §12) on the
// captures of the live run of 2026-11-15: vlan-prune/hand is the switch
// with four spare VLANs made by hand (3977 with an `interface Vlan3977`,
// 3978 named with IGMP snooping off, 3979 bare, 3980 with a trunk group)
// beside the transit VLAN 4040 the driver created; vlan-prune/driver is the
// switch after the driver's own batch there.

// livePushVLANs is the push in force on 2026-11-15 from 17:46 UTC
// (controller-11.0.99/replies-arista-transit.ndjson, record 2).
var livePushVLANs = []int{1, 2, 8, 9, 10, 69, 1000, 2000, 3000, 4000, 4010}

func pruneCollector(t *testing.T, dir string) (*Collector, *FixtureTransport, *strings.Builder) {
	t.Helper()
	ft := NewFixtureTransport(filepath.Join(aclFixtureDir, "vlan-prune", dir))
	c := NewCollector(ft)
	c.host = "192.0.2.1" // the scrubbed Vlan1 address the bridge dials
	buf := &strings.Builder{}
	c.Log = log.New(buf, "", 0)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, ft, buf
}

func batchesSince(ft *FixtureTransport, n int) []string {
	var out []string
	for _, b := range ft.Configured[n:] {
		out = append(out, strings.Join(b, " | "))
	}
	return out
}

func withVLAN(n int) []int { return append(append([]int{}, livePushVLANs...), n) }

// The live run: the push's list with 4040 kept (so the switch was left as
// found) deletes 3978 and 3979 with 3978's IGMP line, in the range form
// EOS took, keeps 3977 (its SVI) and 3980 (its trunk group) with a note
// each, and reads back as the switch did after it. The next cycle writes
// nothing.
func TestPruneLiveRun(t *testing.T) {
	c, ft, buf := pruneCollector(t, "hand")
	ctx := context.Background()
	desired := desiredFrom(c.last.Ports)
	if n, err := c.EnsureVLANs(ctx, withVLAN(4040)); err != nil || n != 0 {
		t.Fatalf("ensure: %d %v", n, err)
	}
	before := len(ft.Configured)
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	want := "enable | configure | ip igmp snooping vlan 3978 | no vlan 3978-3979 | end | write memory"
	if got := batchesSince(ft, before); len(got) != 1 || got[0] != want {
		t.Fatalf("batches %q, want [%s]", got, want)
	}
	for _, note := range []string{
		"VLAN 3977 is no longer in the controller's push, but interface Vlan3977 exists; left on the switch",
		`VLAN 3980 is no longer in the controller's push, but its vlan section carries "trunk group dui-prune-test", which this driver never writes; left on the switch`,
	} {
		if strings.Count(buf.String(), note) != 1 {
			t.Errorf("note %q logged %d times:\n%s", note, strings.Count(buf.String(), note), buf.String())
		}
	}
	for _, cmd := range []string{"show vlan", "show ip igmp snooping", "show running-config"} {
		for _, d := range sameAsCapture(t, ft, filepath.Join("vlan-prune", "driver"), cmd,
			"/vlans/1/", "/vlans/2/", "/vlans/8/", "/vlans/9/", "/vlans/10/", "/vlans/69/", "/vlans/1000/", "/vlans/2000/", "/vlans/3000/",
			"/vlans/3977/", "/vlans/3980/", "/vlans/4000/", "/vlans/4010/", "/vlans/4040/", // their members' state: minutes apart
		) {
			t.Error(d)
		}
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("unmodelled: %q", ft.Unmodelled)
	}
	if _, err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EnsureVLANs(ctx, withVLAN(4040)); err != nil {
		t.Fatal(err)
	}
	before = len(ft.Configured)
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if got := batchesSince(ft, before); len(got) != 0 {
		t.Errorf("second cycle wrote %q", got)
	}
	if strings.Count(buf.String(), "VLAN 3977 is no longer") != 1 {
		t.Error("the note was logged again")
	}
}

// The push in force alone (4040 not in it): what a deploy does on the live
// switch, 4040 and its IGMP line too.
func TestPruneTheTransit(t *testing.T) {
	c, ft, _ := pruneCollector(t, "hand")
	ctx := context.Background()
	if _, err := c.EnsureVLANs(ctx, livePushVLANs); err != nil {
		t.Fatal(err)
	}
	before := len(ft.Configured)
	if _, err := c.ApplyPorts(ctx, desiredFrom(c.last.Ports)); err != nil {
		t.Fatal(err)
	}
	want := "enable | configure | ip igmp snooping vlan 3978 | ip igmp snooping vlan 4040 | no vlan 3978-3979,4040 | end | write memory"
	if got := batchesSince(ft, before); len(got) != 1 || got[0] != want {
		t.Fatalf("batches %q, want [%s]", got, want)
	}
}

// Nothing is deleted from a list the run has not been given: before the
// device's EnsureVLANs (a held first push never reaches it), with no ports
// to write (control.ports off), and VLAN 1 never.
func TestPruneNeedsThePushList(t *testing.T) {
	c, ft, _ := pruneCollector(t, "hand")
	ctx := context.Background()
	desired := desiredFrom(c.last.Ports)
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if len(ft.Configured) != 0 {
		t.Fatalf("wrote before any push: %q", batchesSince(ft, 0))
	}
	if _, err := c.EnsureVLANs(ctx, []int{2, 8, 9, 10, 69, 1000, 2000, 3000, 4000, 4010, 4040, 3977, 3978, 3979, 3980}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyPorts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if len(ft.Configured) != 0 {
		t.Fatalf("a list without VLAN 1, or no ports, deleted: %q", batchesSince(ft, 0))
	}
}

// A port the bridge does not write that names a VLAN keeps it (noted)
// until the port is written again: then the port batch takes it out and
// the delete follows in its own batch. An Allow All trunk names nothing.
func TestPruneKeepsAVLANAnUnwrittenPortNames(t *testing.T) {
	c, ft, buf := pruneCollector(t, "hand")
	ctx := context.Background()
	configureAll(t, ft, "interface Ethernet2", "switchport trunk allowed vlan 1,3979")
	if _, err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	var desired []devicemodel.PortDesired
	for _, d := range desiredFrom(c.last.Ports) {
		if d.Index != 2 {
			d.VLANSet, d.NativeVLAN, d.TaggedAll = true, 1, true
			desired = append(desired, d)
		}
	}
	if _, err := c.EnsureVLANs(ctx, withVLAN(4040)); err != nil {
		t.Fatal(err)
	}
	before := len(ft.Configured)
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if got := batchesSince(ft, before); len(got) != 1 || !strings.Contains(got[0], "| no vlan 3978 |") {
		t.Fatalf("batches %q, want 3978 alone deleted", got)
	}
	if !strings.Contains(buf.String(), "VLAN 3979 is no longer in the controller's push, but Ethernet2 still names it") {
		t.Errorf("no note for Ethernet2:\n%s", buf.String())
	}
	d2 := desiredFrom(c.last.Ports)[1]
	d2.VLANSet, d2.NativeVLAN, d2.TaggedAll = true, 1, true
	desired = append(desired, d2)
	if _, err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	before = len(ft.Configured)
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	got := batchesSince(ft, before)
	if len(got) != 2 || !strings.HasPrefix(got[0], "enable | configure | interface Ethernet2 | ") || !strings.Contains(got[0], "switchport trunk allowed vlan all") || got[1] != "enable | configure | no vlan 3979 | end | write memory" {
		t.Fatalf("batches %q, want Ethernet2 written, then 3979 deleted", got)
	}
}

// An attached lane switch: nothing is deleted until it has given its list,
// then what its list names stays, and its list leaves the device's alone.
func TestPruneWaitsForTheLaneSwitch(t *testing.T) {
	c, ft, buf := pruneCollector(t, "hand")
	ctx := context.Background()
	if err := c.AttachLaneSwitch(54, devicemodel.Attachment{MAC: childMAC, Name: "lane-switch"}); err != nil {
		t.Fatal(err)
	}
	desired := desiredFrom(c.last.Ports)
	if _, err := c.EnsureVLANs(ctx, livePushVLANs); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if len(ft.Configured) != 0 || !strings.Contains(buf.String(), "the lane switch on cage 54 has not applied a push yet") {
		t.Fatalf("deleted before the lane switch's push: %q\n%s", batchesSince(ft, 0), buf.String())
	}
	if n, err := c.EnsureLaneVLANs(ctx, 54, []int{1, 3978, 4040}); err != nil || n != 0 {
		t.Fatalf("lane ensure: %d %v", n, err)
	}
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	want := "enable | configure | no vlan 3979 | end | write memory"
	if got := batchesSince(ft, 0); len(got) != 1 || got[0] != want {
		t.Fatalf("batches %q, want [%s]", got, want)
	}
	if c.siteVLANs[len(c.siteVLANs)-1] != 4010 {
		t.Errorf("the lane switch's list replaced the device's: %v", c.siteVLANs)
	}
}

// What `no vlan` does on EOS, held to the hand captures (vlan-prune/
// forms-*): created with IGMP off, deleted (the VLANs leave `show vlan`,
// `show ip igmp snooping` and their sections; the IGMP lines stay), the
// IGMP lines cleared for VLANs that no longer exist.
func TestFixtureVLANDeleteForms(t *testing.T) {
	skip := []string{"/cmds/interface Vlan3977", "/cmds/no spanning-tree vlan-id", "/vlans/3977/interfaces", "/vlans/3978/interfaces"}
	ft := NewFixtureTransport(filepath.Join(aclFixtureDir, "vlan-prune", "forms-0-before"))
	configureAll(t, ft, "vlan 3977,3978", "exit", "no ip igmp snooping vlan 3977", "no ip igmp snooping vlan 3978")
	for _, cmd := range []string{"show vlan", "show ip igmp snooping", "show running-config"} {
		for _, d := range sameAsCapture(t, ft, filepath.Join("vlan-prune", "forms-1-created"), cmd, skip...) {
			t.Error(d)
		}
	}
	ft = NewFixtureTransport(filepath.Join(aclFixtureDir, "vlan-prune", "forms-1-created"))
	configureAll(t, ft, "no vlan 3977,3978")
	for _, cmd := range []string{"show vlan", "show ip igmp snooping", "show running-config"} {
		for _, d := range sameAsCapture(t, ft, filepath.Join("vlan-prune", "forms-2-deleted"), cmd) {
			t.Error(d)
		}
	}
	configureAll(t, ft, "ip igmp snooping vlan 3977", "ip igmp snooping vlan 3978")
	for _, d := range sameAsCapture(t, ft, filepath.Join("vlan-prune", "forms-3-igmp-cleared"), "show running-config") {
		t.Error(d)
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("unmodelled: %q", ft.Unmodelled)
	}
}
