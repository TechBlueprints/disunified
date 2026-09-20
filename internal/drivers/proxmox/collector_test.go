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

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "fixtures", "proxmox-9.1.6", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestCollector(t *testing.T, fixture string) (*Collector, *FixtureRunner) {
	t.Helper()
	r := &FixtureRunner{Fixture: loadFixture(t, fixture)}
	c := NewCollector(r)
	pm, err := loadPortMap(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.ports = pm
	c.cycleDelay = time.Millisecond
	c.ManageLLDP = false // the fixture's lldpd config is scrubbed; covered by TestEnsureLLDPWritesConfig
	return c, r
}

func TestEnsureLLDPWritesConfig(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2.txt")
	c.ManageLLDP = true
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.Commands) != 1 || !strings.HasPrefix(r.Commands[0], "install -m 644 /dev/stdin /etc/lldpd.d/switch-to-unifi.conf && systemctl restart lldpd") {
		t.Fatalf("lldpd config not written: %v", r.Commands)
	}
	// A node without lldpd gets a warning, not a write.
	r2 := &FixtureRunner{Fixture: strings.Replace(loadFixture(t, "collect-node2.txt"), "@@@ lldpdconf\npresent\n", "@@@ lldpdconf\n", 1)}
	c2 := NewCollector(r2)
	c2.ports, _ = loadPortMap(t.TempDir())
	if _, err := c2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r2.Commands) != 0 {
		t.Errorf("wrote lldpd config on a node without lldpd: %v", r2.Commands)
	}
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
	if len(snap.Ports) != 54 {
		t.Fatalf("ports = %d, want 54", len(snap.Ports))
	}
	sys := snap.System
	if sys.Vendor != "Proxmox" || sys.Version != "9.1.6" || sys.Hostname != "proxmox-2" || sys.MAC == "" {
		t.Errorf("system = %+v", sys)
	}
	// The node is the switch: its bridge MAC is the device MAC.
	if sys.MAC != "02:00:00:00:00:01" {
		t.Errorf("device MAC = %s, want the bridge's 02:00:00:00:00:01", sys.MAC)
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
	if len(sys.Addresses) != 1 || sys.Addresses[0].Iface != "vmbr0" || sys.Addresses[0].PrefixLen != 16 || len(sys.ARP) < 5 || len(sys.LoadAvg) != 3 {
		t.Errorf("reachability: addrs %+v arp %d load %v", sys.Addresses, len(sys.ARP), sys.LoadAvg)
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
	// Unassigned guest slots are empty cages; the primary NIC is the last
	// port, the second NIC two below the host.
	if e := snap.Ports[25]; e.Present || e.IfName != "" || e.Index != 26 {
		t.Errorf("empty slot = %+v", e)
	}
	if e := snap.Ports[48]; e.Present || e.Index != 49 {
		t.Errorf("slot 49 should be empty: %+v", e)
	}
	// The bond's slaves are separate ports at the top, "bond0-1" (active,
	// forwarding) and "bond0-2" (standby: linked, blocking), no LAG.
	u := snap.Ports[53]
	if u.IfName != "bond0-1" || u.Index != 54 || !u.Up || u.SpeedMbps != 100000 || u.LAG != "" || u.STPState != "forwarding" || u.Optic == nil || u.Optic.Part != "QSFP-100G-CU2M" || len(u.Interfaces) != 1 || u.Interfaces[0] != "ens1f0np0" {
		t.Errorf("uplink = %+v", u)
	}
	if s := snap.Ports[52]; s.IfName != "bond0-2" || s.Index != 53 || !s.Up || s.SpeedMbps != 10000 || s.STPState != "blocking" || s.LAG != "" || len(s.MACs) != 0 || s.Media != switchmodel.MediaSFP28 {
		t.Errorf("standby = %+v", s)
	}
	if len(u.SpeedCaps) == 0 || u.SpeedCaps[len(u.SpeedCaps)-1] != 100000 || !u.FECCapable || len(u.MACs) < 50 {
		t.Errorf("uplink caps/macs = %v %v %d", u.SpeedCaps, u.FECCapable, len(u.MACs))
	}
	if u.Health.LinkChanges == 0 {
		t.Errorf("uplink carrier changes not read")
	}
	if snap.UplinkHint != 54 || snap.UplinkPort() != 54 {
		t.Errorf("uplink hint = %d", snap.UplinkHint)
	}
	for _, idx := range []int{49, 50, 51, 52} {
		if e := snap.Ports[idx-1]; e.Present || e.Index != idx {
			t.Errorf("slot %d should be empty: %+v", idx, e)
		}
	}
	ups := physicalMembers(sections(loadFixture(t, "collect-node2.txt"))["phys"], map[string]ipLink{"bond0": {Ifname: "bond0", Master: "vmbr0", Ifindex: 5}}, parseBonding(sections(loadFixture(t, "collect-node2.txt"))["bonding"]), "vmbr0")
	if cfg, announce := lldpdConfig(ups, func(i int) int { return 54 - i }, "02:00:00:00:00:01"); len(announce) != 1 || announce[0] != "ens1f0np0" || !strings.Contains(cfg, "chassisid 02:00:00:00:00:01") || !strings.Contains(cfg, `ens1f1np1 lldp portidsubtype local "Port 53"`) {
		t.Errorf("lldpd config = %q announce %v", cfg, announce)
	}
	if len(snap.VLANs) < 3 || snap.VLANs[0] != 1 {
		t.Errorf("vlans = %v", snap.VLANs)
	}
	if got := c.DeviceName(snap.System); got != "pve-proxmox-2" {
		t.Errorf("device name = %q", got)
	}
	if c.PortName(p1) != "VM-100" {
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
	if err != nil || n != 0 || len(r.Commands) != 0 {
		t.Fatalf("converged apply: n=%d err=%v cmds=%v", n, err, r.Commands)
	}
	// Disable port 1 (VM 100 on this node) and put it on native 10 + tagged 20,30.
	d := switchmodel.PortDesired{Index: 1, Enabled: false, VLANSet: true, NativeVLAN: 10, TaggedVLANs: []int{20, 30, 10}}
	if plan := c.PlanPorts(append(desired, d)); len(plan) != 1 || plan[0] != 1 || len(r.Commands) != 0 {
		t.Errorf("plan = %v (commands %v)", plan, r.Commands)
	}
	n, err = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{d})
	if err != nil || n != 1 || len(r.Commands) != 1 {
		t.Fatalf("apply: n=%d err=%v cmds=%v", n, err, r.Commands)
	}
	mac, _ := vm100(t, "collect-node2.txt")
	want := "qm set 100 --net0 'virtio=" + mac + ",bridge=vmbr0,tag=10,trunks=20;30,link_down=1'"
	if r.Commands[0] != want {
		t.Errorf("cmd = %q\nwant %q", r.Commands[0], want)
	}
	// Idempotent: the same intent again writes nothing.
	n, _ = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{d})
	if n != 0 || len(r.Commands) != 1 {
		t.Errorf("second apply wrote %v", r.Commands[1:])
	}
	// A guest on another node is never written from here.
	p := portByIf(t, snap, "vm119-net0")
	n, _ = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{all(p.Index)})
	if n != 0 || len(r.Commands) != 1 {
		t.Errorf("wrote a foreign guest: %v", r.Commands)
	}
	// Back to default: options removed.
	n, _ = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{all(1)})
	if n != 1 || r.Commands[len(r.Commands)-1] != "qm set 100 --net0 'virtio="+mac+",bridge=vmbr0'" {
		t.Errorf("restore = %v", r.Commands)
	}
	// Port cycle: down then back to the (restored) config.
	if err := c.CyclePort(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := r.Commands[len(r.Commands)-2:]; !strings.HasSuffix(got[0], "link_down=1'") || strings.Contains(got[1], "link_down") {
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
	if err != nil || n != 1 || r.Commands[0] != "echo 0 > /sys/class/net/vmbr0/bridge/multicast_snooping" {
		t.Fatalf("igmp: n=%d err=%v cmds=%v", n, err, r.Commands)
	}
	n, _ = c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{IGMPSnooping: map[int]bool{1: false}})
	if n != 0 {
		t.Errorf("igmp not idempotent")
	}
	// NTP under UniFi's control.
	n, err = c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{ManageNTP: true, NTPServers: []string{"192.0.2.1", "time.example.net"}})
	if err != nil || n != 1 || !strings.HasPrefix(r.Commands[len(r.Commands)-1], "install -m 644 /dev/stdin /etc/chrony/sources.d/") {
		t.Fatalf("ntp: n=%d err=%v cmds=%v", n, err, r.Commands)
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

// TestLACPBondIsPerMemberLAG models an 802.3ad bond, which no host here has:
// the fixture's active-backup bond is rewritten to LACP mode, so this is the
// driver's best model, not a live capture (docs/proxmox.md §1b).
func TestLACPBondIsPerMemberLAG(t *testing.T) {
	fixture := strings.Replace(loadFixture(t, "collect-node2.txt"), "Bonding Mode: fault-tolerance (active-backup)", "Bonding Mode: IEEE 802.3ad Dynamic link aggregation", 1)
	r := &FixtureRunner{Fixture: fixture}
	c := NewCollector(r)
	c.ports, _ = loadPortMap(t.TempDir())
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, b := snap.Ports[53], snap.Ports[52]
	if a.IfName != "bond0-1" || a.LAG != "bond0" || !a.Up || a.SpeedMbps != 100000 || len(a.MACs) < 50 {
		t.Errorf("first member = %+v", a)
	}
	if b.IfName != "bond0-2" || b.LAG != "bond0" || !b.Up || b.SpeedMbps != 10000 || len(b.MACs) != 0 {
		t.Errorf("second member = %+v (MACs belong to the first member only)", b)
	}
	ups := physicalMembers(sections(fixture)["phys"], map[string]ipLink{"bond0": {Ifname: "bond0", Master: "vmbr0", Ifindex: 5}}, parseBonding(sections(fixture)["bonding"]), "vmbr0")
	cfg, announce := lldpdConfig(ups, func(i int) int { return []int{54, 53}[i] }, "02:00:00:00:00:01")
	if len(announce) != 2 || !strings.Contains(cfg, `ens1f0np0 lldp portidsubtype local "Port 54"`) || !strings.Contains(cfg, `ens1f1np1 lldp portidsubtype local "Port 53"`) {
		t.Errorf("lldpd config = %q announce %v", cfg, announce)
	}
}

// TestVLANDeviceOnTheUplink models bond0.10 as the bridge member (not a
// layout any host here has): the port is the device underneath.
func TestVLANDeviceOnTheUplink(t *testing.T) {
	links := map[string]ipLink{
		"bond0":    {Ifname: "bond0", Ifindex: 5},
		"bond0.10": {Ifname: "bond0.10", Ifindex: 9, Master: "vmbr0", Link: "bond0"},
	}
	l := links["bond0.10"]
	l.Linkinfo.InfoKind = "vlan"
	links["bond0.10"] = l
	ups := physicalMembers("ens1f0np0 master=bond0\nens1f1np1 master=bond0\n", links, parseBonding(sections(loadFixture(t, "collect-node2.txt"))["bonding"]), "vmbr0")
	if len(ups) != 2 || ups[0].Name != "bond0.10-1" || ups[0].Member != "bond0.10" || ups[0].Active != "ens1f0np0" || ups[1].Name != "bond0.10-2" || !ups[1].Standby {
		t.Errorf("uplinks = %+v", ups)
	}
}

func TestNodeNumberingKeepsOnlyLocalGuests(t *testing.T) {
	c, _ := newTestCollector(t, "collect-node2.txt")
	c.NodeNumbering = true
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var assigned, local int
	for _, p := range snap.Ports[:48] {
		if p.IfName == "" {
			continue
		}
		assigned++
		if len(p.Interfaces) > 0 || !p.Up {
			local++
		}
	}
	if assigned == 0 || assigned >= 24 {
		t.Errorf("node numbering assigned %d guest ports; want only this node's (fewer than the cluster's 24)", assigned)
	}
	for _, p := range snap.Ports[:48] {
		if p.IfName == "vm119-net0" {
			t.Errorf("a guest on another node got a port under node numbering: %+v", p)
		}
	}
	// VM 100 (on this node) keeps port 1 either way.
	if snap.Ports[0].IfName != "vm100-net0" {
		t.Errorf("port 1 = %+v", snap.Ports[0])
	}
}

// withMSTP composes the node capture with the mstpctl captures (real output
// of mstpd 0.2.0-2 on the same node, from a scratch bridge, port renamed to
// the guest's tap): the bridge under user-space STP.
func withMSTP(t *testing.T, bridgeJSON, portsJSON string) string {
	t.Helper()
	s := loadFixture(t, "collect-node2.txt")
	s = strings.Replace(s, "stp_state=0\n", "stp_state=2\n", 1)
	if bridgeJSON == "" {
		bridgeJSON = loadFixture(t, "mstpctl-showbridge.json")
	}
	if portsJSON == "" {
		portsJSON = loadFixture(t, "mstpctl-showportdetail.json")
	}
	return strings.Replace(s, "@@@ lldp\n", "@@@ mstpctl\npresent\n@@@ mstpbridge\n"+bridgeJSON+"\n@@@ mstpports\n"+portsJSON+"\n@@@ lldp\n", 1)
}

func TestSTPUnderMSTPD(t *testing.T) {
	r := &FixtureRunner{Fixture: withMSTP(t, "", "")}
	c := NewCollector(r)
	c.ManageLLDP = false
	c.ports, _ = loadPortMap(t.TempDir())
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps := c.Capabilities(); !caps.STP || !caps.BPDUGuard || !caps.STPPortCost {
		t.Errorf("capabilities under mstpd = %+v", caps)
	}
	if snap.System.STPMode != "rstp" || snap.System.STPPriority != 61440 {
		t.Errorf("system STP = %s/%d", snap.System.STPMode, snap.System.STPPriority)
	}
	p := snap.Ports[0] // VM 100, tap100i0: the captured port
	if p.STPState != "forwarding" || p.STPRole != "designated" || !p.STPEdge || p.STPPathCost != 2000 || p.BPDUGuard {
		t.Errorf("port 1 STP = state %s role %s edge %v cost %d guard %v", p.STPState, p.STPRole, p.STPEdge, p.STPPathCost, p.BPDUGuard)
	}
	if len(r.Commands) != 0 {
		t.Errorf("a converged bridge was written to: %v", r.Commands)
	}
	// Version follows the controller; priority and STP-off requests do not.
	n, err := c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{STPSet: true, STPEnabled: true, STPMode: "stp", STPPriority: 32768})
	if err != nil || n != 1 || r.Commands[len(r.Commands)-1] != "mstpctl setforcevers vmbr0 stp" {
		t.Errorf("apply stp version: n=%d err=%v cmds=%v", n, err, r.Commands)
	}
	// BPDU guard on the guest port.
	n, err = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{{Index: 1, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true, BPDUGuard: true}})
	if err != nil || n != 1 || r.Commands[len(r.Commands)-1] != "mstpctl -s" || !strings.Contains(r.Stdins[len(r.Stdins)-1], "setportbpduguard vmbr0 tap100i0 yes") {
		t.Errorf("bpdu guard: n=%d err=%v cmds=%v stdin=%q", n, err, r.Commands, r.Stdins[len(r.Stdins)-1])
	}
	n, _ = c.ApplyPorts(context.Background(), []switchmodel.PortDesired{{Index: 1, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true, BPDUGuard: true}})
	if n != 0 {
		t.Errorf("bpdu guard not idempotent")
	}
}

func TestSTPEnforcesPriorityAndEdge(t *testing.T) {
	// A bridge that came up at the default priority with a non-edge tap.
	bridge := strings.Replace(loadFixture(t, "mstpctl-showbridge.json"), "F.000.", "8.000.", -1)
	ports := strings.Replace(loadFixture(t, "mstpctl-showportdetail.json"), `"admin-edge-port":"yes"`, `"admin-edge-port":"no"`, 1)
	r := &FixtureRunner{Fixture: withMSTP(t, bridge, ports)}
	c := NewCollector(r)
	c.ManageLLDP = false
	c.ports, _ = loadPortMap(t.TempDir())
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.System.STPPriority != 32768 {
		t.Errorf("priority read = %d", snap.System.STPPriority)
	}
	if len(r.Commands) != 1 || r.Commands[0] != "mstpctl -s" || !strings.Contains(r.Stdins[0], "settreeprio vmbr0 0 15") || !strings.Contains(r.Stdins[0], "setportadminedge vmbr0 tap100i0 yes") {
		t.Errorf("enforcement = %v %q", r.Commands, r.Stdins)
	}
}

func TestNoSTPWithoutMSTPD(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2.txt")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if caps := c.Capabilities(); caps.STP || caps.BPDUGuard {
		t.Errorf("STP claimed without mstpd: %+v", caps)
	}
	n, _ := c.ApplySwitch(context.Background(), switchmodel.SwitchDesired{STPSet: true, STPEnabled: true, STPMode: "rstp"})
	if n != 0 || len(r.Commands) != 0 {
		t.Errorf("STP applied without mstpd: %d %v", n, r.Commands)
	}
}
