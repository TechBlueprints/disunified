// Package unificfg parses the controller's system_cfg: the UniFi device
// configuration file (key=value text, "#"-comment section headers) that the
// controller pushes in a setparam reply whenever the device's reported
// cfgversion differs from the site's. On a real UniFi switch this is
// /tmp/system.cfg. Observed from Network 10.6.106; a scrubbed capture is in
// docs/fixtures/controller-10.6.106/system_cfg.txt.
package unificfg

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Config is the parsed file. Only the switch.* keys the bridge acts on are
// modelled; everything is also kept verbatim in Raw for diffing and logging.
type Config struct {
	Raw   map[string]string
	Ports map[int]Port // by port index
	VLANs []VLAN
	MTU   int
	STP   STP
	// IGMPSnooping maps VLAN ID -> switch.vlan.<slot>.igmp_snooping for the
	// VLANs the controller mentioned; VLANs without the key are absent.
	IGMPSnooping map[int]bool
	// IGMP is, by VLAN ID, the rest of a VLAN's snooping keys
	// (switch.vlan.<slot>.igmp_* and forward_unknown_multicast), for the
	// VLANs IGMPSnooping lists; Multicast is switch.multicast.*. Captured
	// from the ECS Aggregation's push (controller-10.6.109/
	// system_cfg-ecs-aggregation.txt), which claims advanced snooping.
	IGMP      map[int]IGMP
	Multicast Multicast

	// Outlets is a power device's outlet intent, by 1-based outlet index.
	// Its keys are top-level (outlet.<n>.*), not switch.*, because the
	// controller treats a power device's outlets as a separate plane.
	Outlets map[int]Outlet

	// Address is the controller's IP Settings for the device itself:
	// netconf.1.* and dhcpc.1.status. nil when the push carried no netconf.
	Address *Address

	SSHKeys     []SSHKey // sshd.auth.key.N.*, enabled ones, in order
	Users       []User   // users.N.*: the device login the UniFi terminal uses (password is an MD5-crypt hash)
	NTPServers  []string // ntpclient.N.server, in order, only enabled ones
	SyslogHosts []string // syslog.remote.ip[:port] when syslog.remote.status=enabled (Settings -> System -> remote logging; captured 2026-10-27)
	JumboFrames bool     // switch.jumboframes=enabled
	JumboSet    bool     // switch.jumboframes is present at all
	DHCPSnoop   *bool    // switch.dhcp_snoop.status, nil if absent
	// DHCPGuards is switch.dhcp_guard.N.*, the enabled ones in slot order:
	// the DHCP servers allowed to answer on a VLAN. Pushed only beside
	// switch.dhcp_snoop.status=enabled, one group per trusted server of
	// every network with DHCP Guarding on (captured 2026-11-01 on 10.6.109,
	// docs/fixtures/controller-10.6.109/system_cfg-ecs-dhcp-snoop.txt). A
	// VLAN with no group has no server list.
	DHCPGuards []DHCPGuard
	// MACACLs and IPACLs are switch.acl.mac.N.* and switch.acl.ip.N.*, in
	// slot order: the site's switch ACL rules (Policy Table -> ACL Rules)
	// that apply to this device, pushed only to a device that claims
	// custom ACLs (captured 2026-11-01 on 10.6.109,
	// docs/fixtures/controller-10.6.109/system_cfg-ecs-acl.txt). The slot
	// order is the rule order; nil when no rule applies.
	MACACLs []MACACL
	IPACLs  []IPACL
	SNMP    *SNMP // switch.snmp.*, nil if absent
	// NUTServer is nutserver.*: the controller's "NUT Server" switch for a
	// UPS, pushed only to a device claiming smart_power_caps bit 1. Nil if
	// the block is absent (the switch off, or the capability unclaimed).
	NUTServer *NUTServer

	// Timezone is system.timezone, or locale.timezone when that is absent
	// (every recorded push carries both, equal). Network 10.6.106 sends a
	// POSIX TZ string ("MST7MDT,M3.2.0,M11.1.0"), not a zone name. "" when
	// neither key is present.
	Timezone string
	// Hostname is resolv.host.1.name: the device's UniFi name with the
	// spaces removed, in every push. A real UniFi device takes it as its
	// hostname (and sends it as its DHCP hostname). "" when absent.
	Hostname string
}

// NUTServer is nutserver.* as captured on Network 10.6.106
// (docs/fixtures/controller-10.6.106/system_cfg-ups-nutserver.txt):
// status, id (the UPS name NUT clients address, "ups@host"), port,
// credential (enabled/disabled) and the username/password clients must
// present when it is enabled.
type NUTServer struct {
	Enabled            bool
	ID                 string
	Port               int
	CredentialRequired bool
	Username, Password string
}

// Outlet is outlet.<n>.*: the controller's intent for one outlet on a power
// device. RelayOn defaults to true on first sight, because an outlet the
// controller has not spoken about must not be switched off.
type Outlet struct {
	Index   int
	Name    string // outlet.<n>.name, "" if the controller did not name it
	RelayOn bool   // outlet.<n>.relay_state: "disabled" => false, else true
}

// Address is how the controller wants the device to address itself, from
// three sections of the push (both forms captured on 10.6.106, 2026-09-22):
//
//	Using DHCP  netconf.1.ip=0.0.0.0, dhcpc.1.status=enabled
//	Static      netconf.1.ip=<a>, netconf.1.netmask=<m>, route.1.gateway=<g>
//	            (route.1.ip=0.0.0.0 marks the default route),
//	            resolv.nameserver.N.ip=<dns>, and NO dhcpc.1.* lines at all
//
// So DHCP is "the per-interface DHCP client line exists", not the value of
// a key that is always present.
type Address struct {
	DHCP    bool
	IP      string
	Netmask string
	Gateway string
	DNS     []string
}

// DHCPGuard is one switch.dhcp_guard.N.* group: Server may answer DHCP on
// VLAN. A network trusting three servers is pushed as three groups.
type DHCPGuard struct {
	VLAN   int
	Server string // IPv4 address
}

// MACACL is one switch.acl.mac.N.* group (a UI "MAC" ACL rule): traffic
// within VLAN from Src to Dst is permitted or denied. Name is the UI
// rule's name with "-<n>" appended.
type MACACL struct {
	Name     string
	Permit   bool // action=permit; "deny" => false
	VLAN     int
	Src, Dst MACMatch
	// Unmodelled are the group's lines this parser does not know
	// ("<key>=<value>", the key after switch.acl.mac.N.), and an action
	// other than permit or deny: a shape no capture has shown.
	Unmodelled []string
}

// MACMatch is one side of a MAC ACL: src.mac/src.mask or dst.mac/dst.mask.
// The UI's "Any" is MAC "any" with mask 00:00:00:00:00:00; a single
// address has mask ff:ff:ff:ff:ff:ff; the UI's Netmask 40 is
// ff:ff:ff:ff:ff:00.
type MACMatch struct {
	MAC, Mask string
}

// IPACL is one switch.acl.ip.N.* group (a UI "IPv4" ACL rule). Type is the
// UI's protocol: "ip" (All), "tcp" or "udp"; the UI's TCP/UDP is pushed as
// two groups, a tcp one and a udp one (captured on the DX010, 2026-11-01,
// docs/fixtures/controller-10.6.109/system_cfg-dx010-acl.txt). A UI rule
// naming several destination ports is pushed as one group per port, named
// "<rule>-1", "<rule>-2", ...
type IPACL struct {
	Name     string
	Permit   bool // action=permit; "deny" => false
	Type     string
	Src, Dst IPMatch
	// Unmodelled: as MACACL's (the key after switch.acl.ip.N.).
	Unmodelled []string
}

// IPMatch is one side of an IP ACL. IP is a CIDR, AnyIP for the UI's Any,
// "" when the key is absent (never seen: every captured group names both
// sides). A UI Network side is pushed as a CIDR like an IP side -- the
// network's gateway address with its prefix length, host bits set. On a
// TCP or UDP rule a side with no port is pushed as port_range=1-65535; a
// port the UI named is port=<n>, on either side.
type IPMatch struct {
	IP        string
	Port      int    // .port, 0 when absent
	PortRange string // .port_range as pushed ("1-65535"), "" when absent
}

// AnyPort is the port_range a side with no port is pushed with; AnyIP the
// ip of the UI's Any side.
const (
	AnyPort = "1-65535"
	AnyIP   = "any"
)

// Uncaptured says which part of the rule no capture has shown (the shapes
// recorded are in docs/drivers/sonic.md §11.7.2), "" when every part has
// been seen. A rule shape is translated only once a push of it has been
// recorded: a guess that matches more than the UI rule does is a silent
// hole, one that matches less a silent block.
func (a IPACL) Uncaptured() string {
	switch {
	case len(a.Unmodelled) > 0:
		return "lines never captured: " + strings.Join(a.Unmodelled, ", ")
	case a.Type != "ip" && a.Type != "tcp" && a.Type != "udp":
		return fmt.Sprintf("protocol %q (All, TCP and UDP are captured)", a.Type)
	case a.Src.IP == "" || a.Dst.IP == "":
		return "a side with no ip line (Any is pushed as ip=any)"
	case !ipOrAny(a.Src.IP) || !ipOrAny(a.Dst.IP):
		return "an ip that is neither an IPv4 prefix nor any"
	case a.Type == "ip" && (a.Src.Port != 0 || a.Dst.Port != 0 || a.Src.PortRange != "" || a.Dst.PortRange != ""):
		return "a port on a protocol-All rule (not captured)"
	case a.Src.PortRange != "" && a.Src.PortRange != AnyPort, a.Dst.PortRange != "" && a.Dst.PortRange != AnyPort:
		return "a port range other than " + AnyPort + " (the UI refuses ranges)"
	case a.Src.Port != 0 && a.Src.PortRange != "", a.Dst.Port != 0 && a.Dst.PortRange != "":
		return "a port and a port range on one side (not captured)"
	}
	return ""
}

// ipOrAny: an IPv4 prefix (a.b.c.d/n) or the Any side.
func ipOrAny(s string) bool {
	if s == AnyIP {
		return true
	}
	ip, _, err := net.ParseCIDR(s)
	return err == nil && ip.To4() != nil
}

// Uncaptured is IPACL.Uncaptured for a MAC rule. A side is a MAC under a
// mask, or "any" under the zero mask.
func (a MACACL) Uncaptured() string {
	if len(a.Unmodelled) > 0 {
		return "lines never captured: " + strings.Join(a.Unmodelled, ", ")
	}
	if a.VLAN < 1 || a.VLAN > 4094 {
		return fmt.Sprintf("VLAN %d", a.VLAN)
	}
	for _, m := range []MACMatch{a.Src, a.Dst} {
		isAny := m.MAC == "any" && m.Mask == "00:00:00:00:00:00"
		_, errMAC := net.ParseMAC(m.MAC)
		_, errMask := net.ParseMAC(m.Mask)
		if !isAny && (errMAC != nil || errMask != nil) {
			return fmt.Sprintf("a side %q/%q that is neither a MAC under a mask nor any (not captured)", m.MAC, m.Mask)
		}
	}
	return ""
}

// SNMP is switch.snmp.*: status, version ("1_2c", "3"), and the v1/v2c
// read-only community (switch.snmp.community.public.name).
type SNMP struct {
	Enabled   bool
	Version   string
	Community string
}

// Unsupported lists features the controller asked for by key that no driver
// can honour; the loop logs them so the operator learns why the UI setting
// has no effect. Keyed by feature name -> the raw keys seen.
func (c *Config) Unsupported(features map[string]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range c.Raw {
		for prefix, feature := range features {
			if strings.Contains(k, prefix) && v != "" && v != "disabled" && v != "false" {
				out[feature] = append(out[feature], k+"="+v)
			}
		}
	}
	for f := range out {
		sort.Strings(out[f])
	}
	return out
}

// Port is one switch.port.N.* group plus its switch.vlan.<slot>.port.N.mode lines.
type Port struct {
	Index   int
	Name    string
	Enabled bool   // switch.port.N.status: absent or "enabled" => true, "disabled" => false
	OpMode  string // "switch", ...

	// Link speed. AutoNeg is true unless switch.port.N.autoneg=disabled, in
	// which case SpeedMbps (switch.port.N.speed) and FullDuplex
	// (switch.port.N.duplex=enabled) apply.
	AutoNeg    bool
	SpeedMbps  int
	FullDuplex bool

	// VLAN membership. VLANExplicit is true when the controller sent any
	// switch.vlan.<slot>.port.N.mode line for this port; then NativeVLAN is
	// the untagged VLAN ID (0 = none) and TaggedVLANs the tagged IDs, sorted.
	// Without explicit lines the port carries every VLAN tagged with VLAN 1
	// untagged (the controller's "Allow All" default).
	VLANExplicit bool
	NativeVLAN   int
	TaggedVLANs  []int

	// Feature keys the controller sends only once the device claims the
	// matching capability. FEC: "cl-91" (Reed-Solomon), "cl-74" (fire-code),
	// "" = key absent (leave the switch alone). Storm control levels are
	// percentages; -1 = that traffic type not limited.
	LAG         int // switch.port.N.lag when opmode=aggregate
	MirrorPort  int // switch.port.N.mirror_port when opmode=mirror: the SOURCE whose traffic is copied to this port
	FEC         string
	StormCtrl   StormControl
	STPPortMode bool  // switch.port.N.stp.port_mode (default enabled)
	BPDUGuard   bool  // switch.port.N.stp.bpdu_guard
	LLDPMED     *bool // switch.port.N.lldpmed.opmode, nil if absent
	// STPEdge is switch.port.N.stp.edge as pushed: "auto", "enabled" or
	// "disabled" (Edge Port in the UI; the ECS push carries auto and
	// disabled); "" when absent.
	STPEdge string
	// Keepalive is switch.port.N.keepalive == enabled: the UI's Loop
	// Protection (port_keepalive_enabled in the device record), pushed only
	// to a device claiming KEEPALIVE_LOOP_PROTECTION. The ld_mode key some
	// ports carry beside it is not loop protection: on the ECS push it
	// appears on exactly the ports with a forced speed.
	Keepalive bool
}

// StormControl is switch.port.N.stormctrl.*.
type StormControl struct {
	Enabled bool
	Type    string // "level" (percent) | "rate" (pps)
	Bcast   int
	Mcast   int
	Ucast   int
}

// User is one users.N.* group.
type User struct {
	Name         string
	PasswordHash string
	Enabled      bool
}

// SSHKey is one sshd.auth.key.N.* group.
type SSHKey struct {
	Type    string
	Value   string
	Comment string
	Enabled bool
}

// STP is the switch-level switch.stp.* group. Set is false when absent.
type STP struct {
	Set      bool
	Enabled  bool
	Version  string // "rstp" | "stp" | ...
	Priority int
}

// VLAN is one switch.vlan.N.* group.
type VLAN struct {
	ID        int
	Mode      string // "tagged" | "untagged"
	Enabled   bool
	igmpSet   bool
	igmpSnoop bool
	igmp      IGMP
}

// IGMP is one VLAN's snooping settings beside igmp_snooping. Times are in
// seconds; 0 = absent.
type IGMP struct {
	Querier      bool   // igmp_querier_status
	QuerierAddr  string // igmp_querier ("" when the push leaves it empty)
	FastLeave    bool   // igmp_fastleave
	MaxResponse  int    // igmp_maxresponse
	Membership   int    // igmp_groupmembership
	RouterExpiry int    // igmp_mcrtrexpiretime
	Suppression  bool   // igmp_supression (the controller's spelling)
	// FloodUnknown is forward_unknown_multicast; nil = absent.
	FloodUnknown *bool
}

// Multicast is switch.multicast.*; nil = absent.
type Multicast struct {
	FloodKnownProtocols         *bool // flood_known_protocols
	ForwardUnknownToRouterPorts *bool // forward_unk_mcast_router_ports
}

var (
	portRe     = regexp.MustCompile(`^switch\.port\.(\d+)\.([\w.]+)$`)
	vlanRe     = regexp.MustCompile(`^switch\.vlan\.(\d+)\.(\w+)$`)
	vlanPortRe = regexp.MustCompile(`^switch\.vlan\.(\d+)\.port\.(\d+)\.mode$`)
	outletRe   = regexp.MustCompile(`^outlet\.(\d+)\.([\w.]+)$`)
	guardRe    = regexp.MustCompile(`^switch\.dhcp_guard\.(\d+)\.(\w+)$`)
	aclRe      = regexp.MustCompile(`^switch\.acl\.(mac|ip)\.(\d+)\.([\w.]+)$`)
)

// Parse reads system_cfg text. Unknown keys are kept in Raw and ignored.
func Parse(text string) *Config {
	c := &Config{Raw: map[string]string{}, Ports: map[int]Port{}, Outlets: map[int]Outlet{}}
	routeGW, defaultRoute := map[string]string{}, map[string]bool{}
	vlans := map[int]*VLAN{}
	type membership struct {
		slot, port int
		mode       string
	}
	var members []membership
	type ntpEntry struct {
		server  string
		enabled bool
	}
	ntp := map[int]ntpEntry{}
	sshKeys := map[int]*SSHKey{}
	users := map[int]*User{}
	type guardEntry struct {
		DHCPGuard
		enabled bool
	}
	guards := map[int]*guardEntry{}
	macACLs, ipACLs := map[int]*MACACL{}, map[int]*IPACL{}
	syslogEnabled, syslogIP, syslogPort := false, "", ""
	localeTZ := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		c.Raw[k] = v
		if m := vlanPortRe.FindStringSubmatch(k); m != nil {
			slot, _ := strconv.Atoi(m[1])
			port, _ := strconv.Atoi(m[2])
			members = append(members, membership{slot, port, v})
			continue
		}
		if strings.HasPrefix(k, "netconf.1.") || k == "dhcpc.1.status" || strings.HasPrefix(k, "route.") || strings.HasPrefix(k, "resolv.nameserver.") {
			if c.Address == nil {
				c.Address = &Address{}
			}
			switch {
			case k == "netconf.1.ip":
				if v != "0.0.0.0" {
					c.Address.IP = v
				}
			case k == "netconf.1.netmask":
				c.Address.Netmask = v
			case k == "dhcpc.1.status":
				c.Address.DHCP = v == "enabled"
			case strings.HasPrefix(k, "route.") && strings.HasSuffix(k, ".gateway"):
				// route.N.gateway is the default gateway when route.N.ip is
				// 0.0.0.0; the one static push on record has exactly one route.
				routeGW[strings.TrimSuffix(strings.TrimPrefix(k, "route."), ".gateway")] = v
			case strings.HasPrefix(k, "route.") && strings.HasSuffix(k, ".ip"):
				if v == "0.0.0.0" {
					defaultRoute[strings.TrimSuffix(strings.TrimPrefix(k, "route."), ".ip")] = true
				}
			case strings.HasPrefix(k, "resolv.nameserver.") && strings.HasSuffix(k, ".ip"):
				if v != "" && v != "0.0.0.0" {
					c.Address.DNS = append(c.Address.DNS, v)
				}
			}
			c.Raw[k] = v
			continue
		}
		if m := outletRe.FindStringSubmatch(k); m != nil {
			idx, _ := strconv.Atoi(m[1])
			o := c.outlet(idx)
			switch m[2] {
			case "relay_state":
				o.RelayOn = v != "disabled"
			case "name":
				o.Name = v
			}
			c.Outlets[idx] = o
			continue
		}
		if m := guardRe.FindStringSubmatch(k); m != nil {
			n, _ := strconv.Atoi(m[1])
			g := guards[n]
			if g == nil {
				g = &guardEntry{}
				guards[n] = g
			}
			switch m[2] {
			case "server":
				g.Server = v
			case "vlan":
				g.VLAN, _ = strconv.Atoi(v)
			case "status":
				g.enabled = v == "enabled"
			}
			continue
		}
		if m := aclRe.FindStringSubmatch(k); m != nil {
			n, _ := strconv.Atoi(m[2])
			if m[1] == "mac" {
				a := macACLs[n]
				if a == nil {
					a = &MACACL{}
					macACLs[n] = a
				}
				switch m[3] {
				case "name":
					a.Name = v
				case "action":
					a.Permit = v == "permit"
					if v != "permit" && v != "deny" {
						a.Unmodelled = append(a.Unmodelled, m[3]+"="+v)
					}
				case "vlan":
					a.VLAN, _ = strconv.Atoi(v)
				case "src.mac":
					a.Src.MAC = v
				case "src.mask":
					a.Src.Mask = v
				case "dst.mac":
					a.Dst.MAC = v
				case "dst.mask":
					a.Dst.Mask = v
				default:
					a.Unmodelled = append(a.Unmodelled, m[3]+"="+v)
				}
				continue
			}
			a := ipACLs[n]
			if a == nil {
				a = &IPACL{}
				ipACLs[n] = a
			}
			side, field, _ := strings.Cut(m[3], ".")
			var end *IPMatch
			switch side {
			case "src":
				end = &a.Src
			case "dst":
				end = &a.Dst
			}
			switch {
			case m[3] == "name":
				a.Name = v
			case m[3] == "action":
				a.Permit = v == "permit"
				if v != "permit" && v != "deny" {
					a.Unmodelled = append(a.Unmodelled, m[3]+"="+v)
				}
			case m[3] == "type":
				a.Type = v
			case end != nil && field == "ip":
				end.IP = v
			case end != nil && field == "port":
				end.Port, _ = strconv.Atoi(v)
			case end != nil && field == "port_range":
				end.PortRange = v
			default:
				a.Unmodelled = append(a.Unmodelled, m[3]+"="+v)
			}
			continue
		}
		if m := portRe.FindStringSubmatch(k); m != nil {
			idx, _ := strconv.Atoi(m[1])
			p := c.port(idx)
			switch m[2] {
			case "name":
				p.Name = v
			case "status":
				p.Enabled = v != "disabled"
			case "opmode":
				p.OpMode = v
			case "autoneg":
				p.AutoNeg = v != "disabled"
			case "speed":
				p.SpeedMbps, _ = strconv.Atoi(v)
			case "duplex":
				p.FullDuplex = v == "enabled"
			case "lag":
				p.LAG, _ = strconv.Atoi(v)
			case "mirror_port":
				p.MirrorPort, _ = strconv.Atoi(v)
			case "fec":
				p.FEC = v
			case "stp.port_mode":
				p.STPPortMode = v != "disabled"
			case "stp.bpdu_guard":
				p.BPDUGuard = v == "enabled"
			case "stp.edge":
				p.STPEdge = v
			case "keepalive":
				p.Keepalive = v == "enabled"
			case "lldpmed.opmode":
				b := v == "enabled"
				p.LLDPMED = &b
			case "stormctrl.status":
				p.StormCtrl.Enabled = v == "enabled"
			case "stormctrl.type":
				p.StormCtrl.Type = v
			case "stormctrl.bcast":
				p.StormCtrl.Bcast, _ = strconv.Atoi(v)
			case "stormctrl.mcast":
				p.StormCtrl.Mcast, _ = strconv.Atoi(v)
			case "stormctrl.ucast":
				p.StormCtrl.Ucast, _ = strconv.Atoi(v)
			}
			c.Ports[idx] = p
			continue
		}
		if m := vlanRe.FindStringSubmatch(k); m != nil {
			n, _ := strconv.Atoi(m[1])
			vl, seen := vlans[n]
			if !seen {
				vl = &VLAN{Enabled: true}
				vlans[n] = vl
			}
			switch m[2] {
			case "id":
				vl.ID, _ = strconv.Atoi(v)
			case "mode":
				vl.Mode = v
			case "status":
				vl.Enabled = v != "disabled"
			case "igmp_snooping":
				vl.igmpSet, vl.igmpSnoop = true, v == "true"
			case "igmp_querier_status":
				vl.igmp.Querier = v == "true"
			case "igmp_querier":
				vl.igmp.QuerierAddr = v
			case "igmp_fastleave":
				vl.igmp.FastLeave = v == "true"
			case "igmp_maxresponse":
				vl.igmp.MaxResponse, _ = strconv.Atoi(v)
			case "igmp_groupmembership":
				vl.igmp.Membership, _ = strconv.Atoi(v)
			case "igmp_mcrtrexpiretime":
				vl.igmp.RouterExpiry, _ = strconv.Atoi(v)
			case "igmp_supression":
				vl.igmp.Suppression = v == "true"
			case "forward_unknown_multicast":
				b := v == "true"
				vl.igmp.FloodUnknown = &b
			}
			continue
		}
		if strings.HasPrefix(k, "users.") && k != "users.status" {
			var n int
			var field string
			if _, err := fmt.Sscanf(k, "users.%d.%s", &n, &field); err == nil {
				u := users[n]
				if u == nil {
					u = &User{}
					users[n] = u
				}
				switch field {
				case "name":
					u.Name = v
				case "password":
					u.PasswordHash = v
				case "status":
					u.Enabled = v == "enabled"
				}
			}
			continue
		}
		if strings.HasPrefix(k, "sshd.auth.key.") {
			var n int
			var field string
			if _, err := fmt.Sscanf(k, "sshd.auth.key.%d.%s", &n, &field); err == nil {
				e := sshKeys[n]
				if e == nil {
					e = &SSHKey{}
					sshKeys[n] = e
				}
				switch field {
				case "type":
					e.Type = v
				case "value":
					e.Value = v
				case "comment":
					e.Comment = v
				case "status":
					e.Enabled = v == "enabled"
				}
			}
			continue
		}
		if strings.HasPrefix(k, "ntpclient.") {
			var n int
			var field string
			if _, err := fmt.Sscanf(k, "ntpclient.%d.%s", &n, &field); err == nil {
				e := ntp[n]
				switch field {
				case "server":
					e.server = v
				case "status":
					e.enabled = v == "enabled"
				}
				ntp[n] = e
			}
			continue
		}
		switch k {
		case "system.timezone":
			c.Timezone = strings.TrimSpace(v)
		case "locale.timezone":
			localeTZ = strings.TrimSpace(v)
		case "resolv.host.1.name":
			c.Hostname = strings.TrimSpace(v)
		case "syslog.remote.status": // syslog.status/syslog.level are the device's own logging, pushed enabled/7 even with remote logging off
			syslogEnabled = v == "enabled"
		case "syslog.remote.ip":
			syslogIP = v
		case "syslog.remote.port":
			syslogPort = v
		case "switch.jumboframes":
			c.JumboFrames = v == "enabled"
			c.JumboSet = true
		case "switch.dhcp_snoop.status":
			b := v == "enabled"
			c.DHCPSnoop = &b
		case "switch.snmp.status", "switch.snmp.version", "switch.snmp.community.public.name":
			if c.SNMP == nil {
				c.SNMP = &SNMP{}
			}
			switch k {
			case "switch.snmp.status":
				c.SNMP.Enabled = v == "enabled"
			case "switch.snmp.version":
				c.SNMP.Version = v
			default:
				c.SNMP.Community = v
			}
		case "nutserver.status", "nutserver.id", "nutserver.port", "nutserver.credential", "nutserver.username", "nutserver.password":
			if c.NUTServer == nil {
				c.NUTServer = &NUTServer{}
			}
			switch k {
			case "nutserver.status":
				c.NUTServer.Enabled = v == "enabled"
			case "nutserver.id":
				c.NUTServer.ID = v
			case "nutserver.port":
				c.NUTServer.Port, _ = strconv.Atoi(v)
			case "nutserver.credential":
				c.NUTServer.CredentialRequired = v == "enabled"
			case "nutserver.username":
				c.NUTServer.Username = v
			case "nutserver.password":
				c.NUTServer.Password = v
			}
		case "switch.mtu":
			c.MTU, _ = strconv.Atoi(v)
		case "switch.multicast.flood_known_protocols":
			b := v == "true"
			c.Multicast.FloodKnownProtocols = &b
		case "switch.multicast.forward_unk_mcast_router_ports":
			b := v == "true"
			c.Multicast.ForwardUnknownToRouterPorts = &b
		case "switch.stp.status":
			c.STP.Set, c.STP.Enabled = true, v != "disabled"
		case "switch.stp.version":
			c.STP.Set, c.STP.Version = true, v
		case "switch.stp.priority":
			c.STP.Set = true
			c.STP.Priority, _ = strconv.Atoi(v)
		}
	}
	keys := make([]int, 0, len(vlans))
	for n := range vlans {
		keys = append(keys, n)
	}
	sort.Ints(keys)
	for _, n := range keys {
		c.VLANs = append(c.VLANs, *vlans[n])
		if vlans[n].igmpSet && vlans[n].ID > 0 {
			if c.IGMPSnooping == nil {
				c.IGMPSnooping = map[int]bool{}
			}
			c.IGMPSnooping[vlans[n].ID] = vlans[n].igmpSnoop
			if c.IGMP == nil {
				c.IGMP = map[int]IGMP{}
			}
			c.IGMP[vlans[n].ID] = vlans[n].igmp
		}
	}
	// Resolve per-port VLAN membership: slot -> VLAN ID.
	for _, mb := range members {
		vl, ok := vlans[mb.slot]
		if !ok || vl.ID == 0 {
			continue
		}
		p := c.port(mb.port)
		p.VLANExplicit = true
		switch mb.mode {
		case "untagged":
			p.NativeVLAN = vl.ID
		case "tagged":
			p.TaggedVLANs = append(p.TaggedVLANs, vl.ID)
		}
		c.Ports[mb.port] = p
	}
	for idx, p := range c.Ports {
		sort.Ints(p.TaggedVLANs)
		c.Ports[idx] = p
	}
	nkeys := make([]int, 0, len(ntp))
	for n := range ntp {
		nkeys = append(nkeys, n)
	}
	sort.Ints(nkeys)
	for _, n := range nkeys {
		if e := ntp[n]; e.enabled && e.server != "" {
			c.NTPServers = append(c.NTPServers, e.server)
		}
	}
	ukeys := make([]int, 0, len(users))
	for n := range users {
		ukeys = append(ukeys, n)
	}
	sort.Ints(ukeys)
	for _, n := range ukeys {
		if u := users[n]; u.Enabled && u.Name != "" && strings.HasPrefix(u.PasswordHash, "$") {
			c.Users = append(c.Users, *u)
		}
	}
	skeys := make([]int, 0, len(sshKeys))
	for n := range sshKeys {
		skeys = append(skeys, n)
	}
	sort.Ints(skeys)
	for _, n := range skeys {
		if e := sshKeys[n]; e.Enabled && e.Type != "" && e.Value != "" {
			c.SSHKeys = append(c.SSHKeys, *e)
		}
	}
	gkeys := make([]int, 0, len(guards))
	for n := range guards {
		gkeys = append(gkeys, n)
	}
	sort.Ints(gkeys)
	for _, n := range gkeys {
		if g := guards[n]; g.enabled && g.Server != "" && g.VLAN > 0 {
			c.DHCPGuards = append(c.DHCPGuards, g.DHCPGuard)
		}
	}
	mkeys := make([]int, 0, len(macACLs))
	for n := range macACLs {
		mkeys = append(mkeys, n)
	}
	sort.Ints(mkeys)
	for _, n := range mkeys {
		c.MACACLs = append(c.MACACLs, *macACLs[n])
	}
	ikeys := make([]int, 0, len(ipACLs))
	for n := range ipACLs {
		ikeys = append(ikeys, n)
	}
	sort.Ints(ikeys)
	for _, n := range ikeys {
		c.IPACLs = append(c.IPACLs, *ipACLs[n])
	}
	if syslogEnabled && syslogIP != "" {
		h := syslogIP
		if syslogPort != "" && syslogPort != "514" {
			h += ":" + syslogPort
		}
		c.SyslogHosts = []string{h}
	}
	if c.Timezone == "" {
		c.Timezone = localeTZ
	}
	if c.Address != nil {
		for n, gw := range routeGW {
			if defaultRoute[n] {
				c.Address.Gateway = gw
			}
		}
		sort.Strings(c.Address.DNS)
	}
	return c
}

// port returns the port's current parse state, creating the defaults
// (enabled, autoneg) on first sight.
func (c *Config) port(idx int) Port {
	p, seen := c.Ports[idx]
	if !seen {
		p = Port{Index: idx, Enabled: true, AutoNeg: true, FullDuplex: true, STPPortMode: true,
			StormCtrl: StormControl{Bcast: -1, Mcast: -1, Ucast: -1}}
	}
	return p
}

// outlet returns the outlet's current parse state, creating the default
// (relay on) on first sight: an outlet the controller has not switched off
// must not be switched off by a missing key.
func (c *Config) outlet(idx int) Outlet {
	o, seen := c.Outlets[idx]
	if !seen {
		o = Outlet{Index: idx, RelayOn: true}
	}
	return o
}

// OutletIndexes returns the configured outlet indexes in ascending order.
func (c *Config) OutletIndexes() []int {
	out := make([]int, 0, len(c.Outlets))
	for i := range c.Outlets {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}

// VLANIDs returns every site VLAN ID the config lists, ascending.
func (c *Config) VLANIDs() []int {
	out := make([]int, 0, len(c.VLANs))
	for _, v := range c.VLANs {
		if v.ID > 0 {
			out = append(out, v.ID)
		}
	}
	sort.Ints(out)
	return out
}

// PortIndexes returns the configured port indexes in ascending order.
func (c *Config) PortIndexes() []int {
	out := make([]int, 0, len(c.Ports))
	for i := range c.Ports {
		out = append(out, i)
	}
	sort.Ints(out)
	return out
}
