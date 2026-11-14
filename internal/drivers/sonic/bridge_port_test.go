package sonic

import (
	"context"
	"log"
	"slices"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The DX010 on igmp-202511.20261106-f2e25f54c, 2026-11-14 17:30 UTC
// (docs/drivers/sonic.md §8.15): snooping on Vlan4094 with the reserved
// groups' static members on every port, and cage 9 (Ethernet32) stranded
// since a native-VLAN move an hour before -- its bridge port admin down in
// ASIC_DB (asic_bridge_port), every other port's up.
const fixtureStranded = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-stranded.txt"

func strandedRig(t *testing.T, edit func(*FixtureRunner)) *controlRig {
	t.Helper()
	fr, err := NewFixtureRunner(fixtureStranded)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(fr)
	}
	var buf strings.Builder
	c := NewCollector(fr)
	c.Log = log.New(&buf, "", 0)
	c.L2 = false
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &controlRig{t: t, c: c, fr: fr, log: &buf}
}

// cage 13 (Ethernet48) moved from the Default network to VLAN 4000, as the
// UI's native-network change on cage 9 did.
func moveCage13(r *controlRig) []devicemodel.PortDesired {
	return edit(r.baseline(), 13, func(d *devicemodel.PortDesired) { d.NativeVLAN = 4000 })
}

// The capture reads as it is: cage 9's bridge port down, reported once.
func TestStrandedBridgePortIsRead(t *testing.T) {
	r := strandedRig(t, nil)
	r.c.mu.Lock()
	down := sortedKeysOf(r.c.state.bridgeDown)
	r.c.mu.Unlock()
	if !slices.Equal(down, []string{"Ethernet32"}) {
		t.Fatalf("bridge ports down = %q, want Ethernet32", down)
	}
	if n := strings.Count(r.log.String(), "Ethernet32's bridge port is held admin down"); n != 1 {
		t.Errorf("warned %d times, want once: %s", n, r.log.String())
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(r.log.String(), "Ethernet32's bridge port is held admin down"); n != 1 {
		t.Errorf("warned again on the next collect")
	}
	if !strings.Contains(r.log.String(), "lacks the image kit's sonic-swss 0004") {
		t.Errorf("the image's orchagent not reported: %s", r.log.String())
	}
}

// Without sonic-swss 0004 a port leaving its last VLAN waits for its bridge
// port to go before the next VLAN is added: the statics first (the CLI's
// refusal), the removal, the wait, then the add -- and nothing is stranded.
func TestNativeMoveWaitsForTheBridgePort(t *testing.T) {
	r := strandedRig(t, nil)
	got, _ := r.apply(moveCage13(r))
	static := slices.IndexFunc(got, func(s string) bool {
		return strings.HasPrefix(s, "sudo sonic-db-cli CONFIG_DB del 'L2MC_STATIC_MEMBER|Vlan4094|") && strings.HasSuffix(s, "|Ethernet48'")
	})
	del := slices.Index(got, "sudo config vlan member del 4094 Ethernet48")
	wait := slices.Index(got, bridgePortWait("Ethernet48"))
	add := slices.Index(got, "sudo config vlan member add -u 4000 Ethernet48")
	if static < 0 || del < 0 || wait < 0 || add < 0 || !(static < del && del < wait && wait < add) {
		t.Fatalf("order: statics %d, del %d, wait %d, add %d in %q", static, del, wait, add, got)
	}
	if len(r.fr.Stranded) != 0 {
		t.Errorf("stranded %q", r.fr.Stranded)
	}
	if got, _ := r.apply(moveCage13(r)); len(got) != 0 {
		t.Errorf("second apply wrote %q", got)
	}
}

// The model catches the order the driver used before §8.15: the same
// commands without the wait strand the port.
func TestMoveWithoutTheWaitStrands(t *testing.T) {
	r := strandedRig(t, nil)
	got, _ := r.apply(moveCage13(r))
	var cmds []string
	for _, c := range got {
		if !strings.HasPrefix(c, "python3 -c ") && !strings.HasPrefix(c, "sudo config save") && c != l2mcCommand {
			cmds = append(cmds, c)
		}
	}
	r = strandedRig(t, nil)
	r.baseline()
	if err := r.c.runBatch(context.Background(), cmds[:slices.Index(cmds, "sudo config vlan member add -u 4000 Ethernet48")+1]); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.fr.Stranded, []string{"Ethernet48"}) {
		t.Errorf("stranded = %q, want Ethernet48", r.fr.Stranded)
	}
}

// With sonic-swss 0004 orchagent drops the port's members with its VLAN
// membership: no wait, nothing stranded.
func TestPatchedOrchNeedsNoWait(t *testing.T) {
	r := strandedRig(t, func(fr *FixtureRunner) { fr.OrchL2MCFix = "1" })
	if strings.Contains(r.log.String(), "lacks the image kit's sonic-swss 0004") {
		t.Errorf("a patched orchagent reported as unpatched")
	}
	got, _ := r.apply(moveCage13(r))
	if slices.Contains(got, bridgePortWait("Ethernet48")) {
		t.Errorf("waited on a patched orchagent: %q", got)
	}
	if !slices.Contains(got, "sudo config vlan member add -u 4000 Ethernet48") || len(r.fr.Stranded) != 0 {
		t.Errorf("move not made cleanly: %q, stranded %q", got, r.fr.Stranded)
	}
}

// A port whose L2MC count leaked can never lose its bridge port: the wait
// fails the batch with the reason before the add would strand it.
func TestLeakedCountFailsTheWait(t *testing.T) {
	r := strandedRig(t, func(fr *FixtureRunner) { fr.L2MCLeaked = map[string]bool{"Ethernet48": true} })
	_, err := r.c.ApplyPorts(context.Background(), moveCage13(r))
	if err == nil || !strings.Contains(err.Error(), "has not removed its bridge port") {
		t.Fatalf("err = %v, want the wait's refusal", err)
	}
	if len(r.fr.Stranded) != 0 || slices.Contains(r.fr.Commands, "sudo config vlan member add -u 4000 Ethernet48") {
		t.Errorf("the add ran: %q", r.fr.Commands)
	}
}

// A port already stranded keeps its VLANs: every membership change would
// land on the refused bridge port. The note says why.
func TestStrandedPortIsLeftAlone(t *testing.T) {
	r := strandedRig(t, nil)
	ds := edit(r.baseline(), 9, func(d *devicemodel.PortDesired) { d.NativeVLAN = 4000 })
	got, _ := r.apply(ds)
	for _, c := range got {
		if strings.Contains(c, "Ethernet32") {
			t.Errorf("wrote %q for the stranded port", c)
		}
	}
	if !strings.Contains(r.log.String(), "Ethernet32's bridge port is held admin down in the chip (orchagent refused its removal") {
		t.Errorf("no note: %s", r.log.String())
	}
}
