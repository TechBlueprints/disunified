package sonic

import (
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The DX010 as captured before the port-feature claims (2026-11-07,
// docs/drivers/sonic.md 11.11): no isolation group, no provisioned FDB
// entry, no shaper, and lldpd sending the LLDP-MED TLV on no port.
const fixturePortFeat = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-portfeat.txt"

func medOff(ds []devicemodel.PortDesired) []devicemodel.PortDesired {
	off := false
	for i := range ds {
		ds[i].LLDPMED = &off
	}
	return ds
}

// Two ports at one rate share one profile; a third rate makes a second; a
// profile goes only once no port names it (the fixture refuses deleting a
// referenced one, as qosorch would leave it).
func TestEgressShapersShareAProfile(t *testing.T) {
	r := rig20261106(t, fixturePortFeat)
	base := medOff(r.baseline())
	if got, _ := r.apply(base); len(got) != 0 {
		t.Fatalf("baseline wrote %q", got)
	}
	ds := edit(edit(base, 30, func(d *devicemodel.PortDesired) { d.EgressKbps = 250000 }), 31, func(d *devicemodel.PortDesired) { d.EgressKbps = 250000 })
	r.expect("two at 250M", ds, []string{
		"sudo sonic-db-cli CONFIG_DB hset 'SCHEDULER|DUI_EGRESS_250000' meter_type bytes pir 31250000 pbs 312500",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT_QOS_MAP|Ethernet116' scheduler DUI_EGRESS_250000",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT_QOS_MAP|Ethernet120' scheduler DUI_EGRESS_250000",
		save})
	ds = edit(ds, 31, func(d *devicemodel.PortDesired) { d.EgressKbps = 1000 })
	r.expect("one moves to 1M", ds, []string{
		"sudo sonic-db-cli CONFIG_DB hset 'SCHEDULER|DUI_EGRESS_1000' meter_type bytes pir 125000 pbs 18432",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT_QOS_MAP|Ethernet120' scheduler DUI_EGRESS_1000",
		save})
	snap := r.expect("both off", base, []string{
		"sudo sonic-db-cli CONFIG_DB hdel 'PORT_QOS_MAP|Ethernet116' scheduler",
		"sudo sonic-db-cli CONFIG_DB hdel 'PORT_QOS_MAP|Ethernet120' scheduler",
		"sudo sonic-db-cli CONFIG_DB del 'SCHEDULER|DUI_EGRESS_1000'",
		"sudo sonic-db-cli CONFIG_DB del 'SCHEDULER|DUI_EGRESS_250000'",
		save})
	if p := port(snap, 30); p.EgressKbps != 0 {
		t.Errorf("port 30 reads back %d kbps", p.EgressKbps)
	}
}

// learn_mode is in no YANG model, so a breakout -- which validates all of
// CONFIG_DB first -- would be refused while any port is secured: the rows
// leave for the breakout and come back after it.
func TestBreakoutHoldsLearnMode(t *testing.T) {
	r := rig20261106(t, fixturePortFeat)
	base := medOff(r.baseline())
	secure := edit(base, 30, func(d *devicemodel.PortDesired) {
		d.PortSecurity, d.AllowedMACs = true, []string{"02:00:00:00:00:01"}
	})
	if got, _ := r.apply(secure); len(got) == 0 {
		t.Fatal("securing port 30 wrote nothing")
	}
	got, _ := r.apply(speedOn(secure, 2, 25000))
	if !containsInOrder(got,
		"sudo sonic-db-cli CONFIG_DB hdel 'PORT|Ethernet116' learn_mode",
		"sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet116' learn_mode drop") {
		t.Errorf("the breakout under port security wrote %q\n%s", got, r.log.String())
	}
	if again, _ := r.apply(speedOn(secure, 2, 25000)); len(again) != 0 {
		t.Errorf("a second apply wrote %q", again)
	}
}

// A secured aggregate member: the learn mode goes on its PortChannel
// (teammgrd hands PORTCHANNEL learn_mode to the LAG), the FDB entries name
// the PortChannel.
func TestPortSecurityOnAnAggregate(t *testing.T) {
	r := rig20261106(t, fixturePortFeat)
	base := medOff(r.baseline())
	lag := edit(edit(base, 30, func(d *devicemodel.PortDesired) { d.LAG = 30 }), 31, func(d *devicemodel.PortDesired) { d.LAG = 30 })
	if got, _ := r.apply(lag); len(got) == 0 {
		t.Fatal("the aggregate wrote nothing")
	}
	sec := func(d *devicemodel.PortDesired) { d.PortSecurity, d.AllowedMACs = true, []string{"02:00:00:00:00:01"} }
	got, _ := r.apply(edit(edit(lag, 30, sec), 31, sec))
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "sudo sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel30' learn_mode drop") ||
		strings.Contains(joined, "'PORT|Ethernet116' learn_mode") ||
		!strings.Contains(joined, `{"op":"set","table":"FDB_TABLE","key":"Vlan4094:02:00:00:00:00:01","fv":["port","PortChannel30","type","static"]}`) {
		t.Errorf("port security on the aggregate wrote %q", got)
	}
}

// A provisioned FDB entry is an allowed MAC, not a station: on a port with
// no link it is not reported (it showed as a connected client, live).
func TestStaticFDBOnADownPortIsNotAClient(t *testing.T) {
	fr, err := NewFixtureRunner(fixturePortFeat)
	if err != nil {
		t.Fatal(err)
	}
	fdb, _ := parseTable("fdb", sections(fr.Fixture)["fdb"])
	var learnt string
	for k := range fdb {
		learnt = k
		break
	}
	_, rest, _ := strings.Cut(learnt, "|")
	vlan, _, _ := strings.Cut(rest, ":")
	fdb["FDB_TABLE|"+vlan+":02:00:00:00:00:01"] = map[string]string{"port": "Ethernet116", "type": "static"}
	byIf := map[string]int{"Ethernet116": 30, fdb[learnt]["port"]: 1}
	macs := buildMACTable(fdb, byIf, 4094, func(p string) bool { return p == "Ethernet116" })
	for _, m := range macs {
		if m.MAC == "02:00:00:00:00:01" {
			t.Errorf("the allowed MAC on a down port is reported: %+v", m)
		}
	}
	if len(macs) != len(fdb)-1 {
		t.Errorf("%d entries reported of %d learnt", len(macs), len(fdb)-1)
	}

}

// containsInOrder: each of want appears in got, in that order.
func containsInOrder(got []string, want ...string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}
