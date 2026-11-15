package sonic

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The VLANs follow the ports' MTU (docs/drivers/sonic.md §12). On the
// 202511 capture every port is at 1500 with jumbo off, while every VLAN --
// eleven, the native stand-in Vlan4094 among them -- has no mtu in
// CONFIG_DB (SONiC's 9100) and its kernel netdev is at the Bridge's 9100:
// the state that kept the on-switch bridge's inform from getting out.
func TestVLANsFollowThePortsMTU(t *testing.T) {
	r := rig202511(t, func(fr *FixtureRunner) {
		// One VLAN already at 1500, in CONFIG_DB and in the kernel.
		if err := fr.update("cfg_vlan", func(tb table) error {
			tb["VLAN|Vlan69"]["mtu"] = "1500"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := fr.kernelMTU("Vlan69", "1500"); err != nil {
			t.Fatal(err)
		}
	})
	if r.c.state.netMTU["Bridge"] != 9100 || r.c.state.netMTU["Vlan4094"] != 9100 || r.c.state.vlanMTU[4094] != "" {
		t.Fatalf("capture changed: Bridge %d, Vlan4094 %d, VLAN|Vlan4094 mtu %q", r.c.state.netMTU["Bridge"], r.c.state.netMTU["Vlan4094"], r.c.state.vlanMTU[4094])
	}
	mtu := func(v int) []devicemodel.PortDesired {
		ds := r.baseline()
		for i := range ds {
			ds[i].MTU = v
		}
		return ds
	}
	var want []string
	for _, id := range []string{"2", "8", "9", "10", "1000", "2000", "3000", "4000", "4010", "4094"} {
		want = append(want,
			"sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan"+id+"' mtu 1500",
			"sudo ip link set dev Vlan"+id+" mtu 1500")
	}
	r.expect("jumbo off, the VLANs behind", mtu(1500), append(want, save))
	if r.c.state.netMTU["Vlan4094"] != 1500 || r.c.state.vlanMTU[2] != "1500" {
		t.Errorf("read back: Vlan4094 %d, VLAN|Vlan2 mtu %q", r.c.state.netMTU["Vlan4094"], r.c.state.vlanMTU[2])
	}

	// No MTU in the push: the driver does not manage it, nothing is written.
	if got, _ := r.apply(r.baseline()); len(got) != 0 {
		t.Errorf("a push without an MTU wrote %q", got)
	}

	// A VLAN made while the ports run 1500 is made at 1500; its netdev
	// follows on the next apply, once a collect has seen it.
	n := len(r.fr.Commands)
	if _, err := r.c.EnsureVLANs(context.Background(), []int{1, 2, 5}); err != nil {
		t.Fatal(err)
	}
	if got, want := r.fr.Commands[n:], []string{"sudo config vlan add 5", "sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan5' mtu 1500", save}; !reflect.DeepEqual(got, want) {
		t.Errorf("EnsureVLANs:\n  got  %q\n  want %q", got, want)
	}
	r.expect("the new VLAN already at 1500", mtu(1500), nil)
}

// A VLAN only an attached lane switch lists (docs/drivers/sonic.md
// §11.29.4, docs/drivers/breakout.md §7). Over the 4x25G capture (every
// port and VLAN at 1500 with jumbo off), with vlanmgrd's netdev modelled:
// the lane switch's push makes Vlan77 at the lanes' 1500 in CONFIG_DB, the
// next collect sees its netdev at the Bridge's 9100, and whichever plan
// runs next brings it to 1500. A jumbo flip on the lane switch then moves
// its VLANs with its lanes, and one on the device moves the device's own:
// the native stand-in, which both carry, takes the lower of the two and
// is the device plan's alone. Until 2026-11-15 no plan listed Vlan77: its
// netdev stayed at 9100 and no flip reached either half.
func TestLaneOnlyVLANMTU(t *testing.T) {
	for _, first := range []string{"device", "lanes"} {
		t.Run(first+" plan first", func(t *testing.T) {
			r := laneRig(t)
			r.fr.VLANNetdevs = true
			ctx := context.Background()
			dev := func(m int) []devicemodel.PortDesired {
				ds := r.baseline()
				for i := range ds {
					ds[i].MTU = m
				}
				return ds
			}
			lanes := func(m int) []devicemodel.PortDesired {
				ds := r.laneBase()
				for i := range ds {
					ds[i].MTU = m
				}
				return ds
			}
			if _, err := r.c.EnsureVLANs(ctx, []int{1}); err != nil {
				t.Fatal(err)
			}
			r.expect("the device's push, jumbo off", dev(1500), nil)
			n := len(r.fr.Commands)
			if _, err := r.c.EnsureLaneVLANs(ctx, 2, []int{1, 77}); err != nil {
				t.Fatal(err)
			}
			if got, want := r.fr.Commands[n:], []string{"sudo config vlan add 77", "sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan77' mtu 1500", save}; !reflect.DeepEqual(got, want) {
				t.Errorf("EnsureLaneVLANs:\n  got  %q\n  want %q", got, want)
			}
			// The same cycle's lane plan: the netdev is not seen yet.
			n = len(r.fr.Commands)
			if _, err := r.c.ApplyLanes(ctx, 2, lanes(1500)); err != nil || len(r.fr.Commands) != n {
				t.Errorf("the lane switch's push wrote %q (%v)", r.fr.Commands[n:], err)
			}
			if _, err := r.c.Collect(ctx); err != nil {
				t.Fatal(err)
			}
			if got := r.c.state.netMTU["Vlan77"]; got != 9100 {
				t.Fatalf("the new VLAN's netdev is at %d, want vlanmgrd's 9100", got)
			}
			netdev := []string{"sudo ip link set dev Vlan77 mtu 1500", save}
			if first == "device" {
				r.expect("the device's next cycle", dev(1500), netdev)
				r.applyLanes("the lane switch's next cycle", lanes(1500), nil)
			} else {
				r.applyLanes("the lane switch's next cycle", lanes(1500), netdev)
				r.expect("the device's next cycle", dev(1500), nil)
			}

			// Jumbo on the lane switch: its lanes and its own VLAN; the native
			// stand-in stays at the device's 1500.
			r.applyLanes("jumbo on the lane switch", lanes(9216), []string{
				"sudo config interface mtu Ethernet4 9216", "sudo config interface mtu Ethernet5 9216",
				"sudo config interface mtu Ethernet6 9216", "sudo config interface mtu Ethernet7 9216",
				"sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan77' mtu 9216", "sudo ip link set dev Vlan77 mtu 9100", save,
			})
			r.expect("the device after the lane switch's flip", dev(1500), nil)

			// A VLAN the lane switch lists now is made at its lanes' 9216, not
			// at the device's 1500; its netdev at 9100 is already the cap.
			n = len(r.fr.Commands)
			if _, err := r.c.EnsureLaneVLANs(ctx, 2, []int{1, 77, 78}); err != nil {
				t.Fatal(err)
			}
			if got, want := r.fr.Commands[n:], []string{"sudo config vlan add 78", "sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan78' mtu 9216", save}; !reflect.DeepEqual(got, want) {
				t.Errorf("EnsureLaneVLANs at 9216:\n  got  %q\n  want %q", got, want)
			}
			if _, err := r.c.Collect(ctx); err != nil {
				t.Fatal(err)
			}
			r.applyLanes("the lane switch with Vlan78", lanes(9216), nil)

			// Jumbo on the device too: its ports, and now the native stand-in.
			got, _ := r.apply(dev(9216))
			if !slices.Contains(got, "sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan4094' mtu 9216") || !slices.Contains(got, "sudo ip link set dev Vlan4094 mtu 9100") || slices.ContainsFunc(got, func(c string) bool { return strings.Contains(c, "Vlan7") }) {
				t.Errorf("jumbo on the device wrote %q", got)
			}
			if _, err := r.c.Collect(ctx); err != nil {
				t.Fatal(err)
			}
			r.applyLanes("the lane switch after both flips", lanes(9216), nil)

			// Jumbo off on the lane switch alone: its VLAN, and the native
			// stand-in follows the lower at the device's next plan.
			r.applyLanes("jumbo off on the lane switch", lanes(1500), []string{
				"sudo config interface mtu Ethernet4 1500", "sudo config interface mtu Ethernet5 1500",
				"sudo config interface mtu Ethernet6 1500", "sudo config interface mtu Ethernet7 1500",
				"sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan77' mtu 1500", "sudo ip link set dev Vlan77 mtu 1500",
				"sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan78' mtu 1500", "sudo ip link set dev Vlan78 mtu 1500", save,
			})
			r.expect("the device after the lane switch went back", dev(9216), []string{
				"sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan4094' mtu 1500", "sudo ip link set dev Vlan4094 mtu 1500", save,
			})

			// The lane switch stops listing Vlan78: the device's plan deletes
			// it (vlan_prune.go; the capture's ten unlisted VLANs go too,
			// once the routing intent is known), and no plan sets an MTU on
			// it after.
			if _, err := r.c.ApplyDevice(ctx, devicemodel.DeviceDesired{L3Set: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.c.EnsureLaneVLANs(ctx, 2, []int{1, 77}); err != nil {
				t.Fatal(err)
			}
			r.expect("the device after Vlan78 left the lane switch's push", dev(9216), []string{"sudo config vlan del -m 2,8,9,10,69,78,1000,2000,3000,4000,4010", save})
			if _, ok := r.c.state.netMTU["Vlan78"]; ok || r.c.state.vlans[78] {
				t.Errorf("Vlan78 is still on the switch")
			}
			r.applyLanes("the lane switch after Vlan78 went", lanes(1500), nil)
		})
	}
}

// Before the device's own push has listed its VLANs, its plan guesses
// every VLAN on the switch (what Allow All may tag), but a VLAN the lane
// switch lists stays at the lane switch's MTU: the two plans do not undo
// each other.
func TestLaneVLANMTUBeforeTheDevicesList(t *testing.T) {
	r := laneRig(t)
	ctx := context.Background()
	if _, err := r.c.EnsureLaneVLANs(ctx, 2, []int{1, 69}); err != nil {
		t.Fatal(err)
	}
	lanes := r.laneBase()
	for i := range lanes {
		lanes[i].MTU = 9216
	}
	r.applyLanes("jumbo on the lane switch", lanes, []string{
		"sudo config interface mtu Ethernet4 9216", "sudo config interface mtu Ethernet5 9216",
		"sudo config interface mtu Ethernet6 9216", "sudo config interface mtu Ethernet7 9216",
		"sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan69' mtu 9216", "sudo ip link set dev Vlan69 mtu 9100", save,
	})
	dev := r.baseline()
	for i := range dev {
		dev[i].MTU = 1500
	}
	if got, _ := r.apply(dev); slices.ContainsFunc(got, func(c string) bool { return strings.Contains(c, "Vlan69") }) {
		t.Errorf("the device's plan, its list not yet pushed, wrote %q", got)
	}
}
