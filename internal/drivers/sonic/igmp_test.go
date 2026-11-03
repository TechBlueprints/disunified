package sonic

import (
	"context"
	"log"
	"slices"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The DX010 on the IGMP image (igmp-202511.20261102-e53777446) captured
// during §8.14's hardware test, 2026-11-03: l2mcd enabled, test VLAN 3999
// snooping (v3, querier and fast leave on, optimised flood disabled) on
// Ethernet4 and Ethernet8, a static group 239.1.2.3 on Ethernet4, and
// Ethernet4 out of Vlan4094 (the MAC-loopback step).
const fixtureSnooping = "../../../docs/fixtures/sonic-igmp-202511.20261102-e53777446/collect-snooping.txt"

func snoopingRig(t *testing.T, igmp bool) *controlRig {
	t.Helper()
	fr, err := NewFixtureRunner(fixtureSnooping)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	c := NewCollector(fr)
	c.Log = log.New(&buf, "", 0)
	c.L2 = false // spanning tree is l2's tests'
	c.IGMP = igmp
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &controlRig{t: t, c: c, fr: fr, log: &buf}
}

// ecsSnooping is the ECS Aggregation's snooping settings as the controller
// pushed them (controller-10.6.109/system_cfg-ecs-aggregation.txt, parsed in
// unificfg's TestParseECSIGMPSnooping): VLANs 1, 2 and 69 snoop, the
// querier on everywhere, VLAN 1 naming the gateway, VLAN 69 keeping unknown
// multicast to the router ports; the other networks off.
func ecsSnooping() devicemodel.DeviceDesired {
	yes := true
	vlan := func(addr string, flood bool) devicemodel.IGMPVLAN {
		f := flood
		return devicemodel.IGMPVLAN{Querier: true, QuerierAddress: addr, MaxResponse: 10, Membership: 260, RouterExpiry: 180, FloodUnknown: &f}
	}
	return devicemodel.DeviceDesired{
		IGMPSnooping:         map[int]bool{1: true, 2: true, 8: false, 9: false, 10: false, 69: true, 1000: false, 2000: false, 3000: false, 4000: false, 4010: false},
		IGMP:                 map[int]devicemodel.IGMPVLAN{1: vlan("192.0.2.1", true), 2: vlan("", true), 69: vlan("", false)},
		IGMPFloodReserved:    &yes,
		IGMPUnknownToRouters: &yes,
	}
}

// Off by default (options.igmp, until §8.9 H4's traffic test), and only on
// an image with the series whose tables the collector reads.
func TestIGMPClaimIsGated(t *testing.T) {
	off := snoopingRig(t, false)
	if caps := off.c.Capabilities(); caps.IGMPSnooping || caps.IGMPSnoopingAdvanced {
		t.Errorf("claimed with options.igmp off: %+v", caps)
	}
	if got := off.deviceCmds(ecsSnooping()); len(got) != 0 {
		t.Errorf("options.igmp off wrote %q", got)
	}
	if !strings.Contains(off.log.String(), "IGMP snooping requested; options.igmp is off; not written") {
		t.Errorf("no log for the refused push: %s", off.log.String())
	}
	if slices.Contains(off.fr.Helps, "sudo config feature --help") {
		t.Errorf("Start checked config feature with snooping off: %v", off.fr.Helps)
	}

	on := snoopingRig(t, true)
	if caps := on.c.Capabilities(); !caps.IGMPSnooping || !caps.IGMPSnoopingAdvanced {
		t.Errorf("not claimed with options.igmp on: %+v", caps)
	}
	if !slices.Contains(on.fr.Helps, "sudo config feature --help") {
		t.Errorf("Start did not check config feature: %v", on.fr.Helps)
	}
	if got := on.c.last.System.IGMPSnooping; len(got) != 1 || !got[3999] {
		t.Errorf("reported snooping = %v, want VLAN 3999", got)
	}

	// The image before cfg_l2mc joined collect.py: l2mcd there, its tables
	// unread -- never claimed, never written.
	fr, err := NewFixtureRunner(fixture202511)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	c.Log, c.IGMP = log.New(&strings.Builder{}, "", 0), true
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if caps := c.Capabilities(); caps.IGMPSnooping {
		t.Errorf("claimed without the L2MC tables: %+v", caps)
	}
}

// The ECS's settings on the DX010: one row per snooping VLAN with every
// field l2mcd reads (version 3, the query interval from the membership
// time), the seven reserved groups on every member of a snooping VLAN, the
// test VLAN's rows gone (the push does not name it), optimised flood never
// written, and the keys without an l2mcd field logged. A second apply
// writes nothing.
func TestIGMPApplyECSSettings(t *testing.T) {
	r := snoopingRig(t, true)
	got := r.deviceCmds(ecsSnooping())
	if want := []string{l2mcCommand, "sudo config save -y"}; !slices.Equal(got, want) {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	if len(r.fr.L2MCScripts) != 1 {
		t.Fatalf("%d scripts", len(r.fr.L2MCScripts))
	}
	script := r.fr.L2MCScripts[0]
	// The CLI's order: the test VLAN's statics, then its suppression row,
	// then its L2MC row; the new rows before their statics.
	order := []string{
		`\"key\":\"L2MC_STATIC_MEMBER|Vlan3999|239.1.2.3|Ethernet4\"}`,
		`\"key\":\"L2MC_STATIC_GROUP|Vlan3999|239.1.2.3\"}`,
		`\"key\":\"L2MC_SUPPRESS|Vlan3999\"}`,
		`\"key\":\"L2MC|Vlan3999\"}`,
		`\"key\":\"L2MC|Vlan4094\",\"fields\":{\"enabled\":\"true\",\"fast-leave\":\"false\",\"last-member-query-interval\":\"1000\",\"querier\":\"true\",\"query-interval\":\"125\",\"query-max-response-time\":\"10\",\"version\":\"3\"}`,
		`\"key\":\"L2MC_STATIC_GROUP|Vlan4094|239.255.255.250\"`,
		`\"key\":\"L2MC_STATIC_MEMBER|Vlan4094|224.0.1.1|Ethernet0\",\"fields\":{\"port\":\"Ethernet0\"}`,
	}
	at := -1
	for _, s := range order {
		i := strings.Index(script, s)
		if i < 0 {
			t.Fatalf("script lacks %s:\n%s", s, script)
		}
		if i < at {
			t.Errorf("%s out of order", s)
		}
		at = i
	}
	tbl := r.fr.table("cfg_l2mc")
	for _, v := range []string{"Vlan2", "Vlan69", "Vlan4094"} {
		if tbl["L2MC|"+v]["version"] != "3" {
			t.Errorf("L2MC|%s = %v", v, tbl["L2MC|"+v])
		}
	}
	if tbl["L2MC|Vlan3999"] != nil || len(tbl) != 3+7+7*31 {
		// Vlan4094 holds 31 ports in the capture (Ethernet4 left it); Vlan2
		// and Vlan69 have no member, so no static groups.
		t.Errorf("cfg_l2mc has %d rows: %v", len(tbl), tbl)
	}
	if g := tbl["L2MC_STATIC_GROUP|Vlan4094|224.0.1.129"]["static-members@"]; !strings.HasPrefix(g, "Ethernet0,Ethernet8,Ethernet12,") || strings.Contains(g, "Ethernet4,") {
		t.Errorf("PTP group members %q", g)
	}
	for k := range tbl {
		if strings.HasPrefix(k, "L2MC_SUPPRESS|") {
			t.Errorf("%s written: optimised flood is refused by this SAI", k)
		}
	}
	logText := r.log.String()
	for _, want := range []string{
		"IGMP VLAN 1: querier address 192.0.2.1 has no l2mcd field; queries leave from Vlan4094's own",
		"IGMP VLAN 2: querier on and Vlan2 has no address; queries leave from 0.0.0.0",
		"IGMP: the multicast-router expiry (VLANs 1, 2, 69) has no l2mcd setting; not written",
		"IGMP: unknown multicast to the router ports only (VLANs 69) is refused by this switch's SAI",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("log lacks %q:\n%s", want, logText)
		}
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.System.IGMPSnooping; len(got) != 3 || !got[1] || !got[2] || !got[69] {
		t.Errorf("reported snooping %v", got)
	}
	if got := r.deviceCmds(ecsSnooping()); len(got) != 0 {
		t.Errorf("second apply wrote %q", got)
	}
	// Snooping off everywhere: every row goes, the feature stays on (the
	// ECS's way).
	none := ecsSnooping()
	for id := range none.IGMPSnooping {
		none.IGMPSnooping[id] = false
	}
	if got := r.deviceCmds(none); !slices.Equal(got, []string{l2mcCommand, "sudo config save -y"}) {
		t.Errorf("snooping off wrote %q", got)
	}
	if tbl := r.fr.table("cfg_l2mc"); len(tbl) != 0 {
		t.Errorf("rows left: %v", tbl)
	}
	if st := r.fr.table("cfg_feature")["FEATURE|l2mcd"]["state"]; st != "enabled" {
		t.Errorf("l2mcd %s", st)
	}
}

// The feature goes on with the first snooping VLAN (the image ships it
// disabled).
func TestIGMPEnablesTheFeature(t *testing.T) {
	r := snoopingRig(t, true)
	if err := r.fr.featureState("l2mcd", "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := r.deviceCmds(ecsSnooping())
	if want := []string{"sudo config feature state l2mcd enabled", l2mcCommand, "sudo config save -y"}; !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

// The CLI refuses to take a port out of a VLAN while a static group names
// it there. The port plan drops the port's statics first -- here the test
// VLAN's group on Ethernet4, which the baseline takes out of Vlan3999 --
// and with snooping managed the statics follow the membership after the
// batch: Ethernet4 back in Vlan4094 gets the reserved groups.
func TestIGMPStaticsFollowVLANMembership(t *testing.T) {
	r := snoopingRig(t, false)
	got, _ := r.apply(r.baseline())
	del := slices.Index(got, "sudo sonic-db-cli CONFIG_DB del 'L2MC_STATIC_MEMBER|Vlan3999|239.1.2.3|Ethernet4'")
	member := slices.Index(got, "sudo config vlan member del 3999 Ethernet4")
	if del < 0 || member < 0 || del > member {
		t.Fatalf("static member not dropped before the VLAN removal: %q", got)
	}

	r = snoopingRig(t, true)
	r.deviceCmds(ecsSnooping())
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ = r.apply(r.baseline())
	if !slices.Contains(got, l2mcCommand) {
		t.Fatalf("no IGMP write after the ports: %q", got)
	}
	tbl := r.fr.table("cfg_l2mc")
	if tbl["L2MC_STATIC_MEMBER|Vlan4094|239.255.255.250|Ethernet4"] == nil || len(tbl) != 3+7+7*32 {
		t.Errorf("Ethernet4 not in the reserved groups (%d rows)", len(tbl))
	}
	// Ethernet8 moves to VLAN 2 untagged: its Vlan4094 statics go before
	// the removal, and VLAN 2 (snooping) gains it.
	ds := edit(r.baseline(), 3, func(d *devicemodel.PortDesired) { d.NativeVLAN = 2 })
	got, _ = r.apply(ds)
	first := slices.IndexFunc(got, func(s string) bool { return strings.Contains(s, "|Ethernet8'") })
	member = slices.Index(got, "sudo config vlan member del 4094 Ethernet8")
	if first < 0 || member < 0 || first > member {
		t.Fatalf("Ethernet8's statics not dropped first: %q", got)
	}
	tbl = r.fr.table("cfg_l2mc")
	if tbl["L2MC_STATIC_MEMBER|Vlan2|224.0.1.1|Ethernet8"] == nil || tbl["L2MC_STATIC_MEMBER|Vlan4094|224.0.1.1|Ethernet8"] != nil {
		t.Errorf("statics did not follow Ethernet8 to VLAN 2")
	}
	if got, _ := r.apply(ds); len(got) != 0 {
		t.Errorf("second apply wrote %q", got)
	}
}
