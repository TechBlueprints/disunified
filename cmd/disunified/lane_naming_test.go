package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unifiapi"
	"github.com/TechBlueprints/disunified/internal/unifimodel"
)

// A lane switch's UniFi name (docs/drivers/breakout.md 16.14) against the
// controller's list right after the DX010's cage 26 lane switch was adopted
// (Network 11.0.99, 2026-11-18; stat-device-lane-switch-adopted.json): the
// parent is "device-1" (scrubbed), the lane switch 02:00:00:00:00:1a holds
// the controller's default "USW Flex XG".

// fakeLane stands in for the breakout device: its parent's MAC and cage.
type fakeLane struct {
	devicemodel.Device
	parent string
	cage   int
}

func (f fakeLane) ParentMAC() string { return f.parent }
func (f fakeLane) Cage() int         { return f.cage }

// provisionLane runs one Provision of the lane switch against the captured
// list, with the record's name replaced by name when set, and returns the
// name the PUT wrote ("" for no PUT or no name).
func provisionLane(t *testing.T, name string, parentReadable bool) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", "stat-device-lane-switch-adopted.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Data []map[string]any `json:"data"`
		Meta map[string]any   `json:"meta"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if name != "" {
		list.Data[1]["name"] = name
	}
	if !parentReadable {
		list.Data = list.Data[1:]
	}
	body, _ := json.Marshal(list)
	var wrote string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stat/device"):
			w.Write(body)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/rest/device/"):
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			wrote, _ = m["name"].(string)
			w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	api := unifiapi.New(srv.URL, "test-key", "", false)
	sw := fakeLane{parent: "02:00:00:00:00:01", cage: 26}
	snap := &devicemodel.Snapshot{System: devicemodel.System{Vendor: "disunified", Model: "Lane switch", Hostname: "sw-cage26"}}
	base := devicemodel.Namer(devicemodel.DefaultNamer{})
	defaults := append([]string{"USFXG"}, unifimodel.ControllerDisplayNames("USFXG")...) // as main builds them, less the profile's display
	ctx := context.Background()
	if _, err := api.Provision(ctx, "02:00:00:00:00:1a", snap, namerForProvision(ctx, sw, base, api.DeviceByMAC), defaults, func(int, string) bool { return true }, false); err != nil {
		t.Fatal(err)
	}
	return wrote
}

func TestLaneSwitchNamedAfterItsParentInUniFi(t *testing.T) {
	if got := provisionLane(t, "", true); got != "device-1-cage26" {
		t.Errorf("the adopted lane switch (default %q) was named %q, want device-1-cage26", "USW Flex XG", got)
	}
	// What the driver's own namer gives (a build before 2026-11-18 would
	// have written it) is a default too.
	if got := provisionLane(t, "disunified Lane switch", true); got != "device-1-cage26" {
		t.Errorf("the generic name: %q", got)
	}
	// An operator's name stays; so does the name once given (a later
	// rename of the parent does not follow it).
	for _, kept := range []string{"rack-breakout", "old-parent-name-cage26"} {
		if got := provisionLane(t, kept, true); got != "" {
			t.Errorf("%q was renamed to %q", kept, got)
		}
	}
	// The parent's record cannot be read: no device name this time.
	if got := provisionLane(t, "", false); got != "" {
		t.Errorf("without the parent's record: named %q", got)
	}
}

func TestLaneSwitchUniFiName(t *testing.T) {
	if got := laneSwitchUniFiName("aggregation", 3); got != "aggregation-cage3" {
		t.Errorf("%q", got)
	}
	if laneSwitchUniFiName("", 3) != "" || laneSwitchUniFiName("x", 0) != "" {
		t.Error("a name without a parent name or a cage")
	}
	// A device that is not a lane switch keeps its namer.
	n := devicemodel.Namer(devicemodel.DefaultNamer{})
	if got := namerForProvision(context.Background(), nil, n, func(context.Context, string) (*unifiapi.Device, error) { return nil, errors.New("unused") }); got != n {
		t.Error("a device that is not a lane switch got a lane namer")
	}
}
