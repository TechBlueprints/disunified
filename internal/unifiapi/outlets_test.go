package unifiapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The controller's overrides come back from JSON (float64 indices) and
// carry names the operator may have set; only relay_state on the
// placeholder indices may change.
func TestPlaceholderOverridesForceOnlyThePlaceholdersOff(t *testing.T) {
	existing := []map[string]any{
		{"index": float64(1), "relay_state": true, "name": "Outlet 1"},
		{"index": float64(2), "relay_state": true, "name": "Rack"},
		{"index": float64(3), "relay_state": true, "name": "Outlet 3"},
		{"index": float64(4), "relay_state": false, "name": "Outlet 4"}, // already off: untouched
		{"index": float64(8), "relay_state": true, "name": "Outlet 8"},
	}
	list, changed := placeholderOverrides(existing, []int{3, 4, 5, 6, 7, 8})
	if changed != 5 {
		t.Errorf("changed = %d, want 5 (3, 5, 6, 7 and 8; 4 was off already)", changed)
	}
	if len(list) != 8 {
		t.Fatalf("len = %d, want 8 (three added for 5, 6, 7)", len(list))
	}
	for i, o := range list {
		if outletIndex(o) != i+1 {
			t.Errorf("list[%d] index = %d, want %d (sorted by index)", i, outletIndex(o), i+1)
		}
	}
	for _, o := range list {
		idx := outletIndex(o)
		on, _ := o["relay_state"].(bool)
		wantOn := idx <= 2
		if on != wantOn {
			t.Errorf("index %d relay_state = %v, want %v", idx, on, wantOn)
		}
	}
	if list[1]["name"] != "Rack" || list[2]["name"] != "Outlet 3" {
		t.Errorf("names must pass through untouched: %v %v", list[1]["name"], list[2]["name"])
	}
	if _, has := list[4]["name"]; has {
		t.Errorf("an added override carries only index and relay_state: %v", list[4])
	}
}

func TestPlaceholderOverridesSteadyStateChangesNothing(t *testing.T) {
	existing := []map[string]any{
		{"index": float64(1), "relay_state": true},
		{"index": float64(3), "relay_state": false},
		{"index": float64(4), "relay_state": false},
	}
	if _, changed := placeholderOverrides(existing, []int{3, 4}); changed != 0 {
		t.Errorf("changed = %d, want 0", changed)
	}
	if _, changed := placeholderOverrides(existing, nil); changed != 0 {
		t.Errorf("changed = %d, want 0 with no placeholders", changed)
	}
}

// On the power path the provisioner names the device but sends no
// port_overrides: the controller answers api.err.Invalid to any update
// carrying them on a "usp" record (2026-09-26). Driven against a fake
// controller so the PUT body itself is checked.
func TestPowerPathProvisionNamesTheDeviceAndSendsNoPortOverrides(t *testing.T) {
	var put map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/stat/device"):
			_, _ = w.Write([]byte(`{"data":[{"_id":"id1","mac":"02:00:00:00:00:01","name":"UPS 2U Pro","model":"USPDA2B","port_table":[{"port_idx":1,"name":"Port 1"}]}]}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/rest/device/id1"):
			_ = json.NewDecoder(r.Body).Decode(&put)
			_, _ = w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
		default:
			http.Error(w, r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "default", true)
	c.PowerPath = true
	snap := &devicemodel.Snapshot{
		System: devicemodel.System{Model: "APC UPS", Hostname: "ups-1"},
		Ports:  []devicemodel.Port{{Index: 1, IfName: "eth0", Present: true, Enabled: true}},
	}
	r, err := c.Provision(context.Background(), "02:00:00:00:00:01", snap, devicemodel.DefaultNamer{}, []string{"UPS 2U Pro"}, func(int, string) bool { return true }, true)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !r.RenamedDevice || r.RenamedPorts != 0 || r.Seeded != 0 {
		t.Errorf("result = %+v, want the device renamed and no port work", r)
	}
	if put == nil {
		t.Fatal("no PUT reached the controller")
	}
	if _, has := put["port_overrides"]; has {
		t.Errorf("PUT carried port_overrides on the power path: %v", put)
	}
	if n, _ := put["name"].(string); n == "" {
		t.Errorf("PUT carried no name: %v", put)
	}
}
