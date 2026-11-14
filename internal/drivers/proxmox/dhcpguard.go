package proxmox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// DHCP guarding: UniFi's Rogue DHCP Server Detection on a node
// (docs/drivers/proxmox.md §4f). While the controller has it on, a DHCP
// server reply (UDP 67 -> 68) that enters the bridge from one of this
// node's guest NICs on a guarded VLAN, from an address that is not one of
// that VLAN's trusted servers, is counted against the guest's port and
// dropped. The rules live in an nftables table of the driver's own,
// `bridge disunified`, and nothing else: Proxmox's firewall (iptables, or
// its own nftables tables) is neither read nor touched. The table is
// replaced as a whole in one nft transaction when the rules change, and
// deleted when the controller turns guarding off or no port has a guarded
// VLAN. The node's own uplinks are not guarded: replies from the LAN have
// already passed the upstream switch.
//
// The chain hooks the bridge's prerouting, which sees a frame once as it
// enters from the port; the forward hook runs once per egress port of a
// flooded frame, so a broadcast reply was counted once for every other
// port in its VLAN there (tested). Matching, as tested on the node's
// kernel (docs/fixtures/proxmox-9.1.6/dhcpguard-netns-test.*): a tagged
// frame from a trunk NIC matches `vlan id V`; an untagged frame carries no
// VLAN id (at the forward hook not even the port's PVID), so it is matched
// as `ether type ip` on the member and the guard of the port's untagged
// VLAN applies.

const (
	guardTable = "disunified"
	guardChain = "dhcp_guard"
	guardMark  = "dui:"
)

// guardPort is a guest NIC of this node as the guard sees it: its port,
// the interfaces it can enter the bridge through (the tap/veth, or the
// fwpr side of Proxmox's firewall bridge) and its VLANs.
type guardPort struct {
	Index   int
	Members []string
	VLAN    devicemodel.PortVLAN
}

// guardState is the table as the last collect read it back.
type guardState struct {
	Present bool
	Mark    string         // the table comment after "dui:", the rule set's fingerprint
	Drops   map[int]uint64 // port index -> DHCP server replies dropped since the table was written
}

// guardServers are the trusted IPv4 servers by VLAN; nil when guarding is
// off. A server that is not an IPv4 address is skipped with a note.
func guardServers(guards []devicemodel.DHCPGuard, note func(string, ...any)) map[int][]string {
	out := map[int][]string{}
	for _, g := range guards {
		ip := net.ParseIP(strings.TrimSpace(g.Server)).To4()
		if ip == nil {
			note("DHCP guard: server %q on VLAN %d is not an IPv4 address; skipped", g.Server, g.VLAN)
			continue
		}
		s := ip.String()
		have := false
		for _, x := range out[g.VLAN] {
			have = have || x == s
		}
		if !have {
			out[g.VLAN] = append(out[g.VLAN], s)
		}
	}
	for v := range out {
		sort.Strings(out[v])
	}
	return out
}

// guardRules renders the dhcp_guard chain's rules: per port, one for its
// untagged VLAN and one per tagged VLAN it carries, each only for a
// guarded VLAN.
func guardRules(ports []guardPort, servers map[int][]string) []string {
	var vlans []int
	for v := range servers {
		vlans = append(vlans, v)
	}
	sort.Ints(vlans)
	sorted := append([]guardPort(nil), ports...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })
	var rules []string
	for _, p := range sorted {
		if len(p.Members) == 0 {
			continue
		}
		iif := `iifname { "` + strings.Join(p.Members, `", "`) + `" }`
		tail := func(v int) string {
			return fmt.Sprintf(`ip saddr != { %s } counter drop comment "port %d"`, strings.Join(servers[v], ", "), p.Index)
		}
		native := p.VLAN.NativeVLAN
		if native == 0 {
			native = 1
		}
		if len(servers[native]) > 0 {
			rules = append(rules, iif+" ether type ip "+tail(native))
		}
		if p.VLAN.Mode == "access" {
			continue
		}
		for _, v := range vlans {
			if v == native || !(p.VLAN.AllowAll || containsInt(p.VLAN.Allowed, v)) {
				continue
			}
			rules = append(rules, iif+" vlan id "+strconv.Itoa(v)+" "+tail(v))
		}
	}
	return rules
}

// guardRuleset is the whole table for nft -f, and its fingerprint (kept
// in the table's comment, so a collect can tell whether the node has
// these rules). No rules = "".
func guardRuleset(bridge string, rules []string) (text, mark string) {
	if len(rules) == 0 {
		return "", ""
	}
	var body strings.Builder
	fmt.Fprintf(&body, "\tchain prerouting {\n\t\ttype filter hook prerouting priority filter; policy accept;\n")
	fmt.Fprintf(&body, "\t\tmeta ibrname %q meta protocol ip meta l4proto udp udp sport 67 udp dport 68 jump %s\n\t}\n", bridge, guardChain)
	fmt.Fprintf(&body, "\tchain %s {\n", guardChain)
	for _, r := range rules {
		fmt.Fprintf(&body, "\t\t%s\n", r)
	}
	body.WriteString("\t}\n")
	sum := sha256.Sum256([]byte(body.String()))
	mark = hex.EncodeToString(sum[:6])
	text = fmt.Sprintf("table bridge %s {\n\tcomment %q\n%s}\n", guardTable, guardMark+mark, body.String())
	return text, mark
}

// guardWrite is the nft transaction that replaces the table: the empty
// "table" line makes the delete valid when there is none yet.
func guardWrite(ruleset string) string {
	return fmt.Sprintf("table bridge %s\ndelete table bridge %s\n%s", guardTable, guardTable, ruleset)
}

// nftList is `nft -j list table bridge disunified`.
type nftList struct {
	Nftables []struct {
		Table *struct {
			Name    string `json:"name"`
			Comment string `json:"comment"`
		} `json:"table"`
		Rule *struct {
			Chain   string            `json:"chain"`
			Comment string            `json:"comment"`
			Expr    []json.RawMessage `json:"expr"`
		} `json:"rule"`
	} `json:"nftables"`
}

// parseGuard reads the dhcpguard section: empty when the table is absent.
func parseGuard(body string) (guardState, error) {
	st := guardState{Drops: map[int]uint64{}}
	var l nftList
	if err := decodeJSON("dhcpguard", body, &l); err != nil {
		return st, err
	}
	for _, o := range l.Nftables {
		if o.Table != nil && o.Table.Name == guardTable {
			st.Present = true
			st.Mark = strings.TrimPrefix(o.Table.Comment, guardMark)
		}
		if o.Rule == nil || o.Rule.Chain != guardChain {
			continue
		}
		var idx int
		if _, err := fmt.Sscanf(o.Rule.Comment, "port %d", &idx); err != nil {
			continue
		}
		for _, e := range o.Rule.Expr {
			var c struct {
				Counter *struct {
					Packets uint64 `json:"packets"`
				} `json:"counter"`
			}
			if json.Unmarshal(e, &c) == nil && c.Counter != nil {
				st.Drops[idx] += c.Counter.Packets
			}
		}
	}
	return st, nil
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
