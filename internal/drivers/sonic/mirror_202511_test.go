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

// §11.8: 202511's `config mirror_session span add` asks the SAI whether the
// port can mirror in the direction asked, and the DX010's SAI says no to
// both (PORT_INGRESS/EGRESS_MIRROR_CAPABLE 'false') while programming SPAN
// sessions all the same. On that build the driver writes the row itself;
// on 202505, which has no flags and no gate, the CLI is still used
// (TestApplyMirror).
func TestMirrorWrittenDirectWhereTheCLIRefuses(t *testing.T) {
	r := rig202511(t, nil)
	if _, err := r.fr.Run(context.Background(), batchCommand, "set -e\nsudo config mirror_session span add dui4 Ethernet12 Ethernet16 both\n"); err == nil || !strings.Contains(err.Error(), "Error: Port mirror direction 'both' is not supported by the ASIC") {
		t.Fatalf("the capture must refuse the CLI add as 202511 does: %v", err)
	}
	base := r.baseline()
	if got, n := r.apply(base); len(got) != 0 || n != 0 {
		t.Fatalf("the unit as captured needs no write: %q", got)
	}
	ds := edit(base, 4, func(d *devicemodel.PortDesired) { d.MirrorSource = 5 })
	snap := r.expect("mirror 4 <- 5", ds, []string{
		"sudo config vlan member del 4094 Ethernet12",
		"sudo sonic-db-cli CONFIG_DB hset 'MIRROR_SESSION|dui4' type SPAN dst_port Ethernet12 src_port Ethernet16 direction BOTH",
		save,
	})
	if p := port(snap, 4); p.MirrorFrom != 5 || p.VLAN.NativeVLAN != 0 {
		t.Errorf("read back: mirror from %d, VLAN %+v", p.MirrorFrom, p.VLAN)
	}
	r.expect("mirror off", base, []string{
		"sudo config mirror_session remove dui4",
		"sudo config vlan member add -u 4094 Ethernet12",
		save,
	})
}

// §11.8: on the DX010 every breakout failed sonic-yang's validation of the
// whole CONFIG_DB on two dangling leafrefs: the STP trap's group (l2d now
// writes it, TestL2dWritesTheTrapGroupWithTheTrap) and MGMT_INTERFACE|eth0
// without a MGMT_PORT row, left by §10.6's hand fix. The driver adds the
// row before the breakout, once.
func TestBreakoutAddsTheManagementPortRow(t *testing.T) {
	r := rig202511(t, nil)
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
