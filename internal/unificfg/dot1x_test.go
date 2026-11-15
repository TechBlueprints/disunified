package unificfg

import (
	"os"
	"testing"
)

// The DX010's 802.1X pushes (Network 11.0.99, 2026-11-15): 802.1X Control
// on for the device with the UniFi Integrated server, then each port method
// on an empty cage, External RADIUS, and 802.1X off.
func TestDot1XPushes(t *testing.T) {
	read := func(name string) *Config {
		b, err := os.ReadFile("../../docs/fixtures/controller-11.0.99/system_cfg-dx010-dot1x-" + name + ".txt")
		if err != nil {
			t.Fatal(err)
		}
		return Parse(string(b))
	}
	on := read("1-enabled")
	if on.Dot1X == nil || !on.Dot1X.Enabled || on.Dot1X.DynVLAN || len(on.Dot1X.Servers) != 1 {
		t.Fatalf("enabled: %+v", on.Dot1X)
	}
	if s := on.Dot1X.Servers[0]; s.IP != "192.0.2.2" || s.Port != 1812 || s.Secret != "<secret>" {
		t.Errorf("enabled: server %+v", s)
	}
	for idx := 1; idx <= 32; idx++ {
		if m := on.Ports[idx].Dot1XMode; m != "force_authorized" {
			t.Errorf("enabled: port %d mode %q, want force_authorized on every port", idx, m)
		}
	}
	for _, tc := range []struct {
		file string
		port int
		mode string
		idle int
	}{
		{"2-port31-credentials", 31, "auto", 0},
		{"3-port30-multi", 30, "multi_host", 0},
		{"4-port29-mac", 29, "mac_based", 300},
		{"5-port28-unauth", 28, "force_unauthorized", 0},
	} {
		c := read(tc.file)
		if p := c.Ports[tc.port]; p.Dot1XMode != tc.mode || p.Dot1XIdleTimeout != tc.idle {
			t.Errorf("%s: port %d = %q idle %d, want %q idle %d", tc.file, tc.port, p.Dot1XMode, p.Dot1XIdleTimeout, tc.mode, tc.idle)
		}
	}
	if s := read("7-external").Dot1X.Servers; len(s) != 1 || s[0].IP != "192.0.2.5" {
		t.Errorf("external: servers %+v", s)
	}
	off := read("8-off")
	if off.Dot1X == nil || off.Dot1X.Enabled || len(off.Dot1X.Servers) != 0 || off.Ports[31].Dot1XMode != "" {
		t.Errorf("off: %+v, port 31 %q", off.Dot1X, off.Ports[31].Dot1XMode)
	}
	b, err := os.ReadFile("../../docs/fixtures/controller-10.6.109/system_cfg-dx010-portfeat-0-baseline.txt")
	if err != nil {
		t.Fatal(err)
	}
	if c := Parse(string(b)); c.Dot1X != nil {
		t.Errorf("a push to a device that does not claim DOT1X: %+v", c.Dot1X)
	}
}
