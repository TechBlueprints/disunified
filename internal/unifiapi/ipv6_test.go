package unifiapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The site's networks as Network 11.0.99 lists them (rest/networkconf,
// captured 2026-11-14 and reduced to the fields read): Default and one
// corporate network with prefix delegation and ipv6_enabled, a guest
// network with prefix delegation and no ipv6_enabled (the gateway serves
// it IPv6 all the same), corporate and VLAN-only networks with IPv6
// Interface Type None, VLAN-only networks with no IPv6 keys, and WANs.
func networkconfServer(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", "rest-networkconf.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/api/s/default/rest/networkconf") {
			http.Error(w, r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestManagementIPv6FollowsTheInterfaceType(t *testing.T) {
	c := New(networkconfServer(t).URL, "k", "default", true)
	for _, tc := range []struct {
		vlan int
		want bool
	}{
		{1, true},     // Default: pd, ipv6_enabled
		{2, true},     // corporate: pd, ipv6_enabled
		{69, true},    // guest: pd, no ipv6_enabled
		{8, false},    // corporate: none
		{10, false},   // VLAN-only: none
		{1000, false}, // VLAN-only: no IPv6 keys
	} {
		got, err := c.ManagementIPv6(context.Background(), tc.vlan)
		if err != nil || got != tc.want {
			t.Errorf("VLAN %d: IPv6 = %v, %v; want %v", tc.vlan, got, err, tc.want)
		}
	}
	if _, err := c.ManagementIPv6(context.Background(), 3999); err == nil {
		t.Error("a VLAN no network has: want an error (the loop then leaves the device alone)")
	}
}
