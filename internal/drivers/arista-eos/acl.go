package aristaeos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// DHCP guarding and switch ACL rules as an EOS port ACL
// (docs/drivers/arista-eos.md §8).
//
// EOS 4.26's own `ip dhcp snooping` cannot block a rogue server (§6), but an
// ingress IPv4 ACL can: for each guarded VLAN, permit UDP 67 -> 68 from each
// trusted server, deny the rest of UDP 67 -> 68 on that VLAN. An interface
// takes one IPv4 ACL per direction, so the guard and the controller's IP
// rules share one list, the guard first (its drop wins over an Allow rule),
// and an explicit `permit ip any any` last: EOS ACLs end in an implicit
// deny, UniFi's are default-allow.
//
// The list is the driver's own (ipACLName) and nothing else is touched: an
// interface already carrying another IPv4 ACL inbound is left alone. Every
// block of rules starts with a remark naming it and a hash of the lines the
// driver rendered, so the diff does not depend on how EOS rewrites a rule
// for display (`eq 67` comes back as `eq bootps`), and a block the
// controller leaves alone is carried over as the switch shows it.

// ipACLName is the driver's IPv4 ACL. EOS ACL names start with a letter and
// carry no space or quote.
const ipACLName = "DUI_IP_ACL"

// aclSession is the configuration session an ACL change is made in, committed
// with commitTimer like an address move: the ACL is bound to the uplink too,
// so a rule that blocks the bridge's own traffic reverts by itself.
const aclSession = "dui-acl"

// aclSettleDefault is how long the driver waits after a timed commit before
// dialling the switch to confirm it: the ACL is programmed into the hardware
// after the commit returns, and a check made before that would pass whatever
// the rules say.
const aclSettleDefault = 5 * time.Second

// The blocks of the driver's IPv4 ACL, in list order. Each starts with
// `remark disunified <block> <hash>`; the default block is the closing
// permit and carries no hash.
const (
	aclRemark       = "disunified"
	blockDHCPGuard  = "dhcp-guard"
	blockIPRules    = "ip-rules"
	blockDefault    = "default"
	aclDefaultAllow = "permit ip any any"
)

// aclBlock is one block of the driver's ACL: the hash in its remark and its
// rules as the switch shows them (or as the driver rendered them, for a
// block it just wrote).
type aclBlock struct {
	hash  string
	lines []string
}

// aclState is what the running configuration says about the driver's ACL.
type aclState struct {
	exists bool
	seqs   []int // every sequence number in the list, remarks included
	blocks map[string]aclBlock
	stray  int             // rules outside any block (added by hand): the list is rewritten
	bound  map[string]bool // interfaces carrying `ip access-group DUI_IP_ACL in`
	// foreign is every interface carrying another IPv4 ACL inbound, with its
	// name. EOS takes one per direction, so the driver does not bind there.
	foreign map[string]string
}

// aclWant is what the ACL should hold. A block marked keep stays as the
// switch has it: the controller sent nothing for it this time.
type aclWant struct {
	guard, rules         []string
	keepGuard, keepRules bool
}

// aclFromRunningConfig reads the driver's ACL and every interface's inbound
// IPv4 ACL out of the structured running configuration. EOS keys a list's
// rules by "<seq> <rule>".
func aclFromRunningConfig(rc showRunningConfig) aclState {
	st := aclState{blocks: map[string]aclBlock{}, bound: map[string]bool{}, foreign: map[string]string{}}
	if sec, ok := rc.Cmds["ip access-list "+ipACLName]; ok {
		st.exists = true
		type rule struct {
			seq  int
			text string
		}
		var rules []rule
		if sec != nil {
			for line := range sec.Cmds {
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
			st.seqs = append(st.seqs, r.seq)
			if f := strings.Fields(r.text); len(f) >= 3 && f[0] == "remark" && f[1] == aclRemark {
				block = f[2]
				b := aclBlock{}
				if len(f) >= 4 {
					b.hash = f[3]
				}
				st.blocks[block] = b
				continue
			}
			if block == "" || strings.HasPrefix(r.text, "remark ") {
				st.stray++
				continue
			}
			b := st.blocks[block]
			b.lines = append(b.lines, r.text)
			st.blocks[block] = b
		}
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
			if name == ipACLName {
				st.bound[iface] = true
			} else {
				st.foreign[iface] = name
			}
		}
	}
	return st
}

// aclTargets is every interface the ACL is bound to: each lane of a port that
// is not in an aggregate, and the Port-Channel of every aggregate. The
// management port and VLAN interfaces are never bound.
func aclTargets(ports []devicemodel.Port) []string {
	set := map[string]bool{}
	for _, p := range ports {
		if p.LAGID > 0 {
			set["Port-Channel"+strconv.Itoa(p.LAGID)] = true
			continue
		}
		for _, l := range laneNames(p) {
			set[l] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// blockHash names a block's rendered lines in its remark.
func blockHash(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:6])
}

// wantBlock is the block the switch should hold: the controller's lines, or
// the switch's own when the block is kept.
func wantBlock(have aclState, name string, lines []string, keep bool) aclBlock {
	if keep {
		return have.blocks[name]
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

// aclPlan is the session body that brings the driver's ACL and its bindings
// to want, and the state the switch has once it is committed. An empty body
// means the switch already matches. skipped lists the interfaces left alone
// because they carry another IPv4 ACL.
func aclPlan(have aclState, want aclWant, targets []string) (body []string, next aclState, skipped []string) {
	guard := wantBlock(have, blockDHCPGuard, want.guard, want.keepGuard)
	rules := wantBlock(have, blockIPRules, want.rules, want.keepRules)
	needACL := len(guard.lines)+len(rules.lines) > 0

	next = aclState{blocks: map[string]aclBlock{}, bound: map[string]bool{}, foreign: have.foreign}
	wantBound := map[string]bool{}
	if needACL {
		for _, t := range targets {
			if other, ok := have.foreign[t]; ok {
				skipped = append(skipped, t+" ("+other+")")
				continue
			}
			wantBound[t] = true
		}
	}

	if needACL {
		current := have.exists && have.stray == 0 &&
			sameBlock(have.blocks[blockDHCPGuard], guard) && sameBlock(have.blocks[blockIPRules], rules) &&
			len(have.blocks[blockDefault].lines) == 1 && have.blocks[blockDefault].lines[0] == aclDefaultAllow
		var lines []string
		for _, b := range []struct {
			name string
			blk  aclBlock
		}{{blockDHCPGuard, guard}, {blockIPRules, rules}} {
			if len(b.blk.lines) == 0 {
				continue
			}
			lines = append(lines, "remark "+aclRemark+" "+b.name+" "+b.blk.hash)
			lines = append(lines, b.blk.lines...)
			next.blocks[b.name] = b.blk
		}
		lines = append(lines, "remark "+aclRemark+" "+blockDefault, aclDefaultAllow)
		next.blocks[blockDefault] = aclBlock{lines: []string{aclDefaultAllow}}
		next.exists = true
		if current {
			next.seqs = have.seqs
		} else {
			body = append(body, "ip access-list "+ipACLName)
			for _, s := range have.seqs {
				body = append(body, "no "+strconv.Itoa(s))
			}
			for i, l := range lines {
				seq := 10 * (i + 1)
				body = append(body, strconv.Itoa(seq)+" "+l)
				next.seqs = append(next.seqs, seq)
			}
			body = append(body, "exit")
		}
	}

	var bind, unbind []string
	for _, t := range targets {
		if wantBound[t] && !have.bound[t] {
			bind = append(bind, t)
		}
	}
	for iface := range have.bound {
		if !wantBound[iface] {
			unbind = append(unbind, iface)
		}
	}
	sort.Strings(unbind)
	for _, iface := range unbind {
		body = append(body, "interface "+iface, "no ip access-group "+ipACLName+" in", "exit")
	}
	for _, iface := range bind {
		body = append(body, "interface "+iface, "ip access-group "+ipACLName+" in", "exit")
	}
	for iface := range wantBound {
		next.bound[iface] = true
	}
	if !needACL && have.exists {
		body = append(body, "no ip access-list "+ipACLName)
	}
	return body, next, skipped
}

// guardLines is the DHCP guard for one VLAN: replies from each trusted
// server pass, every other UDP 67 -> 68 on the VLAN is dropped. The VLAN
// qualifier's mask 0xfff compares all twelve bits.
func guardLines(vlan int, servers []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, s := range servers {
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("DHCP server %q on VLAN %d is not an IPv4 address", s, vlan)
		}
		if seen[a.String()] {
			continue
		}
		seen[a.String()] = true
		out = append(out, fmt.Sprintf("permit vlan %d 0xfff udp host %s eq 67 any eq 68", vlan, a))
	}
	return append(out, fmt.Sprintf("deny vlan %d 0xfff udp any eq 67 any eq 68", vlan)), nil
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
func (c *Collector) commitACL(ctx context.Context, body []string) (unreachable bool, err error) {
	cmds := append([]string{"enable", "configure session " + aclSession}, body...)
	cmds = append(cmds, "commit timer "+commitTimer)
	c.logConfig(cmds)
	if err := c.t.Configure(ctx, cmds); err != nil {
		abort := []string{"enable", "configure session " + aclSession + " abort"}
		c.logConfig(abort)
		if aerr := c.t.Configure(ctx, abort); aerr != nil {
			return false, fmt.Errorf("arista-eos: ACL change: %w (aborting session %s: %v)", err, aclSession, aerr)
		}
		return false, fmt.Errorf("arista-eos: ACL change: %w", err)
	}
	select {
	case <-ctx.Done():
		return false, fmt.Errorf("arista-eos: ACL change in session %s not confirmed (%v); the switch reverts it in %s", aclSession, ctx.Err(), commitTimer)
	case <-time.After(c.aclSettle):
	}
	check := c.t
	if c.redial != nil && c.host != "" {
		check = c.redial(c.host)
		defer check.Close()
	}
	if _, err := check.Run(ctx, []string{"enable", "show version"}); err != nil {
		return true, fmt.Errorf("arista-eos: the switch does not answer a new connection after the ACL change (%v); leaving session %s to revert in %s", err, aclSession, commitTimer)
	}
	confirm := []string{"enable", "configure session " + aclSession + " commit", "write memory"}
	c.logConfig(confirm)
	if err := check.Configure(ctx, confirm); err != nil {
		return false, fmt.Errorf("arista-eos: confirm session %s: %w; the switch reverts it in %s", aclSession, err, commitTimer)
	}
	return false, nil
}

// applyACL brings the switch's ACL to want. A change the switch would not
// let the bridge confirm is held: the same rules are not sent again (each
// attempt would cut the bridge off for the timer's length) until the
// controller sends different ones.
func (c *Collector) applyACL(ctx context.Context, snap *devicemodel.Snapshot, want aclWant) (int, error) {
	c.mu.Lock()
	have := c.acl
	c.mu.Unlock()
	body, next, skipped := aclPlan(have, want, aclTargets(snap.Ports))
	if len(body) == 0 {
		return 0, nil
	}
	key := blockHash(body)
	if key == c.aclHeld {
		return 0, fmt.Errorf("arista-eos: holding the ACL change that left the switch unreachable; it is sent again once the controller's rules change")
	}
	if len(skipped) > 0 && c.Log != nil {
		c.Log.Printf("arista: not binding %s on %s: each carries its own IPv4 ACL inbound, and EOS takes one", ipACLName, strings.Join(skipped, ", "))
	}
	if unreachable, err := c.commitACL(ctx, body); err != nil {
		if unreachable {
			c.aclHeld = key
		}
		return 0, err
	}
	c.aclHeld = ""
	c.mu.Lock()
	c.acl = next
	c.mu.Unlock()
	return len(body), nil
}
