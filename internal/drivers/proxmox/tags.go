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
	for _, nic := range nics {
		for _, t := range nic.Tags {
			cl, ok := parseClaim(t)
			if !ok || cl.Bridge != c.Bridge || cl.NIC != nic.Index || cl.Scope != scope || cl.Port > n {
				continue
			}
			if scope == "h" && cl.Host != nic.Node {
				continue // tagged for another node's switch: it migrated
			}
			if _, dup := taken[cl.Port]; dup {
				continue // the lower VMID keeps the port
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
		c.warnOnce("too-many-guests", "more guest NICs on %s than guest ports (ports=%d, uplink_ports=%d): %v not shown", c.Bridge, c.Ports, c.UplinkPorts, unshown)
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
		var keep, mine []string
		for _, t := range current[g] {
			cl, ok := parseClaim(t)
			switch {
			case strings.HasPrefix(strings.ToLower(t), tagPrefix) && (!ok || cl.Bridge == c.Bridge):
				mine = append(mine, strings.ToLower(t)) // ours: replaced below
			default:
				keep = append(keep, t) // the operator's, or another bridge's
			}
		}
		if sameSet(mine, want[g]) {
			continue
		}
		out = append(out, retag{Kind: g.kind, VMID: g.vmid, Tags: append(keep, want[g]...)})
	}
	return slotOf, out
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
// (the guest is local, so qm/pct set works). A failure is logged and the
// next poll tries again; meanwhile the guest is already shown at its port.
func (c *Collector) writeTags(ctx context.Context, retags []retag) {
	for _, r := range retags {
		bad := false
		for _, t := range r.Tags {
			if !pveTag.MatchString(t) {
				c.Log.Printf("proxmox %s: not rewriting %s %d's tags: %q is not a valid Proxmox tag", c.node, r.Kind, r.VMID, t)
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
		c.Log.Printf("proxmox %s: %s", c.node, cmd)
		if out, err := c.r.Run(ctx, cmd, ""); err != nil {
			c.Log.Printf("proxmox %s: tagging %s %d failed (retried on the next poll): %v", c.node, r.Kind, r.VMID, err)
		} else if s := strings.TrimSpace(out); s != "" && !strings.HasPrefix(s, "update ") {
			c.Log.Printf("proxmox %s: %s", c.node, truncate(s, 200))
		}
	}
}
