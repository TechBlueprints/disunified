package aristaeos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The aggregate writes, held to the captures of the switch before and after
// the same batch (fixture_lag.go): every reply the collector reads that the
// batch changes must read, after the transport applied it, as the switch
// printed it, numbers (counters, timestamps) aside.

// lagReads are the replies an aggregate changes.
var lagReads = []string{
	"show port-channel summary", "show interfaces switchport", "show interfaces status",
	"show running-config", "show interfaces",
}

// sameAsCapture lists where the transport's reply to cmd differs from the
// capture in dir; skip names top-level paths ("/cmds/alias ...") to leave out.
func sameAsCapture(t *testing.T, ft *FixtureTransport, dir, cmd string, skip ...string) []string {
	t.Helper()
	got := readJSON(t, ft, cmd)
	b, err := os.ReadFile(filepath.Join(aclFixtureDir, dir, fixtureName(cmd, ".json")))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	var diffs []string
	var walk func(p string, a, b any)
	walk = func(p string, a, b any) {
		for _, s := range skip {
			if strings.HasPrefix(p, s) {
				return
			}
		}
		am, aok := a.(map[string]any)
		bm, bok := b.(map[string]any)
		if aok && bok {
			keys := map[string]bool{}
			for k := range am {
				keys[k] = true
			}
			for k := range bm {
				keys[k] = true
			}
			for k := range keys {
				walk(p+"/"+k, am[k], bm[k])
			}
			return
		}
		if _, ok := a.(float64); ok {
			if _, ok := b.(float64); ok {
				return
			}
		}
		if fmt.Sprint(a) != fmt.Sprint(b) {
			diffs = append(diffs, fmt.Sprintf("%s %s: transport %v, switch %v", cmd, p, a, b))
		}
	}
	walk("", got, want)
	sort.Strings(diffs)
	return diffs
}

func holdToCaptures(t *testing.T, ft *FixtureTransport, dir string, skip ...string) {
	t.Helper()
	for _, cmd := range lagReads {
		for _, d := range sameAsCapture(t, ft, dir, cmd, skip...) {
			t.Error(d)
		}
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("unmodelled: %q", ft.Unmodelled)
	}
}

// The noise of the minutes between two captures, which no write makes.
var lagCaptureNoise = []string{
	"/interfaces/Ethernet1/", "/interfaces/Ethernet49", "/interfaces/Ethernet53", "/interfaces/Management1",
	"/interfaceStatuses/Ethernet1/", "/interfaceStatuses/Management1",
	"/cmds/alias ", // the hold session's marker
}

// empty-po-lag5-removed-fixed was taken after the lag5-acl step, whose
// list stayed.
var afterACLTest = append(append([]string{}, lagCaptureNoise...), "/cmds/ip access-list PO_TEST")

// The driver's build batch on the switch with the four leftovers
// (empty-po-leftovers) reads as empty-po-lag5; its removal batch, members
// out then `no interface Port-Channel5`, as empty-po-lag5-removed-fixed.
func TestFixtureAggregateBuildAndRemoval(t *testing.T) {
	ft := NewFixtureTransport(filepath.Join(aclFixtureDir, "empty-po-leftovers"))
	configureAll(t, ft, "interface Ethernet2", "channel-group 5 mode active", "interface Ethernet54/1", "channel-group 5 mode active",
		"interface Port-Channel5", "switchport mode trunk", "no switchport trunk native vlan", "switchport trunk allowed vlan all")
	holdToCaptures(t, ft, "empty-po-lag5", append(lagCaptureNoise, "/interfaces/Port-Channel5/physicalAddress")...)
	configureAll(t, ft, "interface Ethernet2", "no channel-group", "interface Ethernet54/1", "no channel-group", "no interface Port-Channel5")
	holdToCaptures(t, ft, "empty-po-lag5-removed-fixed", afterACLTest...)
}

// `no channel-group` on the last members leaves the Port-Channel, unlisted
// in the summary (empty-po-lag5-removed, the old removal batch).
func TestFixtureAggregateEmptied(t *testing.T) {
	ft := NewFixtureTransport(filepath.Join(aclFixtureDir, "empty-po-lag5"))
	configureAll(t, ft, "interface Ethernet2", "no channel-group", "interface Ethernet54/1", "no channel-group")
	holdToCaptures(t, ft, "empty-po-lag5-removed", append(lagCaptureNoise, "/interfaces/Port-Channel5/physicalAddress")...)
	// The capture pair of one member: a Port-Channel removed with its
	// member in it takes the member out too.
	ft = NewFixtureTransport(filepath.Join(aclFixtureDir, "empty-po-lag5"))
	configureAll(t, ft, "no interface Port-Channel5")
	holdToCaptures(t, ft, "empty-po-lag5-removed-fixed", afterACLTest...)
}

// Three lanes of the split cage into Port-Channel1541 (the lane switch's
// aggregate batch) read as split-cage54-flexxg.
func TestFixtureLaneAggregate(t *testing.T) {
	ft := NewFixtureTransport(filepath.Join(aclFixtureDir, "split-cage54"))
	var lines []string
	for _, l := range []string{"54/2", "54/3", "54/4"} {
		lines = append(lines, "interface Ethernet"+l, "channel-group 1541 mode active", "error-correction encoding reed-solomon")
	}
	configureAll(t, ft, append(lines, "interface Port-Channel1541", "switchport mode trunk", "no switchport trunk native vlan", "switchport trunk allowed vlan all")...)
	holdToCaptures(t, ft, "split-cage54-flexxg", lagCaptureNoise...)
}

// What is not captured stays unmodelled and changes nothing: a port with
// a link, a move between groups, a mode other than active, a new group
// from a port that is not a plain trunk.
func TestFixtureAggregateUnmodelled(t *testing.T) {
	ft := NewFixtureTransport(filepath.Join(aclFixtureDir, "empty-po-lag5"))
	before := readJSON(t, ft, "show port-channel summary")
	configureAll(t, ft,
		"interface Ethernet1", "channel-group 7 mode active", // the uplink: a link
		"interface Ethernet2", "channel-group 6 mode active", // already in 5
		"interface Ethernet3", "channel-group 7 mode passive",
		"interface Ethernet4", "switchport mode access", "channel-group 8 mode active")
	if got := len(ft.Unmodelled); got != 4 {
		t.Errorf("unmodelled %d: %q", got, ft.Unmodelled)
	}
	if after := readJSON(t, ft, "show port-channel summary"); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("summary changed: %v", after)
	}
}

// A collect after the build reads the aggregate, and the plan is then
// idempotent: nothing written again.
func TestAggregateIsReadBackOnTheNextCollect(t *testing.T) {
	c, ft := emptyPOCollector(t, "none")
	desired := desiredFrom(c.last.Ports)
	for i := range desired {
		if desired[i].Index == 2 || desired[i].Index == 54 {
			desired[i].LAG = 5
			desired[i].VLANSet, desired[i].NativeVLAN, desired[i].TaggedAll = true, 1, true
		}
	}
	ctx := context.Background()
	if _, err := c.ApplyPorts(ctx, desired); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int{2, 54} {
		if p := portByIndex(t, snap, idx); p.LAG != "Port-Channel5" || p.LAGID != 5 {
			t.Errorf("port %d after the build: %q/%d", idx, p.LAG, p.LAGID)
		}
	}
	if v, ok := c.portChannelVLAN("Port-Channel5"); !ok || v.Mode != "trunk" || !v.AllowAll {
		t.Errorf("Port-Channel5's VLANs read back: %+v %v", v, ok)
	}
	n := len(ft.Configured)
	if k, err := c.ApplyPorts(ctx, desired); err != nil || k != 0 || len(ft.Configured) != n {
		t.Errorf("after the collect the build wrote again (%d, %v): %q", k, err, ft.Configured[n:])
	}
	// The removal, collected: no member, no Port-Channel, nothing to write.
	if _, err := c.ApplyPorts(ctx, without(desired, 2, 54)); err != nil {
		t.Fatal(err)
	}
	if snap, err = c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	for _, idx := range []int{2, 54} {
		if p := portByIndex(t, snap, idx); p.LAG != "" {
			t.Errorf("port %d after the removal still in %s", idx, p.LAG)
		}
	}
	if _, ok := c.channelCfg[5]; ok {
		t.Error("Port-Channel5 still in the running config")
	}
	n = len(ft.Configured)
	if k, _ := c.ApplyPorts(ctx, without(desired, 2, 54)); k != 0 || len(ft.Configured) != n {
		t.Errorf("after the removal wrote again: %q", ft.Configured[n:])
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("unmodelled: %q", ft.Unmodelled)
	}
}

// The templates are the captures' own entries.
func TestFixtureAggregateTemplates(t *testing.T) {
	load := func(rel string, keys ...string) any {
		b, err := os.ReadFile(filepath.Join(aclFixtureDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return path(v, keys...)
	}
	asCaptured := func(name, tmpl string, want any, skip ...string) {
		var got map[string]any
		_ = json.Unmarshal([]byte(tmpl), &got)
		for _, k := range skip {
			delete(got, k)
			delete(want.(map[string]any), k)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s\n%v\ncaptured\n%v", name, got, want)
		}
	}
	asCaptured("show interfaces", poInterfaceJSON,
		load("channel-group-po9/1-channel-group/show-interfaces-Port-Channel9.json", "interfaces", "Port-Channel9"), "name", "physicalAddress")
	asCaptured("show interfaces status", poStatusJSON, load("empty-po-lag5/show-interfaces-status.json", "interfaceStatuses", "Port-Channel5"))
	asCaptured("bare switchport", poBareSwitchports, load("empty-po-leftovers/show-interfaces-switchport.json", "switchports", "Port-Channel4"))
	asCaptured("bare status", poBareStatusVLAN, load("empty-po-leftovers/show-interfaces-status.json", "interfaceStatuses", "Port-Channel4", "vlanInformation"))
	asCaptured("summary", poSummaryJSON, load("empty-po-lag5/show-port-channel-summary.json", "portChannels", "Port-Channel5"), "ports")
	asCaptured("copper member", poMemberJSON, load("empty-po-lag5/show-port-channel-summary.json", "portChannels", "Port-Channel5", "ports", "Ethernet2"), "intf")
	asCaptured("lane LACP", poLaneLACPJSON, load("split-cage54-flexxg/show-port-channel-summary.json", "portChannels", "Port-Channel1541", "ports", "Ethernet54/3", "lacpState"))
}
