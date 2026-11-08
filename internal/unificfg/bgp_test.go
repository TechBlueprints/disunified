package unificfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The DX010's BGP pushes of 2026-11-07: a BGP router created for the switch
// under Settings -> Routing -> BGP with an uploaded FRR file, then the same
// router switched off in the list. A push before it carries no switch.bgp.
func TestParseBGP(t *testing.T) {
	if c := readL3(t, "system_cfg-dx010-l3-4-none.txt"); c.BGP != nil {
		t.Errorf("a push without switch.bgp parses as %+v", c.BGP)
	}

	c := readL3(t, "system_cfg-dx010-bgp-1-enabled.txt")
	if c.BGP == nil || !c.BGP.Enabled || c.BGP.DecodeErr != nil {
		t.Fatalf("enabled = %+v", c.BGP)
	}
	if !strings.HasPrefix(c.BGP.Config, "router bgp 65010\n bgp router-id 198.51.100.1\n") ||
		!strings.Contains(c.BGP.Config, "\nroute-map DUI-TEST-IN permit 10\n match ip address prefix-list DUI-TEST-PL\n") {
		t.Errorf("config = %q", c.BGP.Config)
	}
	if c.Routing == nil || c.Routing.Enabled {
		t.Errorf("BGP with nothing routed: routing = %+v", c.Routing)
	}

	off := readL3(t, "system_cfg-dx010-bgp-2-disabled.txt")
	if off.BGP == nil || off.BGP.Enabled || off.BGP.Config != c.BGP.Config {
		t.Errorf("disabled = %+v (a disabled router keeps its file)", off.BGP)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-dx010-bgp-1-enabled.txt"))
	if err != nil {
		t.Fatal(err)
	}
	bad := Parse(strings.Replace(string(raw), "raw_config_base64=", "raw_config_base64=!", 1))
	if bad.BGP == nil || bad.BGP.DecodeErr == nil || bad.BGP.Config != "" {
		t.Errorf("undecodable file = %+v", bad.BGP)
	}
}
