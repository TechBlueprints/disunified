package proxmox

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/switchmodel"
)

// Link aggregation on the node's physical ports, from the controller's
// aggregate (switch.port.N.opmode=aggregate + lag=<id>, the same keys the
// Arista driver honours). The node's bond is the LAG: an aggregate over
// every slave of a bond converts it to 802.3ad (LACP) through the Proxmox
// API — pvesh, so Proxmox writes and applies its own network config —
// and removing the aggregate puts the bond back to active-backup. Plain
// NICs with no bond are joined into a new bond.
//
// UNTESTED LIVE: Clint's switches have no spare LACP-capable ports (his
// nodes bond to two different switches, which no LACP can span). The
// commands are what Proxmox's API documents; the tests cover the plan.
// PRs from a site that can exercise it are welcome (docs/drivers/proxmox.md §3b).
//
// Guards, each refused with a log line and no write: members that are not
// all slaves of one bond (or all plain NICs); a bond only partly covered;
// members whose LLDP neighbours are different chassis (a LAG cannot span
// switches, and applying it would take the node off the network); an
// aggregate that includes a guest port.
func (c *Collector) applyLAG(ctx context.Context, desired []switchmodel.PortDesired) (int, error) {
	c.mu.Lock()
	node, uplinks, bondModes, snap := c.node, c.uplinks, c.bondModes, c.last
	c.mu.Unlock()
	if len(uplinks) == 0 || snap == nil {
		return 0, nil
	}
	portBy := map[int]switchmodel.Port{}
	for _, p := range snap.Ports {
		portBy[p.Index] = p
	}
	groups := map[int][]int{} // lag id -> port indexes
	wantLAG := map[int]bool{}
	for _, d := range desired {
		if d.LAG > 0 {
			groups[d.LAG] = append(groups[d.LAG], d.Index)
			wantLAG[d.Index] = true
		}
	}
	changed := 0
	// Aggregates to create or convert.
	ids := make([]int, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		members := groups[id]
		sort.Ints(members)
		var us []uplink
		for _, idx := range members {
			u, ok := uplinks[idx]
			if !ok {
				c.warnOnce(fmt.Sprintf("lag-guest-%d", id), "aggregate %d includes port %d, which is not a physical port of the node; a guest port cannot be aggregated — ignored", id, idx)
				us = nil
				break
			}
			us = append(us, u)
		}
		if len(us) < 2 {
			if len(us) == 1 {
				c.warnOnce(fmt.Sprintf("lag-single-%d", id), "aggregate %d has one member (port %d); nothing to aggregate", id, members[0])
			}
			continue
		}
		if !sameNeighbour(members, portBy) {
			c.warnOnce(fmt.Sprintf("lag-span-%d", id), "aggregate %d over ports %v refused: their LLDP neighbours are different switches (or unknown); a LAG cannot span switches and applying it would take the node off the network", id, members)
			continue
		}
		bond := us[0].Member
		sameBond := bond != us[0].Active // a bond slave, not a plain NIC
		for _, u := range us {
			if (u.Member != bond) || (sameBond && u.Member == u.Active) {
				sameBond = false
				bond = ""
			}
		}
		switch {
		case bond != "":
			// Every slave of the bond must be in the aggregate.
			if n := countSlaves(uplinks, bond); n != len(us) {
				c.warnOnce(fmt.Sprintf("lag-partial-%d", id), "aggregate %d covers %d of %s's %d slaves; a bond converts to LACP only as a whole — ignored", id, len(us), bond, n)
				continue
			}
			if isLACP(bondModes[bond]) {
				continue // already an LACP bond
			}
			cmds := []string{
				fmt.Sprintf("pvesh set /nodes/%s/network/%s --bond_mode 802.3ad --bond_xmit_hash_policy layer2+3", node, bond),
				fmt.Sprintf("pvesh set /nodes/%s/network", node),
			}
			if err := c.runAll(ctx, cmds); err != nil {
				return changed, err
			}
			c.mu.Lock()
			c.bondModes[bond] = "IEEE 802.3ad Dynamic link aggregation"
			c.mu.Unlock()
			changed++
		default:
			// Plain NICs: make a bond of them and put it in the bridge in
			// their place.
			var nics []string
			for _, u := range us {
				if u.Member != u.Active {
					nics = nil
					break
				}
				nics = append(nics, u.Active)
			}
			if nics == nil {
				c.warnOnce(fmt.Sprintf("lag-mixed-%d", id), "aggregate %d mixes bond slaves and plain NICs — ignored", id)
				continue
			}
			newBond := fmt.Sprintf("bond%d", id-1)
			cmds := []string{
				fmt.Sprintf("pvesh create /nodes/%s/network --iface %s --type bond --slaves '%s' --bond_mode 802.3ad --bond_xmit_hash_policy layer2+3 --autostart 1", node, newBond, strings.Join(nics, " ")),
				fmt.Sprintf("pvesh set /nodes/%s/network/%s --bridge_ports '%s'", node, c.Bridge, strings.Join(replaceMembers(bridgeMembers(uplinks), nics, newBond), " ")),
				fmt.Sprintf("pvesh set /nodes/%s/network", node),
			}
			if err := c.runAll(ctx, cmds); err != nil {
				return changed, err
			}
			changed++
		}
	}
	// Aggregates removed: an LACP bond whose slaves are no longer
	// aggregated goes back to active-backup (the bond stays).
	seen := map[string]bool{}
	idxs := make([]int, 0, len(uplinks))
	for idx := range uplinks {
		idxs = append(idxs, idx)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(idxs))) // the bond's first slave sits at the highest port
	for _, idx := range idxs {
		u := uplinks[idx]
		bond := u.Member
		if bond == u.Active || seen[bond] || !isLACP(bondModes[bond]) || wantLAG[idx] {
			continue
		}
		seen[bond] = true
		stillWanted := false
		for j, v := range uplinks {
			if v.Member == bond && wantLAG[j] {
				stillWanted = true
			}
		}
		if stillWanted {
			continue
		}
		cmds := []string{
			fmt.Sprintf("pvesh set /nodes/%s/network/%s --bond_mode active-backup --bond-primary %s", node, bond, u.Ifaces[0]),
			fmt.Sprintf("pvesh set /nodes/%s/network", node),
		}
		if err := c.runAll(ctx, cmds); err != nil {
			return changed, err
		}
		c.mu.Lock()
		c.bondModes[bond] = "fault-tolerance (active-backup)"
		c.mu.Unlock()
		changed++
	}
	return changed, nil
}

func isLACP(mode string) bool { return strings.Contains(mode, "802.3ad") }

func countSlaves(uplinks map[int]uplink, bond string) int {
	n := 0
	for _, u := range uplinks {
		if u.Member == bond && u.Member != u.Active {
			n++
		}
	}
	return n
}

// sameNeighbour reports whether every member has an LLDP neighbour and
// they all name the same chassis.
func sameNeighbour(members []int, portBy map[int]switchmodel.Port) bool {
	chassis := ""
	for _, idx := range members {
		p := portBy[idx]
		if p.Neighbor == nil || p.Neighbor.ChassisID == "" {
			return false
		}
		if chassis == "" {
			chassis = p.Neighbor.ChassisID
		} else if !strings.EqualFold(chassis, p.Neighbor.ChassisID) {
			return false
		}
	}
	return chassis != ""
}

// bridgeMembers lists the bridge's members (each once) from the uplinks.
func bridgeMembers(uplinks map[int]uplink) []string {
	seen := map[string]bool{}
	var out []string
	idxs := make([]int, 0, len(uplinks))
	for idx := range uplinks {
		idxs = append(idxs, idx)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(idxs)))
	for _, idx := range idxs {
		m := uplinks[idx].Member
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// replaceMembers swaps the NICs joined into a bond for the bond itself.
func replaceMembers(members, nics []string, bond string) []string {
	drop := map[string]bool{}
	for _, n := range nics {
		drop[n] = true
	}
	var out []string
	added := false
	for _, m := range members {
		if drop[m] {
			if !added {
				out = append(out, bond)
				added = true
			}
			continue
		}
		out = append(out, m)
	}
	if !added {
		out = append(out, bond)
	}
	return out
}

func (c *Collector) runAll(ctx context.Context, cmds []string) error {
	for _, cmd := range cmds {
		c.Log.Printf("proxmox %s: %s", c.node, cmd)
		if _, err := c.r.Run(ctx, cmd, ""); err != nil {
			return fmt.Errorf("proxmox: %w", err)
		}
	}
	return nil
}
