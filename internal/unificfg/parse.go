// Package unificfg parses the controller's system_cfg: the UniFi device
// configuration file (key=value text, "#"-comment section headers) that the
// controller pushes in a setparam reply whenever the device's reported
// cfgversion differs from the site's. On a real UniFi switch this is
// /tmp/system.cfg. Observed from Network 10.6.106; a scrubbed capture is in
// docs/fixtures/controller-10.6.106/system_cfg.txt.
package unificfg

import (
	"fmt"
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

	// Outlets is a power device's outlet intent, by 1-based outlet index.
	// Its keys are top-level (outlet.<n>.*), not switch.*, because the
	// controller treats a power device's outlets as a separate plane.
	Outlets map[int]Outlet

	SSHKeys     []SSHKey // sshd.auth.key.N.*, enabled ones, in order
	Users       []User   // users.N.*: the device login the UniFi terminal uses (password is an MD5-crypt hash)
	NTPServers  []string // ntpclient.N.server, in order, only enabled ones
	SyslogHosts []string // syslog.ip[:port] when syslog.status=enabled and an ip is set
	JumboFrames bool     // switch.jumboframes=enabled
	DHCPSnoop   *bool    // switch.dhcp_snoop.status, nil if absent
	SNMP        *SNMP    // switch.snmp.*, nil if absent
}

// Outlet is outlet.<n>.*: the controller's intent for one outlet on a power
// device. RelayOn defaults to true on first sight, because an outlet the
// controller has not spoken about must not be switched off.
type Outlet struct {
	Index   int
	Name    string // outlet.<n>.name, "" if the controller did not name it
	RelayOn bool   // outlet.<n>.relay_state: "disabled" => false, else true
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
}

var (
	portRe     = regexp.MustCompile(`^switch\.port\.(\d+)\.([\w.]+)$`)
	vlanRe     = regexp.MustCompile(`^switch\.vlan\.(\d+)\.(\w+)$`)
	vlanPortRe = regexp.MustCompile(`^switch\.vlan\.(\d+)\.port\.(\d+)\.mode$`)
	outletRe   = regexp.MustCompile(`^outlet\.(\d+)\.([\w.]+)$`)
)

// Parse reads system_cfg text. Unknown keys are kept in Raw and ignored.
func Parse(text string) *Config {
	c := &Config{Raw: map[string]string{}, Ports: map[int]Port{}, Outlets: map[int]Outlet{}}
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
	syslogEnabled, syslogIP, syslogPort := false, "", ""
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
		case "syslog.status":
			syslogEnabled = v == "enabled"
		case "syslog.ip":
			syslogIP = v
		case "syslog.port":
			syslogPort = v
		case "switch.jumboframes":
			c.JumboFrames = v == "enabled"
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
		case "switch.mtu":
			c.MTU, _ = strconv.Atoi(v)
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
	if syslogEnabled && syslogIP != "" {
		h := syslogIP
		if syslogPort != "" && syslogPort != "514" {
			h += ":" + syslogPort
		}
		c.SyslogHosts = []string{h}
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
