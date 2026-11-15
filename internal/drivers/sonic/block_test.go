package sonic

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// UniFi's client Block (block.go, docs/drivers/sonic.md §11.33): the
// blocked clients the DX010's own setparams carried on Network 11.0.99
// while a test client was blocked beside the site's one
// (controller-11.0.99/replies-dx010-block.ndjson, scrubbed), written
// into DUI_MAC_ACL as the ECS writes its MB_<mac> rule, beside the ECS's
// captured MAC rules, and dropped in the kernel through DUI_MAC_BLOCK.

// capturedBlocked is the DX010's blocked_sta as the session reads it, at
// its longest (both clients blocked).
func capturedBlocked(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "docs", "fixtures", "controller-11.0.99", "replies-dx010-block.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	var longest []string
	for sc.Scan() {
		var rec struct {
			Payload struct {
				BlockedSTA *string `json:"blocked_sta"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Payload.BlockedSTA != nil {
			if macs := device.ParseBlockedSTA(*rec.Payload.BlockedSTA); len(macs) > len(longest) {
				longest = macs
			}
		}
	}
	if len(longest) != 2 {
		t.Fatalf("capture changed: blocked %q", longest)
	}
	return longest
}

func newBlockRig(t *testing.T) *controlRig {
	t.Helper()
	return newACLRig(t, func(fr *FixtureRunner) { withL2(fr); fr.UseSection("mac_block", "") })
}

const blockCmd = "sudo bash -s -- disunified-mac-block"

func blockHset(mac string) string {
	return macHset("MB_"+mac, "PACKET_ACTION 'DROP' PRIORITY '1023' SRC_MAC '"+mac+"'")
}

func TestBlockedClientWrittenAsTheECSDoes(t *testing.T) {
	r := newBlockRig(t)
	macs := capturedBlocked(t)
	d := devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: macs}
	st := r.expectDevice("block", d, []string{
		"sudo config acl add table DUI_MAC_ACL L2 -p " + dx010Ports() + " -s ingress",
		blockHset(macs[0]), blockHset(macs[1]),
		save, blockCmd,
	})
	for _, m := range macs {
		if s := st.aclStatus[macACLTable+"|MB_"+m]; s != "Active" {
			t.Errorf("rule %s status %q", m, s)
		}
	}
	// The kernel drops the CPU's copy: one script, the chain and its hook.
	if len(r.fr.BlockScripts) != 1 || !reflect.DeepEqual(r.fr.BlockMACs, macs) {
		t.Fatalf("kernel scripts %q, chain %q", r.fr.BlockScripts, r.fr.BlockMACs)
	}
	want := "set -e\n" +
		"ebtables -N DUI_MAC_BLOCK -P RETURN 2>/dev/null || ebtables -F DUI_MAC_BLOCK\n" +
		"ebtables -A DUI_MAC_BLOCK -s " + macs[0] + " -j DROP\n" +
		"ebtables -A DUI_MAC_BLOCK -s " + macs[1] + " -j DROP\n" +
		"ebtables -L INPUT | grep -q -- '^-j DUI_MAC_BLOCK *$' || ebtables -I INPUT 1 -j DUI_MAC_BLOCK\n" +
		"install -d -m 755 /run/disunified\n" +
		"echo " + blockHash(macs) + " > /run/disunified/mac-block\n"
	if r.fr.BlockScripts[0] != want {
		t.Errorf("kernel script\n%s\nwant\n%s", r.fr.BlockScripts[0], want)
	}
	// A reboot clears the chain and its marker: the next apply writes it again.
	r.fr.UseSection("mac_block", "")
	r.collect()
	if got := r.deviceCmds(d); !reflect.DeepEqual(got, []string{blockCmd}) {
		t.Errorf("after a lost marker: %q, want only the kernel chain again", got)
	}
	if len(r.fr.BlockScripts) != 2 {
		t.Errorf("a lost marker did not rewrite the chain: %d scripts", len(r.fr.BlockScripts))
	}

	// The test client unblocked: its rule goes, the chain follows.
	r.expectDevice("unblock one", devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: macs[1:]}, []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_MAC_ACL|MB_" + macs[0] + "'",
		save, blockCmd,
	})
	if !reflect.DeepEqual(r.fr.BlockMACs, macs[1:]) {
		t.Errorf("chain after one unblock %q", r.fr.BlockMACs)
	}
	// None blocked: the rule and the table go, then the chain.
	r.expectDevice("unblock", devicemodel.DeviceDesired{BlockedSet: true}, []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_MAC_ACL|MB_" + macs[1] + "'",
		"sudo config acl remove table DUI_MAC_ACL",
		save, blockCmd,
	})
	last := r.fr.BlockScripts[len(r.fr.BlockScripts)-1]
	if r.fr.BlockMACs != nil || !strings.Contains(last, "ebtables -X DUI_MAC_BLOCK") {
		t.Errorf("chain after the unblock %q, script\n%s", r.fr.BlockMACs, last)
	}
}

// The ECS's MAC rules and a block share the table; each half is kept as
// the switch has it while the other changes, and the block is not counted
// against the MAC rules' limit.
func TestBlockedClientBesideTheMACRules(t *testing.T) {
	r := newBlockRig(t)
	macs := capturedBlocked(t)
	rules := macACLsOf(ecsPush(t, "system_cfg-ecs-acl.txt"))
	both := devicemodel.DeviceDesired{MACACLSet: true, MACACLs: rules, BlockedSet: true, BlockedClients: macs}
	got := r.deviceCmds(both)
	if len(got) != 7 || got[3] != blockHset(macs[0]) || got[4] != blockHset(macs[1]) || got[6] != blockCmd {
		t.Fatalf("rules and block wrote %q", got)
	}
	r.collect()
	// The controller's rules held (an untranslatable rule) or not yet in
	// this push: the RULE_s stay, the block follows.
	if got := r.deviceCmds(devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: macs}); len(got) != 0 {
		t.Errorf("MAC rules not set: wrote %q", got)
	}
	// No setparam yet (a restart before the session knew): the block stays
	// while the rules change.
	st := r.expectDevice("rules change, block unknown", devicemodel.DeviceDesired{MACACLSet: true, MACACLs: rules[1:]}, []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_MAC_ACL|RULE_2'",
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_MAC_ACL|RULE_1'",
		macHset("RULE_1", "PACKET_ACTION 'DROP' PRIORITY '9998' SRC_MAC '02:00:00:00:00:00/ff:ff:ff:ff:ff:00' VLAN_ID '4010'"),
		save,
	})
	if _, ok := st.rulesOf(macACLTable)["MB_"+macs[1]]; !ok {
		t.Errorf("the block went with the rules' change: %v", st.rulesOf(macACLTable))
	}
	// No MAC rules left, the client still blocked: the table stays for it.
	r.expectDevice("rules gone", devicemodel.DeviceDesired{MACACLSet: true, BlockedSet: true, BlockedClients: macs}, []string{
		"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_MAC_ACL|RULE_1'",
		save,
	})
}

// A block of the switch's own gateway would cut it off from the
// controller: held with a note, the other blocks written.
func TestBlockedGatewayIsHeld(t *testing.T) {
	r := newBlockRig(t)
	gw := r.system().GatewayMAC
	if gw == "" {
		t.Fatal("capture changed: no gateway MAC")
	}
	macs := append(capturedBlocked(t), gw)
	got := r.deviceCmds(devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: macs})
	for _, c := range got {
		if strings.Contains(c, gw) {
			t.Errorf("the gateway was blocked: %q", c)
		}
	}
	if len(got) != 5 || got[1] != blockHset(macs[0]) || got[2] != blockHset(macs[1]) {
		t.Errorf("wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "blocked client "+gw+": it is on the switch's management path") {
		t.Errorf("no note for the gateway:\n%s", r.log.String())
	}
}

// Without the kit's L2 table type the block is noted, not written.
func TestBlockedClientWithoutTheL2Table(t *testing.T) {
	r := newACLRig(t, nil)
	if got := r.deviceCmds(devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: capturedBlocked(t)}); len(got) != 0 {
		t.Errorf("wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "blocked clients (2): this image's orchagent has no L2 table type") {
		t.Errorf("no note:\n%s", r.log.String())
	}
}
