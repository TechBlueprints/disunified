package aristaeos

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// addressSession is the configuration session an address change is made in.
// A session lets the whole change land at once and, with a commit timer, be
// undone by the switch itself if the bridge never confirms it.
const addressSession = "dui-address"

// commitTimer is how long the switch waits for confirmation before reverting
// an address change. Long enough for a fresh eAPI dial and one command; short
// enough that a wrong address does not keep the switch unreachable.
const commitTimer = "00:02:00"

// ApplyAddress sets the switch's own management address from the
// controller's IP Settings. Only a static setting reaches here (the loop
// never forwards the DHCP default), and it goes to the interface carrying
// the address the bridge itself connects through.
//
// An address change severs the bridge's own connection, so it is made in a
// configuration session committed with a timer (`commit timer`, verified on
// 4.26.14M): the switch applies it at once and reverts it unless confirmed.
// The driver then dials the switch at the new address, and only if `show
// version` answers there does it confirm the session, persist, and switch
// its own transport over. If the new address does not answer, it returns an
// error and leaves the timer to put the old address back.
//
// It is a diff against the running configuration, so re-sending what the
// switch already has does nothing.
func (c *Collector) ApplyAddress(ctx context.Context, d devicemodel.AddressDesired) (bool, error) {
	if d.DHCP {
		return false, nil // never applied: see the loop
	}
	if d.IP == "" || d.PrefixLen <= 0 {
		return false, fmt.Errorf("arista-eos: static address without an IP and prefix length")
	}
	c.mu.Lock()
	last, host, dns := c.last, c.host, append([]string(nil), c.nameServers...)
	c.mu.Unlock()
	if last == nil {
		return false, fmt.Errorf("arista-eos: no snapshot yet")
	}
	iface, cur := managementInterface(last, host)
	if iface == "" {
		return false, fmt.Errorf("arista-eos: no interface carries the bridge's own target %s; not changing an address blind", host)
	}
	wantDNS := append([]string(nil), d.DNS...)
	sort.Strings(wantDNS)
	sameDNS := len(wantDNS) == 0 || strings.Join(wantDNS, ",") == strings.Join(dns, ",")
	if cur.IP == d.IP && cur.PrefixLen == d.PrefixLen && (d.Gateway == "" || d.Gateway == last.System.Gateway) && sameDNS {
		return false, nil
	}

	cmds := []string{"enable", "configure session " + addressSession,
		"interface " + iface, fmt.Sprintf("ip address %s/%d", d.IP, d.PrefixLen), "exit"}
	if d.Gateway != "" && d.Gateway != last.System.Gateway {
		if last.System.Gateway != "" {
			cmds = append(cmds, "no ip route 0.0.0.0/0 "+last.System.Gateway)
		}
		cmds = append(cmds, "ip route 0.0.0.0/0 "+d.Gateway)
	}
	if !sameDNS {
		for _, s := range dns {
			cmds = append(cmds, "no ip name-server vrf default "+s)
		}
		for _, s := range wantDNS {
			cmds = append(cmds, "ip name-server vrf default "+s)
		}
	}
	cmds = append(cmds, "commit timer "+commitTimer)
	c.logConfig(cmds)
	if err := c.t.Configure(ctx, cmds); err != nil {
		return false, fmt.Errorf("arista-eos: address change: %w", err)
	}

	// Confirm through whichever transport reaches the switch after the
	// change. Same address: the current one. New address: a fresh dial that
	// must answer first, or the switch reverts on its own.
	confirmVia := c.t
	if d.IP != cur.IP {
		if c.redial == nil {
			return false, fmt.Errorf("arista-eos: address changed to %s in session %s, but this transport cannot be re-dialled to confirm it; the switch will revert in %s", d.IP, addressSession, commitTimer)
		}
		nt := c.redial(d.IP)
		if _, err := nt.Run(ctx, []string{"enable", "show version"}); err != nil {
			_ = nt.Close()
			return false, fmt.Errorf("arista-eos: the switch does not answer at %s (%v); leaving session %s to revert in %s", d.IP, err, addressSession, commitTimer)
		}
		confirmVia = nt
	}
	confirm := []string{"enable", "configure session " + addressSession + " commit", "write memory"}
	c.logConfig(confirm)
	if err := confirmVia.Configure(ctx, confirm); err != nil {
		if confirmVia != c.t {
			_ = confirmVia.Close()
		}
		return false, fmt.Errorf("arista-eos: confirm session %s: %w", addressSession, err)
	}
	c.mu.Lock()
	if confirmVia != c.t {
		_ = c.t.Close()
		c.t, c.host = confirmVia, d.IP
	}
	if d.Gateway != "" {
		last.System.Gateway = d.Gateway
	}
	if !sameDNS {
		c.nameServers = wantDNS
	}
	for i := range last.System.Addresses {
		if last.System.Addresses[i].Iface == iface {
			last.System.Addresses[i].IP, last.System.Addresses[i].PrefixLen = d.IP, d.PrefixLen
		}
	}
	c.mu.Unlock()
	return true, nil
}

// managementInterface is the interface whose primary address is the one the
// bridge connects to, with that address.
func managementInterface(snap *devicemodel.Snapshot, host string) (string, devicemodel.IfAddress) {
	for _, a := range snap.System.Addresses {
		if a.IP == host {
			return a.Iface, a
		}
	}
	return "", devicemodel.IfAddress{}
}

// addressingFromRunningConfig reads the switch's default gateway, its name
// servers, and whether the management interface takes its address from
// DHCP, out of the structured running configuration.
func addressingFromRunningConfig(rc showRunningConfig, mgmtIface string) (gateway string, dns []string, dhcp bool) {
	for k, sec := range rc.Cmds {
		switch {
		case strings.HasPrefix(k, "ip route 0.0.0.0/0 "):
			gateway = strings.Fields(strings.TrimPrefix(k, "ip route 0.0.0.0/0 "))[0]
		case strings.HasPrefix(k, "ip name-server "):
			f := strings.Fields(k)
			dns = append(dns, f[len(f)-1])
		case k == "interface "+mgmtIface && sec != nil:
			for line := range sec.Cmds {
				if strings.HasPrefix(line, "ip address dhcp") {
					dhcp = true
				}
			}
		}
	}
	sort.Strings(dns)
	return gateway, dns, dhcp
}

// logConfig echoes a config batch to the driver's log, the way ApplyPorts does.
func (c *Collector) logConfig(cmds []string) {
	if c.Log != nil {
		c.Log.Printf("arista: %s", strings.Join(cmds, " / "))
	}
}
