package aristaeos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// DHCP guarding and switch ACL rules as EOS port ACLs
// (docs/drivers/arista-eos.md §9).
//
// EOS 4.26's own `ip dhcp snooping` cannot block a rogue server (§6), but an
// ingress IPv4 ACL can drop UDP 67 -> 68 (a server's reply) from every
// address but the trusted servers'. Two facts of the 7160 on 4.26.14M shape
// the rest (§9.1, measured 2026-11-14):
//
//   - No VLAN match: EOS takes `vlan N MASK` in a rule and stores it with
//     mask 0 (any VLAN), whatever the mask is written as, and the chip's
//     ACL key has no VLAN field. A guard cannot be limited to the VLANs
//     UniFi guards, so it is limited by port instead: a port is guarded
//     only when every VLAN it carries is guarded (guardExact), or, with
//     options.dhcp_guard: edge, every port but the uplink and other
//     switches' ports (guardEdge).
//   - One list bound to several interfaces is one set of hardware entries
//     with one counter per entry, while separate lists get separate entries
//     and counters. So every interface gets a list of its own,
//     DUI_IP_ACL_<interface>, with `counters per-entry`, and the guard's
//     drop entry counts the rogue replies dropped on that port alone
//     (acl_counters.go).
//
// An interface takes one IPv4 ACL per direction, so a port's guard and the
// controller's IP rules share its list, the guard first (its drop wins over
// an Allow rule), and an explicit `permit ip any any` last: EOS ACLs end in
// an implicit deny, UniFi's are default-allow.
//
// Only the driver's lists are touched: an interface already carrying another
// IPv4 ACL inbound is left alone. Every block of rules starts with a remark
// naming it and a hash of the lines the driver rendered, so the diff does
// not depend on how EOS rewrites a rule for display (`eq 67` comes back as
// `eq bootps`), and a block the controller leaves alone is carried over as
// the switch shows it.

// aclPrefix starts the name of each of the driver's IPv4 ACLs; the rest is
// the interface it is bound to, "/" written "-". legacyACLName is the one
// shared list of the first design (never deployed), removed if found.
const (
	aclPrefix     = "DUI_IP_ACL_"
	legacyACLName = "DUI_IP_ACL"
)

// aclListName is the driver's list for one interface.
func aclListName(iface string) string { return aclPrefix + strings.ReplaceAll(iface, "/", "-") }

// ownACL: the list is one of the driver's.
func ownACL(name string) bool { return name == legacyACLName || strings.HasPrefix(name, aclPrefix) }

// aclCounters is the list option that gives every entry its own hardware
// counter; without it EOS reports no match counts.
const aclCounters = "counters per-entry"

// aclCounterPool is how many ACL counters the 7160's chip has, for every
// list together (measured 2026-11-14, §9.1: counter indices 1-4095; five
// lists of 819 counted rules fill it, while one list of 4096, or five of
// 820, leave entries without a counter: no error at the commit, only the
// summary's countersIncomplete). Only a list with a guard is counted.
const aclCounterPool = 4095

// maxIPRules is how many of the controller's IP rules the driver claims
// (max_custom_ip_acls) and writes; a longer list is held. The rules go
// into every interface's list, so the worst case is every interface of the
// switch (72 with every cage split) carrying the guard and the rules
// (§9.4): at 32 rules that is 72 x (32 + S + 2) counted rules for S
// trusted servers, inside the 4095 counters up to S = 22, and two hardware
// entries per counted rule, 144 x (34 + S), a fraction of the 20094
// entries five lists programmed without an error.
const maxIPRules = 32

// The guard modes (options.dhcp_guard).
const (
	guardExact = "exact" // a port is guarded when every VLAN it carries is guarded (the default)
	guardEdge  = "edge"  // every port carrying a guarded VLAN, but the uplink and ports facing a switch
)

// aclSession is the prefix of the configuration session an ACL change is made
// in (sessionName adds a suffix per change), committed with commitTimer like
// an address move: the ACL is bound to the uplink too,
// so a rule that blocks the bridge's own traffic reverts by itself.
const aclSession = "dui-acl"

// applyBudget is the time the driver asks the loop for per write: an ACL
// change is the session batch, the settle wait, a fresh dial and the
// confirm, which the collect budget (20 s by default) may not hold. It
// stays well inside commitTimer, so a confirm never races the revert.
const applyBudget = 60 * time.Second

// ApplyTimeout implements devicemodel.ApplyBudget.
func (c *Collector) ApplyTimeout() time.Duration { return applyBudget }

// aclSettleDefault is how long the driver waits after a timed commit before
// dialling the switch to confirm it: the ACL is programmed into the hardware
// after the commit returns, and a check made before that would pass whatever
// the rules say.
const aclSettleDefault = 5 * time.Second

// The blocks of a driver list, in list order. Each starts with
// `remark disunified <block> <hash>`; the default block is the closing
// permit and carries no hash.
const (
	aclRemark       = "disunified"
	blockDHCPGuard  = "dhcp-guard"
	blockIPRules    = "ip-rules"
	blockDefault    = "default"
	aclDefaultAllow = "permit ip any any"
)

// aclBlock is one block of a driver list: the hash in its remark and its
// rules as the switch shows them (or as the driver rendered them, for a
// block it just wrote).
type aclBlock struct {
	hash  string
	lines []string
}

// aclList is one of the driver's lists as the running configuration has it.
type aclList struct {
	seqs     []int // every sequence number in the list, remarks included
	blocks   map[string]aclBlock
	stray    int  // rules outside any block (added by hand): the list is rewritten
	counters bool // `counters per-entry` is set
}

// aclState is what the running configuration says about the driver's lists.
type aclState struct {
	lists map[string]aclList // the driver's lists by name
	bound map[string]string  // interface -> the driver list it carries inbound
	// foreign is every interface carrying another IPv4 ACL inbound, with its
	// name. EOS takes one per direction, so the driver does not bind there.
	foreign map[string]string
}

func newACLState() aclState {
	return aclState{lists: map[string]aclList{}, bound: map[string]string{}, foreign: map[string]string{}}
}

// aclWant is what the lists should hold. A block marked keep stays as the
// switch has it: the controller sent nothing for it this time.
type aclWant struct {
	guards               map[int][]string // guarded VLAN -> its trusted servers, in push order
	rules                []string
	keepGuard, keepRules bool
	mode                 string
}

// aclWantFrom translates the controller's DHCP guards and IP rules. A block
// the controller did not send (snooping not pushed, IP rules held by the
// loop) is kept as the switch has it, and so is one this driver cannot
// write in full: a partial rule list could let through what a later Block
// meant to stop. Guards on a VLAN the switch does not carry are skipped.
// MAC rules are refused (§9.2).
func aclWantFrom(d devicemodel.DeviceDesired, vlans []int, mode string) (aclWant, error) {
	w := aclWant{keepGuard: d.DHCPSnooping == nil, keepRules: !d.IPACLSet, mode: mode}
	var errs []error
	if d.DHCPSnooping != nil && *d.DHCPSnooping {
		carried := map[int]bool{}
		for _, v := range vlans {
			carried[v] = true
		}
		w.guards = map[int][]string{}
		for _, g := range d.DHCPGuards {
			if !carried[g.VLAN] {
				continue
			}
			a, err := netip.ParseAddr(g.Server)
			if err != nil || !a.Is4() {
				errs = append(errs, fmt.Errorf("arista-eos: DHCP guard left as the switch has it: DHCP server %q on VLAN %d is not an IPv4 address", g.Server, g.VLAN))
				w.guards, w.keepGuard = nil, true
				break
			}
			w.guards[g.VLAN] = append(w.guards[g.VLAN], a.String())
		}
	}
	if d.IPACLSet {
		for _, r := range d.IPACLs {
			line, err := ipRuleLine(r.Permit, r.Protocol,
				ipSide{CIDR: r.Src.CIDR, Port: r.Src.Port, PortMin: r.Src.PortMin, PortMax: r.Src.PortMax},
				ipSide{CIDR: r.Dst.CIDR, Port: r.Dst.Port, PortMin: r.Dst.PortMin, PortMax: r.Dst.PortMax})
			if err != nil {
				errs = append(errs, fmt.Errorf("arista-eos: IP rules left as the switch has them: rule %q: %w", r.Name, err))
				w.rules, w.keepRules = nil, true
				break
			}
			w.rules = append(w.rules, line)
		}
		if len(w.rules) > maxIPRules {
			errs = append(errs, fmt.Errorf("arista-eos: IP rules left as the switch has them: %d rules, and the driver writes at most %d (docs/drivers/arista-eos.md §9.4)", len(w.rules), maxIPRules))
			w.rules, w.keepRules = nil, true
		}
	}
	if d.MACACLSet && len(d.MACACLs) > 0 {
		errs = append(errs, fmt.Errorf("arista-eos: %d MAC rules not applied: the 7160 binds no MAC ACL to a switchport (`mac access-group` is unavailable on an Ethernet interface on 4.26.14M; docs/drivers/arista-eos.md §9.2)", len(d.MACACLs)))
	}
	return w, errors.Join(errs...)
}

// holdIfCutsManagement holds the IP rules (the switch keeps the ones it
// has) when one would drop the bridge's own connection to the switch: the
// lists are bound to the uplink too. The commit timer would revert such a
// change by itself (§9.3), but each attempt would cut the bridge off for
// the timer's length. The bridge's address as the switch sees it is not
// known to the driver (eAPI through NAT), so every source counts.
func (c *Collector) holdIfCutsManagement(d devicemodel.DeviceDesired, w *aclWant) error {
	if w.keepRules || len(d.IPACLs) == 0 {
		return nil
	}
	why := devicemodel.IPRulesCutManagement(d.IPACLs, "", c.dialAddrs(), c.mgmtPortOr())
	if why == "" {
		return nil
	}
	w.rules, w.keepRules = nil, true
	return fmt.Errorf("arista-eos: IP rules left as the switch has them: %s", why)
}

// dialAddrs is the switch's address the bridge connects to: the configured
// host, resolved when it is a name. Empty when it cannot be resolved; the
// commit timer is then the only guard.
func (c *Collector) dialAddrs() []string {
	if c.host == "" || c.local {
		// local: no connection to the switch crosses its ports; the probe
		// before the commit is the guard.
		return nil
	}
	if a, err := netip.ParseAddr(c.host); err == nil {
		return []string{a.String()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, c.host)
	if err != nil {
		return nil
	}
	return addrs
}

// mgmtPortOr is the TCP port the bridge reaches the switch on: eAPI's
// HTTPS by default.
func (c *Collector) mgmtPortOr() int {
	if c.mgmtPort > 0 {
		return c.mgmtPort
	}
	return 443
}

// aclFromRunningConfig reads the driver's lists and every interface's
// inbound IPv4 ACL out of the structured running configuration. EOS keys a
// list's rules by "<seq> <rule>".
func aclFromRunningConfig(rc showRunningConfig) aclState {
	st := newACLState()
	for section, sec := range rc.Cmds {
		name, ok := strings.CutPrefix(section, "ip access-list ")
		if !ok || !ownACL(name) {
			continue
		}
		l := aclList{blocks: map[string]aclBlock{}}
		type rule struct {
			seq  int
			text string
		}
		var rules []rule
		if sec != nil {
			for line := range sec.Cmds {
				if line == aclCounters {
					l.counters = true
					continue
				}
				seq, text, ok := strings.Cut(line, " ")
				n, err := strconv.Atoi(seq)
				if !ok || err != nil {
					continue
				}
				rules = append(rules, rule{n, text})
			}
		}
		sort.Slice(rules, func(i, j int) bool { return rules[i].seq < rules[j].seq })
		block := ""
		for _, r := range rules {
			l.seqs = append(l.seqs, r.seq)
			if f := strings.Fields(r.text); len(f) >= 3 && f[0] == "remark" && f[1] == aclRemark {
				block = f[2]
				b := aclBlock{}
				if len(f) >= 4 {
					b.hash = f[3]
				}
				l.blocks[block] = b
				continue
			}
			if block == "" || strings.HasPrefix(r.text, "remark ") {
				l.stray++
				continue
			}
			b := l.blocks[block]
			b.lines = append(b.lines, r.text)
			l.blocks[block] = b
		}
		st.lists[name] = l
	}
	for section, body := range rc.Cmds {
		if !strings.HasPrefix(section, "interface ") || body == nil {
			continue
		}
		iface := strings.TrimPrefix(section, "interface ")
		for line := range body.Cmds {
			name, ok := strings.CutPrefix(line, "ip access-group ")
			if !ok || !strings.HasSuffix(name, " in") {
				continue
			}
			name = strings.TrimSuffix(name, " in")
			if ownACL(name) {
				st.bound[iface] = name
			} else {
				st.foreign[iface] = name
			}
		}
	}
	return st
}

// aclTarget is one interface a list can be bound to, with the port it
// belongs to (for a Port-Channel, its first member: members share their
// switchport settings).
type aclTarget struct {
	iface string
	port  devicemodel.Port
}

// aclTargets is every interface a list is bound to: each lane of a port that
// is not in an aggregate, and the Port-Channel of every aggregate. The
// management port and VLAN interfaces are never bound.
func aclTargets(ports []devicemodel.Port) []aclTarget {
	set := map[string]devicemodel.Port{}
	for _, p := range ports {
		if p.LAGID > 0 {
			po := "Port-Channel" + strconv.Itoa(p.LAGID)
			if q, ok := set[po]; !ok || p.Index < q.Index {
				set[po] = p
			}
			continue
		}
		for _, l := range laneNames(p) {
			set[l] = p
		}
	}
	out := make([]aclTarget, 0, len(set))
	for k, p := range set {
		out = append(out, aclTarget{iface: k, port: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].iface < out[j].iface })
	return out
}

// carriedVLANs is every VLAN a port's ingress frames can belong to: an
// access port's VLAN, a trunk's allowed list (every VLAN the switch has, for
// "allowed vlan all") and its native VLAN.
func carriedVLANs(p devicemodel.Port, switchVLANs []int) []int {
	set := map[int]bool{}
	if p.VLAN.NativeVLAN > 0 {
		set[p.VLAN.NativeVLAN] = true
	}
	if p.VLAN.Mode != "access" {
		if p.VLAN.AllowAll {
			for _, v := range switchVLANs {
				set[v] = true
			}
		} else {
			for _, v := range p.VLAN.Allowed {
				set[v] = true
			}
		}
	}
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// guardExempt is, for guardEdge, the ports never guarded: the uplink (the
// port the gateway's MAC is learned on, and the LLDP uplink) and every port
// whose LLDP neighbour is a bridge. Behind those sit the gateway and other
// switches, where a server of a VLAN UniFi does not guard may answer.
// Empty when none is known.
func guardExempt(snap *devicemodel.Snapshot) map[int]bool {
	ex := map[int]bool{}
	for _, p := range snap.Ports {
		if p.Neighbor != nil && p.Neighbor.IsBridge {
			ex[p.Index] = true
		}
	}
	if up := snap.UplinkPort(); up > 0 {
		ex[up] = true
	}
	if mac := snap.System.ARP[snap.System.Gateway]; mac != "" {
		for _, e := range snap.MACTable {
			if e.MAC == mac && e.PortIndex > 0 {
				ex[e.PortIndex] = true
			}
		}
	}
	return ex
}

// portGuard is the guard for one port: replies from the trusted servers of
// the guarded VLANs it carries pass, every other UDP 67 -> 68 entering the
// port is dropped. Without a VLAN match the servers are one list for the
// port, so a server trusted on one of its VLANs is trusted on all of them.
// nil when the port is not guarded; reason says why, for the log.
func portGuard(w aclWant, p devicemodel.Port, switchVLANs []int, exempt map[int]bool) (lines []string, reason string) {
	if len(w.guards) == 0 {
		return nil, ""
	}
	carried := carriedVLANs(p, switchVLANs)
	var guarded, open []int
	for _, v := range carried {
		if _, ok := w.guards[v]; ok {
			guarded = append(guarded, v)
		} else {
			open = append(open, v)
		}
	}
	if len(guarded) == 0 {
		return nil, ""
	}
	switch w.mode {
	case guardEdge:
		if exempt[p.Index] {
			return nil, "uplink or a switch's port"
		}
	default:
		if len(open) > 0 {
			return nil, "carries unguarded VLAN " + joinInts(open)
		}
	}
	seen := map[string]bool{}
	for _, v := range guarded {
		for _, s := range w.guards[v] {
			if seen[s] {
				continue
			}
			seen[s] = true
			lines = append(lines, "permit udp host "+s+" eq 67 any eq 68")
		}
	}
	return append(lines, guardDrop), ""
}

// guardDrop is the guard's drop entry, whose counter is the port's rogue
// DHCP count. EOS shows it as `deny udp any eq bootps any eq bootpc`.
const guardDrop = "deny udp any eq 67 any eq 68"

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = strconv.Itoa(x)
	}
	return strings.Join(s, ",")
}

// blockHash names a block's rendered lines in its remark.
func blockHash(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:6])
}

// wantBlock is the block a list should hold: the controller's lines, or the
// switch's own when the block is kept.
func wantBlock(have aclBlock, lines []string, keep bool) aclBlock {
	if keep {
		return have
	}
	if len(lines) == 0 {
		return aclBlock{}
	}
	return aclBlock{hash: blockHash(lines), lines: lines}
}

// sameBlock: the switch holds what the driver last rendered for this block.
func sameBlock(have aclBlock, want aclBlock) bool {
	return have.hash == want.hash && len(have.lines) == len(want.lines)
}

// aclPlan is the session body that brings the driver's lists and their
// bindings to want, and the state the switch has once it is committed. An
// empty body means the switch already matches. skipped lists the interfaces
// left alone because they carry another IPv4 ACL; unguarded the ports
// carrying a guarded VLAN that get no guard, with the reason. over is the
// number of counted entries the guarded lists would need when it exceeds
// the chip's counter pool, and no list is then counted (0 otherwise).
func aclPlan(have aclState, want aclWant, targets []aclTarget, switchVLANs []int, exempt map[int]bool) (body []string, next aclState, skipped, unguarded []string, over int) {
	next = newACLState()
	for k, v := range have.foreign {
		next.foreign[k] = v
	}
	// A kept rules block is the same in every list; take it from any.
	var keptRules aclBlock
	if want.keepRules {
		names := make([]string, 0, len(have.lists))
		for n := range have.lists {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if b, ok := have.lists[n].blocks[blockIPRules]; ok {
				keptRules = b
				break
			}
		}
	}
	// Each target's blocks first: whether its entries are counted depends
	// on what every list together asks of the chip's counter pool.
	type planned struct {
		iface, name   string
		guard, rules  aclBlock
		old           aclList
		exists, count bool
	}
	var plans []planned
	counted := 0 // counted entries if every guarded list is counted
	for _, t := range targets {
		name := aclListName(t.iface)
		old := have.lists[name]
		var guard aclBlock
		if want.keepGuard {
			guard = old.blocks[blockDHCPGuard]
		} else {
			lines, why := portGuard(want, t.port, switchVLANs, exempt)
			if why != "" {
				unguarded = append(unguarded, t.iface+" ("+why+")")
			}
			guard = wantBlock(aclBlock{}, lines, false)
		}
		rules := wantBlock(keptRules, want.rules, want.keepRules)
		if len(guard.lines)+len(rules.lines) == 0 {
			continue
		}
		if other, ok := have.foreign[t.iface]; ok {
			skipped = append(skipped, t.iface+" ("+other+")")
			continue
		}
		_, exists := have.lists[name]
		// Only a list with a guard is counted: its drop entry is the
		// port's rogue DHCP count, and nothing reads the other entries'.
		pl := planned{iface: t.iface, name: name, guard: guard, rules: rules, old: old, exists: exists, count: len(guard.lines) > 0}
		if pl.count {
			counted += len(guard.lines) + len(rules.lines) + 1 // remarks take no counter
		}
		plans = append(plans, pl)
	}
	// Past the pool, EOS programs the rules and leaves some entries without
	// a counter, saying nothing (measured 2026-11-14, §9.1): a guard drop
	// entry could read 0 whatever it drops. Then no list is counted and
	// the rogue DHCP counts are not reported; the filtering is the same.
	if counted > aclCounterPool {
		over = counted
		for i := range plans {
			plans[i].count = false
		}
	}
	wanted := map[string]bool{} // list names to keep
	var bind []string
	for _, pl := range plans {
		name, old := pl.name, pl.old
		wanted[name] = true
		current := pl.exists && old.counters == pl.count && old.stray == 0 &&
			sameBlock(old.blocks[blockDHCPGuard], pl.guard) && sameBlock(old.blocks[blockIPRules], pl.rules) &&
			len(old.blocks[blockDefault].lines) == 1 && old.blocks[blockDefault].lines[0] == aclDefaultAllow
		var lines []string
		nl := aclList{blocks: map[string]aclBlock{}, counters: pl.count}
		for _, b := range []struct {
			name string
			blk  aclBlock
		}{{blockDHCPGuard, pl.guard}, {blockIPRules, pl.rules}} {
			if len(b.blk.lines) == 0 {
				continue
			}
			lines = append(lines, "remark "+aclRemark+" "+b.name+" "+b.blk.hash)
			lines = append(lines, b.blk.lines...)
			nl.blocks[b.name] = b.blk
		}
		lines = append(lines, "remark "+aclRemark+" "+blockDefault, aclDefaultAllow)
		nl.blocks[blockDefault] = aclBlock{lines: []string{aclDefaultAllow}}
		if current {
			nl.seqs = old.seqs
		} else {
			body = append(body, "ip access-list "+name)
			if pl.count {
				body = append(body, aclCounters)
			} else if old.counters {
				body = append(body, "no "+aclCounters)
			}
			for _, s := range old.seqs {
				body = append(body, "no "+strconv.Itoa(s))
			}
			for i, l := range lines {
				seq := 10 * (i + 1)
				body = append(body, strconv.Itoa(seq)+" "+l)
				nl.seqs = append(nl.seqs, seq)
			}
			body = append(body, "exit")
		}
		next.lists[name] = nl
		next.bound[pl.iface] = name
		if have.bound[pl.iface] != name {
			bind = append(bind, pl.iface)
		}
	}
	var unbind []string
	for iface, name := range have.bound {
		if next.bound[iface] != name {
			unbind = append(unbind, iface)
		}
	}
	sort.Strings(unbind)
	for _, iface := range unbind {
		body = append(body, "interface "+iface, "no ip access-group "+have.bound[iface]+" in", "exit")
	}
	for _, iface := range bind {
		body = append(body, "interface "+iface, "ip access-group "+next.bound[iface]+" in", "exit")
	}
	var drop []string
	for name := range have.lists {
		if !wanted[name] {
			drop = append(drop, name)
		}
	}
	sort.Strings(drop)
	for _, name := range drop {
		body = append(body, "no ip access-list "+name)
	}
	return body, next, skipped, unguarded, over
}

// ipRuleLine renders one IPv4 rule: action, protocol ("" is every IPv4
// packet), and each side's prefix ("" = any) with, for TCP and UDP, its port
// or port range (none, or the whole 1-65535, = any).
func ipRuleLine(permit bool, protocol string, src, dst ipSide) (string, error) {
	action := "deny"
	if permit {
		action = "permit"
	}
	proto := protocol
	switch protocol {
	case "":
		proto = "ip"
	case "tcp", "udp":
	default:
		return "", fmt.Errorf("protocol %q", protocol)
	}
	parts := []string{action, proto}
	for _, s := range []ipSide{src, dst} {
		addr, err := addrClause(s.CIDR)
		if err != nil {
			return "", err
		}
		ports, err := portClause(s.Port, s.PortMin, s.PortMax)
		if err != nil {
			return "", err
		}
		if ports != "" && proto == "ip" {
			return "", fmt.Errorf("a port (%s) on a rule for every protocol", ports)
		}
		parts = append(parts, addr)
		if ports != "" {
			parts = append(parts, ports)
		}
	}
	return strings.Join(parts, " "), nil
}

// ipSide is one side of an IPv4 rule: prefix, and a port or a port range.
type ipSide struct {
	CIDR             string
	Port             int
	PortMin, PortMax int
}

func addrClause(cidr string) (string, error) {
	if cidr == "" {
		return "any", nil
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() {
		return "", fmt.Errorf("address %q is not an IPv4 prefix", cidr)
	}
	p = p.Masked()
	if p.Bits() == 32 {
		return "host " + p.Addr().String(), nil
	}
	return p.String(), nil
}

func portClause(port, lo, hi int) (string, error) {
	switch {
	case port != 0 && (lo != 0 || hi != 0):
		return "", fmt.Errorf("both a port (%d) and a port range (%d-%d)", port, lo, hi)
	case port != 0:
		if port < 1 || port > 65535 {
			return "", fmt.Errorf("port %d", port)
		}
		return "eq " + strconv.Itoa(port), nil
	case lo == 0 && hi == 0, lo == 1 && hi == 65535:
		return "", nil
	case 1 <= lo && lo <= hi && hi <= 65535:
		if lo == hi {
			return "eq " + strconv.Itoa(lo), nil
		}
		return fmt.Sprintf("range %d %d", lo, hi), nil
	}
	return "", fmt.Errorf("port range %d-%d", lo, hi)
}

// commitACL makes an ACL change in a configuration session committed with a
// timer, then confirms it only once a fresh connection to the switch answers:
// a rule that blocks the bridge's own traffic is reverted by the switch.
// unreachable reports that the change went in and the switch then stopped
// answering.
//
// A batch EOS rejects leaves the session uncommitted with whatever came
// before the error in it, and entering the session again would resume it;
// it is aborted (`configure session <name> abort`, which also reverts one
// pending on its timer).
//
// A commit EOS answers with `Error in ACL commit` (a warning: the lists
// commit whatever the hardware took, 4.26.14M, §9.4) is aborted too, and
// reported as unprogrammable: the same rules would fail the same way.
func (c *Collector) commitACL(ctx context.Context, body []string) (held string, err error) {
	session := sessionName(aclSession)
	cmds := append([]string{"enable", "configure session " + session}, body...)
	cmds = append(cmds, "commit timer "+commitTimer)
	c.logConfig(cmds)
	warns, err := configureWarn(ctx, c.t, cmds)
	if err == nil {
		if bad := aclCommitError(warns); bad != "" {
			abort := []string{"enable", "configure session " + session + " abort"}
			c.logConfig(abort)
			why := fmt.Sprintf("EOS could not program the ACL change (%s; lists written: %s)", bad, listsIn(body))
			if aerr := c.t.Configure(ctx, abort); aerr != nil {
				return why, fmt.Errorf("arista-eos: %s; aborting session %s: %v; the switch reverts it in %s", why, session, aerr, commitTimer)
			}
			return why, fmt.Errorf("arista-eos: %s; session %s aborted", why, session)
		}
	}
	if err != nil {
		abort := []string{"enable", "configure session " + session + " abort"}
		c.logConfig(abort)
		if aerr := c.t.Configure(ctx, abort); aerr != nil {
			return "", fmt.Errorf("arista-eos: ACL change: %w (aborting session %s: %v)", err, session, aerr)
		}
		return "", fmt.Errorf("arista-eos: ACL change: %w", err)
	}
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("arista-eos: ACL change in session %s not confirmed (%v); the switch reverts it in %s", session, ctx.Err(), commitTimer)
	case <-time.After(c.aclSettle):
	}
	check := c.t
	if c.redial != nil && c.host != "" {
		check = c.redial(c.host)
		defer check.Close()
	}
	if c.local && c.probe != nil && c.probeAddr != "" {
		// On the switch itself the socket answers whatever the lists do:
		// the proof is that the switch still reaches the controller.
		if err := c.probe(ctx, c.probeAddr, ""); err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("arista-eos: ACL change in session %s not confirmed (%v); the switch reverts it in %s", session, ctx.Err(), commitTimer)
			}
			return "the switch did not reach the controller after it", fmt.Errorf("arista-eos: the switch does not reach %s after the ACL change (%v); leaving session %s to revert in %s", c.probeAddr, err, session, commitTimer)
		}
	}
	if _, err := check.Run(ctx, []string{"enable", "show version"}); err != nil {
		if ctx.Err() != nil {
			// The write budget ran out, which says nothing about the rules.
			return "", fmt.Errorf("arista-eos: ACL change in session %s not confirmed (%v); the switch reverts it in %s", session, ctx.Err(), commitTimer)
		}
		return "the switch did not answer a new connection after it", fmt.Errorf("arista-eos: the switch does not answer a new connection after the ACL change (%v); leaving session %s to revert in %s", err, session, commitTimer)
	}
	confirm := []string{"enable", "configure session " + session + " commit", "write memory"}
	c.logConfig(confirm)
	if err := check.Configure(ctx, confirm); err != nil {
		return "", fmt.Errorf("arista-eos: confirm session %s: %w; the switch reverts it in %s", session, err, commitTimer)
	}
	return "", nil
}

// aclCommitError is the commit's `Error in ACL commit` warning, "" when the
// hardware took every list.
func aclCommitError(warns []string) string {
	for _, w := range warns {
		if strings.HasPrefix(w, "Error in ACL commit") {
			return w
		}
	}
	return ""
}

// listsIn names the lists a session body writes (the first five, then a
// count).
func listsIn(body []string) string {
	var names []string
	for _, l := range body {
		if n, ok := strings.CutPrefix(l, "ip access-list "); ok {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "none, bindings only"
	}
	if len(names) > 5 {
		return strings.Join(names[:5], ", ") + fmt.Sprintf(" and %d more", len(names)-5)
	}
	return strings.Join(names, ", ")
}

// applyACL brings the switch's lists to want. A change the switch would not
// let the bridge confirm is held: the same rules are not sent again (each
// attempt would cut the bridge off for the timer's length) until the
// controller sends different ones.
func (c *Collector) applyACL(ctx context.Context, snap *devicemodel.Snapshot, want aclWant) (int, error) {
	c.mu.Lock()
	have := c.acl
	c.mu.Unlock()
	if have.lists == nil {
		have = newACLState()
	}
	var exempt map[int]bool
	if want.mode == guardEdge {
		exempt = guardExempt(snap)
		if len(exempt) == 0 {
			if c.Log != nil {
				c.Log.Printf("arista: dhcp_guard edge: no uplink known (no bridge LLDP neighbour, gateway MAC not learned); guarding as %s", guardExact)
			}
			want.mode = guardExact
		}
	}
	body, next, skipped, unguarded, over := aclPlan(have, want, aclTargets(snap.Ports), snap.VLANs, exempt)
	if len(body) == 0 {
		return 0, nil
	}
	key := blockHash(body)
	if key == c.aclHeld {
		return 0, fmt.Errorf("arista-eos: holding the ACL change (%s); it is sent again once the controller's rules change", c.aclHeldWhy)
	}
	if c.Log != nil {
		if len(skipped) > 0 {
			c.Log.Printf("arista: no list of the driver's on %s: each carries its own IPv4 ACL inbound, and EOS takes one", strings.Join(skipped, ", "))
		}
		if len(unguarded) > 0 {
			c.Log.Printf("arista: DHCP guard (%s) leaves %d interfaces unguarded, the 7160 matching no VLAN in an ACL: %s", want.mode, len(unguarded), strings.Join(unguarded, "; "))
		}
		if over > 0 {
			c.Log.Printf("arista: the guarded lists need %d counted entries and the chip has %d counters: writing every list without `%s`, so no rogue DHCP count is reported (the guard itself is unchanged; docs/drivers/arista-eos.md §9.4)", over, aclCounterPool, aclCounters)
		}
	}
	if held, err := c.commitACL(ctx, body); err != nil {
		if held != "" {
			c.aclHeld, c.aclHeldWhy = key, held
		}
		return 0, err
	}
	c.aclHeld, c.aclHeldWhy = "", ""
	c.mu.Lock()
	c.acl = next
	c.mu.Unlock()
	return len(body), nil
}
