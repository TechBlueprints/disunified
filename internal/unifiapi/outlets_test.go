package unifiapi

import "testing"

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
