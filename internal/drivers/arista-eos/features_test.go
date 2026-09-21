package aristaeos

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/switchmodel"
)

func f64(v float64) *float64 { return &v }

func TestFeatureStateFromFixtures(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p54 := portByIndex(t, snap, 54)
	if !p54.BPDUGuard || p54.FECConfig != switchmodel.FECRS {
		t.Errorf("port 54 config = bpduguard %v fec %q", p54.BPDUGuard, p54.FECConfig)
	}
	if p54.StormCtrl == nil || !pctEqual(p54.StormCtrl.BroadcastPct, f64(5)) || !pctEqual(p54.StormCtrl.MulticastPct, f64(1)) || p54.StormCtrl.UnknownUnicastPct != nil {
		t.Errorf("port 54 storm = %+v", p54.StormCtrl)
	}
	if p49 := portByIndex(t, snap, 49); p49.FECConfig != switchmodel.FECRS || p49.BPDUGuard || p49.StormCtrl != nil {
		t.Errorf("port 49 config = %+v %q %v", p49.StormCtrl, p49.FECConfig, p49.BPDUGuard)
	}
	if p2 := portByIndex(t, snap, 2); p2.FECConfig != "" || p2.StormCtrl != nil {
		t.Errorf("port 2 config = %q %+v", p2.FECConfig, p2.StormCtrl)
	}
	if snap.System.STPMode != "rstp" || snap.System.STPPriority != 32768 {
		t.Errorf("stp = %s %d", snap.System.STPMode, snap.System.STPPriority)
	}
	if snap.System.STPRoot != "02:00:00:00:00:28" {
		t.Errorf("stp root = %q (from show spanning-tree root detail)", snap.System.STPRoot)
	}
	if !snap.System.IGMPSnooping[1] || !snap.System.IGMPSnooping[10] {
		t.Errorf("igmp = %v", snap.System.IGMPSnooping)
	}
}

func TestApplyFeatureCommands(t *testing.T) {
	c, ft := startedCollector(t)
	rs, off := switchmodel.FECRS, switchmodel.FECDisabled
	n, err := c.ApplyPorts(context.Background(), []switchmodel.PortDesired{
		// 52: empty cage, nothing configured -> RS-FEC + storm + bpduguard
		{Index: 52, Enabled: true, FEC: &rs, StormCtrl: &switchmodel.StormControlSpec{BroadcastPct: f64(5), MulticastPct: f64(1)}, BPDUGuard: true},
		// 54: already exactly that -> no-op
		{Index: 54, Enabled: true, FEC: &rs, StormCtrl: &switchmodel.StormControlSpec{BroadcastPct: f64(5), MulticastPct: f64(1)}, BPDUGuard: true},
		// 49: FEC key absent -> untouched even though RS is configured
		{Index: 49, Enabled: true},
		// 51: UniFi says disabled -> explicit RS config removed
		{Index: 51, Enabled: true, FEC: &off},
	})
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v cmds=%v", n, err, ft.configured)
	}
	got := strings.Join(ft.configured[0], "\n")
	for _, want := range []string{
		"interface Ethernet52/1\nerror-correction encoding reed-solomon\nstorm-control broadcast level 5\nstorm-control multicast level 1\nspanning-tree bpduguard enable",
		"interface Ethernet51/1\nno error-correction encoding",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing:\n%s\nin:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Ethernet49/1") || strings.Contains(got, "Ethernet54/1") {
		t.Errorf("unexpected writes:\n%s", got)
	}
	// Removing storm control and bpduguard from 54.
	ft.configured = nil
	n, err = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{{Index: 54, Enabled: true, FEC: &rs}})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got = strings.Join(ft.configured[0], "\n")
	if !strings.Contains(got, "interface Ethernet54/1\nno storm-control broadcast\nno storm-control multicast\nno spanning-tree bpduguard") {
		t.Errorf("cmds:\n%s", got)
	}
}

func TestApplySwitchSettings(t *testing.T) {
	c, ft := startedCollector(t)
	// Same as the switch: no-op.
	n, err := c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{STPSet: true, STPEnabled: true, STPMode: "rstp", STPPriority: 32768})
	if err != nil || n != 0 {
		t.Fatalf("no-op: n=%d err=%v cmds=%v", n, err, ft.configured)
	}
	n, err = c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{
		STPSet: true, STPEnabled: true, STPMode: "rstp", STPPriority: 4096,
		IGMPSnooping: map[int]bool{1: true, 10: false, 69: true},
	})
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := strings.Join(ft.configured[0], "\n")
	if got != "enable\nconfigure\nspanning-tree priority 4096\nno ip igmp snooping vlan 10\nip igmp snooping vlan 69\nend\nwrite memory" {
		t.Errorf("cmds:\n%s", got)
	}
	n, _ = c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{STPSet: true, STPEnabled: false})
	if n != 1 || !strings.Contains(strings.Join(ft.configured[1], "\n"), "spanning-tree mode none") {
		t.Errorf("disable stp: n=%d cmds=%v", n, ft.configured)
	}
}

func TestApplyAggregationAndMirror(t *testing.T) {
	c, ft := startedCollector(t)
	n, err := c.ApplyPorts(context.Background(), []switchmodel.PortDesired{
		// LAG 7 does not exist on the fixture switch (Po1-4 do), so its
		// Port-Channel gets the VLAN config written.
		{Index: 2, Enabled: true, LAG: 7, VLANSet: true, NativeVLAN: 1, TaggedAll: true},
		{Index: 3, Enabled: true, LAG: 7, VLANSet: true, NativeVLAN: 1, TaggedAll: true},
		{Index: 50, Enabled: true, LAG: 7, VLANSet: true, NativeVLAN: 1, TaggedAll: true}, // breakout: every lane joins
		{Index: 4, Enabled: true, MirrorSource: 5},
	})
	if err != nil || n == 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := strings.Join(ft.configured[0], "\n")
	for _, want := range []string{
		"interface Ethernet2\nchannel-group 7 mode active",
		"interface Ethernet3\nchannel-group 7 mode active",
		"interface Ethernet50/1\nchannel-group 7 mode active",
		"interface Ethernet50/4\nchannel-group 7 mode active",
		"monitor session 4 source Ethernet5 both\nmonitor session 4 destination Ethernet4",
		"interface Port-Channel7\nswitchport mode trunk\nno switchport trunk native vlan\nswitchport trunk allowed vlan all",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing:\n%s\nin:\n%s", want, got)
		}
	}
	// Leaving the LAG and dropping the mirror.
	ft.configured = nil
	n, err = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{
		{Index: 2, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true},
		{Index: 4, Enabled: true},
	})
	if err != nil || n == 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got = strings.Join(ft.configured[0], "\n")
	if !strings.Contains(got, "no monitor session 4") || !strings.Contains(got, "interface Ethernet2\nno channel-group") {
		t.Errorf("cmds:\n%s", got)
	}
}

func TestLAGMembershipFromSummary(t *testing.T) {
	c, ft := newFixtureCollector(t)
	ft.overrides = map[string]string{"show port-channel summary": "show-port-channel-summary-lag.json"}
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The fixture captured ports 2 and 3 in Port-Channel1 (LACP active).
	for _, idx := range []int{2, 3} {
		if p := portByIndex(t, snap, idx); p.LAGID != 1 || p.LAG != "Port-Channel1" {
			t.Errorf("port %d lag = %q/%d", idx, p.LAG, p.LAGID)
		}
	}
	if p := portByIndex(t, snap, 4); p.LAGID != 0 {
		t.Errorf("port 4 lag = %d", p.LAGID)
	}
}

func TestApplyDHCPSnoopingIsIgnored(t *testing.T) {
	// EOS 4.26 cannot block rogue DHCP servers (no trusted-port snooping),
	// so the controller's key must write nothing, in either direction.
	c, ft := startedCollector(t)
	off, on := false, true
	for _, v := range []*bool{&on, &off, &on} {
		n, err := c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{DHCPSnooping: v})
		if err != nil || n != 0 || len(ft.configured) != 0 {
			t.Fatalf("dhcp snooping %v: n=%d err=%v cmds=%v", *v, n, err, ft.configured)
		}
	}
}
