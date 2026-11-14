package sonic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// IPv6 on the management interface (docs/drivers/sonic.md §11.22).
//
// A UniFi switch takes an IPv6 address from the gateway's router
// advertisements (SLAAC) on its management interface whenever the network
// runs IPv6. A SONiC switch never does in-band: SONiC turns forwarding on
// for every router interface (net.ipv6.conf.all.forwarding=1), and Linux
// ignores RAs on a forwarding interface unless accept_ra is 2 -- SONiC's
// own sysctl files set up RAs for eth0 only. So the driver sets accept_ra
// on the interface carrying the address it is managed at (ntpSource) to 2
// while the controller runs IPv6 on the management network, and back to
// SONiC's 1 when it does not.
//
// The address alone is not enough. orchagent programs an interface
// address's ip2me route (the address -> CPU, through the ip2me CoPP group,
// 6000 packets/s) only from an APPL_DB INTF_TABLE row, which intfmgrd
// writes for CONFIG_DB addresses only. A SLAAC address without one reaches
// the CPU through the default trap at the default group's 600 packets/s:
// measured on the DX010, an upload to it ran at 0.9 MB/s, and with the row
// at IPv4's ~8.8 MB/s (both at the ip2me group's rate, back to back). So each SLAAC address gets a row of the driver's
// own (ProducerStateTable, like portfeat.go's), and a row for an address
// the kernel no longer has goes. The /64 itself needs nothing: FRR hands
// the connected route to orchagent through fpmsyncd, as it does the RA's
// default route.
//
// Off undoes all of it: the rows, accept_ra, the SLAAC addresses (`ip -6
// addr flush ... scope global dynamic`), the RA's default route (`proto
// ra`) and its on-link prefix routes, which outlive the address (seen on
// the DX010: a /64 with an expiry and `proto kernel` stayed after the
// flush). An address CONFIG_DB carries, its row and its prefix route (no
// expiry) are never touched.
//
// The kernel half is lost on a reboot and the APPL_DB rows on a swss
// restart; the loop re-applies every cycle.

// v6State is collect.py's ipv6_ra and appl_intf sections and the dynamic
// global IPv6 addresses of `ip -j addr`.
type v6State struct {
	// known: collect.py printed ipv6_ra (captures before 2026-11-14 lack it).
	known bool
	// acceptRA is net.ipv6.conf.<netdev>.accept_ra per in-band netdev.
	acceptRA map[string]string
	// routes are the routes the kernel learned from RAs: the default
	// (protocol "ra") and the on-link prefixes (an expiry).
	routes []raRoute
	// slaac are each netdev's dynamic global IPv6 addresses, "<addr>/<len>".
	slaac map[string][]string
	// rows are APPL_DB INTF_TABLE's IPv6 address rows by interface,
	// "<addr>/<len>".
	rows map[string]map[string]bool
}

type raRoute struct {
	Dst      string `json:"dst"`
	Dev      string `json:"dev"`
	Protocol string `json:"protocol"`
}

func (v v6State) clone() v6State {
	c := v6State{known: v.known, acceptRA: copyFields(v.acceptRA), routes: append([]raRoute(nil), v.routes...),
		slaac: map[string][]string{}, rows: map[string]map[string]bool{}}
	for k, l := range v.slaac {
		c.slaac[k] = append([]string(nil), l...)
	}
	for k, m := range v.rows {
		c.rows[k] = copySet(m)
	}
	return c
}

// v6Prefix normalises "<addr>/<len>" (IPv6 only): "" for anything else.
func v6Prefix(s string) string {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() {
		return ""
	}
	return p.String()
}

// parseV6 reads the sections; known stays false without ipv6_ra.
func parseV6(raBody string, intf table, addrs []ipAddr) v6State {
	v := v6State{acceptRA: map[string]string{}, slaac: map[string][]string{}, rows: map[string]map[string]bool{}}
	var sec struct {
		AcceptRA map[string]string `json:"accept_ra"`
		Routes   []raRoute         `json:"routes"`
	}
	if body := strings.TrimSpace(raBody); body != "" && body != "error" && json.Unmarshal([]byte(body), &sec) == nil && sec.AcceptRA != nil {
		v.known = true
		v.acceptRA = sec.AcceptRA
		v.routes = sec.Routes
	}
	for _, a := range addrs {
		for _, ai := range a.AddrInfo {
			if ai.Family != "inet6" || ai.Scope != "global" || !ai.Dynamic {
				continue
			}
			if p := v6Prefix(fmt.Sprintf("%s/%d", ai.Local, ai.PrefixLen)); p != "" {
				v.slaac[a.Ifname] = append(v.slaac[a.Ifname], p)
			}
		}
	}
	for k := range intf {
		rest, ok := strings.CutPrefix(k, "INTF_TABLE:")
		if !ok {
			continue
		}
		iface, addr, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		if p := v6Prefix(addr); p != "" {
			if v.rows[iface] == nil {
				v.rows[iface] = map[string]bool{}
			}
			v.rows[iface][p] = true
		}
	}
	return v
}

// acceptRACmd sets an interface's accept_ra (no sysctl on admin's PATH,
// as for promote_secondaries).
func acceptRACmd(iface, val string) string {
	return "echo " + val + " | sudo tee /proc/sys/net/ipv6/conf/" + iface + "/accept_ra >/dev/null"
}

// planIPv6 is the batch that brings iface to on (RAs taken, an ip2me row
// per SLAAC address) or off (none of it), and the state after it.
// configured are CONFIG_DB's in-band addresses ("<iface>|<addr>/<len>"),
// which are intfmgrd's and never the driver's.
func planIPv6(v v6State, iface string, on bool, configured map[string]bool) (cmds, logged []string, next v6State) {
	next = v.clone()
	cfg := map[string]bool{}
	for k := range configured {
		if i, a, ok := strings.Cut(k, "|"); ok && i == iface {
			if p := v6Prefix(a); p != "" {
				cfg[p] = true
			}
		}
	}
	var appl []applOp
	setRow := func(p string) {
		appl = append(appl, applOp{Op: "set", Table: "INTF_TABLE", Key: iface + ":" + p, FV: []string{"scope", "global", "family", "IPv6"}})
		if next.rows[iface] == nil {
			next.rows[iface] = map[string]bool{}
		}
		next.rows[iface][p] = true
	}
	delRow := func(p string) {
		appl = append(appl, applOp{Op: "del", Table: "INTF_TABLE", Key: iface + ":" + p})
		delete(next.rows[iface], p)
	}
	flushAppl := func() {
		if len(appl) == 0 {
			return
		}
		b, _ := json.Marshal(appl)
		cmds = append(cmds, applCommand+" "+sshrun.ShellQuote(string(b)))
		var what []string
		for _, o := range appl {
			what = append(what, o.Op+" INTF_TABLE:"+o.Key)
		}
		logged = append(logged, applLogged+" ("+strings.Join(what, ", ")+")")
		appl = nil
	}
	add := func(cmd string) {
		cmds = append(cmds, cmd)
		logged = append(logged, cmd)
	}
	have := map[string]bool{}
	for _, p := range v.slaac[iface] {
		have[p] = true
	}
	rows := sortedSet(v.rows[iface])

	if on {
		if v.acceptRA[iface] != "2" {
			add(acceptRACmd(iface, "2"))
			next.acceptRA[iface] = "2"
		}
		for _, p := range v.slaac[iface] {
			if !v.rows[iface][p] && !cfg[p] {
				setRow(p)
			}
		}
		for _, p := range rows {
			if !have[p] && !cfg[p] {
				delRow(p)
			}
		}
		flushAppl()
		return cmds, logged, next
	}

	// Off: the chip's routes first, then the kernel's learning, then what
	// it learned.
	for _, p := range rows {
		if !cfg[p] {
			delRow(p)
		}
	}
	flushAppl()
	if v.acceptRA[iface] == "2" {
		add(acceptRACmd(iface, "1"))
		next.acceptRA[iface] = "1"
	}
	if len(v.slaac[iface]) > 0 {
		add("sudo ip -6 addr flush dev " + iface + " scope global dynamic")
		delete(next.slaac, iface)
	}
	var keep []raRoute
	flushRA := false
	var prefixes []string
	for _, r := range v.routes {
		switch {
		case r.Dev != iface:
			keep = append(keep, r)
		case r.Protocol == "ra":
			flushRA = true
		default:
			prefixes = append(prefixes, r.Dst)
		}
	}
	if flushRA {
		add("sudo ip -6 route flush dev " + iface + " proto ra")
	}
	sort.Strings(prefixes)
	for _, d := range prefixes {
		add("sudo ip -6 route del " + d + " dev " + iface)
	}
	next.routes = keep
	return cmds, logged, next
}

// ApplyManagementIPv6 implements devicemodel.ManagementIPv6Controller:
// on, the interface the bridge manages the switch at takes the gateway's
// router advertisements and each SLAAC address gets its ip2me row; off,
// all of that goes.
func (c *Collector) ApplyManagementIPv6(ctx context.Context, on bool) (int, error) {
	unlock, err := c.lockApply(ctx)
	if err != nil {
		return 0, fmt.Errorf("sonic: IPv6: %w", err)
	}
	defer unlock()
	c.mu.Lock()
	st, snap, dial := c.state, c.last, c.Dial
	c.mu.Unlock()
	if st == nil {
		return 0, fmt.Errorf("sonic: no snapshot yet")
	}
	if !st.v6.known {
		c.warnOnce("ipv6-section", "IPv6 on the management network not applied: the collector printed no ipv6_ra section")
		return 0, nil
	}
	iface := ntpSource(snap, st, dial)
	if iface == "" {
		c.warnOnce("ipv6-iface", "IPv6 on the management network not applied: the address the bridge manages the switch at (%s) is on no in-band interface CONFIG_DB carries", dial)
		return 0, nil
	}
	if _, ok := st.v6.acceptRA[iface]; !ok {
		c.warnOnce("ipv6-netdev "+iface, "IPv6 on the management network not applied: no IPv6 settings for %s in the kernel", iface)
		return 0, nil
	}
	cmds, logged, next := planIPv6(st.v6, iface, on, st.ifaddrs)
	if len(cmds) == 0 {
		return 0, nil
	}
	why := "no IPv6 on the management network"
	if on {
		why = "the management network runs IPv6"
	}
	c.Log.Printf("sonic ipv6 %s (%s): %s", iface, why, strings.Join(logged, " ; "))
	if err := c.runBatch(ctx, cmds); err != nil {
		return 0, fmt.Errorf("sonic: IPv6 on %s: %w", iface, err)
	}
	c.mu.Lock()
	if c.state == st {
		c.state.v6 = next
	}
	c.mu.Unlock()
	return len(cmds), nil
}
