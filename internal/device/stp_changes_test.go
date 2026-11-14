package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	aristaeos "github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
)

// The controller's STP-flap alert ("<device> Port N is experiencing frequent
// STP role state changes") is raised from a port's stp_state_change_count.
// The basis is two captures from the same real switch (a USW Aggregation on
// 7.5.15, the only device on the site that raised the alert): its inform
// printed by mca-dump on the switch, and the controller's v2
// system-log/device-alert entries for its two flapping ports. Each alert's
// COUNT is that port's change_count at the time: port 2's exceeds port 3's
// by exactly 10 in every alert, and so does its change_count in the inform
// (their link_down_count differ by 5). No inform anywhere carries an
// stp_state_change_sum key.
const (
	usl8aInform = "controller-11.0.99/inform-usw-aggregation-usl8a.json"
	usl8aAlerts = "controller-11.0.99/system-log-device-alert-stp-flap.json"
)

// stpChangeCount reads a port entry's stp_state_change_count in the real
// shape: a one-element list of {change_count, mst: 0}.
func stpChangeCount(t *testing.T, where string, p map[string]any) float64 {
	t.Helper()
	if _, ok := p["stp_state_change_sum"]; ok {
		t.Errorf("%s port %v: carries stp_state_change_sum, which no real switch sends", where, p["port_idx"])
	}
	list, ok := p["stp_state_change_count"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("%s port %v: stp_state_change_count = %#v, want a one-element list", where, p["port_idx"], p["stp_state_change_count"])
	}
	e, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("%s port %v: stp_state_change_count[0] = %#v, want an object", where, p["port_idx"], list[0])
	}
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "change_count" || keys[1] != "mst" {
		t.Errorf("%s port %v: stp_state_change_count[0] keys %v, want [change_count mst]", where, p["port_idx"], keys)
	}
	if mst, _ := e["mst"].(float64); mst != 0 {
		t.Errorf("%s port %v: mst = %v, want 0 (the CIST)", where, p["port_idx"], e["mst"])
	}
	n, ok := e["change_count"].(float64)
	if !ok || n < 0 || n != float64(int64(n)) {
		t.Fatalf("%s port %v: change_count = %#v, want a non-negative integer", where, p["port_idx"], e["change_count"])
	}
	return n
}

func TestSTPStateChangeCountReference(t *testing.T) {
	ref := loadFixtureJSON(t, usl8aInform)
	counts := map[string]float64{}
	ports := ref["port_table"].([]any)
	for _, e := range ports {
		p := e.(map[string]any)
		counts[strconv.Itoa(int(p["port_idx"].(float64)))] = stpChangeCount(t, "reference", p)
	}
	if len(counts) != 8 {
		t.Fatalf("reference has %d ports, want 8 (every port carries the field, down ones too)", len(counts))
	}

	alerts := loadFixtureJSON(t, usl8aAlerts)["data"].([]any)
	byTime := map[int64]map[string]float64{}
	for _, a := range alerts {
		a := a.(map[string]any)
		par := a["parameters"].(map[string]any)
		port := par["PORT"].(map[string]any)["id"].(string)
		n, err := strconv.ParseFloat(par["COUNT"].(map[string]any)["name"].(string), 64)
		if err != nil {
			t.Fatal(err)
		}
		if n > counts[port] {
			t.Errorf("alert on port %s: COUNT %v exceeds the later inform's change_count %v", port, n, counts[port])
		}
		sec := int64(a["timestamp"].(float64)) / 1000 // a pair is raised in the same second
		if byTime[sec] == nil {
			byTime[sec] = map[string]float64{}
		}
		byTime[sec][port] = n
	}
	if len(byTime) != 3 {
		t.Fatalf("alert fixture: %d alert times, want 3 (one pair a night)", len(byTime))
	}
	want := counts["2"] - counts["3"]
	if want != 10 {
		t.Fatalf("reference: port 2 - port 3 change_count = %v, want 10", want)
	}
	for ts, pair := range byTime {
		if got := pair["2"] - pair["3"]; got != want {
			t.Errorf("alerts at %v: port 2 - port 3 COUNT = %v, want %v (the inform's change_count gap)", ts, got, want)
		}
	}
}

// TestSTPStateChangeCountFromHealth: every port entry the bridge sends
// carries Port.Health.STPChanges in the reference's shape, down ports
// included, and its growth between informs is the growth of the count.
func TestSTPStateChangeCountFromHealth(t *testing.T) {
	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
	c := aristaeos.NewCollector(ft)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := DescriptorFor("UDC48X6", snap, Identity{MAC: snap.System.MAC, IP: "192.0.2.9", Hostname: "arista", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetCapabilities(c.Capabilities())
	sess.SetGatewayIP("192.0.2.1")

	inform := func() map[int]float64 {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
			t.Fatal(err)
		}
		got := map[int]float64{}
		for _, e := range m["port_table"].([]any) {
			p := e.(map[string]any)
			got[int(p["port_idx"].(float64))] = stpChangeCount(t, "bridge", p)
		}
		return got
	}

	var down int
	for _, p := range snap.Ports {
		if !p.Up {
			down = p.Index
			break
		}
	}
	if down == 0 {
		t.Fatal("capture has no down port")
	}
	want := map[int]int{}
	for i := range snap.Ports {
		p := &snap.Ports[i]
		if p.Index == down {
			p.Health.STPChanges = 368382 // a flapping port's count from the reference, on a port that is down
		}
		want[p.Index] = p.Health.STPChanges
	}
	sess.SetSnapshot(snap)
	first := inform()
	if len(first) != len(snap.Ports) {
		t.Fatalf("port_table has %d entries, want %d", len(first), len(snap.Ports))
	}
	for idx, n := range want {
		if first[idx] != float64(n) {
			t.Errorf("port %d: change_count %v, want Health.STPChanges %d", idx, first[idx], n)
		}
	}

	next := *snap
	next.TakenAt = snap.TakenAt.Add(70 * time.Second)
	next.Ports = append(next.Ports[:0:0], snap.Ports...)
	for i := range next.Ports {
		if next.Ports[i].Index == down {
			next.Ports[i].Health.STPChanges += 20
		}
	}
	sess.SetSnapshot(&next)
	second := inform()
	for idx := range want {
		grew := second[idx] - first[idx]
		if idx == down && grew != 20 {
			t.Errorf("port %d: change_count grew %v, want 20", idx, grew)
		}
		if idx != down && grew != 0 {
			t.Errorf("port %d: change_count grew %v, want 0", idx, grew)
		}
	}
}
