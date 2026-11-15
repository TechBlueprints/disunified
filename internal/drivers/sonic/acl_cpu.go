package sonic

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// The CPU's copy (docs/drivers/sonic.md §11.32). On the DX010's SAI an ACL
// DROP removes the forwarding copy of a frame but leaves the to-CPU traps'
// copy alone (§11.7.8): a packet addressed to the switch reaches its
// kernel whatever the driver's tables say. UniFi's own SONiC switch, the
// ECS, answers on an address behind an internal switch port, so its ingress
// drop keeps a blocked source off the switch itself. The driver therefore
// mirrors its L3 tables into the kernel: iptables' raw table, PREROUTING,
// for packets that arrive on a front-panel interface (a Vlan, an Ethernet
// routed port, a PortChannel), each table a chain of its own with its rules
// in priority order -- FORWARD as RETURN, DROP as DROP. The chains run one
// after the other, as the chip's tables are separate lookups in which a drop
// wins.
//
// The raw table, because caclmgrd rewrites the filter table on any
// ACL_TABLE event (the driver's own `config acl add table` included),
// flushing every chain in it; it never touches IPv4's raw table. The
// write leaves the mirror's hash in a marker file under /run, which a
// reboot clears with the rules; collect.py reads it (acl_cpu) without sudo,
// and a collect whose hash differs from the tables' writes the mirror again.
//
// MAC rules (DUI_MAC_ACL) are not mirrored: the UI refuses the management
// network for a MAC rule, which is the only network either switch has an
// address on (§11.32).

const (
	cpuChain = "DUI_ACL_CPU"
	cpuMark  = "/run/disunified/acl-cpu"
	// aclCPUCommand runs cpuScript (stdin) as root.
	aclCPUCommand = "sudo bash -s -- disunified-acl-cpu"
)

// cpuIfaces are the front-panel L3 interfaces the tables' ports reach the
// kernel through. eth0, lo and docker0 are not bound in the chip either.
var cpuIfaces = []string{"Vlan+", "Ethernet+", "PortChannel+"}

// cpuTables are the driver's L3 tables, mirrored in this order.
var cpuTables = []string{guardTable, ipACLTable}

// cpuMirror is the iptables-restore input (raw table, no flush) that
// mirrors st's L3 tables, or nil when no table has a rule. A table whose
// rules hold something the kernel match cannot express is left out with a
// note: a FORWARD left out would let a lower DROP catch what the chip
// forwards.
func cpuMirror(st *swState) (lines, notes []string) {
	body := []string{"*raw", ":" + cpuChain + " - [0:0]"}
	for _, t := range cpuTables {
		body = append(body, ":"+t+" - [0:0]")
	}
	any := false
	for _, t := range cpuTables {
		rules := st.rulesOf(t)
		if len(rules) == 0 {
			continue
		}
		var out []string
		why := ""
		for _, r := range byPriority(rules) {
			args, skip, w := cpuRule(rules[r])
			if w != "" {
				why = fmt.Sprintf("rule %s: %s", r, w)
				break
			}
			if skip {
				continue
			}
			out = append(out, "-A "+t+" "+args)
		}
		if why != "" {
			notes = append(notes, fmt.Sprintf("ACL table %s is not mirrored for the switch's own addresses (%s)", t, why))
			continue
		}
		if len(out) == 0 {
			continue
		}
		any = true
		body = append(body, "-A "+cpuChain+" -j "+t)
		body = append(body, out...)
	}
	if !any {
		return nil, notes
	}
	return append(body, "COMMIT"), notes
}

// cpuRule is one ACL_RULE's iptables match and target. skip: a counted
// DROP of the guard (IN_PORTS), whose match is its VLAN's DROP limited to
// one port, so the DROP below it covers it. why: a field or action the
// mirror cannot express.
func cpuRule(f map[string]string) (args string, skip bool, why string) {
	known := map[string]bool{
		"PRIORITY": true, "PACKET_ACTION": true, "IN_PORTS": true, "IP_TYPE": true, "VLAN_ID": true,
		"SRC_IP": true, "DST_IP": true, "IP_PROTOCOL": true,
		"L4_SRC_PORT": true, "L4_DST_PORT": true, "L4_SRC_PORT_RANGE": true, "L4_DST_PORT_RANGE": true,
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !known[k] {
			return "", false, "field " + k
		}
	}
	if _, ok := f["IN_PORTS"]; ok {
		if f["PACKET_ACTION"] != "DROP" {
			return "", false, "IN_PORTS on a rule that does not drop"
		}
		skip = true
	}
	if v, ok := f["IP_TYPE"]; ok && v != "IPV4ANY" && v != "IP" && v != "IPV4" {
		return "", false, "IP_TYPE " + v
	}
	// In iptables' own order: interface, addresses, protocol, ports.
	var a []string
	if v, ok := f["VLAN_ID"]; ok {
		if atoi(v) < 1 || atoi(v) > 4094 {
			return "", false, "VLAN_ID " + v
		}
		a = append(a, "-i", "Vlan"+v)
	}
	if v, ok := f["SRC_IP"]; ok {
		a = append(a, "-s", v)
	}
	if v, ok := f["DST_IP"]; ok {
		a = append(a, "-d", v)
	}
	proto := ""
	if v, ok := f["IP_PROTOCOL"]; ok {
		switch v {
		case "6":
			proto = "tcp"
		case "17":
			proto = "udp"
		case "1":
			proto = "icmp"
		default:
			if atoi(v) < 1 || atoi(v) > 255 {
				return "", false, "IP_PROTOCOL " + v
			}
			proto = v
		}
		a = append(a, "-p", proto)
	}
	ports := []struct{ field, flag string }{
		{"L4_SRC_PORT", "--sport"}, {"L4_DST_PORT", "--dport"},
		{"L4_SRC_PORT_RANGE", "--sport"}, {"L4_DST_PORT_RANGE", "--dport"},
	}
	m := false
	for _, p := range ports {
		v, ok := f[p.field]
		if !ok {
			continue
		}
		if proto != "tcp" && proto != "udp" {
			return "", false, p.field + " without TCP or UDP"
		}
		if !m {
			a = append(a, "-m", proto)
			m = true
		}
		a = append(a, p.flag, strings.Replace(v, "-", ":", 1))
	}
	switch f["PACKET_ACTION"] {
	case "FORWARD":
		a = append(a, "-j", "RETURN")
	case "DROP":
		a = append(a, "-j", "DROP")
	default:
		return "", false, "PACKET_ACTION " + f["PACKET_ACTION"]
	}
	return strings.Join(a, " "), skip, ""
}

// cpuHash names a mirror: the marker's content, "" for no mirror.
func cpuHash(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return sha256Hex(strings.Join(lines, "\n"))[:16]
}

// cpuScript writes the mirror (lines) and its marker, or removes both
// (lines nil). Restored without a flush, each declared chain is emptied and
// filled in one transaction; the PREROUTING hooks are added once each.
func cpuScript(lines []string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	ifaces := strings.Join(cpuIfaces, " ")
	if len(lines) == 0 {
		b.WriteString("for i in " + ifaces + "; do while iptables -t raw -D PREROUTING -i \"$i\" -j " + cpuChain + " 2>/dev/null; do :; done; done\n")
		chains := append([]string{cpuChain}, cpuTables...)
		for _, op := range []string{"-F", "-X"} {
			b.WriteString("for c in " + strings.Join(chains, " ") + "; do iptables -t raw " + op + " \"$c\" 2>/dev/null || true; done\n")
		}
		b.WriteString("rm -f " + cpuMark + "\n")
		return b.String()
	}
	b.WriteString("printf '%s\\n'")
	for _, l := range lines {
		b.WriteString(" " + sshrun.ShellQuote(l))
	}
	b.WriteString(" | iptables-restore -n\n")
	b.WriteString("for i in " + ifaces + "; do iptables -t raw -C PREROUTING -i \"$i\" -j " + cpuChain + " 2>/dev/null || iptables -t raw -A PREROUTING -i \"$i\" -j " + cpuChain + "; done\n")
	b.WriteString("install -d -m 755 " + dirOf(cpuMark) + "\n")
	b.WriteString("echo " + cpuHash(lines) + " > " + cpuMark + "\n")
	return b.String()
}

func dirOf(p string) string { return p[:strings.LastIndex(p, "/")] }

// applyCPU brings the kernel mirror in line with st's tables (after the
// chip writes, applyACL): written when the marker's hash differs from
// the tables', removed when no table has a rule. Only where the collector
// reads the marker.
func (c *Collector) applyCPU(ctx context.Context, st *swState) (bool, error) {
	c.mu.Lock()
	known := st != nil && st.aclKnown && st.aclCPUKnown
	var lines, notes []string
	have := ""
	if known {
		lines, notes = cpuMirror(st)
		have = st.aclCPU
	}
	c.mu.Unlock()
	if !known {
		return false, nil
	}
	for _, n := range notes {
		c.warnOnce("acl cpu: "+n, "sonic: %s", n)
	}
	h := cpuHash(lines)
	if h == have {
		return false, nil
	}
	what := "removed"
	if h != "" {
		n := 0
		for _, l := range lines {
			if strings.HasPrefix(l, "-A ") && !strings.HasPrefix(l, "-A "+cpuChain+" ") {
				n++
			}
		}
		what = fmt.Sprintf("%d rules, %s", n, h)
	}
	c.Log.Printf("sonic apply acl cpu mirror (iptables raw %s, %s): %s", cpuChain, what, strings.Join(lines, " ; "))
	if _, err := c.run(ctx, aclCPUCommand, cpuScript(lines)); err != nil {
		return false, fmt.Errorf("sonic apply acl cpu mirror: %w", err)
	}
	c.mu.Lock()
	st.aclCPU = h
	c.mu.Unlock()
	return true, nil
}
