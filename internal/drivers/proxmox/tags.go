package proxmox

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A guest NIC's switch port lives in the guest's own Proxmox tags, not in
// a file of the bridge's (Clint, 2026-09-20: the container is stateless
// and there is no other cluster-wide place a NIC can carry metadata; the
// NIC option string is a closed schema and unknown config keys are
// dropped). The tag grammar:
//
//	unifi.p<port>.<c|h.<host>>[.<bridge>][.net<N>]
//
//	unifi.p25.c              net0 is port 25 on every node (cluster numbering)
//	unifi.p25.h.proxmox-2    net0 is port 25 on proxmox-2's switch (node numbering)
//	unifi.p27.c.net1         net1 is port 27
//	unifi.p3.c.vmbr1         net0 is port 3 on the vmbr1 switch
//
// Proxmox tags allow only [a-z0-9_][a-z0-9_\-\+\.]* (lowercased by default),
// so "." is the field separator: node names are single labels and never
// contain one. The bridge on the node a guest lives on ("the owner")
// assigns a port and writes the tag; every other bridge only reads it.
// A hand-edited tag moves the guest to that port on the next poll. Two
// guests claiming one port (a clone or a restored backup copies tags): the
// lower VMID keeps it and the other is retagged by its owner. Malformed
// or out-of-range "unifi." tags for this bridge are replaced.
const tagPrefix = "unifi."

// pveTag is Proxmox's own tag validation (PVE::JSONSchema::PVE_TAG_RE).
var pveTag = regexp.MustCompile(`(?i)^[a-z0-9_][a-z0-9_\-\+\.]*$`)

// portClaim is one parsed tag.
type portClaim struct {
	Port   int
	Scope  string // "c" (cluster numbering) or "h" (node numbering)
	Host   string // the node, for "h"
	Bridge string // "vmbr0" when the tag omits it
	NIC    int    // netN; 0 when omitted
}

// parseClaim reads a tag; ok is false for anything that is not a
// well-formed port tag.
func parseClaim(tag string) (portClaim, bool) {
	f := strings.Split(strings.ToLower(tag), ".")
	if len(f) < 3 || f[0] != "unifi" || !strings.HasPrefix(f[1], "p") {
		return portClaim{}, false
	}
	port, err := strconv.Atoi(strings.TrimPrefix(f[1], "p"))
	if err != nil || port < 1 {
		return portClaim{}, false
	}
	c := portClaim{Port: port, Scope: f[2], Bridge: "vmbr0"}
	i := 3
	switch c.Scope {
	case "c":
	case "h":
		if len(f) < 4 || f[3] == "" {
			return portClaim{}, false
		}
		c.Host = f[3]
		i = 4
	default:
		return portClaim{}, false
	}
	if i < len(f) && strings.HasPrefix(f[i], "vmbr") {
		c.Bridge = f[i]
		i++
	}
	if i < len(f) && strings.HasPrefix(f[i], "net") {
		n, err := strconv.Atoi(strings.TrimPrefix(f[i], "net"))
		if err != nil || n < 0 {
			return portClaim{}, false
		}
		c.NIC = n
		i++
	}
	if i != len(f) {
		return portClaim{}, false
	}
	return c, true
}

// String renders the claim as its tag.
func (c portClaim) String() string {
	s := fmt.Sprintf("%sp%d.%s", tagPrefix, c.Port, c.Scope)
	if c.Scope == "h" {
		s += "." + c.Host
	}
	if c.Bridge != "vmbr0" {
		s += "." + c.Bridge
	}
	if c.NIC != 0 {
		s += fmt.Sprintf(".net%d", c.NIC)
	}
	return s
}

// retag is one guest whose tag list must be rewritten by this node.
type retag struct {
	Kind string
	VMID int
	Tags []string
}

// assignPorts gives the NICs on this bridge their ports from their tags,
// assigns the lowest free port to the untagged ones, and lists the guests
// on this node whose tags must be rewritten. nics are sorted (vmid, kind,
// index), which is the order new guests are numbered in and the order
// that wins a duplicate claim. Every bridge computes the same ports for
// untagged NICs from the same tags, so an untagged guest on another node
// is shown at the port its owner is about to write; once the tag exists
// it alone decides.
func (c *Collector) assignPorts(nics []guestNIC, host string, n int) (map[string]int, []retag) {
	scope := "c"
	if c.NodeNumbering {
		scope = "h"
	}
	slotOf := map[string]int{}
	taken := map[int]string{} // port -> key
	nicBy := map[string]guestNIC{}
	for _, nic := range nics {
		nicBy[nic.Key()] = nic
	}
	for _, nic := range nics {
		for _, t := range nic.Tags {
			cl, ok := parseClaim(t)
			if !ok || cl.Bridge != c.Bridge || cl.NIC != nic.Index {
				continue
			}
			// A well-formed tag for this NIC that this bridge cannot take
			// means the nodes' settings disagree (ports, guest_ports,
			// numbering): said once, as every node sees it; the owner
			// replaces it below.
			if cl.Scope != scope {
				c.warnOnce("tag-scope:"+nic.Key()+":"+t, "%s on %s carries %s, a tag for %s numbering; this bridge numbers by %s, so it is ignored (numbering must be the same on every node)", nic.label(), nic.Node, t, numberingName(cl.Scope), numberingName(scope))
				continue
			}
			if cl.Port > n {
				c.warnOnce("tag-range:"+nic.Key()+":"+t, "%s on %s carries %s, above this node's %d guest ports, so it is ignored (ports and guest_ports must be the same on every node)", nic.label(), nic.Node, t, n)
				continue
			}
			if scope == "h" && cl.Host != nic.Node {
				continue // tagged for another node's switch: it migrated
			}
			if held, dup := taken[cl.Port]; dup {
				// The lower VMID keeps the port (nics are in VMID order).
				// Every node logs it once; the loser's owner renumbers it.
				w := nicBy[held]
				c.warnOnce("tag-dup:"+fmt.Sprint(cl.Port)+":"+nic.Key(), "port %d: %s keeps it, %s's claim %s is dropped and %s renumbers it (the lower VMID keeps a port two guests claim)", cl.Port, w.label(), nic.label(), t, nic.Node)
				continue
			}
			slotOf[nic.Key()] = cl.Port
			taken[cl.Port] = nic.Key()
			break
		}
	}
	free := func() int {
		for p := 1; p <= n; p++ {
			if _, used := taken[p]; !used {
				return p
			}
		}
		return 0
	}
	var unshown []string
	for _, nic := range nics {
		if _, ok := slotOf[nic.Key()]; ok {
			continue
		}
		p := free()
		if p == 0 {
			unshown = append(unshown, nic.Key())
			continue
		}
		slotOf[nic.Key()] = p
		taken[p] = nic.Key()
	}
	if len(unshown) > 0 {
		c.warnOnce("too-many-guests", "more guest NICs on %s than its %d guest ports (ports=%d): %v not shown", c.Bridge, n, c.Ports, unshown)
	}

	// The tags each guest on this node should carry for this bridge.
	type guest struct {
		kind string
		vmid int
	}
	want := map[guest][]string{}
	current := map[guest][]string{}
	var order []guest
	for _, nic := range nics {
		if nic.Node != host {
			continue
		}
		g := guest{nic.Kind, nic.VMID}
		if _, seen := current[g]; !seen {
			current[g] = nic.Tags
			order = append(order, g)
		}
		if p, ok := slotOf[nic.Key()]; ok {
			want[g] = append(want[g], portClaim{Port: p, Scope: scope, Host: nic.Node, Bridge: c.Bridge, NIC: nic.Index}.String())
		}
	}
	var out []retag
	for _, g := range order {
		keep, mine := c.splitTags(current[g])
		if sameSet(mine, want[g]) {
			continue
		}
		out = append(out, retag{Kind: g.kind, VMID: g.vmid, Tags: append(keep, want[g]...)})
	}
	return slotOf, out
}

// splitTags separates this bridge's tags (well-formed for this bridge, or
// malformed "unifi." ones), lowercased, from the rest (the operator's own
// and other bridges'), which keep their order and case.
func (c *Collector) splitTags(tags []string) (keep, mine []string) {
	for _, t := range tags {
		cl, ok := parseClaim(t)
		if strings.HasPrefix(strings.ToLower(t), tagPrefix) && (!ok || cl.Bridge == c.Bridge) {
			mine = append(mine, strings.ToLower(t))
		} else {
			keep = append(keep, t)
		}
	}
	return keep, mine
}

// untagTemplates lists this node's templates that carry this bridge's
// tags. A template is not a port (build leaves it out of assignPorts), but
// every clone of it inherits its tags, and each clone would then claim its
// template's port; so the owner removes them, as it would replace them on
// a guest, keeping the operator's and other bridges' tags in order
// (docs/drivers/proxmox-on-node.md §5.4). templates are the NICs of
// templates, on any bridge: the tags are the guest's, not a NIC's.
func (c *Collector) untagTemplates(templates []guestNIC, host string) []retag {
	var out []retag
	seen := map[string]bool{}
	for _, n := range templates {
		g := fmt.Sprintf("%s/%d", n.Kind, n.VMID)
		if n.Node != host || seen[g] {
			continue
		}
		seen[g] = true
		keep, mine := c.splitTags(n.Tags)
		if len(mine) == 0 {
			continue
		}
		out = append(out, retag{Kind: n.Kind, VMID: n.VMID, Tags: keep})
	}
	return out
}

// label names a guest NIC in a log line: "VM 119", "VM 119 net1", "CT 200".
func (n guestNIC) label() string {
	kind := "VM"
	if n.Kind == "lxc" {
		kind = "CT"
	}
	if n.Index == 0 {
		return fmt.Sprintf("%s %d", kind, n.VMID)
	}
	return fmt.Sprintf("%s %d net%d", kind, n.VMID, n.Index)
}

func numberingName(scope string) string {
	if scope == "h" {
		return "node"
	}
	return "cluster"
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// writeTags rewrites the tag lists assignPorts asked for, on this node
// (the guest is local, so qm/pct set works). A failure (no quorum, a
// guest under a backup or migrate lock) is logged once, the next poll
// tries again, and the retries stay quiet until one succeeds, which is
// logged too; meanwhile the guest is already shown at its port.
func (c *Collector) writeTags(ctx context.Context, retags []retag) {
	// A guest that no longer needs a rewrite (retagged by hand, migrated
	// away) is no longer failing: a later failure is news again.
	c.mu.Lock()
	for g := range c.tagFailing {
		still := false
		for _, r := range retags {
			still = still || g == fmt.Sprintf("%s/%d", r.Kind, r.VMID)
		}
		if !still {
			delete(c.tagFailing, g)
		}
	}
	c.mu.Unlock()
	for _, r := range retags {
		g := fmt.Sprintf("%s/%d", r.Kind, r.VMID)
		bad := false
		for _, t := range r.Tags {
			if !pveTag.MatchString(t) {
				c.warnOnce("tag-invalid:"+g+":"+t, "not rewriting %s %d's tags: %q is not a valid Proxmox tag", r.Kind, r.VMID, t)
				bad = true
			}
		}
		if bad {
			continue
		}
		tool := "qm"
		if r.Kind == "lxc" {
			tool = "pct"
		}
		cmd := fmt.Sprintf("%s set %d --tags %s", tool, r.VMID, shellQuote(strings.Join(r.Tags, ";")))
		if len(r.Tags) == 0 {
			cmd = fmt.Sprintf("%s set %d --delete tags", tool, r.VMID) // the guest's last tag was ours
		}
		c.mu.Lock()
		failing := c.tagFailing[g]
		c.mu.Unlock()
		if !failing {
			c.Log.Printf("proxmox %s: %s", c.node, cmd)
		}
		out, err := c.r.Run(ctx, cmd, "")
		if err != nil {
			if !failing {
				c.Log.Printf("proxmox %s: tagging %s %d failed (retried every poll, logged again when it succeeds): %v", c.node, r.Kind, r.VMID, err)
				c.mu.Lock()
				if c.tagFailing == nil {
					c.tagFailing = map[string]bool{}
				}
				c.tagFailing[g] = true
				c.mu.Unlock()
			}
			continue
		}
		if failing {
			c.Log.Printf("proxmox %s: tagging %s %d succeeded: %s", c.node, r.Kind, r.VMID, cmd)
			c.mu.Lock()
			delete(c.tagFailing, g)
			c.mu.Unlock()
		}
		if s := strings.TrimSpace(out); s != "" && !strings.HasPrefix(s, "update ") {
			c.Log.Printf("proxmox %s: %s", c.node, truncate(s, 200))
		}
	}
}
