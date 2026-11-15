package sonic

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// A VLAN the controller stops pushing (docs/drivers/sonic.md §11.29).
//
// The controller's push names a VLAN only while something on the device
// uses it: a port carrying it (untagged, tagged, a voice VLAN, every
// network under Allow All) or a network the switch routes (its
// switch.routing_interface, the Inter-VLAN transit's VLAN among them).
// A network deleted from the site, a port set from Allow All to Block All,
// a tagged set that drops it, the transit of a removed routed network: in
// each the VLAN leaves the push. The ECS follows its push and removes such
// a VLAN with its members (Vlan4040, 2026-11-15, two minutes after the
// transit went). So does the driver, as UniFi's list replaces the device's
// for NTP and syslog: once the port plan, the IGMP rows and the routed
// network's address have gone, a VLAN no push names is deleted.
//
// Kept: the native_vlan stand-in (UniFi's VLAN 1), every VLAN the device's
// push or an attached lane switch's push names, the VLAN of every network
// the switch routes, and any VLAN still holding what the driver did not
// take out: a member (a port the bridge does not write, a lane a lane
// switch owns, a PortChannel outside the plan), an address (the
// management address, a hand-made one), a VLAN_INTERFACE row bound to a
// VRF or running IPv6 link-local, a DHCP relay, an IGMP row the IGMP
// apply leaves (options.igmp off). Those are what UniFi does not express,
// and SONiC's CLI would refuse most of them anyway. (An MLD snooping row,
// which collect.py does not read, makes the CLI refuse the delete: the
// batch fails with its message.)
//
// Nothing is deleted from a list this run has not been given: the device's
// push (EnsureVLANs), each attached lane switch's, and the routing intent
// where the image routes.

// pruneVLANs deletes the VLANs no push names any more, as one batch.
func (c *Collector) pruneVLANs(ctx context.Context) (int, error) {
	del, notes := c.planPrune()
	for _, n := range notes {
		c.warnOnce("prune: "+n, "sonic: %s", n)
	}
	if len(del) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	st := c.state
	c.mu.Unlock()
	cmd := "sudo config vlan del " + strconv.Itoa(del[0])
	if len(del) > 1 {
		cmd = "sudo config vlan del -m " + joinInts(del)
	}
	cmds := []string{cmd, "sudo config save -y"}
	c.Log.Printf("sonic: deleting VLANs %v, which the controller no longer pushes: %s", del, strings.Join(cmds, " ; "))
	if err := c.runBatch(ctx, cmds); err != nil {
		return 0, fmt.Errorf("sonic delete vlans %v: %w", del, err)
	}
	c.mu.Lock()
	if c.state == st {
		next := st.clone()
		for _, id := range del {
			next.dropVLAN(id)
		}
		c.state = next
	}
	c.mu.Unlock()
	return len(del), nil
}

// planPrune is the VLANs pruneVLANs would delete, ascending, and why each
// other candidate stays.
func (c *Collector) planPrune() ([]int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.state
	if st == nil || !c.siteKnown {
		return nil, nil
	}
	keep := map[int]bool{c.NativeVLAN: true}
	for _, id := range c.siteVLANs {
		keep[id] = true
	}
	var cages []int
	for cage := range c.children {
		cages = append(cages, cage)
	}
	sort.Ints(cages)
	for _, cage := range cages {
		l, ok := c.laneVLANs[cage]
		if !ok {
			return nil, []string{fmt.Sprintf("the lane switch on cage %d has not applied a push yet; no VLAN is deleted until it has (its lanes may carry one the device's push does not name)", cage)}
		}
		for _, id := range l {
			keep[id] = true
		}
	}
	if c.l3 != nil {
		for _, id := range c.routedVLANs() {
			keep[id] = true
		}
	} else if st.bgpOn() {
		return nil, nil // the routing intent is not known yet (ApplyDevice runs first)
	}
	var del []int
	var notes []string
	for _, id := range sortedInts(st.vlans) {
		if keep[id] {
			continue
		}
		if why := st.vlanHeld(id); why != "" {
			notes = append(notes, fmt.Sprintf("VLAN %d is no longer in the controller's push, but %s; left on the switch", id, why))
			continue
		}
		del = append(del, id)
	}
	return del, notes
}

// routedVLANs is the SONiC VLAN ids of the networks the last push routes
// on a VLAN (not a routed port or aggregate), ascending; UniFi's VLAN 1 is
// the native stand-in. Called with c.mu held.
func (c *Collector) routedVLANs() []int {
	if c.l3 == nil {
		return nil
	}
	seen := map[int]bool{}
	var ids []int
	for _, in := range c.l3.L3Interfaces {
		if in.Port > 0 || in.LAG > 0 || in.VLAN < 1 {
			continue
		}
		id := in.VLAN
		if id == 1 {
			id = c.NativeVLAN
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// vlanHeld says what keeps a VLAN on the switch whatever the push says:
// something the driver's plans did not take out of it. "" = nothing.
func (s *swState) vlanHeld(id int) string {
	vlan := "Vlan" + strconv.Itoa(id)
	var members []string
	for iface, m := range s.members {
		if _, ok := m[id]; ok {
			members = append(members, iface)
		}
	}
	if len(members) > 0 {
		sort.Strings(members)
		return fmt.Sprintf("%s still carries it (a port or PortChannel the bridge does not write)", strings.Join(members, ","))
	}
	for k := range s.ifaddrs {
		if strings.HasPrefix(k, vlan+"|") {
			return fmt.Sprintf("%s has an address the driver did not add (%s)", vlan, strings.TrimPrefix(k, vlan+"|"))
		}
	}
	for f := range s.vlanIfRows[vlan] {
		if f != "NULL" {
			return fmt.Sprintf("its VLAN_INTERFACE row carries %s", f)
		}
	}
	if len(s.vlanRelay[id]) > 0 {
		return "it has a DHCP relay the driver did not set"
	}
	for k := range s.l2mc {
		if parts := strings.Split(k, "|"); len(parts) > 1 && parts[1] == vlan {
			return fmt.Sprintf("an IGMP snooping row names it (%s; the driver writes IGMP only with options.igmp on)", parts[0])
		}
	}
	return ""
}

// dropVLAN is `config vlan del`'s effect on the plan's copy of CONFIG_DB.
func (s *swState) dropVLAN(id int) {
	delete(s.vlans, id)
	delete(s.vlanMTU, id)
	delete(s.vlanRelay, id)
	delete(s.netMTU, "Vlan"+strconv.Itoa(id))
	for iface, m := range s.members {
		delete(m, id)
		if len(m) == 0 {
			delete(s.members, iface)
		}
	}
}
