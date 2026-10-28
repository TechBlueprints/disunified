package sonic

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// ApplyAddress implements devicemodel.AddressController (§6.4): the
// controller's static IP Settings on the interface carrying the address the
// bridge dials (Vlan<native_vlan> on a switch managed in-band).
//
// SONiC keeps both addresses while the new one is added, so nothing is lost
// before it is proven: add the new address, the static default route when
// the gateway changes and the DNS servers; dial the switch at the new
// address with a fresh SSH runner (same user, key and known_hosts) and run
// `true`. Then, over the new address: turn on promote_secondaries for the
// interface, remove the old address, verify the new one survived, remove
// the stale default route, save -- and only then move the runner and the
// dial target over (persisted, so a restart keeps it). If the new address
// does not answer, it is removed again (with the route it brought) and the
// old one never went. If the finish batch fails, nothing is saved and the
// new address stays a pending dial target the runner falls back to.
//
// known_hosts: sshrun checks the host key by address, so the operator's
// known_hosts must carry the switch's key under the new address too (the
// key is the same; add a line for the new address, or a host pattern that
// covers both, before changing it). A dial that fails for that reason
// fails like any other and leaves the switch on its old address.
func (c *Collector) ApplyAddress(ctx context.Context, d devicemodel.AddressDesired) (bool, error) {
	if d.DHCP {
		return false, fmt.Errorf("sonic: the controller wants this switch on DHCP; a SONiC VLAN interface has no DHCP client, and a lease could not be confirmed by dialling it before the old address goes -- the switch keeps its static address; set it to static in the controller")
	}
	if d.IP == "" || d.PrefixLen <= 0 {
		return false, fmt.Errorf("sonic: static address without an IP and prefix length")
	}
	c.mu.Lock()
	snap, st, dial, redial := c.last, c.state, c.Dial, c.Redial
	c.mu.Unlock()
	if snap == nil || st == nil {
		return false, fmt.Errorf("sonic: no snapshot yet")
	}
	isDialled := map[string]bool{}
	for _, ip := range dialAddrs(dial) {
		isDialled[ip] = true
	}
	var cur devicemodel.IfAddress
	for _, a := range snap.System.Addresses {
		if isDialled[a.IP] {
			cur = a
			break
		}
	}
	if cur.Iface == "" {
		return false, fmt.Errorf("sonic: no interface carries the bridge's own target %s; not changing an address blind", dial)
	}
	oldGW := st.defaultRoute()
	gwChange := d.Gateway != "" && d.Gateway != oldGW
	var dnsAdd, dnsDel []string
	if len(d.DNS) > 0 {
		want := map[string]bool{}
		for _, s := range d.DNS {
			want[s] = true
			if !st.dns[s] {
				dnsAdd = append(dnsAdd, s)
			}
		}
		for s := range st.dns {
			if !want[s] {
				dnsDel = append(dnsDel, s)
			}
		}
		sort.Strings(dnsAdd)
		sort.Strings(dnsDel)
	}
	sameIP := cur.IP == d.IP
	if sameIP && cur.PrefixLen == d.PrefixLen && !gwChange && len(dnsAdd) == 0 && len(dnsDel) == 0 {
		return false, nil
	}
	if why := addressMovable(snap.System, st, cur); why != "" {
		c.warnOnce("address: "+why, "IP Settings not applied: %s", why)
		return false, fmt.Errorf("sonic: IP Settings not applied: %s", why)
	}
	if sameIP && cur.PrefixLen != d.PrefixLen {
		return false, fmt.Errorf("sonic: changing the prefix length of the address the bridge dials (%s/%d to /%d) would drop it; change the address instead", cur.IP, cur.PrefixLen, d.PrefixLen)
	}

	newAddr := fmt.Sprintf("%s/%d", d.IP, d.PrefixLen)
	oldAddr := fmt.Sprintf("%s/%d", cur.IP, cur.PrefixLen)
	var first []string
	if !sameIP {
		first = append(first, fmt.Sprintf("sudo config interface ip add %s %s", cur.Iface, newAddr))
	}
	if gwChange {
		first = append(first, "sudo config route add prefix 0.0.0.0/0 nexthop "+d.Gateway)
	}
	// Removals first: SONiC holds at most three nameservers ("The maximum
	// number (3) of nameservers exceeded", probed), so a replacement on a
	// full list must make room before it adds.
	for _, s := range dnsDel {
		first = append(first, "sudo config dns nameserver del "+s)
	}
	for _, s := range dnsAdd {
		first = append(first, "sudo config dns nameserver add "+s)
	}
	if sameIP {
		var stale []string
		if gwChange && oldGW != "" {
			stale = append(stale, "sudo config route del prefix 0.0.0.0/0 nexthop "+oldGW)
		}
		// The bridge's own address stays: one batch through the runner it has.
		cmds := append(append(first, stale...), "sudo config save -y")
		c.Log.Printf("sonic address: %s", strings.Join(cmds, " ; "))
		if err := c.runBatch(ctx, cmds); err != nil {
			return false, fmt.Errorf("sonic: gateway/DNS change: %w", err)
		}
		c.noteAddress(cur, d, oldGW, dnsAdd, dnsDel)
		return true, nil
	}

	c.Log.Printf("sonic address: %s", strings.Join(first, " ; "))
	if err := c.runBatch(ctx, first); err != nil {
		return false, fmt.Errorf("sonic: adding %s on %s: %w", newAddr, cur.Iface, err)
	}
	if redial == nil {
		return false, c.undoAddress(ctx, cur.Iface, newAddr, d.Gateway, gwChange, fmt.Errorf("this runner cannot be re-dialled"))
	}
	nr := redial(d.IP)
	if _, err := nr.Run(ctx, "true", ""); err != nil {
		_ = nr.Close()
		return false, c.undoAddress(ctx, cur.Iface, newAddr, d.Gateway, gwChange, fmt.Errorf("the switch does not answer at %s (%v; is its host key in known_hosts for that address?)", d.IP, err))
	}
	// Proven: finish through the new address. From here on a lost
	// connection must not strand the bridge on the old address, so the new
	// one is recorded as a pending dial target first.
	c.setPendingDial(d.IP)
	finish := moveFinish(cur.Iface, oldAddr, newAddr)
	if gwChange && oldGW != "" {
		finish = append(finish, "sudo config route del prefix 0.0.0.0/0 nexthop "+oldGW)
	}
	finish = append(finish, "sudo config save -y")
	c.Log.Printf("sonic address (via %s): %s", d.IP, strings.Join(finish, " ; "))
	if _, err := nr.Run(ctx, batchCommand, "set -e\n"+strings.Join(finish, "\n")+"\n"); err != nil {
		_ = nr.Close()
		return false, fmt.Errorf("sonic: the switch answers at %s, but removing %s did not leave %s on %s (%w); nothing was saved -- the bridge keeps %s and tries %s if that stops answering", d.IP, oldAddr, newAddr, cur.Iface, err, dial, d.IP)
	}
	c.adoptDial(d.IP, nr)
	c.noteAddress(cur, d, oldGW, dnsAdd, dnsDel)
	return true, nil
}

// addressMovable says why the address the bridge dials cannot be moved by
// this driver, "" if it can: it must be a static in-band address that
// CONFIG_DB carries on a VLAN, front-panel, PortChannel or loopback
// interface. A DHCP lease cannot be replaced before the bridge has proven
// the new address (and SONiC's VLAN interfaces have no DHCP client to hand
// it back to); eth0 is MGMT_INTERFACE, which holds one entry per address
// family -- an add replaces the dialled address instead of standing beside
// it -- and keeps its gateway in its own gwaddr field, not STATIC_ROUTE.
func addressMovable(sys devicemodel.System, st *swState, cur devicemodel.IfAddress) string {
	if sys.DHCP {
		return fmt.Sprintf("the bridge dials %s, a DHCP lease on %s; a lease cannot be replaced before the new address is proven -- give the switch a static in-band address (a Vlan interface) first", cur.IP, cur.Iface)
	}
	inband := false
	for _, p := range []string{"Vlan", "Ethernet", "PortChannel", "Loopback"} {
		if strings.HasPrefix(cur.Iface, p) {
			inband = true
		}
	}
	if !inband {
		return fmt.Sprintf("the bridge dials %s on %s, the management port (MGMT_INTERFACE holds one address, which an add replaces, and its own gateway); the driver moves in-band addresses only", cur.IP, cur.Iface)
	}
	if key := fmt.Sprintf("%s|%s/%d", cur.Iface, cur.IP, cur.PrefixLen); !st.ifaddrs[key] {
		return fmt.Sprintf("%s/%d on %s is not in CONFIG_DB (VLAN_INTERFACE, INTERFACE, PORTCHANNEL_INTERFACE, LOOPBACK_INTERFACE); not changing an address SONiC does not own", cur.IP, cur.PrefixLen, cur.Iface)
	}
	return ""
}

// moveFinish is the start of the batch run over the new address once it
// answers. Linux's promote_secondaries is 0 on SONiC, so deleting the
// primary address of a subnet deletes its secondaries too -- and the new
// address, added second in the same subnet, is a secondary: removing the
// old one took both, and the switch off the network (live, 2026-10-27).
// The sysctl is turned on for the interface first (sysctl is not on the
// admin user's PATH; the /proc file is written through sudo tee), and the
// new address is verified to have survived the removal before anything is
// saved; set -e stops the batch there if it did not.
func moveFinish(iface, oldAddr, newAddr string) []string {
	return []string{
		fmt.Sprintf("echo 1 | sudo tee /proc/sys/net/ipv4/conf/%s/promote_secondaries >/dev/null", iface),
		fmt.Sprintf("sudo config interface ip remove %s %s", iface, oldAddr),
		fmt.Sprintf("ip -4 -o addr show dev %s | grep -q ' %s '", iface, newAddr),
	}
}

// undoAddress removes the new address (and the route it brought) through
// the old runner, and returns why.
func (c *Collector) undoAddress(ctx context.Context, iface, newAddr, gw string, gwChange bool, why error) error {
	undo := []string{fmt.Sprintf("sudo config interface ip remove %s %s", iface, newAddr)}
	if gwChange {
		undo = append(undo, "sudo config route del prefix 0.0.0.0/0 nexthop "+gw)
	}
	c.Log.Printf("sonic address: %v; undoing: %s", why, strings.Join(undo, " ; "))
	if err := c.runBatch(ctx, undo); err != nil {
		return fmt.Errorf("sonic: %v; and removing %s again failed (%v) -- the switch carries both addresses", why, newAddr, err)
	}
	return fmt.Errorf("sonic: %v; the new address was removed again and the switch keeps its old one", why)
}

// noteAddress records an applied address in the snapshot and state, so the
// next reconcile before a collect finds nothing to do.
func (c *Collector) noteAddress(cur devicemodel.IfAddress, d devicemodel.AddressDesired, oldGW string, dnsAdd, dnsDel []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last != nil {
		snap := *c.last
		snap.System.Addresses = append([]devicemodel.IfAddress(nil), c.last.System.Addresses...)
		for i := range snap.System.Addresses {
			if snap.System.Addresses[i] == cur {
				snap.System.Addresses[i].IP, snap.System.Addresses[i].PrefixLen = d.IP, d.PrefixLen
			}
		}
		if d.Gateway != "" {
			snap.System.Gateway = d.Gateway
		}
		c.last = &snap
	}
	if c.state != nil {
		next := c.state.clone()
		if d.Gateway != "" && d.Gateway != oldGW {
			next.routes["default|0.0.0.0/0"] = map[string]string{"nexthop": d.Gateway}
		}
		for _, s := range dnsAdd {
			next.dns[s] = true
		}
		for _, s := range dnsDel {
			delete(next.dns, s)
		}
		delete(next.ifaddrs, fmt.Sprintf("%s|%s/%d", cur.Iface, cur.IP, cur.PrefixLen))
		next.ifaddrs[fmt.Sprintf("%s|%s/%d", cur.Iface, d.IP, d.PrefixLen)] = true
		c.state = next
	}
}
