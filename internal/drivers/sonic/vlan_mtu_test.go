package sonic

import (
	"context"
	"reflect"
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
