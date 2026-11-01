package unificfg

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseCapturedSystemCfg(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106/system_cfg.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	if len(c.Ports) != 54 {
		t.Fatalf("parsed %d ports, want 54", len(c.Ports))
	}
	if p := c.Ports[2]; p.Enabled || p.Name != "SFP28 2" || p.OpMode != "switch" {
		t.Errorf("port 2 = %+v (the capture has it disabled)", p)
	}
	if p := c.Ports[1]; !p.Enabled || p.Name != "SFP28 1" {
		t.Errorf("port 1 = %+v", p)
	}
	if p := c.Ports[49]; !p.Enabled || p.Name != "QSFP28 1" {
		t.Errorf("port 49 = %+v", p)
	}
	if len(c.VLANs) != 10 || c.VLANs[0].ID != 1 || c.VLANs[0].Mode != "untagged" || c.VLANs[9].ID != 4000 || c.VLANs[9].Mode != "tagged" {
		t.Errorf("vlans = %+v", c.VLANs)
	}
	if c.MTU != 9216 {
		t.Errorf("mtu = %d", c.MTU)
	}
	if c.Raw["unifi.version"] != "10.6.106" {
		t.Errorf("raw unifi.version = %q", c.Raw["unifi.version"])
	}
	if idx := c.PortIndexes(); idx[0] != 1 || idx[53] != 54 {
		t.Errorf("port indexes = %v", idx)
	}
}

func TestParseDefaults(t *testing.T) {
	c := Parse("switch.port.7.name=x\r\n# comment\nbogus line\nswitch.port.7.status=enabled\n")
	if p := c.Ports[7]; !p.Enabled || p.Name != "x" {
		t.Errorf("port 7 = %+v", p)
	}
}

func TestParsePort2Capture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106/system_cfg-port2-name-speed-vlans.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	p := c.Ports[2]
	if p.Name != "stu capture" || p.AutoNeg || p.SpeedMbps != 10000 || !p.FullDuplex {
		t.Errorf("port 2 = %+v", p)
	}
	if !p.VLANExplicit || p.NativeVLAN != 1 || len(p.TaggedVLANs) != 2 || p.TaggedVLANs[0] != 2 || p.TaggedVLANs[1] != 10 {
		t.Errorf("port 2 vlans = %+v", p)
	}
	if q := c.Ports[1]; q.VLANExplicit || !q.AutoNeg {
		t.Errorf("port 1 (untouched) = %+v", q)
	}
	if ids := c.VLANIDs(); len(ids) != 10 || ids[0] != 1 || ids[9] != 4000 {
		t.Errorf("vlan ids = %v", ids)
	}
}

func TestParsePort54FeatureCapture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106/system_cfg-port54-fec-storm-bpdu-caps.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	p := c.Ports[54]
	if p.FEC != "cl-91" || p.AutoNeg || p.SpeedMbps != 100000 || !p.BPDUGuard || !p.STPPortMode {
		t.Errorf("port 54 = %+v", p)
	}
	if !p.StormCtrl.Enabled || p.StormCtrl.Type != "level" || p.StormCtrl.Bcast != 5 || p.StormCtrl.Mcast != 1 || p.StormCtrl.Ucast != -1 {
		t.Errorf("port 54 storm = %+v", p.StormCtrl)
	}
	if p.LLDPMED == nil || *p.LLDPMED {
		t.Errorf("port 54 lldpmed = %v", p.LLDPMED)
	}
	if q := c.Ports[1]; q.FEC != "" || q.BPDUGuard || q.StormCtrl.Enabled || q.LLDPMED == nil || !*q.LLDPMED {
		t.Errorf("port 1 = %+v", q)
	}
	if !c.STP.Set || !c.STP.Enabled || c.STP.Version != "rstp" || c.STP.Priority != 32768 {
		t.Errorf("stp = %+v", c.STP)
	}
	if len(c.IGMPSnooping) != 3 || !c.IGMPSnooping[1] || !c.IGMPSnooping[2] || !c.IGMPSnooping[69] {
		t.Errorf("igmp = %v", c.IGMPSnooping)
	}
}

// Every recorded push carries system.timezone and locale.timezone (a POSIX
// TZ string on 10.6.106) and resolv.host.1.name (the UniFi name without
// spaces).
func TestParseTimezoneAndHostname(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	c := Parse(read("system_cfg.txt"))
	if c.Timezone != "MST7MDT,M3.2.0,M11.1.0" {
		t.Errorf("timezone = %q", c.Timezone)
	}
	pdu := Parse(read("system_cfg-pdu-outlets.txt"))
	if pdu.Hostname != "APCAP7931" || pdu.Timezone != "MST7MDT,M3.2.0,M11.1.0" {
		t.Errorf("pdu push: hostname %q timezone %q", pdu.Hostname, pdu.Timezone)
	}
	// system.timezone wins; locale.timezone fills in when it is absent.
	var lines []string
	for _, l := range strings.Split(read("system_cfg.txt"), "\n") {
		if !strings.HasPrefix(l, "system.timezone=") {
			lines = append(lines, l)
		}
	}
	if got := Parse(strings.Join(lines, "\n")).Timezone; got != "MST7MDT,M3.2.0,M11.1.0" {
		t.Errorf("locale.timezone fallback = %q", got)
	}
	if got := Parse(strings.Join(lines, "\n") + "\nsystem.timezone=UTC0\n").Timezone; got != "UTC0" {
		t.Errorf("system.timezone must win, got %q", got)
	}
	if got := Parse("switch.port.1.name=x\n"); got.Timezone != "" || got.Hostname != "" {
		t.Errorf("absent keys: %+v", got)
	}
}

// Remote logging switched on in Settings -> System (10.6.109, 2026-10-27): the
// host arrives as syslog.remote.ip/port/status, not the syslog.ip/port unifi-emu's
// notes guessed. syslog.status=enabled and syslog.level=7 are in every push,
// remote logging on or off, and are the device's own logging, not a host.
func TestParseSyslogRemote(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106/system_cfg-syslog-remote.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	if len(c.SyslogHosts) != 1 || c.SyslogHosts[0] != "192.0.2.40" {
		t.Errorf("syslog hosts = %v, want [192.0.2.40] (port 514 is the default and stays implicit)", c.SyslogHosts)
	}
	if c.Raw["syslog.remote.port"] != "514" || c.Raw["syslog.status"] != "enabled" {
		t.Errorf("raw syslog keys: %q %q", c.Raw["syslog.remote.port"], c.Raw["syslog.status"])
	}
	off := Parse(strings.Replace(string(b), "syslog.remote.status=enabled", "syslog.remote.status=disabled", 1))
	if len(off.SyslogHosts) != 0 {
		t.Errorf("remote logging off must leave no host, got %v", off.SyslogHosts)
	}
}

// The ECS Aggregation's push (Ubiquiti's own SONiC switch, which claims STP,
// BPDU guard, auto edge and loop protection) carries the spanning-tree and
// loop-protection keys the controller withholds from a device that does not
// claim them.
func TestParseECSSpanningTreeAndLoopProtection(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-ecs-aggregation.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	if !c.STP.Set || !c.STP.Enabled || c.STP.Version != "rstp" || c.STP.Priority != 8192 {
		t.Errorf("stp = %+v, want enabled rstp 8192", c.STP)
	}
	edge := map[string]int{}
	for _, idx := range c.PortIndexes() {
		p := c.Ports[idx]
		edge[p.STPEdge]++
		if !p.STPPortMode || p.BPDUGuard || p.Keepalive {
			t.Errorf("port %d = port_mode %v bpdu_guard %v keepalive %v; the capture has every port enabled/disabled/disabled", idx, p.STPPortMode, p.BPDUGuard, p.Keepalive)
		}
	}
	if edge["auto"] != 46 || edge["disabled"] != 8 {
		t.Errorf("stp.edge values = %v, want 46 auto and 8 disabled", edge)
	}
	if c.Ports[10].STPEdge != "disabled" || c.Ports[1].STPEdge != "auto" {
		t.Errorf("port 1 edge %q, port 10 edge %q", c.Ports[1].STPEdge, c.Ports[10].STPEdge)
	}
	if !strings.Contains(string(b), "switch.port.1.keepalive=disabled") {
		t.Error("fixture lost its keepalive keys")
	}
}

// Rogue DHCP Server Detection on, as the controller pushed it to the ECS
// Aggregation (2026-11-01): the switch-wide flag plus one switch.dhcp_guard
// group per trusted server of every network with DHCP Guarding on. The
// same push with the setting off (system_cfg-ecs-aggregation.txt) carries
// the flag disabled and no guard groups.
func TestParseECSDHCPSnooping(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-ecs-dhcp-snoop.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	if c.DHCPSnoop == nil || !*c.DHCPSnoop {
		t.Fatalf("dhcp_snoop = %v, want enabled", c.DHCPSnoop)
	}
	want := []DHCPGuard{
		{VLAN: 1, Server: "192.0.2.1"},
		{VLAN: 69, Server: "192.0.2.5"}, {VLAN: 69, Server: "192.0.2.1"},
		{VLAN: 2, Server: "192.0.2.6"}, {VLAN: 2, Server: "192.0.2.1"}, {VLAN: 2, Server: "192.0.2.7"},
	}
	if len(c.DHCPGuards) != len(want) {
		t.Fatalf("guards = %+v, want %+v", c.DHCPGuards, want)
	}
	for i := range want {
		if c.DHCPGuards[i] != want[i] {
			t.Errorf("guard %d = %+v, want %+v", i+1, c.DHCPGuards[i], want[i])
		}
	}
	// The gateway, trusted on every guarded network, is the VLAN 1 querier too.
	if c.Raw["switch.vlan.1.igmp_querier"] != "192.0.2.1" {
		t.Errorf("fixture's gateway mapping moved: querier %q", c.Raw["switch.vlan.1.igmp_querier"])
	}
	if len(c.MACACLs)+len(c.IPACLs) != 0 {
		t.Errorf("no ACL rule existed during this capture, parsed %d mac / %d ip", len(c.MACACLs), len(c.IPACLs))
	}

	off, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-ecs-aggregation.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if o := Parse(string(off)); o.DHCPSnoop == nil || *o.DHCPSnoop || o.DHCPGuards != nil {
		t.Errorf("setting off: dhcp_snoop %v, guards %v; want disabled and none", o.DHCPSnoop, o.DHCPGuards)
	}
	// A disabled group is not a trusted server.
	d := Parse(strings.Replace(string(b), "switch.dhcp_guard.6.status=enabled", "switch.dhcp_guard.6.status=disabled", 1))
	if len(d.DHCPGuards) != 5 {
		t.Errorf("with guard 6 disabled: %d guards, want 5", len(d.DHCPGuards))
	}
}

// The site's switch ACL rules as the controller pushed them to the ECS
// Aggregation (2026-11-01), from four UI rules scoped to that switch: an
// IPv4 allow (protocol All), a MAC allow on one VLAN, an IPv4 TCP block to
// two destination ports, and a MAC block on a netmask-40 source. The rule
// values are the documentation addresses and test MACs typed for the capture.
func TestParseECSACLRules(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-ecs-acl.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	wantMAC := []MACACL{
		{Name: "dui-test-allow-mac-1", Permit: true, VLAN: 4010,
			Src: MACMatch{"02:00:00:00:00:01", "ff:ff:ff:ff:ff:ff"}, Dst: MACMatch{"02:00:00:00:00:02", "ff:ff:ff:ff:ff:ff"}},
		{Name: "dui-test-block-mac-1", Permit: false, VLAN: 4010,
			Src: MACMatch{"02:00:00:00:00:00", "ff:ff:ff:ff:ff:00"}, Dst: MACMatch{"any", "00:00:00:00:00:00"}},
	}
	if len(c.MACACLs) != len(wantMAC) {
		t.Fatalf("mac acls = %+v", c.MACACLs)
	}
	for i := range wantMAC {
		if !reflect.DeepEqual(c.MACACLs[i], wantMAC[i]) {
			t.Errorf("mac acl %d = %+v, want %+v", i+1, c.MACACLs[i], wantMAC[i])
		}
	}
	// One UI rule with destination ports 9 and 1001 became two groups.
	wantIP := []IPACL{
		{Name: "dui-test-allow-ip-1", Permit: true, Type: "ip",
			Src: IPMatch{IP: "192.0.2.0/24"}, Dst: IPMatch{IP: "198.51.100.0/24"}},
		{Name: "dui-test-block-ip-1", Permit: false, Type: "tcp",
			Src: IPMatch{IP: "203.0.113.0/24", PortRange: "1-65535"}, Dst: IPMatch{IP: "192.0.2.0/24", Port: 9}},
		{Name: "dui-test-block-ip-2", Permit: false, Type: "tcp",
			Src: IPMatch{IP: "203.0.113.0/24", PortRange: "1-65535"}, Dst: IPMatch{IP: "192.0.2.0/24", Port: 1001}},
	}
	if len(c.IPACLs) != len(wantIP) {
		t.Fatalf("ip acls = %+v", c.IPACLs)
	}
	for i := range wantIP {
		if !reflect.DeepEqual(c.IPACLs[i], wantIP[i]) {
			t.Errorf("ip acl %d = %+v, want %+v", i+1, c.IPACLs[i], wantIP[i])
		}
	}
	if c.DHCPSnoop == nil || *c.DHCPSnoop || c.DHCPGuards != nil {
		t.Errorf("rogue DHCP detection was off during this capture: %v %v", c.DHCPSnoop, c.DHCPGuards)
	}
	if o := Parse(strings.ReplaceAll(string(b), "switch.acl.", "x.")); o.MACACLs != nil || o.IPACLs != nil {
		t.Error("ACLs parsed from lines that are not switch.acl.*")
	}
}

// Every rule shape in the ACL capture has been seen; shapes it does not
// show -- each an edit of a captured line -- are reported, never guessed.
func TestACLUncaptured(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-ecs-acl.txt"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	c := Parse(text)
	for _, a := range c.IPACLs {
		if why := a.Uncaptured(); why != "" {
			t.Errorf("captured IP rule %s: %s", a.Name, why)
		}
	}
	for _, a := range c.MACACLs {
		if why := a.Uncaptured(); why != "" {
			t.Errorf("captured MAC rule %s: %s", a.Name, why)
		}
	}
	ip := func(edited string) IPACL {
		t.Helper()
		e := Parse(edited)
		if len(e.IPACLs) != 3 {
			t.Fatalf("edit lost a rule: %+v", e.IPACLs)
		}
		return e.IPACLs[1]
	}
	cases := map[string]string{
		"protocol \"tcp_udp\"":    strings.Replace(text, "switch.acl.ip.2.type=tcp", "switch.acl.ip.2.type=tcp_udp", 1),
		"no ip line":              strings.Replace(text, "switch.acl.ip.2.src.ip=203.0.113.0/24\n", "", 1),
		"neither an IPv4 prefix":  strings.Replace(text, "switch.acl.ip.2.src.ip=203.0.113.0/24", "switch.acl.ip.2.src.ip=2001:db8::/32", 1),
		"a port range other than": strings.Replace(text, "switch.acl.ip.2.src.port_range=1-65535", "switch.acl.ip.2.src.port_range=1-1024", 1),
		"a port and a port range": strings.Replace(text, "switch.acl.ip.2.src.port_range=1-65535", "switch.acl.ip.2.src.port_range=1-65535\nswitch.acl.ip.2.src.port=53", 1),
		"src.network_id=":         strings.Replace(text, "switch.acl.ip.2.src.ip=203.0.113.0/24", "switch.acl.ip.2.src.ip=203.0.113.0/24\nswitch.acl.ip.2.src.network_id=x", 1),
		"action=redirect":         strings.Replace(text, "switch.acl.ip.2.action=deny", "switch.acl.ip.2.action=redirect", 1),
	}
	for want, edited := range cases {
		if why := ip(edited).Uncaptured(); !strings.Contains(why, want) {
			t.Errorf("%s: Uncaptured() = %q", want, why)
		}
	}
	if why := Parse(strings.Replace(text, "switch.acl.ip.1.type=ip", "switch.acl.ip.1.type=ip\nswitch.acl.ip.1.dst.port=9", 1)).IPACLs[0].Uncaptured(); !strings.Contains(why, "protocol-All") {
		t.Errorf("a port on an All rule: %q", why)
	}
	mac := Parse(strings.Replace(text, "switch.acl.mac.2.dst.mac=any", "switch.acl.mac.2.dst.device=x", 1)).MACACLs[1]
	if why := mac.Uncaptured(); !strings.Contains(why, "dst.device=x") {
		t.Errorf("a MAC rule's unknown side: %q", why)
	}
}

// The DX010's own push with one rule of each shape the ECS round did not
// cover (2026-11-01, scoped to the DX010): UDP, TCP/UDP (two groups), Any
// (ip=any), a source port, and a Network side (a CIDR with host bits set).
func TestParseDX010ACLRules(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-dx010-acl.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	src := IPMatch{IP: "203.0.113.0/24", PortRange: AnyPort}
	dst9 := IPMatch{IP: "192.0.2.0/24", Port: 9}
	want := []IPACL{
		{Name: "dui-test-udp-1", Type: "udp", Src: src, Dst: dst9},
		{Name: "dui-test-tcpudp-1", Type: "tcp", Src: src, Dst: dst9},
		{Name: "dui-test-tcpudp-2", Type: "udp", Src: src, Dst: dst9},
		{Name: "dui-test-any-1", Type: "ip", Src: IPMatch{IP: AnyIP}, Dst: IPMatch{IP: "198.51.100.0/24"}},
		{Name: "dui-test-srcport-1", Type: "tcp", Src: IPMatch{IP: "203.0.113.0/24", Port: 9}, Dst: IPMatch{IP: "192.0.2.0/24", PortRange: AnyPort}},
		{Name: "dui-test-network-1", Type: "ip", Src: IPMatch{IP: "192.0.2.4/24"}, Dst: IPMatch{IP: "198.51.100.0/24"}},
	}
	if !reflect.DeepEqual(c.IPACLs, want) {
		t.Errorf("ip acls:\n got  %+v\n want %+v", c.IPACLs, want)
	}
	for _, a := range c.IPACLs {
		if why := a.Uncaptured(); why != "" {
			t.Errorf("captured rule %s: %s", a.Name, why)
		}
	}
	if c.MACACLs != nil {
		t.Errorf("no MAC rule can target a switch claiming max_custom_mac_acls 0: %+v", c.MACACLs)
	}
	if c.DHCPSnoop == nil || *c.DHCPSnoop {
		t.Errorf("a switch claiming DHCP snooping gets the site's setting (off): %v", c.DHCPSnoop)
	}
}
