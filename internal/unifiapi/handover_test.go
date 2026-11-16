package unifiapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The DX010's record right after its cage 3's lane switch was adopted
// (Network 11.0.99, 2026-11-16; stat-device-dx010-lane-handover.json,
// reduced to the overrides and ports 2-4): port 3 reports QSFP28 and its
// override still holds the fold's `autoneg false, speed 25000, full_duplex
// true, fec_mode fc-fec`. A drawer save of port 4 was answered 400
// `api.err.InvalidFecMode`, `port_idx 3`.
func handoverRecord(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", "stat-device-dx010-lane-handover.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCageToAuto(t *testing.T) {
	var r struct {
		Data []Device `json:"data"`
	}
	if err := json.Unmarshal(handoverRecord(t), &r); err != nil {
		t.Fatal(err)
	}
	existing := r.Data[0].PortOverrides
	list, before, changed := cageToAuto(existing, 3)
	if !changed {
		t.Fatal("port 3's forced 25G was not changed")
	}
	want := map[string]any{"autoneg": false, "speed": float64(25000), "full_duplex": true, "fec_mode": "fc-fec"}
	if !reflect.DeepEqual(before, want) {
		t.Errorf("before = %v, want %v", before, want)
	}
	if len(list) != len(existing) {
		t.Fatalf("%d overrides, had %d", len(list), len(existing))
	}
	for i, o := range list {
		if overrideIndex(o) != 3 {
			if !reflect.DeepEqual(o, existing[i]) {
				t.Errorf("port %d's override changed", overrideIndex(o))
			}
			continue
		}
		if o["autoneg"] != true {
			t.Errorf("port 3 autoneg = %v", o["autoneg"])
		}
		for _, k := range []string{"speed", "full_duplex", "fec_mode"} {
			if _, has := o[k]; has {
				t.Errorf("port 3 still has %s", k)
			}
		}
		// Everything else the operator set stays.
		for k, v := range existing[i] {
			switch k {
			case "autoneg", "speed", "full_duplex", "fec_mode":
			default:
				if !reflect.DeepEqual(o[k], v) {
					t.Errorf("port 3 %s changed: %v -> %v", k, v, o[k])
				}
			}
		}
	}
	// The stored record is not changed in place.
	if existing[2]["autoneg"] != false && overrideIndex(existing[2]) == 3 {
		t.Error("the input list was modified")
	}
	// Nothing for a port on Auto (4), one with no speed keys (2), an unknown
	// port, or the result itself.
	for _, p := range []int{2, 4, 99} {
		if _, _, changed := cageToAuto(existing, p); changed {
			t.Errorf("port %d changed", p)
		}
	}
	if _, _, changed := cageToAuto(list, 3); changed {
		t.Error("a second pass changed port 3 again")
	}
	// A forced 100G (the join) is the operator's and stays.
	joined, _, _ := cageToAuto(existing, 3)
	for _, o := range joined {
		if overrideIndex(o) == 3 {
			o["autoneg"], o["speed"] = false, float64(100000)
		}
	}
	if _, _, changed := cageToAuto(joined, 3); changed {
		t.Error("a forced 100G was reset")
	}
}

func TestCageToAutoWritesTheWholeList(t *testing.T) {
	body := handoverRecord(t)
	var put map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stat/device"):
			w.Write(body)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/rest/device/"):
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &put); err != nil {
				t.Error(err)
			}
			w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "0123456789abcdef0123456789abcdef", "default", true)
	before, err := c.CageToAuto(context.Background(), "02:00:00:00:00:01", 3)
	if err != nil || before["speed"] != float64(25000) {
		t.Fatalf("before %v, err %v", before, err)
	}
	list, _ := put["port_overrides"].([]any)
	if len(put) != 1 || len(list) != 32 {
		t.Errorf("the PUT carried %d keys and %d overrides, want port_overrides alone with all 32", len(put), len(list))
	}
	put = nil
	if before, err := c.CageToAuto(context.Background(), "02:00:00:00:00:01", 4); err != nil || before != nil || put != nil {
		t.Errorf("a port on Auto: before %v, err %v, put %v", before, err, put)
	}
}
