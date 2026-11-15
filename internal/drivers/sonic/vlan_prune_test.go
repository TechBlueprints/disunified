package sonic

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The DX010 as captured after the 2026-11-07 L3 run (collect-l3.txt):
// Vlan4094 and the ten site VLANs the 2026-11-02 Allow All run created,
// none with a member. A VLAN the controller's push no longer names is
// deleted once nothing the driver does not own is left in it
// (docs/drivers/sonic.md §11.29).

const pruneAll = "sudo config vlan del -m 2,8,9,10,69,1000,2000,3000,4000,4010"

func vlanDels(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if strings.HasPrefix(c, "sudo config vlan del ") {
			out = append(out, c)
		}
	}
	return out
}

// Nothing is deleted from a list the run has not been given: no push yet
// (EnsureVLANs), then no routing intent on an image that routes
// (ApplyDevice runs before the ports); with both, the ten go in one
// command, and the next apply writes nothing.
func TestPruneWaitsForThePushAndTheRouting(t *testing.T) {
	r := rig20261106(t, fixtureL3)
	ctx := context.Background()
	base := r.baseline()
	if _, err := r.c.ApplyPorts(ctx, base); err != nil {
		t.Fatal(err)
	}
	if d := vlanDels(r.fr.Commands); len(d) != 0 {
		t.Fatalf("deleted before any push named the VLANs: %q", d)
	}
	if _, err := r.c.EnsureVLANs(ctx, []int{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.ApplyPorts(ctx, base); err != nil {
		t.Fatal(err)
	}
	if d := vlanDels(r.fr.Commands); len(d) != 0 {
		t.Fatalf("deleted before the routing intent was known: %q", d)
	}
	if _, err := r.c.ApplyDevice(ctx, devicemodel.DeviceDesired{L3Set: true}); err != nil {
		t.Fatal(err)
	}
	r.fr.Commands = nil
	if _, err := r.c.ApplyPorts(ctx, base); err != nil {
		t.Fatal(err)
	}
	if got := r.fr.Commands; strings.Join(got, "\n") != pruneAll+"\n"+save {
		t.Fatalf("the push naming only VLAN 1 wrote\n  %s", strings.Join(got, "\n  "))
	}
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	r.fr.Commands = nil
	if _, err := r.c.ApplyPorts(ctx, base); err != nil {
		t.Fatal(err)
	}
	if got := r.fr.Commands; len(got) != 0 {
		t.Errorf("the apply after the delete wrote %q", got)
	}
	if !strings.Contains(r.fr.section("cfg_vlan"), "Vlan4094") || strings.Contains(r.fr.section("cfg_vlan"), "Vlan10\"") {
		t.Errorf("VLANs after the delete:\n%s", r.fr.section("cfg_vlan"))
	}
}

// What the push names stays; a VLAN a port the bridge does not write still
// carries stays (noted), and goes once that port leaves it, after the
// membership in the same apply.
func TestPruneKeepsAVLANAPortStillCarries(t *testing.T) {
	r := rig20261106(t, fixtureL3)
	ctx := context.Background()
	if _, err := r.c.ApplyDevice(ctx, devicemodel.DeviceDesired{L3Set: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.EnsureVLANs(ctx, []int{1, 10}); err != nil {
		t.Fatal(err)
	}
	tagged := edit(r.baseline(), 5, func(d *devicemodel.PortDesired) { d.TaggedVLANs = []int{10} })
	if _, err := r.c.ApplyPorts(ctx, tagged); err != nil {
		t.Fatal(err)
	}
	if got := r.fr.Commands; !containsInOrder(got, "sudo config vlan member add 10 Ethernet16", save, "sudo config vlan del -m 2,8,9,69,1000,2000,3000,4000,4010", save) {
		t.Fatalf("tagging VLAN 10 on port 5 wrote\n  %s", strings.Join(got, "\n  "))
	}

	// The push drops VLAN 10 while port 5 is outside control.ports (not in
	// desired): its membership is not the bridge's to remove, so the VLAN
	// stays.
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.EnsureVLANs(ctx, []int{1}); err != nil {
		t.Fatal(err)
	}
	var scoped []devicemodel.PortDesired
	for _, d := range r.baseline() {
		if d.Index != 5 {
			scoped = append(scoped, d)
		}
	}
	r.fr.Commands = nil
	if _, err := r.c.ApplyPorts(ctx, scoped); err != nil {
		t.Fatal(err)
	}
	if d := vlanDels(r.fr.Commands); len(d) != 0 {
		t.Fatalf("deleted a VLAN a port still carries: %q", d)
	}
	if !strings.Contains(r.log.String(), "VLAN 10 is no longer in the controller's push, but Ethernet16 still carries it") {
		t.Errorf("no note for the kept VLAN:\n%s", r.log.String())
	}

	// Port 5 written again, VLAN 1 only: the member goes, then the VLAN.
	r.fr.Commands = nil
	if _, err := r.c.ApplyPorts(ctx, r.baseline()); err != nil {
		t.Fatal(err)
	}
	if got := r.fr.Commands; !containsInOrder(got, "sudo config vlan member del 10 Ethernet16", save, "sudo config vlan del 10", save) {
		t.Errorf("port 5 leaving VLAN 10 wrote\n  %s", strings.Join(got, "\n  "))
	}
}

// An address the driver did not add (a hand-made one, or the management
// address on a VLAN other than the stand-in) keeps its VLAN; so does the
// native stand-in and the VLAN of a network the switch routes.
func TestPruneKeepsAddressesAndRoutedNetworks(t *testing.T) {
	r := rig20261106(t, fixtureL3)
	ctx := context.Background()
	if _, err := r.fr.Run(ctx, batchCommand, "set -e\nsudo config interface ip add Vlan2000 198.18.9.1/24\n"); err != nil {
		t.Fatal(err)
	}
	r.fr.Commands = nil
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	routed := devicemodel.DeviceDesired{L3Set: true, L3Interfaces: []devicemodel.L3Interface{{Name: "dui-routed", VLAN: 3000, Address: "198.18.3.1/24"}}}
	if _, err := r.c.ApplyDevice(ctx, routed); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.EnsureVLANs(ctx, []int{1}); err != nil {
		t.Fatal(err)
	}
	r.fr.Commands = nil
	if _, err := r.c.ApplyPorts(ctx, r.baseline()); err != nil {
		t.Fatal(err)
	}
	if d := vlanDels(r.fr.Commands); strings.Join(d, "\n") != "sudo config vlan del -m 2,8,9,10,69,1000,4000,4010" {
		t.Errorf("deleted %q; want every VLAN but 2000 (an address not the driver's), 3000 (routed) and 4094", d)
	}
	if !strings.Contains(r.log.String(), "VLAN 2000 is no longer in the controller's push, but Vlan2000 has an address the driver did not add (198.18.9.1/24)") {
		t.Errorf("no note for the addressed VLAN:\n%s", r.log.String())
	}
}

// An attached lane switch's lanes may carry a VLAN the device's push does
// not name: nothing is deleted until the lane switch has applied a push,
// and what its push names stays (EnsureLaneVLANs). Its list is kept apart:
// the device's own list is not replaced by it.
func TestPruneKeepsALaneSwitchsVLANs(t *testing.T) {
	r := rig20261106(t, fixtureL3)
	ctx := context.Background()
	if err := r.c.AttachLaneSwitch(2, devicemodel.Attachment{MAC: "02:00:00:00:00:aa", Name: "lanes"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.ApplyDevice(ctx, devicemodel.DeviceDesired{L3Set: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.EnsureVLANs(ctx, []int{1}); err != nil {
		t.Fatal(err)
	}
	base := r.baseline()
	r.fr.Commands = nil
	if _, err := r.c.ApplyPorts(ctx, base); err != nil {
		t.Fatal(err)
	}
	if d := vlanDels(r.fr.Commands); len(d) != 0 {
		t.Fatalf("deleted before the lane switch's push: %q", d)
	}
	if !strings.Contains(r.log.String(), "the lane switch on cage 2 has not applied a push yet") {
		t.Errorf("no note:\n%s", r.log.String())
	}
	if _, err := r.c.EnsureLaneVLANs(ctx, 2, []int{1, 69}); err != nil {
		t.Fatal(err)
	}
	if got := r.c.siteVLANs; len(got) != 0 {
		t.Errorf("the lane switch's list replaced the device's: %v", got)
	}
	r.fr.Commands = nil
	if _, err := r.c.ApplyPorts(ctx, base); err != nil {
		t.Fatal(err)
	}
	if d := vlanDels(r.fr.Commands); strings.Join(d, "\n") != "sudo config vlan del -m 2,8,9,10,1000,2000,3000,4000,4010" {
		t.Errorf("deleted %q; want every VLAN but 69 (the lane switch's) and 4094", d)
	}
}

// The fixture holds the CLI's refusals (config/vlan.py del_vlan): a VLAN
// with a member, an address or snooping on is refused, one at a time.
func TestFixtureVLANDelRefusals(t *testing.T) {
	r := rig20261106(t, fixtureL3)
	ctx := context.Background()
	for _, tc := range []struct{ cmd, want string }{
		{"sudo config vlan del 4094", "Vlan4094 cannot be removed: IGMP Snooping is enabled (disable first)"},
		{"sudo config vlan del 3", "Vlan3 does not exist"},
	} {
		if _, err := r.fr.Run(ctx, batchCommand, "set -e\n"+tc.cmd+"\n"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.cmd, err, tc.want)
		}
	}
	if _, err := r.fr.Run(ctx, batchCommand, "set -e\nsudo config vlan member add 10 Ethernet16\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fr.Run(ctx, batchCommand, "set -e\nsudo config vlan del -m 9,10\n"); err == nil || !strings.Contains(err.Error(), "VLAN ID 10 can not be removed. First remove all members assigned to this VLAN.") {
		t.Errorf("a VLAN with a member: %v", err)
	}
	if s := r.fr.section("cfg_vlan"); strings.Contains(s, "\"VLAN|Vlan9\"") || !strings.Contains(s, "\"VLAN|Vlan10\"") {
		t.Errorf("-m 9,10 refused at 10 should have deleted 9 first, as the CLI does:\n%s", s)
	}
}
