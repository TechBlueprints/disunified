package sonic

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// DHCP guarding's count per port (docs/drivers/sonic.md §11.7.9). The guard
// drops an untrusted server's replies (UDP 67 -> 68) silently, and its one
// DROP per VLAN is bound to every port, so its counter cannot say which port
// the server is behind. Each guarded VLAN therefore gets one counted DROP per
// front-panel port between its trusted servers' FORWARDs and the shared
// DROP:
//
//	V<v>_S<k>         9000  FORWARD  SRC_IP <server>/32       (unchanged)
//	V<v>_IN_<port>    8500  DROP     IN_PORTS <port>          (counted)
//	V<v>_DROP         8000  DROP                              (unchanged)
//
// Each match is the guard's (VLAN, UDP 67 -> 68) plus the ingress port, so a
// reply counts on the rule of the port it entered; rules of one priority
// never overlap (one port each). The shared DROP stays: a port without its
// own rule yet (a breakout's new lanes, below) is still guarded. The stock L3
// table type has no ingress-port match, and L3V4V6 (which has one) is not
// supported on this ASIC (STATE_DB ACL_STAGE_CAPABILITY_TABLE|INGRESS
// supported_L3V4V6 false), so the table has a type of its own in
// ACL_TABLE_TYPE. Counters reach COUNTERS_DB only while orchagent polls ACL
// counters (FLEX_COUNTER_TABLE|ACL, off on the image): the guard turns that
// on. The bridge reports each port's sum over its lanes and VLANs as
// Health.RogueDHCPDrops.

const (
	// guardCount is the counted per-port DROPs' priority, between the
	// trusted servers' FORWARDs and the shared DROP.
	guardCount = 8500
	// guardEntries caps the guard table (FORWARDs, counted DROPs and shared
	// DROPs together). Measured: 512+ entries per stock L3 table, 2557
	// available beside 3 used on a table of the guard's own type (§11.7.7,
	// §11.7.9); a quarter of that leaves the IP rules' claim of 128 its
	// margin. A VLAN whose counted rules would pass it keeps the guard
	// without them, with a note.
	guardEntries = 256
	// flexACLKey is orchagent's ACL counter polling group; `counterpoll
	// acl enable` writes exactly this field.
	flexACLKey = "FLEX_COUNTER_TABLE|ACL"
)

// guardType is the guard table's type: the stock L3 type's fields the guard
// uses plus IN_PORTS, with a counter on every rule, bound to ports and
// PortChannels. The table and its type share the name. orchagent reads the
// fields without the '@' that marks a CONFIG_DB list (swss-common strips it).
var guardType = map[string]string{
	"MATCHES@":     "IN_PORTS,VLAN_ID,IP_PROTOCOL,L4_SRC_PORT,L4_DST_PORT,SRC_IP",
	"ACTIONS@":     "PACKET_ACTION,COUNTER",
	"BIND_POINTS@": "PORT,PORTCHANNEL",
}

func typeKey(name string) string { return "ACL_TABLE_TYPE|" + name }

// hsetType is the CONFIG_DB write of a table type, its fields in name order.
func hsetType(name string, f map[string]string) string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("sonic-db-cli CONFIG_DB hset " + sshrun.ShellQuote(typeKey(name)))
	for _, k := range keys {
		b.WriteString(" " + sshrun.ShellQuote(k) + " " + sshrun.ShellQuote(f[k]))
	}
	return b.String()
}

func delType(name string) string {
	return "sonic-db-cli CONFIG_DB del " + sshrun.ShellQuote(typeKey(name))
}

// guardCountState is what collect.py reads for the count: the driver's
// table types (ACL_TABLE_TYPE|DUI_*), whether orchagent polls ACL counters,
// the ports orchagent knows (STATE_DB PORT_TABLE: a rule naming any other
// port in IN_PORTS is refused for good), and each counted rule's packets.
type guardCountState struct {
	types   map[string]map[string]string // table type name -> fields
	flexACL string                       // FLEX_COUNTER_STATUS: "enable", "disable", "" (absent)
	orch    map[string]bool              // read-only, shared by clones
	packets map[string]string            // "<table>|<rule>" -> packets ("" = no value yet); read-only
}

func (g guardCountState) clone() guardCountState {
	types := map[string]map[string]string{}
	for k, v := range g.types {
		types[k] = copyFields(v)
	}
	g.types = types
	return g
}

// parseGuardCount fills s.gc from collect.py's cfg_acl_type, cfg_flex_acl,
// acl_counters and state_port sections; a capture before 2026-11-14 lacks
// the first three, which reads as nothing written yet.
func parseGuardCount(s *swState, db map[string]table, sec map[string]string) {
	g := guardCountState{types: map[string]map[string]string{}, orch: map[string]bool{}, packets: map[string]string{}}
	if t, err := parseTable("cfg_acl_type", sec["cfg_acl_type"]); err == nil {
		for k, f := range t {
			if n, ok := strings.CutPrefix(k, "ACL_TABLE_TYPE|"); ok {
				g.types[n] = copyFields(f)
			}
		}
	}
	if t, err := parseTable("cfg_flex_acl", sec["cfg_flex_acl"]); err == nil {
		g.flexACL = t[flexACLKey]["FLEX_COUNTER_STATUS"]
	}
	if t, err := parseTable("acl_counters", sec["acl_counters"]); err == nil {
		for k, f := range t {
			// COUNTERS_DB's ACL_COUNTER_RULE_MAP names a rule <table>:<rule>.
			if tbl, rule, ok := strings.Cut(k, ":"); ok {
				g.packets[tbl+"|"+rule] = f["SAI_ACL_COUNTER_ATTR_PACKETS"]
			}
		}
	}
	for name := range db["state_port"].byName("|") {
		g.orch[name] = true
	}
	s.gc = g
}

// countedRuleName is a guarded VLAN's counted rule for one port.
func countedRuleName(vlan, port string) string { return "V" + vlan + "_IN_" + port }

// withPortCounts adds the counted per-port DROPs to the guard's rules: for
// each shared DROP V<v>_DROP, one V<v>_IN_<port> per front-panel port that
// orchagent knows, VLAN by VLAN in number order while the table stays within
// guardEntries.
func withPortCounts(rules map[string]map[string]string, sim *swState, note func(string, ...any)) map[string]map[string]string {
	if len(rules) == 0 {
		return rules
	}
	var ports []string
	for p := range sim.port {
		if sim.gc.orch[p] {
			ports = append(ports, p)
		}
	}
	set := map[string]bool{}
	for _, p := range ports {
		set[p] = true
	}
	ports = sortIfnames(set)
	var vlans []int
	for r := range rules {
		if v, ok := strings.CutSuffix(r, "_DROP"); ok && strings.HasPrefix(v, "V") {
			if n, err := strconv.Atoi(v[1:]); err == nil {
				vlans = append(vlans, n)
			}
		}
	}
	sort.Ints(vlans)
	out := map[string]map[string]string{}
	for k, f := range rules {
		out[k] = f
	}
	entries := len(rules)
	for _, v := range vlans {
		vs := strconv.Itoa(v)
		if entries+len(ports) > guardEntries {
			note("DHCP guard: VLAN %d's per-port counts would take %s past %d entries (%d ports); guarded without them, so a rogue server there is not reported per port", v, guardTable, guardEntries, len(ports))
			continue
		}
		drop := rules["V"+vs+"_DROP"]
		for _, p := range ports {
			f := copyFields(drop)
			f["PRIORITY"] = strconv.Itoa(guardCount)
			f["IN_PORTS"] = p
			out[countedRuleName(vs, p)] = f
		}
		entries += len(ports)
	}
	return out
}

// isCountedRule: a guard rule whose match names one ingress port.
func isCountedRule(f map[string]string) bool { return f["IN_PORTS"] != "" }

// dropInPortRules deletes, inside the port batch and before a breakout or a
// join (with the breakout's own writer: the controller's, or plan.own), every rule of the driver's tables whose IN_PORTS names one of lanes:
// the ports go away, and a rule's SAI entry holds the port's object id
// (aclorch passes IN_PORTS to the SAI as the ports' oids), which no
// `config interface breakout -f` dependency check sees (IN_PORTS is a plain
// string in sonic-acl.yang). The shared DROP guards the cage meanwhile;
// applyACL adds the new lanes' rules once orchagent has them.
func (pl *plan) dropInPortRules(idx int, lanes []string, write func(int, string, func(*swState))) {
	gone := map[string]bool{}
	for _, l := range lanes {
		gone[l] = true
	}
	var keys []string
	for k, f := range pl.sim.aclRules {
		if p := f["IN_PORTS"]; p != "" && gone[p] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		table, rule, _ := strings.Cut(k, "|")
		key := k
		write(idx, delRule(table, rule), func(s *swState) { delete(s.aclRules, key) })
	}
	// The lanes are new ports after the breakout, even where a name stays
	// (Ethernet4 is the parent and the first child): orchagent has them
	// once STATE_DB says so, at a later collect.
	orch := map[string]bool{}
	for p := range pl.sim.gc.orch {
		if !gone[p] {
			orch[p] = true
		}
	}
	pl.sim.gc.orch = orch
}

// rogueDHCPDrops fills each port's Health.RogueDHCPDrops: the packets of the
// guard's counted rules on the port's lanes, summed over the guarded VLANs.
// A rule without a value (polling off, or not yet polled) adds nothing.
func rogueDHCPDrops(st *swState, ports []cagePort) {
	if st == nil || len(st.gc.packets) == 0 {
		return
	}
	byLane := map[string]uint64{}
	for k, f := range st.aclRules {
		tbl, _, _ := strings.Cut(k, "|")
		if tbl != guardTable || !isCountedRule(f) || f["PACKET_ACTION"] != "DROP" {
			continue
		}
		n, err := strconv.ParseUint(st.gc.packets[k], 10, 64)
		if err != nil {
			continue
		}
		byLane[f["IN_PORTS"]] += n
	}
	for i := range ports {
		var sum uint64
		for _, l := range ports[i].Interfaces {
			sum += byLane[l]
		}
		ports[i].Health.RogueDHCPDrops = sum
	}
}

// guardCountNotes are the reasons the count cannot be read, for the log.
func guardCountNotes(st *swState) []string {
	if st == nil {
		return nil
	}
	if _, ok := st.aclTables[guardTable]; !ok {
		return nil
	}
	if st.gc.flexACL != "enable" {
		return []string{fmt.Sprintf("DHCP guard: orchagent does not poll ACL counters (%s FLEX_COUNTER_STATUS %q); no per-port count", flexACLKey, st.gc.flexACL)}
	}
	return nil
}
