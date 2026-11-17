package podman

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

// UniFi's client Block on a Podman host (docs/drivers/podman.md §9): the
// controller's blocked clients (blocked_sta, docs/feature-map.md §3) are
// dropped by source MAC on the uplink NIC, in both directions. The proxmox
// driver hooks its bridge's prerouting (docs/drivers/proxmox.md §4d); this
// host's LAN side is no bridge: the uplink is a plain NIC, the host's own
// stack sits on it, and the LAN containers are macvlan children of it. So
// the table is of the netdev family, on the NIC itself:
//
//   - ingress: a frame from a blocked client on the LAN is dropped before
//     the kernel hands it to the host's stack or to a macvlan container
//     (the ingress hook runs ahead of the macvlan receive handler), so the
//     client reaches neither the host, a published port of a NAT'd
//     container, nor a LAN container.
//   - egress: a blocked LAN container (the only senders on the uplink with
//     a MAC of their own) no longer reaches the LAN.
//
// What neither hook sees is said in the doc: two macvlan containers on the
// same parent talk inside the macvlan driver (bridge mode), never crossing
// the NIC.
//
// The table is the driver's own, `netdev disunified_block`, apart from
// netavark's and firewalld's, replaced whole in one nft transaction when
// the list or the uplink changes and deleted when no client is blocked. Its
// comment `dui:<fp>` is the fingerprint of its chains. nft tables do not
// outlive a reboot; every collect lists the table, so a lost one is written
// again on the next apply.

const (
	blockTable = "disunified_block"
	blockMark  = "dui:"
)

// blockState is the table as the last collect read it back; Known false
// when the collect printed no macblock section (a capture older than the
// feature).
type blockState struct {
	Known   bool
	Present bool
	Mark    string
}

// nftList is `nft -j list table netdev disunified_block`, as far as the
// driver reads it.
type nftList struct {
	Nftables []struct {
		Table *struct {
			Name    string `json:"name"`
			Comment string `json:"comment"`
		} `json:"table"`
	} `json:"nftables"`
}

// parseBlock reads the macblock section: empty when the table is absent.
func parseBlock(body string, listed bool) (blockState, error) {
	st := blockState{Known: listed}
	if !listed {
		return st, nil
	}
	var l nftList
	if err := decodeJSON("macblock", body, &l); err != nil {
		return st, err
	}
	for _, o := range l.Nftables {
		if o.Table != nil && o.Table.Name == blockTable {
			st.Present = true
			st.Mark = strings.TrimPrefix(o.Table.Comment, blockMark)
		}
	}
	return st, nil
}

// blockRuleset is the whole table for nft -f and its fingerprint; no MACs
// = "".
func blockRuleset(nic string, macs []string) (text, mark string) {
	if len(macs) == 0 {
		return "", ""
	}
	set := strings.Join(macs, ", ")
	var body strings.Builder
	for _, hook := range []string{"ingress", "egress"} {
		fmt.Fprintf(&body, "\tchain %s {\n\t\ttype filter hook %s device %q priority filter; policy accept;\n", hook, hook, nic)
		fmt.Fprintf(&body, "\t\tether saddr { %s } counter drop\n\t}\n", set)
	}
	sum := sha256.Sum256([]byte(body.String()))
	mark = hex.EncodeToString(sum[:6])
	text = fmt.Sprintf("table netdev %s {\n\tcomment %q\n%s}\n", blockTable, blockMark+mark, body.String())
	return text, mark
}

// blockMACs normalises the controller's list (sorted, lowercase, MACs
// only). The host's own MAC is left out with a note: on the uplink's egress
// it is the source of everything the host and its NAT'd containers send.
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
			c.warnOnce("block-self:"+s, "blocked client %s is this host's own uplink MAC; not written", s)
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

// ApplyDevice implements devicemodel.DeviceController. The one device-wide
// setting this driver applies is the client Block; everything else in the
// push (NTP, syslog, SNMP, the hostname) is left to the host's operator.
func (c *Collector) ApplyDevice(ctx context.Context, d devicemodel.DeviceDesired) (int, error) {
	return c.applyBlock(ctx, d)
}

// applyBlock keeps the table at the controller's blocked clients: written
// when the fingerprint differs or the table is gone, deleted when none is
// blocked. Before the controller has said (BlockedSet false) the table is
// left as it is.
func (c *Collector) applyBlock(ctx context.Context, d devicemodel.DeviceDesired) (int, error) {
	if !d.BlockedSet {
		return 0, nil
	}
	c.mu.Lock()
	last, nft, cur, nic := c.last, c.nft, c.block, c.uplinkNIC
	c.mu.Unlock()
	if last == nil {
		return 0, fmt.Errorf("podman: no snapshot yet")
	}
	macs := c.blockMACs(d.BlockedClients, last.System.MAC)
	if !nft || nic == "" {
		if len(macs) > 0 {
			c.warnOnce("block-no-nft", "the controller blocks %d client(s); this host has no nft or no uplink NIC, so the Block is not applied", len(macs))
		}
		return 0, nil
	}
	text, mark := blockRuleset(nic, macs)
	switch {
	case text == "" && cur.Known && !cur.Present, text != "" && cur.Present && cur.Mark == mark:
		return 0, nil
	case text == "":
		c.Log.Printf("podman %s: no client blocked: nft delete table netdev %s", c.host, blockTable)
		if _, err := c.r.Run(ctx, "nft delete table netdev "+blockTable+" 2>/dev/null || true", ""); err != nil {
			return 0, fmt.Errorf("podman: nft delete table: %w", err)
		}
		cur = blockState{Known: true} // none now, whatever the collect printed
	default:
		c.Log.Printf("podman %s: blocked clients %s (nft table netdev %s on %s, %s)", c.host, strings.Join(macs, " "), blockTable, nic, mark)
		// Declaring the table first makes the delete valid when it does
		// not exist yet; the three statements are one transaction.
		if _, err := c.r.Run(ctx, "nft -f -", fmt.Sprintf("table netdev %s\ndelete table netdev %s\n%s", blockTable, blockTable, text)); err != nil {
			return 0, fmt.Errorf("podman: nft -f: %w", err)
		}
		cur = blockState{Known: true, Present: true, Mark: mark}
	}
	c.mu.Lock()
	c.block = cur
	c.mu.Unlock()
	return 1, nil
}

var _ devicemodel.DeviceController = (*Collector)(nil)
