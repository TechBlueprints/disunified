package reach

import (
	"context"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

func TestPrefixLen(t *testing.T) {
	for mask, want := range map[string]int{"255.255.255.0": 24, "255.255.0.0": 16, "": 0, "not-a-mask": 0, "2001:db8::": 0} {
		if got := PrefixLen(mask); got != want {
			t.Errorf("PrefixLen(%q) = %d, want %d", mask, got, want)
		}
	}
}

// The options are taken as written, MACs lower-cased; an operator MAC wins
// over the driver's, none leaves the driver's own.
func TestOptionsFillTheSystem(t *testing.T) {
	var id Identity
	id.SetOptions("192.0.2.4", map[string]string{"mac": " 02:00:00:00:00:AB ", "netmask": "255.255.0.0", "gateway_mac": "02:00:00:00:00:FE"})
	sys := devicemodel.System{MAC: "02:00:00:00:00:01"}
	id.Fill(context.Background(), &sys)
	if sys.MAC != "02:00:00:00:00:ab" || sys.GatewayMAC != "02:00:00:00:00:fe" {
		t.Errorf("mac %q gateway_mac %q", sys.MAC, sys.GatewayMAC)
	}
	if len(sys.Addresses) != 1 || sys.Addresses[0].IP != "192.0.2.4" || sys.Addresses[0].PrefixLen != 16 {
		t.Errorf("addresses %+v, want 192.0.2.4/16", sys.Addresses)
	}
	var none Identity
	sys = devicemodel.System{MAC: "02:00:00:00:00:01"}
	none.Fill(context.Background(), &sys)
	if sys.MAC != "02:00:00:00:00:01" || len(sys.Addresses) != 0 {
		t.Errorf("without options: mac %q addresses %+v", sys.MAC, sys.Addresses)
	}
}
