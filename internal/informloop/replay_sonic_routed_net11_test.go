package informloop

import (
	"context"
	"strings"
	"testing"
)

// The DX010's routed aggregate on Network 11.0.99 (2026-11-15,
// docs/drivers/sonic.md §11.16): Routing + Aggregating on empty cages 15
// and 16 (Ethernet56, Ethernet60) from the port editor, then the network
// removed. As on 10.6.109 the push names only the first member (port 15
// opmode=aggregate, lag=1; the routing interface's lag=15) and leaves port
// 16 out of every key, so the bridge builds a one-member PortChannel and
// leaves Ethernet60 in its VLAN. The push before the run and the last one
// (2b37a0e11994ff97, port 16's name restored) are byte for byte the
// 802.1X series' last push. The bridge's journal for the run wrote the same
// commands. Every push is followed by a cycle that must write nothing.
func TestReplaySonicDX010RoutedAggregateNetwork11(t *testing.T) {
	r, _ := newNet11Rig(t, "system_cfg-dx010-dot1x-8-off.txt", "0214200d9c242877")
	// The removal names port 16 "QSFP28 16", the controller's "<media> k"
	// default, which cmd/disunified's defaultPortNames counts as unnamed
	// (the rig has only the driver's names): live wrote no description.
	r.l.cfg.DefaultPortNames[16] = append(r.l.cfg.DefaultPortNames[16], "QSFP28 16")
	r.l.collect(context.Background())
	r.l.reconcile(context.Background())
	r.writes() // the capture's own drift under the push before is not this test's
	r.cycle("the push before")

	got := r.push11("system_cfg-dx010-routed-agg-1-aggregate.txt", "7a7d497f38cf4c5f")
	if !containsInOrder(got,
		"sudo config vlan add 4040", // the Inter-VLAN routing transit, in the device-wide apply
		"sudo config interface ip add Vlan4040 198.18.253.2/24",
		brkSave,
		"sudo config portchannel add PortChannel1 --fast-rate true --min-links 1",
		"sudo config vlan member del 4094 Ethernet56",
		"sudo config portchannel member add PortChannel1 Ethernet56",
		brkSave,
		"sudo config interface ip add PortChannel1 198.18.4.5/30",
		brkSave,
	) {
		t.Errorf("the routed aggregate wrote\n  %s", strings.Join(got, "\n  "))
	}
	for _, w := range got {
		if strings.Contains(w, "Ethernet60") {
			t.Errorf("the second member, in no key of the push, was written: %s", w)
		}
		if strings.HasPrefix(w, "sudo config vlan member add") && strings.Contains(w, "PortChannel1") {
			t.Errorf("the routed aggregate was put in a VLAN: %s", w)
		}
	}
	r.cycle("the routed aggregate")

	// The network removed (port 16 now carries the controller's default
	// name): the address first, then the member, the port back in VLAN
	// 4094, the PortChannel gone.
	got = r.push11("system_cfg-dx010-routed-agg-2-removed.txt", "58a7dae264a914e4")
	if !containsInOrder(got,
		sonicRIFWait("PortChannel1"),
		"sudo config interface ip remove PortChannel1 198.18.4.5/30 </dev/null",
		brkSave,
		"sudo config portchannel member del PortChannel1 Ethernet56",
		"sudo config vlan member add -u 4094 Ethernet56",
		"sudo config portchannel del PortChannel1",
		brkSave,
	) {
		t.Errorf("the routed aggregate removed wrote\n  %s", strings.Join(got, "\n  "))
	}
	r.cycle("the routed aggregate removed")

	// Port 16's name restored: the push before the run again, and nothing
	// to write ("0 of 32 ports changed" live).
	if got := r.push11("system_cfg-dx010-dot1x-8-off.txt", "2b37a0e11994ff97"); len(got) != 0 {
		t.Errorf("the name restored wrote %q", got)
	}
	r.cycle("the name restored")

	if s := r.log.String(); strings.Contains(s, "UNSUPPORTED") || strings.Contains(s, "not translated") {
		t.Errorf("a routing key was left unhandled:\n%s", s)
	}
}
