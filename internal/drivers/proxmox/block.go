package proxmox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// UniFi's client Block on a node (docs/drivers/proxmox.md §4d): the
// controller's blocked clients (blocked_sta, docs/feature-map.md §3) are
// dropped as they enter the node's bridge from any port, guest NIC or
// uplink, as UniFi's own SONiC switch drops them on every front-panel port
// (docs/drivers/sonic.md §11.33). Bridge prerouting runs before the
// bridge's local delivery, so a blocked client reaches neither another
// port nor the node's own address. The rule lives in an nftables table of
// the driver's own, `bridge disunified_block`, apart from the DHCP guard's
// table and from Proxmox's firewall, replaced whole in one nft transaction
// when the list changes and deleted when no client is blocked. Its comment
// `dui:<fp>` is the list's fingerprint, as the guard's is.

const (
	blockTable = "disunified_block"
	blockChain = "prerouting"
)

// blockState is the table as the last collect read it back; Known false
// when the collect printed no macblock section (an older capture).
type blockState struct {
	Known   bool
	Present bool
	Mark    string
}

// parseBlock reads the macblock section (`nft -j list table bridge
// disunified_block`; empty when the table is absent).
func parseBlock(body string, present bool) (blockState, error) {
	st := blockState{Known: present}
	if !present {
		return st, nil
	}
	var l nftList
	if err := decodeJSON("macblock", body, &l); err != nil {
		return st, err
	}
	for _, o := range l.Nftables {
		if o.Table != nil && o.Table.Name == blockTable {
			st.Present = true
			st.Mark = strings.TrimPrefix(o.Table.Comment, guardMark)
		}
	}
	return st, nil
}

// blockRuleset is the whole table for nft -f and its fingerprint; no MACs
// = "".
func blockRuleset(bridge string, macs []string) (text, mark string) {
	if len(macs) == 0 {
		return "", ""
	}
	var body strings.Builder
	fmt.Fprintf(&body, "\tchain %s {\n\t\ttype filter hook prerouting priority filter; policy accept;\n", blockChain)
	fmt.Fprintf(&body, "\t\tmeta ibrname %q ether saddr { %s } counter drop\n\t}\n", bridge, strings.Join(macs, ", "))
	sum := sha256.Sum256([]byte(body.String()))
	mark = hex.EncodeToString(sum[:6])
	text = fmt.Sprintf("table bridge %s {\n\tcomment %q\n%s}\n", blockTable, guardMark+mark, body.String())
	return text, mark
}

// blockMACs normalises the controller's list (sorted, lowercase, MACs
// only); the node's own bridge MAC is left out with a note, or the node
// would drop its own frames' echoes.
func (c *Collector) blockMACs(list []string, self string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range list {
		hw, err := net.ParseMAC(m)
		if err != nil || len(hw) != 6 {
			c.warnOnce("block-bad:"+m, "blocked client %q is not a MAC address; not written", m)
			continue
		}
		s := hw.String()
		if s == strings.ToLower(self) {
			c.warnOnce("block-self:"+s, "blocked client %s is this node's own bridge MAC; not written", s)
			continue
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// applyBlock keeps the table at the controller's blocked clients: written
// when the fingerprint differs, deleted when none is blocked. Before the
// controller has said (BlockedSet false) the table is left as it is.
func (c *Collector) applyBlock(ctx context.Context, d devicemodel.DeviceDesired) (int, error) {
	if !d.BlockedSet {
		return 0, nil
	}
	c.mu.Lock()
	nft, cur, self := c.nft, c.block, c.bridgeMAC
	c.mu.Unlock()
	macs := c.blockMACs(d.BlockedClients, self)
	if !nft {
		if len(macs) > 0 {
			c.warnOnce("block-no-nft", "the controller blocks %d client(s); this node has no nft, so the Block is not applied", len(macs))
		}
		return 0, nil
	}
	text, mark := blockRuleset(c.Bridge, macs)
	switch {
	case text == "" && cur.Known && !cur.Present, text != "" && cur.Present && cur.Mark == mark:
		return 0, nil
	case text == "":
		c.Log.Printf("proxmox %s: no client blocked: nft delete table bridge %s", c.node, blockTable)
		if _, err := c.r.Run(ctx, "nft delete table bridge "+blockTable+" 2>/dev/null || true", ""); err != nil {
			return 0, err
		}
		cur = blockState{Known: true} // none now, whatever the collect printed
	default:
		c.Log.Printf("proxmox %s: blocked clients %s (nft table bridge %s, %s)", c.node, strings.Join(macs, " "), blockTable, mark)
		if _, err := c.r.Run(ctx, "nft -f -", fmt.Sprintf("table bridge %s\ndelete table bridge %s\n%s", blockTable, blockTable, text)); err != nil {
			return 0, err
		}
		cur = blockState{Known: true, Present: true, Mark: mark}
	}
	c.mu.Lock()
	c.block = cur
	c.mu.Unlock()
	return 1, nil
}
