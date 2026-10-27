package sonic

import (
	"context"
	"log"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

const fixture = "../../../docs/fixtures/sonic-202505.1238524-ff3c0c9f6/collect.txt"

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func startFixture(t *testing.T) (*Collector, *FixtureRunner, *devicemodel.Snapshot) {
	t.Helper()
	fr, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, fr, snap
}

func fixtureSections(t *testing.T) map[string]string {
	t.Helper()
	fr, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	return sections(fr.Fixture)
}

// Every section collect.py prints is in the capture and parses: the Redis
// ones as {key: {field: value}}, the Linux ones as text or ip -j JSON.
// The counts are the VS guest's: 32 ports, one VLAN, the learned FDB, and
// nothing in the hardware-only and unconfigured tables.
func TestEverySectionParses(t *testing.T) {
	sec := fixtureSections(t)
	wantEntries := map[string]int{
		"metadata": 1, "state_metadata": 1, "cfg_port": 32, "appl_port": 34, "state_port": 32,
		"cfg_vlan": 1, "cfg_vlan_member": 32, "cfg_snmp": 1, "lldp_loc": 1, "counters_map": 1,
		"counters": 32, "fdb": 138, "asic_temp": 1,
		// configured nowhere on the L2 preset, or hardware only
		"cfg_lag": 0, "cfg_lag_member": 0, "appl_lag": 0, "cfg_mgmt": 0, "cfg_stp": 0, "cfg_mirror": 0,
		"cfg_ntp": 0, "cfg_syslog": 0, "cfg_breakout": 0, "lldp": 0,
		"xcvr": 0, "dom": 0, "fan": 0, "psu": 0, "temp": 0, "chassis": 0,
	}
	for _, name := range redisSections {
		body, ok := sec[name]
		if !ok {
			t.Errorf("section %s missing from the capture", name)
			continue
		}
		tb, err := parseTable(name, body)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		if want, ok := wantEntries[name]; ok && len(tb) != want {
			t.Errorf("section %s: %d entries, want %d", name, len(tb), want)
		}
	}
	if len(parseTableMust(t, sec, "cfg_feature")) == 0 || len(parseTableMust(t, sec, "rates")) == 0 {
		t.Error("cfg_feature and rates must not be empty on a running switch")
	}
	// APPL_DB carries two markers in PORT_TABLE that are not ports.
	if _, ok := parseTableMust(t, sec, "appl_port")["PORT_TABLE:PortConfigDone"]; !ok {
		t.Error("APPL_DB PORT_TABLE:PortConfigDone marker not in the capture")
	}
	var addrs []ipAddr
	var routes []ipRoute
	var neigh []ipNeigh
	for name, v := range map[string]any{"addr": &addrs, "route": &routes, "neigh": &neigh} {
		if err := decodeJSON(name, sec[name], v); err != nil {
			t.Error(err)
		}
	}
	if len(addrs) == 0 || len(routes) != 1 || len(neigh) == 0 {
		t.Errorf("ip -j: %d links, %d default routes, %d neighbours", len(addrs), len(routes), len(neigh))
	}
	if v := yamlValue(sec["version"], "build_version"); v != "202505.1238524-ff3c0c9f6" {
		t.Errorf("build_version = %q", v)
	}
	if c := parseCPUTimes(sec["stat"]); c.total == 0 || c.idle == 0 {
		t.Errorf("/proc/stat = %+v", c)
	}
	if l := parseLoadAvg(sec["loadavg"]); len(l) != 3 {
		t.Errorf("loadavg = %v", l)
	}
	if m := meminfo(sec["meminfo"]); m["MemTotal"] == 0 || m["MemAvailable"] == 0 {
		t.Errorf("meminfo = %v", m)
	}
	if strings.TrimSpace(sec["hostname"]) != "host-1" {
		t.Errorf("hostname = %q", sec["hostname"])
	}
}

func parseTableMust(t *testing.T, sec map[string]string, name string) table {
	t.Helper()
	tb, err := parseTable(name, sec[name])
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

// LLDP capabilities are the LLDP-MIB BITS string, most significant bit
// first: SONiC announces itself (LLDP_LOC_CHASSIS) as "28 00", bridge and
// router.
func TestLLDPCapabilityBits(t *testing.T) {
	loc := parseTableMust(t, fixtureSections(t), "lldp_loc").first()
	b, r := lldpCaps(loc["lldp_loc_sys_cap_enabled"])
	if !b || !r {
		t.Errorf("lldpCaps(%q) = bridge %v router %v; a SONiC switch is both", loc["lldp_loc_sys_cap_enabled"], b, r)
	}
}

// The full snapshot from the VS guest: 32 QSFP28 cages, port 1 up with
// every learned MAC, the other 31 down, VLAN 1000 untagged everywhere.
func TestSnapshotFromTheVirtualSwitch(t *testing.T) {
	c, fr, snap := startFixture(t)
	if len(snap.Ports) != 32 {
		t.Fatalf("%d ports, want 32", len(snap.Ports))
	}
	for i, p := range snap.Ports {
		if p.Index != i+1 {
			t.Fatalf("port %d at position %d", p.Index, i)
		}
		want := "Ethernet" + itoa(4*i)
		if p.IfName != want || !reflect.DeepEqual(p.Interfaces, []string{want}) || p.Lanes != 1 {
			t.Errorf("port %d: ifname %s interfaces %v lanes %d; want %s alone", p.Index, p.IfName, p.Interfaces, p.Lanes, want)
		}
		if p.Media != devicemodel.MediaQSFP28 || !reflect.DeepEqual(p.SpeedCaps, []int{100000}) || p.AdminSpeed != 100000 {
			t.Errorf("port %d: media %s caps %v admin speed %d; want QSFP28 at 100G", p.Index, p.Media, p.SpeedCaps, p.AdminSpeed)
		}
		if !p.Enabled || p.MTU != 9100 {
			t.Errorf("port %d: enabled %v mtu %d", p.Index, p.Enabled, p.MTU)
		}
		if p.VLAN.Mode != "access" || p.VLAN.NativeVLAN != 1000 || p.VLAN.AllowAll || len(p.VLAN.Allowed) != 0 {
			t.Errorf("port %d VLAN = %+v; want access 1000 (untagged member of Vlan1000)", p.Index, p.VLAN)
		}
		if p.FEC != devicemodel.FECUnknown || p.FECConfig != devicemodel.FECUnknown {
			t.Errorf("port %d FEC %q/%q; the VS reports N/A and configures none", p.Index, p.FEC, p.FECConfig)
		}
		if want := "Eth" + itoa(i+1); p.Name != want || c.PortName(p) != want {
			t.Errorf("port %d named %q, want the alias %s", p.Index, p.Name, want)
		}
		if i == 0 {
			continue
		}
		if p.Up || p.SpeedMbps != 0 || p.Present || len(p.MACs) != 0 {
			t.Errorf("port %d: up %v speed %d present %v macs %d; want down and empty", p.Index, p.Up, p.SpeedMbps, p.Present, len(p.MACs))
		}
	}
	p1 := snap.Ports[0]
	if !p1.Up || p1.SpeedMbps != 100000 || !p1.Present || !p1.FullDuplex {
		t.Errorf("port 1 = up %v speed %d present %v", p1.Up, p1.SpeedMbps, p1.Present)
	}
	if p1.Counters.RxBytes == 0 || p1.Counters.RxPackets == 0 || p1.Counters.TxBytes == 0 {
		t.Errorf("port 1 counters = %+v", p1.Counters)
	}
	if p1.Health.LinkChanges != 1 || p1.Health.HasFECCounters {
		t.Errorf("port 1 health = %+v; flap_count 1, no FEC counters on the VS", p1.Health)
	}
	if len(p1.MACs) != 138 || len(snap.MACTable) != 138 || snap.System.MACTableUsed != 138 {
		t.Errorf("port 1 has %d MACs, table %d, used %d; want the 138 the FDB learned", len(p1.MACs), len(snap.MACTable), snap.System.MACTableUsed)
	}
	for _, m := range snap.MACTable {
		if m.PortIndex != 1 || m.VLAN != 1000 || !m.LastMove.IsZero() || m.MAC != strings.ToLower(m.MAC) {
			t.Errorf("MAC entry %+v", m)
			break
		}
	}
	if !reflect.DeepEqual(snap.VLANs, []int{1000}) {
		t.Errorf("VLANs = %v", snap.VLANs)
	}
	// No LLDP neighbours on the lab guest; the default gateway's MAC is
	// learned on port 1, which is therefore the way out.
	if p1.Neighbor != nil || snap.UplinkHint != 1 || snap.UplinkPort() != 1 {
		t.Errorf("neighbour %v, uplink hint %d, uplink %d; want no LLDP and port 1 from the gateway's MAC", p1.Neighbor, snap.UplinkHint, snap.UplinkPort())
	}

	sys := snap.System
	if sys.Version != "202505.1238524-ff3c0c9f6" || sys.Model != "Seastone-DX010" || sys.Vendor != "Celestica" || sys.Hostname != "host-1" {
		t.Errorf("identity = vendor %q model %q version %q host %q", sys.Vendor, sys.Model, sys.Version, sys.Hostname)
	}
	if sys.Serial != "" {
		t.Errorf("serial = %q; the VS reports N/A", sys.Serial)
	}
	if sys.MAC == "" || sys.MgmtMAC == "" || sys.MAC == sys.MgmtMAC {
		t.Errorf("system MAC %q, eth0 MAC %q: both set and different on the VS", sys.MAC, sys.MgmtMAC)
	}
	if len(sys.OOBInterfaces) != 1 || sys.OOBInterfaces[0].Name != "eth0" || !sys.OOBInterfaces[0].Up || sys.OOBInterfaces[0].IP == "" {
		t.Errorf("OOB = %+v; want eth0, up, with its address", sys.OOBInterfaces)
	}
	if !sys.DHCP || len(sys.Addresses) != 1 || sys.Addresses[0].Iface != "eth0" || sys.Addresses[0].IP != sys.OOBInterfaces[0].IP {
		t.Errorf("addresses %v dhcp %v; want eth0's lease only", sys.Addresses, sys.DHCP)
	}
	if sys.Gateway == "" || sys.GatewayMAC == "" || sys.ARP[sys.Gateway] != sys.GatewayMAC {
		t.Errorf("gateway %q mac %q", sys.Gateway, sys.GatewayMAC)
	}
	if sys.STPMode != "none" || sys.STPPriority != 0 {
		t.Errorf("STP = %s/%d; not configured on the L2 preset", sys.STPMode, sys.STPPriority)
	}
	if !reflect.DeepEqual(sys.SNMPCommunities, []string{"community-1"}) || len(sys.NTPServers) != 0 || len(sys.SyslogHosts) != 0 {
		t.Errorf("snmp %v ntp %v syslog %v", sys.SNMPCommunities, sys.NTPServers, sys.SyslogHosts)
	}
	if sys.MemTotalKB == 0 || sys.MemUsedKB == 0 || sys.Uptime == 0 || len(sys.LoadAvg) != 3 {
		t.Errorf("health: mem %d/%d uptime %s load %v", sys.MemUsedKB, sys.MemTotalKB, sys.Uptime, sys.LoadAvg)
	}
	if len(sys.Fans) != 0 || len(sys.PSUs) != 0 || sys.HasTemperature {
		t.Errorf("pmon on the VS is empty: fans %v psus %v temp %v", sys.Fans, sys.PSUs, sys.HasTemperature)
	}
	if got := c.DeviceName(sys); got != "SONiC Seastone-DX010" {
		t.Errorf("DeviceName = %q", got)
	}
	if got := c.DefaultPortNames(p1); !reflect.DeepEqual(got, []string{"Eth1", "Ethernet0"}) {
		t.Errorf("DefaultPortNames = %v", got)
	}
	if c.Capabilities() != (devicemodel.Capabilities{}) {
		t.Error("the read side claims no capabilities")
	}
	if len(fr.Commands) != 0 {
		t.Errorf("the read side ran %v besides the collector", fr.Commands)
	}
}

// Two collects of the same state give the same snapshot apart from the
// time and the CPU figure (a delta between polls).
func TestCollectIsIdempotent(t *testing.T) {
	c, _, first := startFixture(t)
	second, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, b := *first, *second
	a.TakenAt = b.TakenAt
	a.System.CPUPercent, b.System.CPUPercent = 0, 0
	if !reflect.DeepEqual(a, b) {
		t.Error("two collects of one capture differ")
	}
}

// A broken-out cage: child ports share their parent's index and fold into
// one port. Made from the capture by giving Ethernet4 port 1's index, the
// way a 2x50G split leaves Ethernet0 and Ethernet2 on index 1.
func TestBreakoutChildrenFold(t *testing.T) {
	fr, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	const from = `"alias": "Eth2", "dhcp_rate_limit": "300", "index": "2",`
	if strings.Count(fr.Fixture, from) != 1 { // CONFIG_DB PORT; its index is the one read
		t.Fatalf("capture changed: %q found %d times", from, strings.Count(fr.Fixture, from))
	}
	fr.Fixture = strings.ReplaceAll(fr.Fixture, from, `"alias": "Eth2", "dhcp_rate_limit": "300", "index": "1",`)
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Ports) != 31 {
		t.Fatalf("%d ports, want 31", len(snap.Ports))
	}
	p := snap.Ports[0]
	if p.Index != 1 || p.Lanes != 2 || !reflect.DeepEqual(p.Interfaces, []string{"Ethernet0", "Ethernet4"}) || p.IfName != "Ethernet0" {
		t.Errorf("folded cage = index %d lanes %d interfaces %v ifname %s", p.Index, p.Lanes, p.Interfaces, p.IfName)
	}
	if !p.Up || p.LanesDiverge {
		t.Errorf("folded cage up %v diverge %v; up if any child is, same config", p.Up, p.LanesDiverge)
	}
	if got := c.DefaultPortNames(p); !reflect.DeepEqual(got, []string{"Eth1", "Ethernet0", "Ethernet4"}) {
		t.Errorf("DefaultPortNames = %v", got)
	}
	// A child with its own speed makes the cage diverge.
	fr.Fixture = strings.Replace(fr.Fixture, `"index": "1", "lanes": "69,70,71,72", "speed": "100000"`, `"index": "1", "lanes": "69,70,71,72", "speed": "50000"`, 1)
	snap, err = c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Ports[0].LanesDiverge {
		t.Error("children with different speeds must mark the cage LanesDiverge")
	}
}

func TestVocabulary(t *testing.T) {
	for in, want := range map[string]devicemodel.FEC{"rs": devicemodel.FECRS, "fc": devicemodel.FECFC, "none": devicemodel.FECDisabled, "N/A": devicemodel.FECUnknown, "": devicemodel.FECUnknown} {
		if got := fecOf(in); got != want {
			t.Errorf("fecOf(%q) = %q, want %q", in, got, want)
		}
	}
	if got := vendorOf("Force10-S6000", "x86_64-dell_s6000_s1220-r0"); got != "Dell" {
		t.Errorf("vendorOf(dell platform) = %q", got)
	}
	if got := vendorOf("Seastone-DX010", "x86_64-kvm_x86_64-r0"); got != "Celestica" {
		t.Errorf("vendorOf(DX010 SKU on the VS) = %q", got)
	}
	if got := speedList("100000,10000,40000,25000,"); !reflect.DeepEqual(got, []int{10000, 25000, 40000, 100000}) {
		t.Errorf("speedList = %v", got)
	}
}

func TestOpenRequiresSSHTarget(t *testing.T) {
	for _, bad := range []string{"", "host", "@host", "admin@"} {
		if _, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{SSH: bad}); err == nil {
			t.Errorf("Open(%q) accepted", bad)
		}
	}
	d, err := devicemodel.LookupDriver("sonic")
	if err != nil || d.Name() != "sonic" {
		t.Errorf("driver not registered as sonic: %v", err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
