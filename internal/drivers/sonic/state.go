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
	vlanMTU map[int]string               // VLAN id -> its CONFIG_DB mtu ("" = absent: SONiC's 9100)
	// netMTU is the kernel's MTU per netdev (`ip -j addr`): Vlan<id>, which
	// vlanmgrd never changes from its parent Bridge's 9100 whatever the
	// VLAN table says, and Bridge itself.
	netMTU  map[string]int
	members map[string]map[int]bool      // interface or PortChannel -> SONiC VLAN id -> tagged
	lags    map[string]map[string]bool   // PortChannel -> member interfaces
	lagMTU  map[string]string            // PortChannel -> its CONFIG_DB mtu
	lagLACP map[string]lacp              // PortChannel -> its CONFIG_DB fast_rate and min_links
	mirrors map[string]map[string]string // MIRROR_SESSION name -> fields
	storm   map[string]map[string]int    // interface -> storm type -> kbps
	stp     map[string]map[string]string // STP_PORT by interface

	// Device level (§6.3, §6.4).
	stpGlobal map[string]string // STP|GLOBAL, nil when STP is not configured
	ntp       map[string]bool   // NTP_SERVER addresses
	ntpGlobal map[string]string // NTP|global (src_intf, vrf, …), nil when absent
	// chrony's sources (chrony_sources), how many are online and offline
	// (chrony_activity; chronyKnown false without it), and which interfaces
	// have carrier: read-only, shared by clones (§10.6).
	chrony                      []chronySource
	chronyOnline, chronyOffline int
	chronyKnown                 bool
	carrier                     map[string]bool
	syslog                      map[string]string // SYSLOG_SERVER address -> port ("" = default)
	snmp                        map[string]bool   // SNMP_COMMUNITY names
	// snmpYml are the communities /etc/sonic/snmp.yml names, which the snmp
	// container imports into SNMP_COMMUNITY on every start (§10.1);
	// snmpYmlOK is false when collect.py could not read the file.
	snmpYml   map[string]bool
	snmpYmlOK bool
	dns       map[string]bool              // DNS_NAMESERVER addresses
	routes    map[string]map[string]string // STATIC_ROUTE by "<vrf>|<prefix>"
	keys      []string                     // the bridge's block in authorized_keys, one key per line
	// ifaddrs are the in-band addresses CONFIG_DB carries, "<iface>|<ip>/<len>"
	// from VLAN_INTERFACE, INTERFACE, PORTCHANNEL_INTERFACE and
	// LOOPBACK_INTERFACE (MGMT_INTERFACE, eth0's, is not among them).
	ifaddrs map[string]bool

	meta map[string]string // DEVICE_METADATA|localhost (hostname, timezone)

	stpFeature   bool // FEATURE|stp exists: the image has the STP daemon
	platformJSON bool // the platform has a platform.json: dynamic port breakout
	// platform is platform.json's interfaces block (parent port -> index,
	// lanes, breakout modes), nil where absent; read-only, shared by clones.
	platform map[string]platformPort
	brkout   map[string]string // BREAKOUT_CFG: parent port -> brkout_mode
	// fecs is STATE_DB's supported_fecs per interface ("none,rs" on the
	// DX010; absent on the VS); read-only, shared by clones.
	fecs map[string]map[string]bool
	// switchCap is STATE_DB's SWITCH_CAPABILITY|switch, the SAI's answers
	// about the switch as orchagent publishes them (nil on a capture
	// without the section, from a collect.py before 2026-11-02).
	switchCap map[string]string
	// mgmtPortMissing: eth0 has a MGMT_INTERFACE address and no MGMT_PORT
	// row (§10.6's hand fix makes one and not the other), which sonic-yang
	// rejects as a dangling leafref when a breakout validates CONFIG_DB.
	mgmtPortMissing bool
	// mgmtNoGateway lists eth0's MGMT_INTERFACE prefixes without a gateway
	// of their own family. sonic-mgmt_interface.yang requires one ("Must
	// condition ... contains(../gwaddr, '.')"), `config interface ip add
	// eth0 <ip>` without one writes {"NULL": "NULL"}, and a breakout, which
	// validates the whole CONFIG_DB, fails on it (§11.8).
	mgmtNoGateway []string

	// l2 is disunified-l2d's installation and config (collect.py's l2d
	// section; §11.5).
	l2 l2Inst

	// The bridge's own ACL tables and rules (acl.go, §11.7.5): ACL_TABLE
	// and ACL_RULE entries named DUI_*, read only when the collector has
	// the sections (aclKnown; captures before 2026-11-01 lack them).
	aclKnown  bool
	aclTables map[string]aclTable          // table name -> its CONFIG_DB entry
	aclRules  map[string]map[string]string // "<table>|<rule>" -> fields
	// aclStatus is orchagent's verdict from STATE_DB ("<table>" and
	// "<table>|<rule>" -> Active, Inactive, Pending creation, ...) and
	// aclCRM the CRM ACL counters: read-only, shared by clones.
	aclStatus map[string]string
	aclCRM    table
}

// lacp is a PortChannel's LACP setting as CONFIG_DB holds it ("" = absent).
type lacp struct{ fastRate, minLinks string }

func newState(db map[string]table, sec map[string]string) *swState {
	s := &swState{
		port: map[string]map[string]string{}, vlans: map[int]bool{}, vlanMTU: map[int]string{}, netMTU: map[string]int{}, members: map[string]map[int]bool{},
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
	for k, f := range db["cfg_vlan"] {
		_, n, _ := strings.Cut(k, "|")
		if id := atoi(strings.TrimPrefix(n, "Vlan")); id > 0 {
			s.vlans[id] = true
			s.vlanMTU[id] = f["mtu"]
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
	s.lagMTU, s.lagLACP = map[string]string{}, map[string]lacp{}
	for k, f := range db["cfg_lag"] {
		_, n, _ := strings.Cut(k, "|")
		if s.lags[n] == nil {
			s.lags[n] = map[string]bool{}
		}
		s.lagMTU[n] = f["mtu"]
		s.lagLACP[n] = lacp{fastRate: f["fast_rate"], minLinks: f["min_links"]}
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
	if g, ok := db["cfg_ntp_global"]["NTP|global"]; ok {
		s.ntpGlobal = copyFields(g)
	}
	s.chrony, _ = parseChronySources(sec["chrony_sources"])
	s.chronyOnline, s.chronyOffline, s.chronyKnown = chronyActivity(sec["chrony_activity"])
	s.carrier = map[string]bool{}
	var addrs []ipAddr
	if decodeJSON("addr", sec["addr"], &addrs) == nil {
		for _, a := range addrs {
			s.carrier[a.Ifname] = a.carrier()
			if a.MTU > 0 {
				s.netMTU[a.Ifname] = a.MTU
			}
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
	s.snmpYml, s.snmpYmlOK = snmpYmlNames(sec["snmp_yml"])
	for k, f := range db["cfg_route"] {
		if n, ok := strings.CutPrefix(k, "STATIC_ROUTE|"); ok {
			s.routes[n] = copyFields(f)
		}
	}
	s.ifaddrs = map[string]bool{}
	var mgmtAddr, mgmtPort bool
	s.mgmtNoGateway = nil
	for k, fv := range db["cfg_mgmt"] {
		parts := strings.Split(k, "|")
		switch {
		case k == "MGMT_PORT|eth0":
			mgmtPort = true
		case len(parts) != 3:
		case parts[0] == "MGMT_INTERFACE" && parts[1] == "eth0":
			mgmtAddr = true
			if pfx, gw := parts[2], fv["gwaddr"]; !(strings.Contains(pfx, ":") && strings.Contains(gw, ":")) && !(strings.Contains(pfx, ".") && strings.Contains(gw, ".")) {
				s.mgmtNoGateway = append(s.mgmtNoGateway, pfx)
			}
		case parts[0] == "VLAN_INTERFACE", parts[0] == "INTERFACE", parts[0] == "PORTCHANNEL_INTERFACE", parts[0] == "LOOPBACK_INTERFACE":
			s.ifaddrs[parts[1]+"|"+parts[2]] = true
		}
	}
	s.mgmtPortMissing = mgmtAddr && !mgmtPort
	sort.Strings(s.mgmtNoGateway)
	for _, line := range strings.Split(sec["authorized_keys"], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			s.keys = append(s.keys, line)
		}
	}
	_, s.stpFeature = db["cfg_feature"]["FEATURE|stp"]
	s.l2 = parseL2Inst(sec["l2d"])
	s.platformJSON, s.platform = parsePlatform(sec["platform_json"])
	s.brkout = map[string]string{}
	for k, f := range db["cfg_breakout"] {
		if n, ok := strings.CutPrefix(k, "BREAKOUT_CFG|"); ok {
			s.brkout[n] = f["brkout_mode"]
		}
	}
	s.meta = copyFields(db["metadata"].first())
	s.aclTables, s.aclRules, s.aclStatus = map[string]aclTable{}, map[string]map[string]string{}, map[string]string{}
	if _, ok := sec["cfg_acl_table"]; ok {
		s.aclKnown = true
		for k, f := range db["cfg_acl_table"] {
			if n, ok := strings.CutPrefix(k, "ACL_TABLE|"); ok {
				s.aclTables[n] = parseACLTable(f)
			}
		}
		for k, f := range db["cfg_acl_rule"] {
			if n, ok := strings.CutPrefix(k, "ACL_RULE|"); ok {
				s.aclRules[n] = copyFields(f)
			}
		}
		for k, f := range db["state_acl"] {
			for _, pre := range []string{"ACL_TABLE_TABLE|", "ACL_RULE_TABLE|"} {
				if n, ok := strings.CutPrefix(k, pre); ok {
					s.aclStatus[n] = f["status"]
				}
			}
		}
		s.aclCRM = db["crm_acl"]
	}
	if f, ok := db["state_switch_cap"]["SWITCH_CAPABILITY|switch"]; ok {
		s.switchCap = copyFields(f)
	}
	s.fecs = map[string]map[string]bool{}
	for name, f := range db["state_port"].byName("|") {
		for _, m := range strings.Split(f["supported_fecs"], ",") {
			if m = strings.TrimSpace(m); m != "" {
				if s.fecs[name] == nil {
					s.fecs[name] = map[string]bool{}
				}
				s.fecs[name][m] = true
			}
		}
	}
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
		meta: copyFields(s.meta), brkout: map[string]string{}, fecs: s.fecs, switchCap: s.switchCap, mgmtPortMissing: s.mgmtPortMissing, mgmtNoGateway: s.mgmtNoGateway,
		l2:       s.l2,
		aclKnown: s.aclKnown, aclTables: map[string]aclTable{}, aclRules: map[string]map[string]string{},
		aclStatus: s.aclStatus, aclCRM: s.aclCRM,
	}
	for k, v := range s.aclTables {
		c.aclTables[k] = v.clone()
	}
	for k, v := range s.aclRules {
		c.aclRules[k] = copyFields(v)
	}
	c.l2.Files = map[string]string{}
	for k, v := range s.l2.Files {
		c.l2.Files[k] = v
	}
	if s.l2.Conf != nil {
		cf := *s.l2.Conf
		c.l2.Conf = &cf
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
	c.vlanMTU, c.netMTU = map[int]string{}, map[string]int{}
	for k, v := range s.vlanMTU {
		c.vlanMTU[k] = v
	}
	for k, v := range s.netMTU {
		c.netMTU[k] = v
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
	c.lagMTU, c.lagLACP = map[string]string{}, map[string]lacp{}
	for k, v := range s.lagMTU {
		c.lagMTU[k] = v
	}
	for k, v := range s.lagLACP {
		c.lagLACP[k] = v
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
	if s.ntpGlobal != nil {
		c.ntpGlobal = copyFields(s.ntpGlobal)
	}
	c.chrony, c.chronyOnline, c.chronyOffline, c.chronyKnown, c.carrier = s.chrony, s.chronyOnline, s.chronyOffline, s.chronyKnown, s.carrier
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
	c.snmpYml, c.snmpYmlOK = copySet(s.snmpYml), s.snmpYmlOK
	for k := range s.dns {
		c.dns[k] = true
	}
	for k, v := range s.routes {
		c.routes[k] = copyFields(v)
	}
	c.keys = append([]string(nil), s.keys...)
	c.ifaddrs = map[string]bool{}
	for k := range s.ifaddrs {
		c.ifaddrs[k] = true
	}
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

// portMirrorUnsupported reports whether the SAI says it cannot attach a
// port to a mirror session. 202511 publishes the answer in STATE_DB's
// SWITCH_CAPABILITY|switch (PORT_INGRESS_MIRROR_CAPABLE,
// PORT_EGRESS_MIRROR_CAPABLE) and acts on it twice: the CLI refuses the
// session ("Error: Port mirror direction 'both' is not supported by the
// ASIC"), and orchagent, given the row directly, creates the session
// object but never binds the source port ("setUnsetPortMirror: Port
// ingress mirror is not supported by the ASIC"), so nothing is mirrored
// (DX010, §11.8). A build without the flags (202505) has no such gate.
func (s *swState) portMirrorUnsupported() bool {
	for _, k := range []string{"PORT_INGRESS_MIRROR_CAPABLE", "PORT_EGRESS_MIRROR_CAPABLE"} {
		if v, ok := s.switchCap[k]; ok && v != "true" {
			return true
		}
	}
	return false
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
