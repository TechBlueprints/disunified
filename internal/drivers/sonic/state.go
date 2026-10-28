package sonic

import (
	"sort"
	"strings"
)

// swState is the switch's CONFIG_DB as the control side diffs against it:
// what the last collect read, plus the effect of every write since. A plan
// works on a copy and applies each planned write to it, so later steps of
// the same apply see the earlier ones (a LAG member stripped of its VLANs,
// a mirror destination freed).
type swState struct {
	port    map[string]map[string]string // CONFIG_DB PORT fields by interface (mtu from APPL_DB when CONFIG has none)
	vlans   map[int]bool                 // SONiC VLAN ids
	members map[string]map[int]bool      // interface or PortChannel -> SONiC VLAN id -> tagged
	lags    map[string]map[string]bool   // PortChannel -> member interfaces
	lagMTU  map[string]string            // PortChannel -> its CONFIG_DB mtu
	mirrors map[string]map[string]string // MIRROR_SESSION name -> fields
	storm   map[string]map[string]int    // interface -> storm type -> kbps
	stp     map[string]map[string]string // STP_PORT by interface

	// Device level (§6.3, §6.4).
	stpGlobal map[string]string            // STP|GLOBAL, nil when STP is not configured
	ntp       map[string]bool              // NTP_SERVER addresses
	syslog    map[string]string            // SYSLOG_SERVER address -> port ("" = default)
	snmp      map[string]bool              // SNMP_COMMUNITY names
	dns       map[string]bool              // DNS_NAMESERVER addresses
	routes    map[string]map[string]string // STATIC_ROUTE by "<vrf>|<prefix>"
	keys      []string                     // the bridge's block in authorized_keys, one key per line

	meta map[string]string // DEVICE_METADATA|localhost (hostname, timezone)

	stpFeature   bool // FEATURE|stp exists: the image has the STP daemon
	platformJSON bool // the platform has a platform.json: dynamic port breakout
	// platform is platform.json's interfaces block (parent port -> index,
	// lanes, breakout modes), nil where absent; read-only, shared by clones.
	platform map[string]platformPort
	brkout   map[string]string // BREAKOUT_CFG: parent port -> brkout_mode
}

func newState(db map[string]table, sec map[string]string) *swState {
	s := &swState{
		port: map[string]map[string]string{}, vlans: map[int]bool{}, members: map[string]map[int]bool{},
		lags: map[string]map[string]bool{}, mirrors: map[string]map[string]string{},
		storm: stormTable(db["cfg_storm"]), stp: map[string]map[string]string{},
	}
	appl := db["appl_port"].byName(":")
	for name, f := range db["cfg_port"].byName("|") {
		m := copyFields(f)
		if m["mtu"] == "" && appl[name]["mtu"] != "" {
			m["mtu"] = appl[name]["mtu"]
		}
		s.port[name] = m
	}
	for k := range db["cfg_vlan"] {
		_, n, _ := strings.Cut(k, "|")
		if id := atoi(strings.TrimPrefix(n, "Vlan")); id > 0 {
			s.vlans[id] = true
		}
	}
	for k, f := range db["cfg_vlan_member"] {
		parts := strings.Split(k, "|")
		if len(parts) != 3 {
			continue
		}
		if id := atoi(strings.TrimPrefix(parts[1], "Vlan")); id > 0 {
			s.setMember(parts[2], id, f["tagging_mode"] == "tagged")
		}
	}
	s.lagMTU = map[string]string{}
	for k, f := range db["cfg_lag"] {
		_, n, _ := strings.Cut(k, "|")
		if s.lags[n] == nil {
			s.lags[n] = map[string]bool{}
		}
		s.lagMTU[n] = f["mtu"]
	}
	for k := range db["cfg_lag_member"] {
		parts := strings.Split(k, "|")
		if len(parts) != 3 {
			continue
		}
		if s.lags[parts[1]] == nil {
			s.lags[parts[1]] = map[string]bool{}
		}
		s.lags[parts[1]][parts[2]] = true
	}
	for k, f := range db["cfg_mirror"] {
		_, n, _ := strings.Cut(k, "|")
		s.mirrors[n] = copyFields(f)
	}
	for k, f := range db["cfg_stp"] {
		if name, ok := strings.CutPrefix(k, "STP_PORT|"); ok {
			s.stp[name] = copyFields(f)
		}
	}
	if g, ok := db["cfg_stp"]["STP|GLOBAL"]; ok {
		s.stpGlobal = copyFields(g)
	}
	s.ntp, s.syslog, s.snmp, s.dns, s.routes = map[string]bool{}, map[string]string{}, map[string]bool{}, map[string]bool{}, map[string]map[string]string{}
	for k := range db["cfg_ntp"] {
		if n, ok := strings.CutPrefix(k, "NTP_SERVER|"); ok {
			s.ntp[n] = true
		}
	}
	for k, f := range db["cfg_syslog"] {
		if n, ok := strings.CutPrefix(k, "SYSLOG_SERVER|"); ok {
			s.syslog[n] = f["port"]
		}
	}
	for k := range db["cfg_snmp"] {
		if n, ok := strings.CutPrefix(k, "SNMP_COMMUNITY|"); ok {
			s.snmp[n] = true
		}
	}
	for k := range db["cfg_dns"] {
		if n, ok := strings.CutPrefix(k, "DNS_NAMESERVER|"); ok {
			s.dns[n] = true
		}
	}
	for k, f := range db["cfg_route"] {
		if n, ok := strings.CutPrefix(k, "STATIC_ROUTE|"); ok {
			s.routes[n] = copyFields(f)
		}
	}
	for _, line := range strings.Split(sec["authorized_keys"], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			s.keys = append(s.keys, line)
		}
	}
	_, s.stpFeature = db["cfg_feature"]["FEATURE|stp"]
	s.platformJSON, s.platform = parsePlatform(sec["platform_json"])
	s.brkout = map[string]string{}
	for k, f := range db["cfg_breakout"] {
		if n, ok := strings.CutPrefix(k, "BREAKOUT_CFG|"); ok {
			s.brkout[n] = f["brkout_mode"]
		}
	}
	s.meta = copyFields(db["metadata"].first())
	return s
}

// defaultRoute is the static default route's next hop(s), "" if none.
func (s *swState) defaultRoute() string {
	return s.routes["default|0.0.0.0/0"]["nexthop"]
}

func copyFields(f map[string]string) map[string]string {
	m := make(map[string]string, len(f))
	for k, v := range f {
		m[k] = v
	}
	return m
}

func (s *swState) clone() *swState {
	c := &swState{
		port: map[string]map[string]string{}, vlans: map[int]bool{}, members: map[string]map[int]bool{},
		lags: map[string]map[string]bool{}, mirrors: map[string]map[string]string{},
		storm: map[string]map[string]int{}, stp: map[string]map[string]string{},
		stpFeature: s.stpFeature, platformJSON: s.platformJSON, platform: s.platform,
		meta: copyFields(s.meta), brkout: map[string]string{},
	}
	for k, v := range s.brkout {
		c.brkout[k] = v
	}
	for k, v := range s.port {
		c.port[k] = copyFields(v)
	}
	for k := range s.vlans {
		c.vlans[k] = true
	}
	for k, v := range s.members {
		m := map[int]bool{}
		for id, t := range v {
			m[id] = t
		}
		c.members[k] = m
	}
	for k, v := range s.lags {
		m := map[string]bool{}
		for p := range v {
			m[p] = true
		}
		c.lags[k] = m
	}
	c.lagMTU = map[string]string{}
	for k, v := range s.lagMTU {
		c.lagMTU[k] = v
	}
	for k, v := range s.mirrors {
		c.mirrors[k] = copyFields(v)
	}
	for k, v := range s.storm {
		m := map[string]int{}
		for t, n := range v {
			m[t] = n
		}
		c.storm[k] = m
	}
	for k, v := range s.stp {
		c.stp[k] = copyFields(v)
	}
	if s.stpGlobal != nil {
		c.stpGlobal = copyFields(s.stpGlobal)
	}
	c.ntp, c.syslog, c.snmp, c.dns, c.routes = map[string]bool{}, map[string]string{}, map[string]bool{}, map[string]bool{}, map[string]map[string]string{}
	for k := range s.ntp {
		c.ntp[k] = true
	}
	for k, v := range s.syslog {
		c.syslog[k] = v
	}
	for k := range s.snmp {
		c.snmp[k] = true
	}
	for k := range s.dns {
		c.dns[k] = true
	}
	for k, v := range s.routes {
		c.routes[k] = copyFields(v)
	}
	c.keys = append([]string(nil), s.keys...)
	return c
}

func (s *swState) field(iface, key string) string { return s.port[iface][key] }

func (s *swState) setField(iface, key, value string) {
	if s.port[iface] == nil {
		s.port[iface] = map[string]string{}
	}
	if value == "" {
		delete(s.port[iface], key)
		return
	}
	s.port[iface][key] = value
}

func (s *swState) setMember(iface string, id int, tagged bool) {
	if s.members[iface] == nil {
		s.members[iface] = map[int]bool{}
	}
	s.members[iface][id] = tagged
}

func (s *swState) delMember(iface string, id int) {
	delete(s.members[iface], id)
}

// memberIDs lists an interface's VLAN memberships, ascending.
func (s *swState) memberIDs(iface string) []int {
	var out []int
	for id := range s.members[iface] {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

// lagOf is the PortChannel an interface belongs to, "" if none.
func (s *swState) lagOf(iface string) string {
	for pc, m := range s.lags {
		if m[iface] {
			return pc
		}
	}
	return ""
}

// mirrorTo is the session whose destination is iface, "" if none.
func (s *swState) mirrorTo(iface string) string {
	names := make([]string, 0, len(s.mirrors))
	for n := range s.mirrors {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if s.mirrors[n]["dst_port"] == iface {
			return n
		}
	}
	return ""
}

func (s *swState) setSTP(iface, key, value string) {
	if s.stp[iface] == nil {
		s.stp[iface] = map[string]string{}
	}
	s.stp[iface][key] = value
}
