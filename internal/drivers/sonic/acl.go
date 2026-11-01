package sonic

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// DHCP guarding and the controller's IP ACL rules (docs/drivers/sonic.md
// §11.7.5). Each is an L3 ACL table of the driver's own, bound ingress to
// every front-panel port and PortChannel: DUI_DHCP_GUARD while snooping is
// on and a guard applies, DUI_IP_ACL while a rule does. Tables go through
// `config acl add table` / `remove table` -- never `config acl update`,
// whose acl-loader adds a rule dropping all IPv4 to every ingress L3 table
// it loads (UniFi's ACLs are default-allow). Rules are a diff of the
// ACL_RULE|DUI_* hashes, written into CONFIG_DB, and orchagent's verdict on
// each is read back from STATE_DB on the next collect. Nothing else in
// ACL_* is read or touched. MAC rules need an L2 table type the stock
// orchagent lacks (§11.7.4) and are refused.

const (
	guardTable = "DUI_DHCP_GUARD"
	ipACLTable = "DUI_IP_ACL"
	aclType    = "L3"
	aclStage   = "ingress"

	// ipACLRules is how many IP rules the driver claims
	// (max_custom_ip_acls): the one table holds them with room to spare
	// (§11.7.5; CRM's per-table entry counters on the switch are the
	// authority once the table exists).
	ipACLRules = 128
	// ipACLTop: the controller's rule N is RULE_N at priority ipACLTop-N
	// (a higher priority is matched first).
	ipACLTop = 9999
	// guardForward / guardDrop: a trusted server's replies are forwarded
	// above the drop of every other reply on its VLAN. Rules of one
	// priority never overlap (one VLAN, one server each).
	guardForward = 9000
	guardDrop    = 8000
)

// aclTable is an ACL_TABLE entry: its type, stage and the ports it is
// bound to (the CLI stores them in set order, so compared as a set).
type aclTable struct {
	typ, stage string
	ports      map[string]bool
}

func parseACLTable(f map[string]string) aclTable {
	t := aclTable{typ: f["type"], stage: f["stage"], ports: map[string]bool{}}
	for _, p := range strings.Split(f["ports@"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			t.ports[p] = true
		}
	}
	return t
}

func (t aclTable) clone() aclTable {
	t.ports = copySet(t.ports)
	return t
}

// aclWant is the controller's intent from the last ApplyDevice: each
// managed table's rules by name (an empty map: the table goes). A table
// absent from the map is left as it is.
type aclWant map[string]map[string]map[string]string

// aclSet records the intent of a push (ApplyDevice). The rules depend on
// the switch's VLANs, which EnsureVLANs settles before ApplyDevice runs;
// the ports they bind to are worked out at every apply.
func (c *Collector) aclSet(d devicemodel.DeviceDesired, st *swState) {
	want := aclWant{}
	var notes []string
	note := func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }
	if d.DHCPSnooping != nil {
		want[guardTable] = map[string]map[string]string{}
		if *d.DHCPSnooping {
			want[guardTable] = c.guardRules(d.DHCPGuards, st, note)
		}
	}
	if d.IPACLSet {
		rules, why := ipRules(d.IPACLs)
		if why == "" {
			c.mu.Lock()
			from, to := c.sshFrom, c.sshTo
			c.mu.Unlock()
			dial := []string{to}
			if to == "" {
				dial = dialAddrs(c.CurrentAddress())
			}
			why = cutsManagement(d.IPACLs, from, dial)
		}
		if why != "" {
			note("IP ACL rules: %s; the switch's rules are left as they are", why)
		} else {
			want[ipACLTable] = rules
		}
	}
	if d.MACACLSet && len(d.MACACLs) > 0 {
		note("MAC ACL rules (%d): community SONiC's orchagent has no L2 table type and no outer-MAC match (docs/drivers/sonic.md §11.7.4); not written", len(d.MACACLs))
	}
	for _, n := range notes {
		c.warnOnce("acl: "+n, "%s", n)
	}
	c.mu.Lock()
	c.acl = want
	c.mu.Unlock()
}

// guardRules are DUI_DHCP_GUARD's rules: for each guarded VLAN a FORWARD
// of DHCP replies (UDP 67 -> 68) from each trusted server, V<v>_S<k>, and a
// DROP of every other reply on it, V<v>_DROP. v is UniFi's VLAN; the match
// is the switch's (VLAN 1 is Vlan<native_vlan>). A VLAN the switch does
// not carry gets no rules.
func (c *Collector) guardRules(guards []devicemodel.DHCPGuard, st *swState, note func(string, ...any)) map[string]map[string]string {
	rules := map[string]map[string]string{}
	servers := map[int][]string{}
	var vlans []int
	for _, g := range guards {
		ip := net.ParseIP(strings.TrimSpace(g.Server)).To4()
		if ip == nil {
			note("DHCP guard: server %q on VLAN %d is not an IPv4 address; skipped", g.Server, g.VLAN)
			continue
		}
		if _, seen := servers[g.VLAN]; !seen {
			vlans = append(vlans, g.VLAN)
		}
		if !containsString(servers[g.VLAN], ip.String()) {
			servers[g.VLAN] = append(servers[g.VLAN], ip.String())
		}
	}
	for _, v := range vlans {
		sv := v
		switch {
		case v == 1:
			sv = c.NativeVLAN
		case v == c.NativeVLAN:
			note("DHCP guard: VLAN %d is this switch's native_vlan (UniFi's VLAN 1 is Vlan%d here); not guarded", v, c.NativeVLAN)
			continue
		}
		if !st.vlans[sv] {
			continue
		}
		dhcp := func(prio int, action string) map[string]string {
			return map[string]string{
				"PRIORITY": strconv.Itoa(prio), "PACKET_ACTION": action, "VLAN_ID": strconv.Itoa(sv),
				"IP_PROTOCOL": "17", "L4_SRC_PORT": "67", "L4_DST_PORT": "68",
			}
		}
		for k, s := range servers[v] {
			r := dhcp(guardForward, "FORWARD")
			r["SRC_IP"] = s + "/32"
			rules[fmt.Sprintf("V%d_S%d", v, k+1)] = r
		}
		rules[fmt.Sprintf("V%d_DROP", v)] = dhcp(guardDrop, "DROP")
	}
	return rules
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ipRules are DUI_IP_ACL's rules: the controller's rule N is RULE_N at
// priority ipACLTop-N. A rule the table cannot express holds the whole set
// (why != ""): the rules are ordered, and leaving one out changes what the
// others match.
func ipRules(acls []devicemodel.IPACL) (map[string]map[string]string, string) {
	if len(acls) > ipACLRules {
		return nil, fmt.Sprintf("%d rules, more than the %d this switch claims", len(acls), ipACLRules)
	}
	rules := map[string]map[string]string{}
	for i, a := range acls {
		n := i + 1
		r := map[string]string{"PRIORITY": strconv.Itoa(ipACLTop - n), "PACKET_ACTION": "DROP"}
		if a.Permit {
			r["PACKET_ACTION"] = "FORWARD"
		}
		switch a.Protocol {
		case "":
			r["IP_TYPE"] = "IPV4ANY"
		case "tcp":
			r["IP_PROTOCOL"] = "6"
		case "udp":
			r["IP_PROTOCOL"] = "17"
		default:
			return nil, fmt.Sprintf("rule %d (%q): protocol %q", n, a.Name, a.Protocol)
		}
		for _, end := range []struct {
			side string
			e    devicemodel.IPACLEnd
		}{{"SRC", a.Src}, {"DST", a.Dst}} {
			if end.e.CIDR != "" {
				ip, _, err := net.ParseCIDR(end.e.CIDR)
				if err != nil || ip.To4() == nil {
					return nil, fmt.Sprintf("rule %d (%q): %q is not an IPv4 prefix", n, a.Name, end.e.CIDR)
				}
				r[end.side+"_IP"] = end.e.CIDR
			}
			ports := end.e.Port != 0 || end.e.PortMin != 0 || end.e.PortMax != 0
			if ports && a.Protocol == "" {
				return nil, fmt.Sprintf("rule %d (%q): a port on a rule for every protocol", n, a.Name)
			}
			switch {
			case end.e.Port != 0 && (end.e.PortMin != 0 || end.e.PortMax != 0):
				return nil, fmt.Sprintf("rule %d (%q): a port and a port range on one side", n, a.Name)
			case end.e.Port != 0:
				if end.e.Port < 1 || end.e.Port > 65535 {
					return nil, fmt.Sprintf("rule %d (%q): port %d", n, a.Name, end.e.Port)
				}
				r["L4_"+end.side+"_PORT"] = strconv.Itoa(end.e.Port)
			case ports:
				if end.e.PortMin < 1 || end.e.PortMax > 65535 || end.e.PortMin > end.e.PortMax {
					return nil, fmt.Sprintf("rule %d (%q): port range %d-%d", n, a.Name, end.e.PortMin, end.e.PortMax)
				}
				r["L4_"+end.side+"_PORT_RANGE"] = fmt.Sprintf("%d-%d", end.e.PortMin, end.e.PortMax)
			}
		}
		rules["RULE_"+strconv.Itoa(n)] = r
	}
	return rules, ""
}

// mgmtPort is the port the bridge reaches the switch on (SSH).
const mgmtPort = 22

// cutsManagement says which rule would drop the bridge's own SSH to the
// switch, "" if none would. The rules bind ingress to every port, the
// uplink included, so a Block that matches the bridge's TCP to the address
// it dials would cut the switch off from the bridge -- and from the push
// that would remove the rule again (the DX010 has one link and no console,
// docs/drivers/sonic.md §11.3). from is the bridge's address as the switch
// sees it ("" = unknown: every source counts). Such a set is held, like an
// untranslatable one; an earlier Allow that covers the traffic makes a
// later Block harmless.
func cutsManagement(acls []devicemodel.IPACL, from string, dial []string) string {
	bridge := net.ParseIP(from).To4()
	covers := func(e devicemodel.IPACLEnd, port int, ip net.IP) bool {
		if e.CIDR != "" {
			_, n, err := net.ParseCIDR(e.CIDR)
			if err != nil || !n.Contains(ip) {
				return false
			}
		}
		switch {
		case e.Port != 0:
			return e.Port == port
		case e.PortMin != 0 || e.PortMax != 0:
			return e.PortMin <= port && port <= e.PortMax
		}
		return true
	}
	for _, addr := range dial {
		ip := net.ParseIP(addr).To4()
		if ip == nil {
			continue
		}
		for i, a := range acls {
			if a.Protocol != "" && a.Protocol != "tcp" {
				continue
			}
			// The bridge's source port is ephemeral: a rule naming one
			// source port is taken not to match it.
			if a.Src.Port != 0 || !covers(a.Dst, mgmtPort, ip) {
				continue
			}
			if bridge != nil && !covers(devicemodel.IPACLEnd{CIDR: a.Src.CIDR}, 0, bridge) {
				continue
			}
			if a.Permit {
				if bridge != nil || a.Src.CIDR == "" {
					break // the first match decides: this Allow lets the bridge in
				}
				continue // it may not cover the bridge, whose address is unknown
			}
			return fmt.Sprintf("rule %d (%q) would block the bridge's SSH to this switch (%s, TCP %d) on every port, the uplink included", i+1, a.Name, ip, mgmtPort)
		}
	}
	return ""
}

// aclPorts are the interfaces an ACL table binds to, as the CLI allows them
// (sonic-utilities' get_acl_bound_ports): every port that is not a
// PortChannel member, and every PortChannel that has members. orchagent
// refuses to bind a member.
func aclPorts(st *swState) map[string]bool {
	out := map[string]bool{}
	members := map[string]bool{}
	for pc, ms := range st.lags {
		if len(ms) > 0 {
			out[pc] = true
		}
		for m := range ms {
			members[m] = true
		}
	}
	for p := range st.port {
		if !members[p] {
			out[p] = true
		}
	}
	return out
}

// sortIfnames orders interface names by prefix, then number: Ethernet0,
// Ethernet4, ..., Ethernet124, PortChannel1.
func sortIfnames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		pi, ni := splitIfname(out[i])
		pj, nj := splitIfname(out[j])
		if pi != pj {
			return pi < pj
		}
		if ni != nj {
			return ni < nj
		}
		return out[i] < out[j]
	})
	return out
}

func splitIfname(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(s[i:])
	return s[:i], n
}

// rulesOf are a table's rules in the state, by rule name.
func (s *swState) rulesOf(table string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for k, f := range s.aclRules {
		if t, r, ok := strings.Cut(k, "|"); ok && t == table {
			out[r] = f
		}
	}
	return out
}

// byPriority orders a table's rule names, highest priority first (ties by
// name).
func byPriority(rules map[string]map[string]string) []string {
	out := make([]string, 0, len(rules))
	for n := range rules {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := atoi(rules[out[i]]["PRIORITY"]), atoi(rules[out[j]]["PRIORITY"])
		if pi != pj {
			return pi > pj
		}
		return out[i] < out[j]
	})
	return out
}

func ruleKey(table, rule string) string { return "ACL_RULE|" + table + "|" + rule }

// hsetRule is the CONFIG_DB write of one rule, its fields in name order.
func hsetRule(table, rule string, f map[string]string) string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("sonic-db-cli CONFIG_DB hset " + sshrun.ShellQuote(ruleKey(table, rule)))
	for _, k := range keys {
		b.WriteString(" " + k + " " + sshrun.ShellQuote(f[k]))
	}
	return b.String()
}

func delRule(table, rule string) string {
	return "sonic-db-cli CONFIG_DB del " + sshrun.ShellQuote(ruleKey(table, rule))
}

// applyACL brings the driver's ACL tables in line with the last intent:
// ApplyDevice runs it for a push, ApplyPorts again once ports may have
// changed (a breakout's new ports, a PortChannel's members). A table that
// goes loses its rules first; a table that stays is (re)bound before its
// rules change; within a table stale rules go lowest priority first and
// new or changed ones are written highest first, so a guard's FORWARD
// never trails its DROP. A changed rule is deleted and written again
// (orchagent re-creates a rule from the whole hash).
func (c *Collector) applyACL(ctx context.Context) (int, error) {
	c.mu.Lock()
	st, want := c.state, c.acl
	c.mu.Unlock()
	if st == nil || len(want) == 0 {
		return 0, nil
	}
	if !st.aclKnown {
		for _, rules := range want {
			if len(rules) > 0 {
				c.warnOnce("acl unknown", "sonic: DHCP guarding or ACL rules requested, but the collector reported no ACL tables (an older collect.py); not written")
				break
			}
		}
		return 0, nil
	}
	sim := st.clone()
	var ops []deviceOp
	add := func(cmd string, eff func(*swState)) {
		ops = append(ops, deviceOp{cmd: "sudo " + cmd, logged: "sudo " + cmd, effect: eff})
		eff(sim)
	}
	dropRules := func(table string, names []string) {
		for i := len(names) - 1; i >= 0; i-- {
			r := names[i]
			add(delRule(table, r), func(s *swState) { delete(s.aclRules, table+"|"+r) })
		}
	}
	tables := make([]string, 0, len(want))
	for t := range want {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	ports := aclPorts(sim)
	plist := strings.Join(sortIfnames(ports), ",")
	changed := map[string]bool{}
	for _, t := range tables {
		rules := want[t]
		cur, exists := sim.aclTables[t]
		have := sim.rulesOf(t)
		ours := exists && cur.typ == aclType && cur.stage == aclStage
		if len(rules) == 0 || (exists && !ours) {
			// The table goes (or is not the driver's shape): its rules first.
			dropRules(t, byPriority(have))
			if exists {
				name := t
				add("config acl remove table "+name, func(s *swState) { delete(s.aclTables, name) })
			}
			changed[t] = changed[t] || len(have) > 0 || exists
			if len(rules) == 0 {
				continue
			}
			have, exists = map[string]map[string]string{}, false
		}
		if len(ports) == 0 {
			c.warnOnce("acl no ports", "sonic: no port to bind %s to; not written", t)
			continue
		}
		if !exists || !equalSets(cur.ports, ports) {
			name, bound := t, copySet(ports)
			add("config acl add table "+name+" "+aclType+" -p "+plist+" -s "+aclStage, func(s *swState) {
				s.aclTables[name] = aclTable{typ: aclType, stage: aclStage, ports: copySet(bound)}
			})
			changed[t] = true
		}
		var stale []string
		for _, r := range byPriority(have) {
			if _, keep := rules[r]; !keep {
				stale = append(stale, r)
			}
		}
		dropRules(t, stale)
		for _, r := range byPriority(rules) {
			f := rules[r]
			old, ok := have[r]
			if ok && reflect.DeepEqual(old, f) {
				continue
			}
			table, rule, fields := t, r, copyFields(f)
			if ok {
				add(delRule(table, rule), func(s *swState) { delete(s.aclRules, table+"|"+rule) })
			}
			add(hsetRule(table, rule, fields), func(s *swState) { s.aclRules[table+"|"+rule] = copyFields(fields) })
		}
		changed[t] = changed[t] || len(stale) > 0
		for r, f := range rules {
			if old, ok := have[r]; !ok || !reflect.DeepEqual(old, f) {
				changed[t] = true
			}
		}
	}
	if len(ops) == 0 {
		return 0, nil
	}
	cmds := make([]string, 0, len(ops)+1)
	for _, o := range ops {
		cmds = append(cmds, o.cmd)
	}
	cmds = append(cmds, "sudo config save -y")
	var summary []string
	for _, t := range tables {
		if changed[t] {
			summary = append(summary, fmt.Sprintf("%s %d rules", t, len(want[t])))
		}
	}
	c.Log.Printf("sonic apply acl (%s): %s", strings.Join(summary, ", "), strings.Join(cmds, " ; "))
	if err := c.runBatch(ctx, cmds); err != nil {
		return 0, fmt.Errorf("sonic apply acl: %w", err)
	}
	c.mu.Lock()
	if c.state == st {
		c.state = sim
	}
	c.aclWritten = true
	c.mu.Unlock()
	return len(summary), nil
}

// verifyACL reports orchagent's verdict on the driver's tables and rules
// (STATE_DB), read by the collect after a write: a table or rule that is
// not Active is logged once per status, and the first collect after a
// write logs the tally with CRM's ACL counters (the entry capacity).
func (c *Collector) verifyACL(st *swState) {
	if st == nil || !st.aclKnown {
		return
	}
	c.mu.Lock()
	written := c.aclWritten
	c.aclWritten = false
	c.mu.Unlock()
	names := make([]string, 0, len(st.aclTables))
	for t := range st.aclTables {
		names = append(names, t)
	}
	sort.Strings(names)
	var tally []string
	for _, t := range names {
		if s := st.aclStatus[t]; s != "Active" {
			c.warnOnce("acl status "+t+" "+s, "sonic: ACL table %s is %s in orchagent (STATE_DB ACL_TABLE_TABLE), not Active", t, statusWord(s))
		}
		rules := st.rulesOf(t)
		active := 0
		for _, r := range byPriority(rules) {
			s := st.aclStatus[t+"|"+r]
			if s == "Active" {
				active++
				continue
			}
			c.warnOnce("acl status "+t+"|"+r+" "+s, "sonic: ACL rule %s|%s is %s in orchagent (STATE_DB ACL_RULE_TABLE), not Active: %s", t, r, statusWord(s), pyDict(rules[r]))
		}
		tally = append(tally, fmt.Sprintf("%s %d of %d rules active", t, active, len(rules)))
	}
	if !written {
		return
	}
	if len(tally) == 0 {
		tally = append(tally, "no tables")
	}
	c.Log.Printf("sonic: acl: %s; CRM %s", strings.Join(tally, ", "), crmACL(st.aclCRM))
}

func statusWord(s string) string {
	if s == "" {
		return "without a verdict"
	}
	return s
}

// crmACL summarises CRM's ACL counters: the ingress tables used and free
// per bind point, and each table's entries used and available.
func crmACL(t table) string {
	var parts []string
	for _, bp := range []string{"PORT", "LAG"} {
		if f, ok := t["CRM:ACL_STATS:INGRESS:"+bp]; ok {
			parts = append(parts, fmt.Sprintf("ingress %s tables %s used, %s free", bp, f["crm_stats_acl_table_used"], f["crm_stats_acl_table_available"]))
		}
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		if strings.HasPrefix(k, "CRM:ACL_TABLE_STATS:") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		f := t[k]
		parts = append(parts, fmt.Sprintf("table %s entries %s used, %s available", strings.TrimPrefix(k, "CRM:ACL_TABLE_STATS:"), f["crm_stats_acl_entry_used"], f["crm_stats_acl_entry_available"]))
	}
	if len(parts) == 0 {
		return "counters not reported"
	}
	return strings.Join(parts, "; ")
}
