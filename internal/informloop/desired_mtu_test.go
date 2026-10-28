package informloop

import (
	"testing"

	"github.com/jamesbraid/unifi-emu/inform"
)

// switch.jumboframes and switch.mtu become PortDesired.MTU: the pushed MTU
// when jumbo is on (UniFi's 9216 if the key is missing), 1500 when off, 0
// when the push says nothing about jumbo.
func TestDesiredMTUFromJumbo(t *testing.T) {
	l := &Loop{desc: inform.Descriptor{MAC: "02:00:00:00:00:01"}}
	for _, tc := range []struct {
		cfg  string
		want int
	}{
		{"switch.jumboframes=enabled\nswitch.mtu=9216\n", 9216},
		{"switch.jumboframes=enabled\nswitch.mtu=9000\n", 9000},
		{"switch.jumboframes=enabled\n", 9216},
		{"switch.jumboframes=disabled\nswitch.mtu=9216\n", 1500},
		{"switch.mtu=9216\n", 0},
	} {
		ds := l.desiredPorts(tc.cfg + "switch.port.1.name=Port 1\n")
		if len(ds) != 1 || ds[0].MTU != tc.want {
			t.Errorf("%q: MTU = %+v, want %d", tc.cfg, ds, tc.want)
		}
	}
}
