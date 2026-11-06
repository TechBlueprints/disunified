package sonic

import (
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// The controller's MAC rules (acl.go, docs/drivers/sonic.md §11.7.8) on an
// image whose orchagent has the L2 table type (the kit's sonic-swss 0003;
// FixtureRunner.OrchL2 "1", as macACLCheckCommand reads on it), driven by
// the ECS's captured push (controller-10.6.109/system_cfg-ecs-acl.txt: an
// Allow from one test MAC to another and a Block from a /40 of test MACs
// to Any, both on VLAN 4010). The rules expected are the ones the ECS
// itself wrote for that push (§11.7.3): RULE_N at 9999-N, each side as
// <mac>/<mask>, the VLAN as VLAN_ID, Any as no field.

func withL2(fr *FixtureRunner) { fr.OrchL2 = "1" }

// macACLsOf are the push's MAC groups as the loop hands them over (the
// loop's own translation is tested in internal/informloop).
func macACLsOf(cfg *unificfg.Config) []devicemodel.MACACL {
	var out []devicemodel.MACACL
	end := func(m unificfg.MACMatch) devicemodel.MACACLEnd {
		if m.MAC == "any" {
			return devicemodel.MACACLEnd{}
		}
		return devicemodel.MACACLEnd{MAC: m.MAC, Mask: m.Mask}
	}
	for _, a := range cfg.MACACLs {
		out = append(out, devicemodel.MACACL{Name: a.Name, Permit: a.Permit, VLAN: a.VLAN, Src: end(a.Src), Dst: end(a.Dst)})
	}
	return out
}

func macHset(rule, fields string) string {
	return "sudo sonic-db-cli CONFIG_DB hset 'ACL_RULE|DUI_MAC_ACL|" + rule + "' " + fields
}

func TestMACACLClaimFollowsTheImage(t *testing.T) {
	r := newACLRig(t, withL2)
	if caps := r.c.Capabilities(); caps.MACACLs != macACLRules || caps.IPACLs != ipACLRules {
		t.Errorf("capabilities = %+v: %d MAC and %d IP rules claimed on an image with the L2 table", caps, macACLRules, ipACLRules)
	}
	if !strings.Contains(r.log.String(), "orchagent has the L2 ACL table type: MAC ACL rules claimed (128)") {
		t.Errorf("Start logs the check:\n%s", r.log.String())
	}
	stock := newACLRig(t, nil)
	if caps := stock.c.Capabilities(); caps.MACACLs != 0 {
		t.Errorf("an image without the L2 table claims no MAC rules: %+v", caps)
	}
	if !strings.Contains(stock.log.String(), "orchagent has no L2 ACL table type") {
		t.Errorf("Start logs the check:\n%s", stock.log.String())
	}
}

func TestMACACLsFromTheECSPush(t *testing.T) {
	r := newACLRig(t, withL2)
	cfg := ecsPush(t, "system_cfg-ecs-acl.txt")
	macs := macACLsOf(cfg)
	if len(macs) != 2 {
		t.Fatalf("capture changed: %d MAC rules", len(macs))
	}
	allow := macHset("RULE_1", "DST_MAC '02:00:00:00:00:02/ff:ff:ff:ff:ff:ff' PACKET_ACTION 'FORWARD' PRIORITY '9998' SRC_MAC '02:00:00:00:00:01/ff:ff:ff:ff:ff:ff' VLAN_ID '4010'")
	block := macHset("RULE_2", "PACKET_ACTION 'DROP' PRIORITY '9997' SRC_MAC '02:00:00:00:00:00/ff:ff:ff:ff:ff:00' VLAN_ID '4010'")
	st := r.expectDevice("MAC rules", devicemodel.DeviceDesired{MACACLSet: true, MACACLs: macs}, []string{
		"sudo config acl add table DUI_MAC_ACL L2 -p " + dx010Ports() + " -s ingress",
		allow, block, save,
	})
	if s := st.aclStatus[macACLTable]; s != "Active" {
		t.Errorf("table status %q", s)
	}
	for _, rule := range []string{"RULE_1", "RULE_2"} {
		if s := st.aclStatus[macACLTable+"|"+rule]; s != "Active" {
			t.Errorf("%s status %q", rule, s)
		}
	}
	if !strings.Contains(r.log.String(), "acl: DUI_MAC_ACL 2 of 2 rules active") {
		t.Errorf("the collect after the write logs orchagent's verdict:\n%s", r.log.String())
	}

	// The same push with its IP rules: both tables, each its own type.
	ips := ipACLsOf(cfg)
	got := r.deviceCmds(devicemodel.DeviceDesired{IPACLSet: true, IPACLs: ips, MACACLSet: true, MACACLs: macs})
	if len(got) != 5 || got[0] != "sudo config acl add table DUI_IP_ACL L3 -p "+dx010Ports()+" -s ingress" {
		t.Errorf("IP rules beside the MAC rules wrote %q", got)
	}

	// The Allow deleted in the UI: the Block moves up a slot.
	st = r.expectDevice("first rule gone", devicemodel.DeviceDesired{IPACLSet: true, IPACLs: ips, MACACLSet: true, MACACLs: macs[1:]}, []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_MAC_ACL|RULE_2'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_MAC_ACL|RULE_1'",
		strings.Replace(strings.Replace(block, "RULE_2", "RULE_1", 1), "9997", "9998", 1),
		save,
	})
	// A push without MAC groups: the table goes, its rules first; the IP
	// table stays.
	st = r.expectDevice("no MAC rules", devicemodel.DeviceDesired{IPACLSet: true, IPACLs: ips, MACACLSet: true}, []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_MAC_ACL|RULE_1'",
		"sudo config acl remove table DUI_MAC_ACL",
		save,
	})
	if _, ok := st.aclTables[macACLTable]; ok || len(st.rulesOf(macACLTable)) != 0 {
		t.Errorf("left behind: tables %v rules %v", st.aclTables, st.aclRules)
	}
	if _, ok := st.aclTables[ipACLTable]; !ok {
		t.Errorf("the IP table went with the MAC one")
	}
}

// An image without the L2 table: the push's MAC rules are noted and nothing
// is written, even with the claim somehow made.
func TestMACACLsRefusedWithoutTheL2Table(t *testing.T) {
	r := newACLRig(t, nil)
	macs := macACLsOf(ecsPush(t, "system_cfg-ecs-acl.txt"))
	if got := r.deviceCmds(devicemodel.DeviceDesired{MACACLSet: true, MACACLs: macs}); len(got) != 0 {
		t.Errorf("wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "MAC ACL rules (2): this image's orchagent has no L2 table type") {
		t.Errorf("log:\n%s", r.log.String())
	}
}

// UniFi's VLAN 1 is the native_vlan stand-in; a rule naming the stand-in
// itself, or a side that is not a MAC, holds every MAC rule.
func TestMACACLVLANsAndHolds(t *testing.T) {
	r := newACLRig(t, withL2)
	other := devicemodel.MACACLEnd{MAC: "02:00:00:00:00:09", Mask: "ff:ff:ff:ff:ff:ff"}
	r.expectDevice("VLAN 1", devicemodel.DeviceDesired{MACACLSet: true, MACACLs: []devicemodel.MACACL{{Name: "v1", VLAN: 1, Src: other}}}, []string{
		"sudo config acl add table DUI_MAC_ACL L2 -p " + dx010Ports() + " -s ingress",
		macHset("RULE_1", "PACKET_ACTION 'DROP' PRIORITY '9998' SRC_MAC '02:00:00:00:00:09/ff:ff:ff:ff:ff:ff' VLAN_ID '4094'"),
		save,
	})
	for _, bad := range []devicemodel.MACACL{
		{Name: "native", VLAN: 4094, Src: other},
		{Name: "notmac", VLAN: 2, Src: devicemodel.MACACLEnd{MAC: "02:00:00:00:00", Mask: "ff:ff:ff:ff:ff:ff"}},
	} {
		if got := r.deviceCmds(devicemodel.DeviceDesired{MACACLSet: true, MACACLs: []devicemodel.MACACL{{Name: "ok", VLAN: 2, Src: other}, bad}}); len(got) != 0 {
			t.Errorf("%s: a held set wrote %q", bad.Name, got)
		}
	}
	for _, want := range []string{`rule 2 ("native"): VLAN 4094 is this switch's native_vlan`, `rule 2 ("notmac"): "02:00:00:00:00" is not a MAC address`} {
		if !strings.Contains(r.log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, r.log.String())
		}
	}
}

// A Block that would drop the switch's management traffic -- frames on the
// VLAN its address is on, from the gateway to the switch's MAC or to
// broadcast -- holds the set: the rules bind to the uplink too, and the
// push that would remove the rule could no longer arrive.
func TestMACACLManagementGuard(t *testing.T) {
	r := newACLRig(t, withL2)
	p := r.c.mgmtL2()
	if p.vlan != 4094 || len(p.peers) == 0 || len(p.dsts) != 2 {
		t.Fatalf("the capture's management path: %+v", p)
	}
	gw := devicemodel.MACACLEnd{MAC: p.peers[0].String()}
	own := devicemodel.MACACLEnd{MAC: p.dsts[0].String()}
	other := devicemodel.MACACLEnd{MAC: "02:00:00:00:00:09", Mask: "ff:ff:ff:ff:ff:ff"}
	held := []struct {
		name string
		acls []devicemodel.MACACL
	}{
		{"any to any", []devicemodel.MACACL{{Name: "all", VLAN: 1}}},
		{"from the gateway", []devicemodel.MACACL{{Name: "gw", VLAN: 1, Src: gw}}},
		{"to the switch", []devicemodel.MACACL{{Name: "own", VLAN: 1, Dst: own}}},
		{"an Allow for another source first", []devicemodel.MACACL{{Name: "allow", Permit: true, VLAN: 1, Src: other}, {Name: "all", VLAN: 1}}},
	}
	for _, h := range held {
		if got := r.deviceCmds(devicemodel.DeviceDesired{MACACLSet: true, MACACLs: h.acls}); len(got) != 0 {
			t.Errorf("%s: wrote %q", h.name, got)
		}
	}
	if !strings.Contains(r.log.String(), "would block frames from "+gw.MAC) {
		t.Errorf("log:\n%s", r.log.String())
	}
	passes := []struct {
		name string
		acls []devicemodel.MACACL
	}{
		{"another VLAN", []devicemodel.MACACL{{Name: "v2", VLAN: 2}}},
		{"another source", []devicemodel.MACACL{{Name: "src", VLAN: 1, Src: other}}},
		{"another destination", []devicemodel.MACACL{{Name: "dst", VLAN: 1, Dst: other}}},
	}
	// Every peer (the gateway and the bridge's own host) allowed first.
	var first []devicemodel.MACACL
	for _, peer := range p.peers {
		first = append(first, devicemodel.MACACL{Name: "peer", Permit: true, VLAN: 1, Src: devicemodel.MACACLEnd{MAC: peer.String()}})
	}
	passes = append(passes, struct {
		name string
		acls []devicemodel.MACACL
	}{"the peers allowed first", append(first, devicemodel.MACACL{Name: "rest", VLAN: 1})})
	if why := cutsManagementMAC(append(first[:1:1], devicemodel.MACACL{Name: "rest", VLAN: 1}), r.c.NativeVLAN, p); len(p.peers) > 1 && why == "" {
		t.Errorf("one peer allowed leaves the other blocked")
	}
	for _, ps := range passes {
		if why := cutsManagementMAC(ps.acls, r.c.NativeVLAN, p); why != "" {
			t.Errorf("%s: held: %s", ps.name, why)
		}
	}
	// Nothing known about the path: every VLAN and source counts, an Allow
	// only with source Any.
	unknown := l2Path{vlan: -1}
	if why := cutsManagementMAC([]devicemodel.MACACL{{Name: "src", VLAN: 2, Src: other}}, 4094, unknown); why == "" {
		t.Errorf("with no path known a Block to broadcast must hold")
	}
	if why := cutsManagementMAC([]devicemodel.MACACL{{Name: "a", Permit: true, VLAN: 2}, {Name: "b", VLAN: 2}}, 4094, unknown); why != "" {
		t.Errorf("an Allow Any first lets it through: %s", why)
	}
	// An address not on a VLAN (eth0): no VLAN_ID rule can match it.
	if why := cutsManagementMAC([]devicemodel.MACACL{{Name: "all", VLAN: 1}}, 4094, l2Path{}); why != "" {
		t.Errorf("an address off the VLANs: %s", why)
	}
}
