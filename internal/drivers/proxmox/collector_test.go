package proxmox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// fixtureRunner serves a captured collector run and records every other
// command (the writes).
type fixtureRunner struct {
	fixture string
	cmds    []string
	fail    bool
}

func (f *fixtureRunner) Run(_ context.Context, command, stdin string) (string, error) {
	if strings.HasPrefix(command, "bash -s") {
		return f.fixture, nil
	}
	f.cmds = append(f.cmds, command)
	return "", nil
}

func (f *fixtureRunner) Close() error { return nil }

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "fixtures", "proxmox-9.1.6", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestCollector(t *testing.T, fixture string) (*Collector, *fixtureRunner) {
	t.Helper()
	r := &fixtureRunner{fixture: loadFixture(t, fixture)}
	c := NewCollector(r)
	pm, err := loadPortMap(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.ports = pm
	c.cycleDelay = time.Millisecond
	return c, r
}

// vm100 returns VM 100's scrubbed MAC and name from the fixture (the
// scrubber numbers MACs by first appearance, so they move when sections
// are added).
func vm100(t *testing.T, fixture string) (mac, name string) {
	t.Helper()
	sec := sections(loadFixture(t, fixture))
	nics, _ := parseGuestConfigs(sec["qemu"], "qemu")
	for _, n := range nics {
		if n.VMID == 100 && n.Index == 0 {
			// The MAC as written in the config line (case preserved by renderNICOptions).
			raw := strings.SplitN(n.Raw, ",", 2)[0]
			return strings.TrimPrefix(raw, "virtio="), n.Name
		}
	}
	t.Fatal("VM 100 not in fixture")
	return "", ""
}

func portByIf(t *testing.T, snap *switchmodel.Snapshot, ifname string) switchmodel.Port {
	t.Helper()
	for _, p := range snap.Ports {
		if p.IfName == ifname {
			return p
		}
	}
	t.Fatalf("no port %s", ifname)
	return switchmodel.Port{}
}

func TestCollectNode2(t *testing.T) {
	c, _ := newTestCollector(t, "collect-node2.txt")
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Ports) != 32 {
		t.Fatalf("ports = %d, want 32", len(snap.Ports))
	}
	sys := snap.System
	if sys.Vendor != "Proxmox" || sys.Version != "9.1.6" || sys.Hostname != "proxmox-2" || sys.MAC == "" {
		t.Errorf("system = %+v", sys)
	}
	if !sys.HasTemperature || sys.TemperatureC < 40 || len(sys.Fans) != 4 || !sys.Fans[0].OK {
		t.Errorf("sensors: temp %v fans %+v", sys.TemperatureC, sys.Fans)
	}
	if sys.STPMode != "disabled" || sys.STPPriority != 32768 || !sys.IGMPSnooping[1] {
		t.Errorf("stp/igmp: %+v", sys)
	}
	if sys.MemTotalKB == 0 || sys.MemUsedKB == 0 || sys.Uptime < time.Hour {
		t.Errorf("mem/uptime: %+v", sys)
	}

	// Guest ports are numbered cluster-wide in (vmid, net) order.
	p1 := snap.Ports[0]
	if p1.IfName != "vm100-net0" || p1.Index != 1 || !p1.Up || p1.SpeedMbps != 100000 || p1.Media != switchmodel.MediaQSFP28 {
		t.Errorf("port 1 = %+v", p1)
	}
	if p1.Counters.RxBytes == 0 || len(p1.MACs) != 1 || p1.MACs[0].PortIndex != 1 {
		t.Errorf("port 1 counters/macs = %+v %+v", p1.Counters, p1.MACs)
	}
	if !p1.VLAN.AllowAll || p1.VLAN.NativeVLAN != 1 {
		t.Errorf("port 1 vlan = %+v", p1.VLAN)
	}
	// VM 119 lives on proxmox-1: known here, down here, with its config VLANs.
	p := portByIf(t, snap, "vm119-net0")
	if p.Up || !p.Present || p.VLAN.Mode != "access" || p.VLAN.NativeVLAN != 8 || p.Interfaces != nil {
		t.Errorf("vm119-net0 = %+v", p)
	}
	if !strings.HasSuffix(p.Description, " net0") {
		t.Errorf("multi-NIC guest label = %q", p.Description)
	}
	// A firewall-enabled guest's MACs are learned on its fwpr port.
	p = portByIf(t, snap, "vm120-net0")
	if !p.Up || len(p.MACs) != 1 || p.Interfaces[0] != "tap120i0" {
		t.Errorf("vm120-net0 = %+v", p)
	}
	// Unassigned guest slots are empty cages; the last two are the NICs.
	if e := snap.Ports[25]; e.Present || e.IfName != "" || e.Index != 26 {
		t.Errorf("empty slot = %+v", e)
	}
	u := snap.Ports[30]
	if u.IfName != "ens1f0np0" || u.Index != 31 || !u.Up || u.SpeedMbps != 100000 || u.LAG != "bond0" || u.Optic == nil || u.Optic.Part != "QSFP-100G-CU2M" {
		t.Errorf("uplink = %+v", u)
	}
	if len(u.SpeedCaps) == 0 || u.SpeedCaps[len(u.SpeedCaps)-1] != 100000 || !u.FECCapable || len(u.MACs) < 50 {
		t.Errorf("uplink caps/macs = %v %v %d", u.SpeedCaps, u.FECCapable, len(u.MACs))
	}
	if u.Health.LinkChanges == 0 {
		t.Errorf("uplink carrier changes not read")
	}
	if snap.UplinkHint != 31 || snap.UplinkPort() != 31 {
		t.Errorf("uplink hint = %d", snap.UplinkHint)
	}
	if u2 := snap.Ports[31]; u2.IfName != "ens1f1np1" || u2.Media != switchmodel.MediaSFP28 || u2.SpeedMbps != 10000 || len(u2.MACs) != 0 {
		t.Errorf("second uplink = %+v", u2)
	}
	if len(snap.VLANs) < 3 || snap.VLANs[0] != 1 {
		t.Errorf("vlans = %v", snap.VLANs)
	}
	if got := c.DeviceName(snap.System); got != "pve-proxmox-2" {
		t.Errorf("device name = %q", got)
	}
	if _, name := vm100(t, "collect-node2.txt"); c.PortName(p1) != "100 "+name {
		t.Errorf("port name = %q", c.PortName(p1))
	}
}

func TestCollectNode1TaggedGuest(t *testing.T) {
	c, _ := newTestCollector(t, "collect-node1.txt")
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := portByIf(t, snap, "vm119-net0")
	if !p.Up || p.VLAN.Mode != "access" || p.VLAN.NativeVLAN != 8 || p.Interfaces[0] != "tap119i0" {
		t.Errorf("vm119-net0 = %+v", p)
	}
	// The same guest gets the same port on every node.
	c2, _ := newTestCollector(t, "collect-node2.txt")
	snap2, _ := c2.Start(context.Background())
	if portByIf(t, snap2, "vm119-net0").Index != p.Index {
		t.Errorf("port numbering differs between nodes")
	}
}

func TestPortMapIsStable(t *testing.T) {
	dir := t.TempDir()
	pm, _ := loadPortMap(dir)
	now := time.Now()
	got, _ := pm.assign([]string{"qemu/100/net0", "qemu/101/net0", "qemu/102/net0"}, 5, now)
	if got["qemu/100/net0"] != 1 || got["qemu/102/net0"] != 3 {
		t.Fatalf("seed = %v", got)
	}
	if err := pm.save(); err != nil {
		t.Fatal(err)
	}
	// 101 deleted, 99 created: 99 gets a never-used slot, not 101's.
	pm2, _ := loadPortMap(dir)
	got, changed := pm2.assign([]string{"qemu/99/net0", "qemu/100/net0", "qemu/102/net0"}, 5, now.Add(time.Minute))
	if !changed || got["qemu/99/net0"] != 4 || got["qemu/100/net0"] != 1 {
		t.Fatalf("after delete/create = %v", got)
	}
	// With every slot used, the longest-released one is reused.
	got, _ = pm2.assign([]string{"qemu/99/net0", "qemu/100/net0", "qemu/102/net0", "qemu/103/net0", "qemu/104/net0"}, 5, now.Add(2*time.Minute))
	if got["qemu/104/net0"] != 2 {
		t.Fatalf("reuse = %v", got)
	}
}

func TestApplyPortsWritesOnlyDiffs(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2.txt")
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	all := func(idx int) switchmodel.PortDesired {
		return switchmodel.PortDesired{Index: idx, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true}
	}
	// Everything already matches: nothing written.
	var desired []switchmodel.PortDesired
	for _, p := range snap.Ports {
		if p.IfName != "" && !strings.HasPrefix(p.IfName, "ens") && p.VLAN.AllowAll {
			desired = append(desired, all(p.Index))
		}
	}
	n, err := c.ApplyPorts(context.Background(), desired)
	if err != nil || n != 0 || len(r.cmds) != 0 {
		t.Fatalf("converged apply: n=%d err=%v cmds=%v", n, err, r.cmds)
	}
	// Disable port 1 (VM 100 on this node) and put it on native 10 + tagged 20,30.
	d := switchmodel.PortDesired{Index: 1, Enabled: false, VLANSet: true, NativeVLAN: 10, TaggedVLANs: []int{20, 30, 10}}
	n, err = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{d})
	if err != nil || n != 1 || len(r.cmds) != 1 {
		t.Fatalf("apply: n=%d err=%v cmds=%v", n, err, r.cmds)
	}
	mac, _ := vm100(t, "collect-node2.txt")
	want := "qm set 100 --net0 'virtio=" + mac + ",bridge=vmbr0,tag=10,trunks=20;30,link_down=1'"
	if r.cmds[0] != want {
		t.Errorf("cmd = %q\nwant %q", r.cmds[0], want)
	}
	// Idempotent: the same intent again writes nothing.
	n, _ = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{d})
	if n != 0 || len(r.cmds) != 1 {
		t.Errorf("second apply wrote %v", r.cmds[1:])
	}
	// A guest on another node is never written from here.
	p := portByIf(t, snap, "vm119-net0")
	n, _ = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{all(p.Index)})
	if n != 0 || len(r.cmds) != 1 {
		t.Errorf("wrote a foreign guest: %v", r.cmds)
	}
	// Back to default: options removed.
	n, _ = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{all(1)})
	if n != 1 || r.cmds[len(r.cmds)-1] != "qm set 100 --net0 'virtio="+mac+",bridge=vmbr0'" {
		t.Errorf("restore = %v", r.cmds)
	}
	// Port cycle: down then back to the (restored) config.
	if err := c.CyclePort(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := r.cmds[len(r.cmds)-2:]; !strings.HasSuffix(got[0], "link_down=1'") || strings.Contains(got[1], "link_down") {
		t.Errorf("cycle = %v", got)
	}
}

func TestVLANToConfig(t *testing.T) {
	cases := []struct {
		d      switchmodel.PortDesired
		tag    int
		trunks string
	}{
		{switchmodel.PortDesired{NativeVLAN: 1, TaggedAll: true}, 0, ""},
		{switchmodel.PortDesired{NativeVLAN: 8}, 8, ""},
		{switchmodel.PortDesired{NativeVLAN: 8, TaggedVLANs: []int{9, 10, 11, 20}}, 8, "9-11;20"},
		{switchmodel.PortDesired{NativeVLAN: 1, TaggedVLANs: []int{2, 10}}, 0, "2;10"},
		{switchmodel.PortDesired{NativeVLAN: 5, TaggedAll: true}, 5, "2-4094"},
		{switchmodel.PortDesired{NativeVLAN: 1}, 1, ""},
	}
	for _, tc := range cases {
		tag, trunks := vlanToConfig(tc.d)
		if tag != tc.tag || formatVLANRanges(trunks) != tc.trunks {
			t.Errorf("%+v -> tag %d trunks %q, want %d %q", tc.d, tag, formatVLANRanges(trunks), tc.tag, tc.trunks)
		}
	}
	// Round trip through the parser.
	n := parseNICOptions("virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1,tag=8,trunks=9-11;20,link_down=1,queues=4", "qemu")
	if n.MAC != "aa:bb:cc:dd:ee:ff" || n.Tag != 8 || formatVLANRanges(n.Trunks) != "9-11;20" || !n.LinkDown || !n.Firewall || n.BridgeMember() != "fwpr0p0" {
		t.Errorf("parsed = %+v", n)
	}
	if got := renderNICOptions(n.Raw, n); got != "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1,queues=4,tag=8,trunks=9-11;20,link_down=1" {
		t.Errorf("render = %q", got)
	}
}

func TestApplySwitch(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2.txt")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Snooping is on; UniFi wants it off on VLAN 1.
	n, err := c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{IGMPSnooping: map[int]bool{1: false, 10: true}})
	if err != nil || n != 1 || r.cmds[0] != "echo 0 > /sys/class/net/vmbr0/bridge/multicast_snooping" {
		t.Fatalf("igmp: n=%d err=%v cmds=%v", n, err, r.cmds)
	}
	n, _ = c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{IGMPSnooping: map[int]bool{1: false}})
	if n != 0 {
		t.Errorf("igmp not idempotent")
	}
	// NTP under UniFi's control.
	n, err = c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{ManageNTP: true, NTPServers: []string{"192.0.2.1", "time.example.net"}})
	if err != nil || n != 1 || !strings.HasPrefix(r.cmds[len(r.cmds)-1], "install -m 644 /dev/stdin /etc/chrony/sources.d/") {
		t.Fatalf("ntp: n=%d err=%v cmds=%v", n, err, r.cmds)
	}
	n, _ = c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{ManageNTP: true, NTPServers: []string{"192.0.2.1", "time.example.net"}})
	if n != 0 {
		t.Errorf("ntp not idempotent")
	}
	// STP requests are logged, not applied.
	n, _ = c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{STPSet: true, STPEnabled: true, STPMode: "rstp"})
	if n != 0 {
		t.Errorf("stp applied")
	}
	if got, _ := c.EnsureVLANs(context.Background(), []int{2, 69, 4000}); got != 0 {
		t.Errorf("ensure vlans created %d", got)
	}
}
