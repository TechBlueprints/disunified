package sonic

import (
	"context"
	"log"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The DX010 on the custom 202511 image (docs/drivers/sonic.md 8), captured
// 2026-11-02 during the full retest (11.8): STATE_DB carries
// SWITCH_CAPABILITY|switch, eth0 has an address and no MGMT_PORT row, the
// site's VLANs exist, and cage 3 keeps `fec none` from the retest.
const fixture202511 = "../../../docs/fixtures/sonic-igmp-202511.20261101-22cac574e/collect.txt"

// rig202511 is the DX010 on the 202511 image with that build's own
// config-help.txt and zones.txt beside the capture.
func rig202511(t *testing.T, edit func(*FixtureRunner)) *controlRig {
	t.Helper()
	fr, err := NewFixtureRunner(fixture202511)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(fr)
	}
	var buf strings.Builder
	c := NewCollector(fr)
	c.Log = log.New(&buf, "", 0)
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &controlRig{t: t, c: c, fr: fr, log: &buf}
}

// §11.8: 202511 asks the SAI whether a port can be attached to a mirror
// session, and the DX010's SAI says no to both directions
// (PORT_INGRESS/EGRESS_MIRROR_CAPABLE 'false'). The CLI refuses the session;
// a row written past it (66bf6c8) made orchagent create the session object
// without ever binding the source port ("setUnsetPortMirror: Port ingress
// mirror is not supported by the ASIC"), leaking the object on removal. On
// that build the driver refuses, once, and writes nothing; on 202505, which
// has no flags and no gate, the CLI is still used (TestApplyMirror).
func TestMirrorRefusedWhereTheSAIHasNoPortMirror(t *testing.T) {
	r := rig202511(t, nil)
	if _, err := r.fr.Run(context.Background(), batchCommand, "set -e\nsudo config mirror_session span add dui4 Ethernet12 Ethernet16 both\n"); err == nil || !strings.Contains(err.Error(), "Error: Port mirror direction 'both' is not supported by the ASIC") {
		t.Fatalf("the capture must refuse the CLI add as 202511 does: %v", err)
	}
	base := r.baseline()
	if got, n := r.apply(base); len(got) != 0 || n != 0 {
		t.Fatalf("the unit as captured needs no write: %q", got)
	}
	ds := edit(base, 4, func(d *devicemodel.PortDesired) { d.MirrorSource = 5 })
	for i := 0; i < 2; i++ {
		if got, _ := r.apply(ds); len(got) != 0 {
			t.Fatalf("apply %d wrote %q", i+1, got)
		}
	}
	note := "port 4: mirroring port 5 is not possible on this switch"
	if n := strings.Count(r.log.String(), note); n != 1 {
		t.Errorf("note logged %d times: %s", n, r.log.String())
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := port(snap, 4); p.MirrorFrom != 0 || p.VLAN.NativeVLAN == 0 {
		t.Errorf("read back: mirror from %d, VLAN %+v (the destination keeps its VLAN)", p.MirrorFrom, p.VLAN)
	}
}

// §11.8: on the DX010 every breakout failed sonic-yang's validation of the
// whole CONFIG_DB on two dangling leafrefs: the STP trap's group (l2d now
// writes it, TestL2dWritesTheTrapGroupWithTheTrap) and MGMT_INTERFACE|eth0
// without a MGMT_PORT row, left by §10.6's hand fix. The driver adds the
// row before the breakout, once. (The row here has a gateway; the captured
// one has none, TestBreakoutRefusedWhileEthZeroHasNoGateway.)
func TestBreakoutAddsTheManagementPortRow(t *testing.T) {
	r := rig202511(t, withMgmtGateway)
	brk := "sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null"
	if _, err := r.fr.Run(context.Background(), batchCommand, "set -e\n"+brk+"\n"); err == nil || !strings.Contains(err.Error(), `Leafref "/sonic-mgmt_port:sonic-mgmt_port/sonic-mgmt_port:MGMT_PORT/sonic-mgmt_port:MGMT_PORT_LIST/sonic-mgmt_port:name" of value "eth0" points to a non-existing leaf`) {
		t.Fatalf("the capture must refuse the breakout as sonic-yang did: %v", err)
	}
	base := r.baseline()
	got, _ := r.apply(speedOn(base, 2, 25000))
	row := "sudo sonic-db-cli CONFIG_DB hset 'MGMT_PORT|eth0' alias eth0 admin_status up"
	if len(got) < 3 || got[0] != row || got[1] != brk || got[len(got)-1] != save {
		t.Fatalf("split: %q", got)
	}
	if !strings.Contains(r.log.String(), "port 2: eth0 has an address (MGMT_INTERFACE) and no MGMT_PORT row") {
		t.Errorf("no note: %s", r.log.String())
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := port(snap, 2); p.Lanes != 4 || p.AdminSpeed != 25000 {
		t.Errorf("read back: lanes %d speed %d", p.Lanes, p.AdminSpeed)
	}
	if again, _ := r.apply(speedOn(base, 2, 25000)); len(again) != 0 {
		t.Errorf("a second apply wrote %q", again)
	}
	// Back to one 100G lane: the row is there, nothing is added again.
	got, _ = r.apply(speedOn(base, 2, 100000))
	if len(got) == 0 || got[0] != "sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null" {
		t.Errorf("join: %q", got)
	}
}

// withMgmtGateway gives the captured eth0 address a gateway of its own
// family, the shape sonic-mgmt_interface.yang accepts.
func withMgmtGateway(fr *FixtureRunner) {
	fr.update("cfg_mgmt", func(tb table) error {
		tb["MGMT_INTERFACE|eth0|169.254.1.1/16"] = map[string]string{"gwaddr": "169.254.0.1"}
		return nil
	})
	ntpSourceCleared(fr) // the NTP apply runs before the ports (TestNTPSourceVlanCleared)
}

// §11.8, after the deploy: with the MGMT_PORT row in place the breakout
// still failed, on sonic-yang's rule that every MGMT_INTERFACE row carries
// a gateway of its own family; §10.6's `config interface ip add eth0
// 169.254.1.1/16` wrote {"NULL": "NULL"}. A gateway there is not the
// driver's to invent (it becomes a default route on eth0 that the syslog
// servers' forced rules would follow), so the breakout is refused with one
// note and nothing is written: no retry loop, no half-made rows.
func TestBreakoutRefusedWhileEthZeroHasNoGateway(t *testing.T) {
	withRow := rig202511(t, func(fr *FixtureRunner) {
		fr.update("cfg_mgmt", func(tb table) error {
			tb["MGMT_PORT|eth0"] = map[string]string{"alias": "eth0", "admin_status": "up"}
			return nil
		})
	})
	brk := "sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null"
	if _, err := withRow.fr.Run(context.Background(), batchCommand, "set -e\n"+brk+"\n"); err == nil || !strings.Contains(err.Error(), `Must condition "(contains(current(), ':') and contains(../gwaddr, ':')) or (contains(current(), '.') and contains(../gwaddr, '.'))" not satisfied. (path: /sonic-mgmt_interface:sonic-mgmt_interface/MGMT_INTERFACE/MGMT_INTERFACE_LIST[name='eth0'][ip_prefix='169.254.1.1/16']/ip_prefix)`) {
		t.Fatalf("the capture must refuse the breakout as sonic-yang did live: %v", err)
	}
	r := rig202511(t, nil)
	base := r.baseline()
	for i := 0; i < 2; i++ {
		if got, _ := r.apply(speedOn(base, 2, 25000)); len(got) != 0 {
			t.Fatalf("apply %d wrote %q", i+1, got)
		}
	}
	note := "port 2: 25000 Mb/s needs breakout mode 4x25G[10G]"
	if n := strings.Count(r.log.String(), note); n != 1 {
		t.Errorf("note logged %d times: %s", n, r.log.String())
	}
	if !strings.Contains(r.log.String(), "eth0's address 169.254.1.1/16 (MGMT_INTERFACE) has no gateway") {
		t.Errorf("note does not name the row: %s", r.log.String())
	}
}

// §11.8: with a gateway on eth0's row, sonic-yang's next refusal was the
// driver's own NTP|global src_intf Vlan4094 (sonic-ntp.yang allows no VLAN
// there). The NTP apply now clears it; a VLAN left there still refuses the
// breakout with a note and no write.
func TestBreakoutRefusedWhileNTPSourcesFromAVlan(t *testing.T) {
	r := rig202511(t, func(fr *FixtureRunner) {
		withMgmtGateway(fr)
		fr.update("cfg_mgmt", func(tb table) error {
			tb["MGMT_PORT|eth0"] = map[string]string{"alias": "eth0", "admin_status": "up"}
			return nil
		})
		fr.update("cfg_ntp_global", func(tb table) error {
			tb["NTP|global"]["src_intf"] = "Vlan4094"
			return nil
		})
	})
	brk := "sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null"
	if _, err := r.fr.Run(context.Background(), batchCommand, "set -e\n"+brk+"\n"); err == nil || !strings.Contains(err.Error(), `libyang[0]: Invalid value "Vlan4094" in "src_intf" element. (path: /sonic-ntp:sonic-ntp/NTP/global/src_intf[.='Vlan4094'])`) {
		t.Fatalf("the capture must refuse the breakout as sonic-yang does: %v", err)
	}
	if got, _ := r.apply(speedOn(r.baseline(), 2, 25000)); len(got) != 0 {
		t.Fatalf("wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "refuses NTP|global src_intf Vlan4094") {
		t.Errorf("no note: %s", r.log.String())
	}
}
