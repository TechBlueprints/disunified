package unifiapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The site's networks as Network 11.0.99 lists them (rest/networkconf,
// captured 2026-11-15, reduced to the fields read, scrubbed with each real
// subnet mapped into 198.18.0.0/15 and the names replaced by purpose-N):
// Default is a /16, the other corporate networks and the guest network
// are /24s, one VLAN-only network carries an ip_subnet, and the WANs carry
// none.
func subnetsServer(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", "rest-networkconf-subnets.json"))
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

func TestNetworkForReadsTheSubnetAndGateway(t *testing.T) {
	c := New(subnetsServer(t).URL, "k", "default", true)
	for ip, want := range map[string]devicemodel.SiteNetwork{
		"198.18.110.238": {PrefixLen: 16, Gateway: "198.18.0.1"}, // Default: the gateway's address with the prefix
		"198.19.1.50":    {PrefixLen: 24, Gateway: "198.19.1.1"}, // corporate-1
		"198.19.4.9":     {PrefixLen: 24, Gateway: "198.19.4.1"}, // guest-1
	} {
		got, err := c.NetworkFor(context.Background(), ip)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", ip, got, err, want)
		}
	}
	for _, ip := range []string{
		"198.19.5.7",  // vlan-only-3 carries an ip_subnet; no gateway of UniFi's serves it
		"203.0.113.5", // no network holds it
		"not-an-ip",
	} {
		if got, err := c.NetworkFor(context.Background(), ip); err == nil {
			t.Errorf("%s: %+v, want an error (the device then keeps its options)", ip, got)
		}
	}
}
