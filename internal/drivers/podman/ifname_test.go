package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unifiapi"
)

// A container endpoint's interface name is "ct" and eight hex digits
// (docs/wire-shapes.md §6): an interface name, not the label.
var ctIfName = regexp.MustCompile(`^ct[0-9a-f]{8}$`)

// fixtureContainerPorts is the committed capture's container ports as the
// build before docs/wire-shapes.md §6 presented them: slot, label (its
// IfName, Name and Description then), and the endpoint key the slot file
// holds. The new build must keep every slot and every Name and Description.
var fixtureContainerPorts = []struct {
	idx   int
	label string
	key   string
}{
	{1, "app-1", "app-1/podman"},
	{2, "app-2", "app-2/podman"},
	{3, "app-3", "app-3/net-3"},
	{4, "app-4", "app-4/net-3"},
	{5, "app-5", "app-5/net-3"},
	{6, "app-6", "app-6/net-3"},
	{7, "app-7", "app-7/net-3"},
	{8, "app-8", "app-8/net-3"},
	{9, "app-9", "app-9/net-3"},
	{10, "app-10", "app-10/podman"},
	{11, "app-11", "app-11/podman"},
	{12, "app-12", "app-12/net-1"},
	{13, "disunified-ups", "disunified-ups/disunified-ups_default"},
	{14, "disunified", "disunified/disunified_default"},
}

func ifnameOK(s string) bool {
	return len(s) <= 15 && !strings.ContainsAny(s, " \t\r\n\v\f")
}

// Every container port keeps its slot, its Name and its Description; only
// its IfName changes, to "ct<hash of the endpoint key>". The NIC and the
// free slots are untouched.
func TestContainerPortsKeepSlotsAndLabels(t *testing.T) {
	c, _, snap := startFixture(t)
	byIdx := map[int]devicemodel.Port{}
	for _, p := range snap.Ports {
		byIdx[p.Index] = p
	}
	seen := map[string]int{}
	for _, w := range fixtureContainerPorts {
		p, ok := byIdx[w.idx]
		if !ok || !p.Present {
			t.Errorf("slot %d (%s): no container port", w.idx, w.label)
			continue
		}
		if p.Name != w.label || p.Description != w.label || c.PortName(p) != w.label {
			t.Errorf("slot %d: Name %q Description %q PortName %q, want %q for all three", w.idx, p.Name, p.Description, c.PortName(p), w.label)
		}
		if p.IfName != endpointIfName(w.key) || !ctIfName.MatchString(p.IfName) || !ifnameOK(p.IfName) {
			t.Errorf("slot %d (%s): IfName %q, want %q", w.idx, w.label, p.IfName, endpointIfName(w.key))
		}
		if other, dup := seen[p.IfName]; dup {
			t.Errorf("slots %d and %d share IfName %q", other, w.idx, p.IfName)
		}
		seen[p.IfName] = w.idx
		t.Logf("slot %2d  %-16s %s", w.idx, w.label, p.IfName)
	}
	if up := byIdx[topSlot]; up.IfName != "enp6s18" || up.Name != "enp6s18" {
		t.Errorf("the NIC port = IfName %q Name %q, want enp6s18", up.IfName, up.Name)
	}
	for idx := len(fixtureContainerPorts) + 1; idx < topSlot; idx++ {
		if p := byIdx[idx]; p.Present || p.IfName != "" || p.Name != "" || c.PortName(p) != openLabel(idx) {
			t.Errorf("free slot %d = %+v, PortName %q", idx, p, c.PortName(p))
		}
	}
}

// The name is stable from one collect to the next, across a bridge restart
// (a fresh collector reading the slot file back) and across the container
// being recreated (a new id, the same name): it is a function of the
// endpoint key alone, which is what the slot file keeps.
func TestEndpointIfNameIsStable(t *testing.T) {
	c, fr, snap := startFixture(t)
	names := func(s *devicemodel.Snapshot) map[int]string {
		out := map[int]string{}
		for _, p := range s.Ports {
			out[p.Index] = p.IfName
		}
		return out
	}
	first := names(snap)
	again, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := names(again); !reflect.DeepEqual(got, first) {
		t.Errorf("a second collect changed interface names:\n%v\n%v", first, got)
	}
	c2 := NewCollector(fr)
	c2.Log = log.New(testWriter{t}, "", 0)
	restarted, err := c2.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := names(restarted); !reflect.DeepEqual(got, first) {
		t.Errorf("a restart changed interface names:\n%v\n%v", first, got)
	}
	// Recreate app-3 (running, on net-3): `podman rm` + `podman run --name
	// app-3` gives it a new id everywhere the capture names it.
	const oldID = "37e36f5adb2b"
	if !strings.Contains(fr.Fixture, `"name": "app-3"`) || !strings.Contains(fr.Fixture, oldID) {
		t.Fatal("the capture no longer has app-3 under the id this test recreates")
	}
	fr.Fixture = strings.ReplaceAll(fr.Fixture, oldID, "0f0f0f0f0f0f")
	recreated, err := c2.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, ok := portByName(recreated, "app-3")
	if !ok || p.Index != 3 || p.IfName != first[3] || !p.Up {
		t.Errorf("recreated app-3 = %+v (ok=%v); want slot 3, up, IfName %q", p, ok, first[3])
	}
	// A different endpoint on a slot gets a different name: the loop's
	// layout signature (Index + IfName) sees the new occupant and the
	// bridge names its port.
	if endpointIfName(endpointKey("app-1", "podman")) == endpointIfName(endpointKey("app-13", "podman")) ||
		endpointIfName(endpointKey("app-1", "podman")) == endpointIfName(endpointKey("app-1", "lan")) {
		t.Error("two endpoints share an interface name")
	}
}

// The live host's labels have spaces ("<container> <network>" for a
// container on several networks) and run to 29 characters; the interface
// name fits the rule whatever the label.
func TestEndpointIfNameFitsLongLabels(t *testing.T) {
	name := "app-with-a-long-name-000000"
	e := endpointInfo{key: endpointKey(name, "lan"), container: container{Names: []string{name}}, network: network{Name: "lan"}, multi: true}
	p := endpointPort(7, e)
	if p.Name != name+" lan" || p.Description != p.Name {
		t.Errorf("label = Name %q Description %q", p.Name, p.Description)
	}
	if !ctIfName.MatchString(p.IfName) || !ifnameOK(p.IfName) {
		t.Errorf("IfName %q does not fit the rule", p.IfName)
	}
}

// oldSnapshot is snap as the build before §6 presented it: a container
// port's IfName was its label (endpointPort: IfName = Name = Description).
func oldSnapshot(snap *devicemodel.Snapshot) *devicemodel.Snapshot {
	s := *snap
	s.Ports = append([]devicemodel.Port(nil), snap.Ports...)
	for i, p := range s.Ports {
		if p.Present && len(p.Interfaces) == 0 {
			s.Ports[i].IfName = p.Description
		}
	}
	return &s
}

// payload is what a session sends for snap, built the way
// internal/device/contract_podman_test.go builds it.
func payload(t *testing.T, c *Collector, snap *devicemodel.Snapshot, now time.Time) map[string]any {
	t.Helper()
	desc, err := device.DescriptorFor("UDC48X6", snap, device.Identity{MAC: snap.System.MAC, IP: snap.System.Addresses[0].IP, Hostname: snap.System.Hostname, UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	desc.FWCaps = device.FWCapsFor(c.Capabilities())
	st := device.State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, now)
	sess.SetCapabilities(c.Capabilities())
	sess.SetSnapshot(snap)
	sess.SetGatewayIP("192.0.2.2")
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(now), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The inform changes in port_table[].ifname and nowhere else: the port
// names the controller shows without an override (port_table[].name, from
// Description) are the labels, as before.
func TestNewIfNameChangesOnlyIfnameOnTheWire(t *testing.T) {
	c, _, snap := startFixture(t)
	now := snap.TakenAt
	before, after := payload(t, c, oldSnapshot(snap), now), payload(t, c, snap, now)
	changed := 0
	for i, x := range after["port_table"].([]any) {
		a, b := x.(map[string]any), before["port_table"].([]any)[i].(map[string]any)
		if a["ifname"] != b["ifname"] {
			changed++
			if !ctIfName.MatchString(a["ifname"].(string)) || a["name"] != b["ifname"] {
				t.Errorf("port %v: ifname %v -> %v, name %v", a["port_idx"], b["ifname"], a["ifname"], a["name"])
			}
		}
		delete(a, "ifname")
		delete(b, "ifname")
	}
	if changed != len(fixtureContainerPorts) {
		t.Errorf("%d ports changed ifname, want the %d container ports", changed, len(fixtureContainerPorts))
	}
	if !reflect.DeepEqual(before, after) {
		for k := range after {
			if !reflect.DeepEqual(before[k], after[k]) {
				t.Errorf("payload key %s differs beyond port_table[].ifname:\nbefore %v\nafter  %v", k, before[k], after[k])
			}
		}
	}
}

// fakeController serves one device record and records every write the
// bridge sends (the REST calls unifiapi.Client makes).
type fakeController struct {
	record map[string]any
	writes []string
}

func (f *fakeController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stat/device"):
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{f.record}})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/rest/networkconf"):
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	default:
		b, _ := io.ReadAll(r.Body)
		f.writes = append(f.writes, r.Method+" "+r.URL.Path+" "+string(b))
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	}
}

// defaultNames is cmd/disunified's defaultPortNames: the profile's names
// for each slot and every name the driver could have given it.
func defaultNames(t *testing.T, c *Collector, snap *devicemodel.Snapshot) func(int, string) bool {
	desc, err := device.DescriptorFor("UDC48X6", snap, device.Identity{MAC: snap.System.MAC, IP: snap.System.Addresses[0].IP, Hostname: snap.System.Hostname, UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	defs := map[int][]string{}
	perMedia := map[string]int{}
	for _, p := range desc.Ports {
		perMedia[p.Media]++
		defs[p.PortIdx] = []string{p.Name, fmt.Sprintf("%s %d", p.Media, perMedia[p.Media])}
	}
	for _, p := range snap.Ports {
		defs[p.Index] = append(defs[p.Index], c.DefaultPortNames(p)...)
	}
	return func(idx int, name string) bool {
		for _, d := range defs[idx] {
			if d == name {
				return true
			}
		}
		return false
	}
}

// Nothing the bridge sends to the controller's REST API changes with the
// new IfName: names and overrides are written identically from the old and
// the new snapshot, for a host just adopted (the controller's default
// names), a host named through the inform (port_table names, no overrides:
// how the Podman ports were named, since the inform already carried the
// label), and a host with name overrides and an operator's rename.
func TestNewIfNameChangesNothingSentToTheControllerAPI(t *testing.T) {
	c, _, snap := startFixture(t)
	now := snap.TakenAt
	wire := payload(t, c, oldSnapshot(snap), now)
	var defaultTable, wireTable []any
	desc, err := device.DescriptorFor("UDC48X6", snap, device.Identity{MAC: snap.System.MAC, IP: snap.System.Addresses[0].IP, Hostname: snap.System.Hostname, UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, pp := range desc.Ports {
		defaultTable = append(defaultTable, map[string]any{"port_idx": pp.PortIdx, "name": pp.Name})
	}
	for _, x := range wire["port_table"].([]any) {
		e := x.(map[string]any)
		wireTable = append(wireTable, map[string]any{"port_idx": e["port_idx"], "name": e["name"]})
	}
	var overrides []any
	for _, w := range fixtureContainerPorts {
		name := w.label
		if w.idx == 3 {
			name = "an operator's name"
		}
		overrides = append(overrides, map[string]any{"port_idx": w.idx, "name": name})
	}
	states := []struct {
		what      string
		table     []any
		overrides []any
	}{
		{"just adopted", defaultTable, nil},
		{"named through the inform", wireTable, nil},
		{"name overrides and a rename", wireTable, overrides},
	}
	for _, st := range states {
		for _, seed := range []bool{false, true} {
			var got [2][]string
			for i, s := range []*devicemodel.Snapshot{oldSnapshot(snap), snap} {
				f := &fakeController{record: map[string]any{
					"_id": "0123456789abcdef01234567", "mac": snap.System.MAC, "name": "host-1", "model": "UDC48X6",
					"port_table": st.table, "port_overrides": st.overrides,
				}}
				srv := httptest.NewServer(f)
				api := unifiapi.New(srv.URL, "0123456789abcdef0123456789abcdef", "default", true)
				r, err := api.Provision(context.Background(), snap.System.MAC, s, c, []string{"USW Leaf", "UDC48X6"}, defaultNames(t, c, s), seed)
				srv.Close()
				if err != nil {
					t.Fatalf("%s: %v", st.what, err)
				}
				got[i] = append(f.writes, fmt.Sprintf("result %+v", r))
			}
			if !reflect.DeepEqual(got[0], got[1]) {
				t.Errorf("%s (seed %v): the REST calls differ\nold %v\nnew %v", st.what, seed, got[0], got[1])
			}
			if st.what == "just adopted" && len(got[1]) < 2 {
				t.Errorf("%s: provisioning named nothing (%v); the test is not exercising the names", st.what, got[1])
			}
		}
	}
}
