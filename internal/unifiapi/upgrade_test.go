package unifiapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

const upgradeMAC = "02:00:00:00:00:01"

// serveRecord answers stat/device/<mac> with a captured record and records
// what is posted to cmd/devmgr.
func serveRecord(t *testing.T, fixture string, posted *[]map[string]any) *Client {
	t.Helper()
	body, err := os.ReadFile("../../docs/fixtures/controller-11.0.99/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/proxy/network/api/s/default/stat/device/"+upgradeMAC:
			w.Write(body)
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/network/api/s/default/cmd/devmgr":
			raw, _ := io.ReadAll(r.Body)
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Errorf("cmd/devmgr body: %v", err)
			}
			*posted = append(*posted, m)
			w.Write([]byte(`{"meta":{"rc":"ok"},"data":[]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "0123456789abcdef0123456789abcdef", "default", true)
}

// The record of a connected device the controller has a newer firmware for
// names the version, and the request is the UI's Update: cmd/devmgr upgrade
// with the device's MAC.
func TestOfferedUpgradeAndRequest(t *testing.T) {
	var posted []map[string]any
	c := serveRecord(t, "stat-device-upgrade-offered.json", &posted)
	got, err := c.OfferedUpgrade(context.Background(), upgradeMAC)
	if err != nil || got != "7.6.11.17272" {
		t.Fatalf("OfferedUpgrade = %q, %v; want 7.6.11.17272", got, err)
	}
	if err := c.RequestUpgrade(context.Background(), upgradeMAC); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 || posted[0]["cmd"] != "upgrade" || posted[0]["mac"] != upgradeMAC || len(posted[0]) != 2 {
		t.Fatalf("posted = %v, want one {cmd: upgrade, mac: %s}", posted, upgradeMAC)
	}
}

// The same device after it took the update: nothing offered.
func TestOfferedUpgradeNoneOnceTaken(t *testing.T) {
	var posted []map[string]any
	c := serveRecord(t, "stat-device-upgrade-taken.json", &posted)
	got, err := c.OfferedUpgrade(context.Background(), upgradeMAC)
	if err != nil || got != "" {
		t.Fatalf("OfferedUpgrade = %q, %v; want none", got, err)
	}
	if len(posted) != 0 {
		t.Fatalf("nothing may be posted by a read; got %v", posted)
	}
}
