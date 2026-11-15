package proxmox

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The two lessons of 2026-11-15 from the sonic driver, held on the node
// captures (docs/drivers/proxmox.md §3c). A VLAN the controller stops
// naming for a port leaves that port's NIC options at the next apply, and
// nothing device-wide is created, so nothing is left to delete; mstpd
// settings are written to guest bridge members only, never to a bond, its
// legs or a plain uplink NIC.

func TestDroppedVLANLeavesThePort(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2.txt")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	mac, _ := vm100(t, "collect-node2.txt")
	steps := []struct {
		d    devicemodel.PortDesired
		want string // the NIC options written, "" when nothing is
	}{
		// Native 10 with 20 and 30 tagged.
		{devicemodel.PortDesired{Index: 1, Enabled: true, VLANSet: true, NativeVLAN: 10, TaggedVLANs: []int{20, 30}}, "virtio=" + mac + ",bridge=vmbr0,tag=10,trunks=20;30"},
		// The push stops naming 30 (a tagged set that drops it, or the
		// network deleted from the site): the trunks lose it.
		{devicemodel.PortDesired{Index: 1, Enabled: true, VLANSet: true, NativeVLAN: 10, TaggedVLANs: []int{20}}, "virtio=" + mac + ",bridge=vmbr0,tag=10,trunks=20"},
		// Block All: no trunks at all.
		{devicemodel.PortDesired{Index: 1, Enabled: true, VLANSet: true, NativeVLAN: 10}, "virtio=" + mac + ",bridge=vmbr0,tag=10"},
		// The same again writes nothing.
		{devicemodel.PortDesired{Index: 1, Enabled: true, VLANSet: true, NativeVLAN: 10}, ""},
	}
	for i, s := range steps {
		before := len(r.Commands)
		if _, err := c.ApplyPorts(context.Background(), []devicemodel.PortDesired{s.d}); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		got := r.Commands[before:]
		switch {
		case s.want == "" && len(got) != 0:
			t.Errorf("step %d wrote %v", i, got)
		case s.want != "" && (len(got) != 1 || got[0] != "qm set 100 --net0 '"+s.want+"'"):
			t.Errorf("step %d wrote %v, want qm set 100 --net0 '%s'", i, got, s.want)
		}
	}
	// The device's list shrinking writes nothing: the bridge's VLAN range
	// (bridge-vids) is the node's own network config, never created or
	// deleted by the driver.
	before := len(r.Commands)
	for _, ids := range [][]int{{1, 10, 20, 30}, {1, 10, 20}, {1}} {
		if n, err := c.EnsureVLANs(context.Background(), ids); err != nil || n != 0 {
			t.Errorf("EnsureVLANs(%v) = %d, %v", ids, n, err)
		}
	}
	if len(r.Commands) != before {
		t.Errorf("EnsureVLANs wrote %v", r.Commands[before:])
	}
}

// withMSTPBond is the node under mstpd with its uplink bond as the second
// STP port: the captured mstpd port entry, copied under the bond's name
// and made a non-edge root port, as an uplink is (the capture's scratch
// bridge had the tap alone).
func withMSTPBond(t *testing.T, bondMode string) string {
	t.Helper()
	ports := loadFixture(t, "mstpctl-showportdetail.json")
	entry := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(ports), "["), "]")
	bond := strings.NewReplacer(`"port":"tap100i0"`, `"port":"bond0"`, `"role":"Designated"`, `"role":"Root"`,
		`"admin-edge-port":"yes"`, `"admin-edge-port":"no"`, `"auto-edge-port":"yes"`, `"auto-edge-port":"no"`,
		`"oper-edge-port":"yes"`, `"oper-edge-port":"no"`).Replace(entry)
	s := withMSTP(t, "", "["+entry+","+bond+"]")
	if bondMode != "" {
		s = strings.Replace(s, "Bonding Mode: fault-tolerance (active-backup)", "Bonding Mode: "+bondMode, 1)
	}
	return s
}

func TestSTPNeverWritesTheUplink(t *testing.T) {
	for _, mode := range []string{"", "IEEE 802.3ad Dynamic link aggregation"} {
		r := &FixtureRunner{Fixture: withMSTPBond(t, mode)}
		c := NewCollector(r)
		c.ManageLLDP = false
		snap, err := c.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Both legs report the bond's STP port: the bond is the bridge
		// member, its legs are not.
		for _, idx := range []int{54, 53} {
			p := snap.Ports[idx-1]
			if p.STPEdge || p.STPPathCost != 2000 {
				t.Errorf("%q: port %d (%s) STP = edge %v cost %d, want the bond's", mode, idx, p.IfName, p.STPEdge, p.STPPathCost)
			}
		}
		if p := snap.Ports[53]; p.STPRole != "root" {
			t.Errorf("%q: the first leg's role = %s, want the bond's root", mode, p.STPRole)
		}
		// BPDU guard and edge pushed for every port, the legs included;
		// an aggregate over the legs when the bond is LACP.
		var desired []devicemodel.PortDesired
		for _, p := range snap.Ports {
			d := devicemodel.PortDesired{Index: p.Index, Enabled: p.Enabled, VLANSet: true, NativeVLAN: 1, TaggedAll: true, BPDUGuard: true}
			if mode != "" && (p.Index == 54 || p.Index == 53) {
				d.LAG = 1
			}
			desired = append(desired, d)
		}
		for cycle := 0; cycle < 2; cycle++ {
			if _, err := c.ApplyPorts(context.Background(), desired); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Collect(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		var stp []string
		for i, cmd := range r.Commands {
			if strings.HasPrefix(cmd, "mstpctl") {
				stp = append(stp, cmd+" "+r.Stdins[i])
			}
		}
		joined := strings.Join(stp, "\n")
		for _, name := range []string{"bond0", "ens1f0np0", "ens1f1np1", "eno1"} {
			if strings.Contains(joined, " "+name+" ") || strings.Contains(joined, " "+name+"\n") {
				t.Errorf("%q: mstpd was written for %s:\n%s", mode, name, joined)
			}
		}
		if !strings.Contains(joined, "setportbpduguard vmbr0 tap100i0 yes") {
			t.Errorf("%q: the guest port's BPDU guard was not written:\n%s", mode, joined)
		}
	}
}
