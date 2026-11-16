package informloop

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// A VLAN only the lane switch's push lists, end to end
// (docs/drivers/sonic.md §11.29.4, docs/drivers/breakout.md §7). The
// parent is the 4x25G capture (every port and VLAN at 1500, jumbo off)
// before Vlan69 was made: the CLI's own delete of it, no member, runs on the
// fixture first, and vlanmgrd's netdev is modelled (a new Vlan<id> at the
// Bridge's 9100). The parent's push (the split) lists VLAN 1 alone; the
// lane switch's (the Flex XG's, captured) lists 2..4010, Vlan69 among them.
// The lane switch's push makes Vlan69 at its lanes' 1500, the parent's next
// cycle brings the netdev to 1500, and the lane switch's jumbo, flipped in
// its own push, moves its VLANs with its lanes and leaves the native
// stand-in (the parent's) alone. Before 2026-11-15 the netdev stayed at
// 9100 and the flip reached neither half.
func TestReplayLaneSwitchVLANMTU(t *testing.T) {
	r := newLaneRigWith(t, func(fr *sonic.FixtureRunner) {
		fr.VLANNetdevs = true
		if _, err := fr.Run(context.Background(), "bash -s", "set -e\nsudo config vlan del 69\necho disunified-batch-done\n"); err != nil {
			t.Fatal(err)
		}
	})
	push := readECSPush(t, "system_cfg-flexxg-aggregate.txt")
	got := r.pushChild("system_cfg-flexxg-aggregate.txt", "d4a1a0cac0f5bb83")
	wantWrites(t, "the lane switch's push (its head)", got[:3], []string{
		"sudo config vlan add 69", "sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan69' mtu 1500", brkSave,
	})
	r.parent.collect(context.Background())
	r.parent.reconcile(context.Background())
	wantWrites(t, "the parent's next cycle", r.writes(), []string{"sudo ip link set dev Vlan69 mtu 1500", brkSave})
	r.cycleChild("the new VLAN's netdev")
	r.cycleParent("the new VLAN's netdev")

	laneOnly := []string{"2", "8", "9", "10", "69", "1000", "2000", "3000", "4000", "4010"}
	vlanWrites := func(cmds []string) []string {
		var out []string
		for _, c := range cmds {
			if strings.Contains(c, "'VLAN|Vlan") || strings.Contains(c, "ip link set dev Vlan") {
				out = append(out, c)
			}
		}
		return out
	}
	flip := func(on bool, ver string) {
		t.Helper()
		text := push
		cfg, kernel := "1500", "1500"
		if on {
			text = strings.Replace(push, "switch.jumboframes=disabled", "switch.jumboframes=enabled", 1)
			cfg, kernel = "9216", "9100"
		}
		got := r.pushChildText(text, ver)
		if !slices.Contains(got, "sudo sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel201' mtu "+cfg) {
			t.Errorf("jumbo %v: the lane aggregate did not move: %q", on, got)
		}
		var want []string
		for _, id := range laneOnly {
			want = append(want, "sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan"+id+"' mtu "+cfg, "sudo ip link set dev Vlan"+id+" mtu "+kernel)
		}
		wantWrites(t, "the lane switch's jumbo", vlanWrites(got), want)
		r.cycleParent("the lane switch's jumbo")
		r.cycleChild("the lane switch's jumbo")
	}
	flip(true, "0000000000000011")
	flip(false, "0000000000000012")
}
