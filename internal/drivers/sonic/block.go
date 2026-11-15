package sonic

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
)

// UniFi's client Block (docs/drivers/sonic.md §11.33). The controller sends
// the blocked clients' MACs in every setparam (blocked_sta); UniFi's own
// SONiC switch, the ECS, holds one rule per client in its MAC ACL table:
// MB_<mac>, DROP, PRIORITY 1023, SRC_MAC <mac>, no VLAN, so it covers every
// VLAN, the management VLAN included (§11.32.1). The driver writes the same
// rule into its own MAC table, DUI_MAC_ACL, beside the controller's MAC
// rules (RULE_N, from 9999 down) and outside their limit
// (max_custom_mac_acls): the table needs the image kit's sonic-swss 0003,
// as every MAC rule does.
//
// On this SAI an ACL drop leaves the traps' copy to the CPU alone (§11.7.8),
// so a blocked client would still reach the switch's own addresses, which
// the ECS's drop keeps it off (its address is a switched host). The kernel
// drops that copy: an ebtables chain, DUI_MAC_BLOCK, jumped to from the
// kernel Bridge's INPUT, one `-s <mac> -j DROP` per MB_ rule the chip
// holds, which covers ARP, IPv4 and IPv6 alike (the IPv4 raw mirror of the
// L3 tables, acl_cpu.go, would leave the other two). A routed front-panel
// port is no Bridge member, so its frames do not pass the chain. As with
// that mirror, a marker under /run (collect.py mac_block) holds the
// chain's hash, and a reboot clears both.

const (
	blockPrefix   = "MB_"
	blockPriority = "1023" // the ECS's, below every controller rule
	blockChain    = "DUI_MAC_BLOCK"
	blockMark     = "/run/disunified/mac-block"
	// macBlockCommand runs blockScript (stdin) as root.
	macBlockCommand = "sudo bash -s -- disunified-mac-block"
)

func isBlockRule(name string) bool { return strings.HasPrefix(name, blockPrefix) }

// blockRules are DUI_MAC_ACL's rules for the blocked clients, written as
// the ECS writes its own (a bare MAC, no mask, no VLAN). A client whose MAC
// is the management path's own peer (the gateway, or the host the bridge
// is reached from) is held with a note: the drop would cut the switch off
// from the controller.
func blockRules(macs []string, p l2Path, note func(string, ...any)) map[string]map[string]string {
	rules := map[string]map[string]string{}
	for _, m := range macs {
		hw, err := net.ParseMAC(m)
		if err != nil || len(hw) != 6 {
			note("blocked client %q: not a MAC address; not written", m)
			continue
		}
		mac := hw.String()
		held := false
		for _, peer := range p.peers {
			if peer.String() == mac {
				held = true
			}
		}
		for _, dst := range p.dsts {
			if dst.String() == mac {
				held = true
			}
		}
		if held {
			note("blocked client %s: it is on the switch's management path (its gateway or the bridge's own far end, or the switch itself); not written, or the switch would lose the controller", mac)
			continue
		}
		rules[blockPrefix+mac] = map[string]string{"PACKET_ACTION": "DROP", "PRIORITY": blockPriority, "SRC_MAC": mac}
	}
	return rules
}

// blockMACs are the sources the chip drops for blocked clients: st's MB_
// rules, sorted.
func blockMACs(st *swState) []string {
	var out []string
	for name, f := range st.rulesOf(macACLTable) {
		if isBlockRule(name) && f["PACKET_ACTION"] == "DROP" && f["SRC_MAC"] != "" {
			if hw, err := net.ParseMAC(f["SRC_MAC"]); err == nil {
				out = append(out, hw.String())
			}
		}
	}
	sort.Strings(out)
	return out
}

// blockHash names a kernel chain: the marker's content, "" for none.
func blockHash(macs []string) string {
	if len(macs) == 0 {
		return ""
	}
	return sha256Hex(blockChain + "\n" + strings.Join(macs, "\n"))[:16]
}

// blockScript writes the chain for macs and its marker, or removes both
// (macs nil). The chain is emptied and filled; INPUT's jump goes in first
// in INPUT, once.
func blockScript(macs []string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	unhook := "while ebtables -D INPUT -j " + blockChain + " 2>/dev/null; do :; done\n"
	if len(macs) == 0 {
		b.WriteString(unhook)
		b.WriteString("ebtables -F " + blockChain + " 2>/dev/null || true\n")
		b.WriteString("ebtables -X " + blockChain + " 2>/dev/null || true\n")
		b.WriteString("rm -f " + blockMark + "\n")
		return b.String()
	}
	b.WriteString("ebtables -N " + blockChain + " -P RETURN 2>/dev/null || ebtables -F " + blockChain + "\n")
	for _, m := range macs {
		b.WriteString("ebtables -A " + blockChain + " -s " + m + " -j DROP\n") // a MAC net.ParseMAC wrote: nothing to quote
	}
	b.WriteString("ebtables -L INPUT | grep -q -- '^-j " + blockChain + " *$' || ebtables -I INPUT 1 -j " + blockChain + "\n")
	b.WriteString("install -d -m 755 " + dirOf(blockMark) + "\n")
	b.WriteString("echo " + blockHash(macs) + " > " + blockMark + "\n")
	return b.String()
}

// applyBlockKernel brings the kernel chain in line with the chip's MB_
// rules (after the chip writes, applyACL): written when the marker's hash
// differs, removed when no client is blocked. Only where the collector
// reads the marker.
func (c *Collector) applyBlockKernel(ctx context.Context, st *swState) (bool, error) {
	c.mu.Lock()
	known := st != nil && st.aclKnown && st.macBlockKnown
	var macs []string
	have := ""
	if known {
		macs = blockMACs(st)
		have = st.macBlock
	}
	c.mu.Unlock()
	if !known {
		return false, nil
	}
	h := blockHash(macs)
	if h == have {
		return false, nil
	}
	what := "removed"
	if h != "" {
		what = fmt.Sprintf("%d clients, %s", len(macs), h)
	}
	c.Log.Printf("sonic apply blocked clients in the kernel (ebtables INPUT %s, %s): %s", blockChain, what, strings.Join(macs, " "))
	if _, err := c.run(ctx, macBlockCommand, blockScript(macs)); err != nil {
		return false, fmt.Errorf("sonic apply blocked clients in the kernel: %w", err)
	}
	c.mu.Lock()
	st.macBlock = h
	c.mu.Unlock()
	return true, nil
}
