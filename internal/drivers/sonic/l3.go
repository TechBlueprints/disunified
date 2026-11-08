package sonic

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// UniFi's Layer 3 switching (docs/drivers/sonic.md §11.13, captured and
// built 2026-11-07). A network routed on the switch is the switch's own
// address on the VLAN (VLAN_INTERFACE, `config interface ip add`), its
// routes are STATIC_ROUTE rows, which bgpcfgd hands to FRR (SONiC's
// separated routing mode), its DHCP relay is the VLAN's dhcp_servers list
// (`config vlan dhcp_relay`, dhcprelayd), and its DHCP server is busybox
// udhcpd in a transient unit of the driver's own. When the first network
// is routed on a switch the controller creates "Inter-VLAN routing" itself,
// a transit network to the gateway whose route is the switch's default.
// That route shares the STATIC_ROUTE default row with the management
// gateway the IP Settings own (address.go): the transit hop goes beside it
// and the management hop drops to distance l3MgmtDistance, a floating
// fallback, so the IP Settings keep working and a transit that is taken
// away hands the default back.
//
// Ownership is the managed record (l3Managed): the driver removes only
// addresses, routes, relays and servers it added, so the switch's own
// in-band address -- the one the bridge reaches it on -- is never touched.

const (
	// l3MgmtDistance is the management gateway's administrative distance
	// while a routed network's transit carries the default route.
	l3MgmtDistance = "200"
	// l3Interfaces / l3Routes are what the switch claims (max_l3_intf,
	// max_static_routes), the ECS's numbers.
	l3Interfaces = 64
	l3Routes     = 256

	dhcpdDir        = "/etc/sonic/disunified-dhcpd"
	dhcpdLeases     = "/var/lib/disunified-dhcpd"
	dhcpdUnitPrefix = "disunified-dhcpd-"
)

// l3Managed is what the driver added for routed networks.
type l3Managed struct {
	Addrs  []string `json:"addrs,omitempty"`  // "<iface>|<a.b.c.d/n>"
	Routes []string `json:"routes,omitempty"` // prefixes whose STATIC_ROUTE row is the driver's
	Hop    string   `json:"hop,omitempty"`    // the transit next hop in the default row
	Relays []int    `json:"relays,omitempty"` // SONiC VLAN ids whose dhcp_servers the driver set
	DHCPd  []string `json:"dhcpd,omitempty"`  // interfaces running the driver's DHCP server
}

func (m l3Managed) empty() bool {
	return len(m.Addrs) == 0 && len(m.Routes) == 0 && m.Hop == "" && len(m.Relays) == 0 && len(m.DHCPd) == 0
}

func (m l3Managed) clone() l3Managed {
	return l3Managed{
		Addrs: append([]string(nil), m.Addrs...), Routes: append([]string(nil), m.Routes...), Hop: m.Hop,
		Relays: append([]int(nil), m.Relays...), DHCPd: append([]string(nil), m.DHCPd...),
	}
}

// bgpOn: the image runs FRR (FEATURE|bgp enabled), which installs
// STATIC_ROUTE.
func (s *swState) bgpOn() bool { return s.bgp == "enabled" || s.bgp == "always_enabled" }

// l3Set records the routing the push asked for, the target of every
// reconcile after it.
func (c *Collector) l3Set(d devicemodel.DeviceDesired) {
	if !d.L3Set {
		return
	}
	dd := d
	c.mu.Lock()
	c.l3 = &dd
	c.mu.Unlock()
}

// routeHop is one next hop of a STATIC_ROUTE row: its entries in the row's
// comma lists.
type routeHop struct{ nexthop, ifname, distance, blackhole, vrf string }

// hopsOf splits a STATIC_ROUTE row into its hops; a short list reads as "".
func hopsOf(f map[string]string) []routeHop {
	if f == nil {
		return nil
	}
	split := func(k string) []string { return strings.Split(f[k], ",") }
	nh, ifn, dist, bh, vrf := split("nexthop"), split("ifname"), split("distance"), split("blackhole"), split("nexthop-vrf")
	n := max(len(nh), len(ifn))
	if f["nexthop"] == "" && f["ifname"] == "" {
		n = len(bh)
	}
	get := func(l []string, i int) string {
		if i < len(l) {
			return strings.TrimSpace(l[i])
		}
		return ""
	}
	out := make([]routeHop, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, routeHop{get(nh, i), get(ifn, i), get(dist, i), get(bh, i), get(vrf, i)})
	}
	return out
}

// rowOf is the STATIC_ROUTE fields for hops, every list the same length
// (bgpcfgd refuses a row whose lists differ).
func rowOf(hops []routeHop) map[string]string {
	var nh, ifn, dist, bh, vrf []string
	for _, h := range hops {
		d, b := h.distance, h.blackhole
		if d == "" {
			d = "0"
		}
		if b == "" {
			b = "false"
		}
		nh, ifn, dist, bh, vrf = append(nh, h.nexthop), append(ifn, h.ifname), append(dist, d), append(bh, b), append(vrf, h.vrf)
	}
	j := func(l []string) string { return strings.Join(l, ",") }
	return map[string]string{"nexthop": j(nh), "ifname": j(ifn), "distance": j(dist), "blackhole": j(bh), "nexthop-vrf": j(vrf)}
}

// sameRow compares the five fields bgpcfgd reads.
func sameRow(a, b map[string]string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.DeepEqual(hopsNorm(a), hopsNorm(b))
}

func hopsNorm(f map[string]string) []routeHop {
	h := hopsOf(f)
	for i := range h {
		if h[i].distance == "" {
			h[i].distance = "0"
		}
		if h[i].blackhole == "" {
			h[i].blackhole = "false"
		}
	}
	return h
}

func routeKey(prefix string) string { return "STATIC_ROUTE|default|" + prefix }

func hsetRoute(prefix string, row map[string]string) string {
	q := sshrun.ShellQuote
	return fmt.Sprintf("sonic-db-cli CONFIG_DB hset %s nexthop %s ifname %s distance %s blackhole %s nexthop-vrf %s",
		q(routeKey(prefix)), q(row["nexthop"]), q(row["ifname"]), q(row["distance"]), q(row["blackhole"]), q(row["nexthop-vrf"]))
}

// l3Iface is the SONiC interface of a UniFi VLAN.
func (c *Collector) l3Iface(vlan int) string {
	if vlan == 1 {
		return "Vlan" + strconv.Itoa(c.NativeVLAN)
	}
	return "Vlan" + strconv.Itoa(vlan)
}

// connected is each in-band interface's subnets (CONFIG_DB's addresses).
func connected(ifaddrs map[string]bool) map[string][]*net.IPNet {
	out := map[string][]*net.IPNet{}
	for k := range ifaddrs {
		iface, cidr, _ := strings.Cut(k, "|")
		if _, n, err := net.ParseCIDR(cidr); err == nil && n.IP.To4() != nil {
			out[iface] = append(out[iface], n)
		}
	}
	return out
}

// ifaceFor is the interface whose subnet holds ip, "" if none.
func ifaceFor(ip net.IP, subnets map[string][]*net.IPNet) string {
	names := make([]string, 0, len(subnets))
	for n := range subnets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, s := range subnets[n] {
			if s.Contains(ip) {
				return n
			}
		}
	}
	return ""
}

// carried: the interface reaches a port with link -- a VLAN any of whose
// member ports (or PortChannels) has carrier, or a routed port with
// carrier itself.
func carried(iface string, st *swState) bool {
	id, isVlan := strings.CutPrefix(iface, "Vlan")
	if !isVlan {
		return st.carrier[iface]
	}
	v := atoi(id)
	for port, vl := range st.members {
		if _, in := vl[v]; in && st.carrier[port] {
			return true
		}
	}
	return false
}

// udhcpdConf is a routed network's udhcpd configuration.
func udhcpdConf(iface string, in devicemodel.L3Interface) (string, string) {
	_, n, err := net.ParseCIDR(in.Address)
	if err != nil {
		return "", "address " + in.Address
	}
	lo, hi := net.ParseIP(in.DHCP.Start).To4(), net.ParseIP(in.DHCP.End).To4()
	if lo == nil || hi == nil || !n.Contains(lo) || !n.Contains(hi) {
		return "", fmt.Sprintf("range %s-%s is not inside %s", in.DHCP.Start, in.DHCP.End, n)
	}
	size := int(ipU32(hi)) - int(ipU32(lo)) + 1
	if size < 1 {
		return "", fmt.Sprintf("range %s-%s is empty", in.DHCP.Start, in.DHCP.End)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# disunified: DHCP server of UniFi network %q (docs/drivers/sonic.md 11.13)\n", in.Name)
	fmt.Fprintf(&b, "interface %s\nstart %s\nend %s\nmax_leases %d\n", iface, lo, hi, size)
	fmt.Fprintf(&b, "lease_file %s/%s.leases\nauto_time 300\n", dhcpdLeases, iface)
	fmt.Fprintf(&b, "option subnet %s\n", net.IP(n.Mask).String())
	if in.DHCP.Router != "" {
		fmt.Fprintf(&b, "option router %s\n", in.DHCP.Router)
	}
	if len(in.DHCP.DNS) > 0 {
		fmt.Fprintf(&b, "option dns %s\n", strings.Join(in.DHCP.DNS, " "))
	}
	if in.DHCP.LeaseSecs > 0 {
		fmt.Fprintf(&b, "option lease %d\n", in.DHCP.LeaseSecs)
	}
	return b.String(), ""
}

func ipU32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// dhcpdOp starts (Conf set) or stops (Conf "") one interface's DHCP server.
type dhcpdOp struct {
	Iface string `json:"iface"`
	Conf  string `json:"conf,omitempty"`
}

// dhcpdScript runs as root: for each op it writes the config and
// (re)starts the transient unit, or stops it and removes the config. The
// lease file is kept. Restart=always brings udhcpd back after a crash;
// after a reboot the next reconcile starts it again.
const dhcpdScript = `import json, os, subprocess, sys
D, L, U = "` + dhcpdDir + `", "` + dhcpdLeases + `", "` + dhcpdUnitPrefix + `"
os.makedirs(D, exist_ok=True)
os.makedirs(L, exist_ok=True)
def sc(*a):
    subprocess.run(["systemctl"] + list(a), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
for op in json.loads(sys.argv[2]):
    i = op["iface"]
    unit, conf = U + i, os.path.join(D, i + ".conf")
    sc("stop", unit)
    sc("reset-failed", unit)
    if not op.get("conf"):
        if os.path.exists(conf):
            os.remove(conf)
        continue
    with open(conf + ".tmp", "w") as f:
        f.write(op["conf"])
    os.replace(conf + ".tmp", conf)
    lease = os.path.join(L, i + ".leases")
    if not os.path.exists(lease):
        open(lease, "a").close()
    subprocess.run(["systemd-run", "--unit=" + unit, "--property=Restart=always", "--property=RestartSec=5",
                    "/usr/bin/busybox", "udhcpd", "-f", conf], check=True, stdout=subprocess.DEVNULL)
`

var dhcpdCommand = "sudo python3 -c " + sshrun.ShellQuote("import base64; exec(base64.b64decode('"+base64.StdEncoding.EncodeToString([]byte(dhcpdScript))+"'))") + " disunified-dhcpd"

// applyL3 plans the routed networks against the switch and runs the plan
// as one batch (addresses, VLANs, relays, routes, save), then the DHCP
// servers. Returns how many writes ran.
func (c *Collector) applyL3(ctx context.Context) (int, error) {
	c.mu.Lock()
	st, d, m := c.state, c.l3, c.managedL3.clone()
	c.mu.Unlock()
	if st == nil || d == nil {
		return 0, nil
	}
	if !st.bgpOn() {
		if len(d.L3Interfaces) > 0 {
			c.warnOnce("l3 no frr", "sonic: L3 routing requested, but FEATURE|bgp is not enabled (no FRR to install routes); not written")
		}
		return 0, nil
	}
	pl := c.planL3(st, *d, m)
	for _, n := range pl.notes {
		c.warnOnce("l3: "+n, "sonic: L3 routing: %s", n)
	}
	changed := 0
	if len(pl.ops) > 0 {
		cmds := make([]string, 0, len(pl.ops)+1)
		for _, o := range pl.ops {
			cmds = append(cmds, o.cmd)
		}
		cmds = append(cmds, "sudo config save -y")
		c.Log.Printf("sonic apply L3: %s", strings.Join(cmds, " ; "))
		if err := c.runBatch(ctx, cmds); err != nil {
			return 0, fmt.Errorf("sonic apply L3: %w", err)
		}
		changed += len(pl.ops)
	}
	if len(pl.dhcpd) > 0 {
		b, _ := json.Marshal(pl.dhcpd)
		var names []string
		for _, o := range pl.dhcpd {
			verb := "start"
			if o.Conf == "" {
				verb = "stop"
			}
			names = append(names, verb+" "+o.Iface)
		}
		c.Log.Printf("sonic apply L3: DHCP servers: %s", strings.Join(names, ", "))
		if _, err := c.run(ctx, dhcpdCommand+" "+sshrun.ShellQuote(string(b)), ""); err != nil {
			return changed, fmt.Errorf("sonic apply L3: DHCP servers: %w", err)
		}
		changed += len(pl.dhcpd)
	}
	c.mu.Lock()
	if c.state == st {
		c.state = pl.sim
	}
	before := c.managedL3
	c.managedL3 = pl.managed
	c.mu.Unlock()
	if !reflect.DeepEqual(before, pl.managed) {
		if err := c.saveManaged(); err != nil {
			c.Log.Printf("sonic: keeping %s: %v (the bridge's L3 ownership is held in memory)", managedFile, err)
		}
	}
	return changed, nil
}

// l3Plan is planL3's result.
type l3Plan struct {
	ops     []deviceOp
	dhcpd   []dhcpdOp
	sim     *swState
	managed l3Managed
	notes   []string
}

func (c *Collector) planL3(st *swState, d devicemodel.DeviceDesired, m l3Managed) (pl l3Plan) {
	sim := st.clone()
	pl = l3Plan{sim: sim}
	// Three phases (live, 2026-11-07): `config interface ip remove` refuses
	// the last address of an interface while FRR still lists a static route
	// through it ("A static ip route is still bound to the RIF"; it greps
	// `show ip route vrf all static` for the name), and FRR drops a deleted
	// STATIC_ROUTE row only once bgpcfgd has passed it on. So removals of
	// routes and relays go first, then a wait until FRR no longer names
	// each interface losing its address, then those addresses, then
	// everything added.
	const (
		unbind = iota
		strip
		build
	)
	var phases [3][]deviceOp
	var stripped []string
	add := func(phase int, cmd string, eff func(*swState)) {
		phases[phase] = append(phases[phase], deviceOp{cmd: "sudo " + cmd, logged: "sudo " + cmd, effect: eff})
		if eff != nil {
			eff(sim)
		}
	}
	defer func() {
		pl.ops = append(pl.ops, phases[unbind]...)
		for _, iface := range stripped {
			w := rifWait(iface)
			pl.ops = append(pl.ops, deviceOp{cmd: w, logged: w})
		}
		pl.ops = append(append(pl.ops, phases[strip]...), phases[build]...)
	}()
	note := func(format string, args ...any) { pl.notes = append(pl.notes, fmt.Sprintf(format, args...)) }
	ours := map[string]bool{}
	for _, a := range m.Addrs {
		ours[a] = true
	}

	// The routed interfaces. An interface carrying an address the driver
	// did not add (the switch's own management address) is not the
	// driver's to route: UniFi's Default network belongs to the gateway.
	type routed struct {
		in    devicemodel.L3Interface
		iface string
		key   string
	}
	var want []routed
	wantAddr := map[string]bool{}
	for _, in := range d.L3Interfaces {
		iface := c.l3Iface(in.VLAN)
		key := iface + "|" + in.Address
		foreign := ""
		for k := range sim.ifaddrs {
			if strings.HasPrefix(k, iface+"|") && !ours[k] && k != key {
				foreign = strings.TrimPrefix(k, iface+"|")
			}
		}
		if foreign != "" {
			note("network %q (VLAN %d): %s carries %s, which the driver did not add (the switch's own address); not routed there", in.Name, in.VLAN, iface, foreign)
			continue
		}
		want = append(want, routed{in, iface, key})
		wantAddr[key] = true
	}
	// Removals first: SONiC's promote_secondaries is 0, so removing the
	// primary address of a subnet takes a new one in it along (address.go).
	for _, k := range m.Addrs {
		if wantAddr[k] || !sim.ifaddrs[k] {
			continue
		}
		iface, cidr, _ := strings.Cut(k, "|")
		key := k
		if !containsString(stripped, iface) {
			stripped = append(stripped, iface)
		}
		// The CLI runs vtysh to check the last address of an interface, and
		// vtysh would read the rest of the batch (live, 2026-11-07).
		add(strip, "config interface ip remove "+iface+" "+cidr+" </dev/null", func(s *swState) { delete(s.ifaddrs, key) })
	}
	for _, r := range want {
		if id := r.in.VLAN; id != 1 && !sim.vlans[id] {
			add(build, "config vlan add "+strconv.Itoa(id), func(s *swState) { s.vlans[id] = true })
		}
		if !sim.ifaddrs[r.key] {
			key := r.key
			add(build, "config interface ip add "+r.iface+" "+r.in.Address, func(s *swState) { s.ifaddrs[key] = true })
		}
	}
	for k := range wantAddr {
		pl.managed.Addrs = append(pl.managed.Addrs, k)
	}
	sort.Strings(pl.managed.Addrs)

	// DHCP relay: the VLAN's dhcp_servers list is the push's; a VLAN the
	// driver relayed on and no longer routes, or no longer relays on,
	// loses its servers. One add and one del per VLAN (each restarts the
	// dhcp_relay service).
	wantRelay := map[int][]string{}
	for _, r := range want {
		if r.in.DHCP.Mode == "relay" {
			id := atoi(strings.TrimPrefix(r.iface, "Vlan"))
			wantRelay[id] = r.in.DHCP.Relays
		}
	}
	relayVLANs := map[int]bool{}
	for _, id := range m.Relays {
		relayVLANs[id] = true
	}
	for id := range wantRelay {
		relayVLANs[id] = true
	}
	for _, id := range sortedInts(relayVLANs) {
		cur, w := sim.vlanRelay[id], wantRelay[id]
		var del, plus []string
		for _, s := range cur {
			if !containsString(w, s) {
				del = append(del, s)
			}
		}
		for _, s := range w {
			if !containsString(cur, s) {
				plus = append(plus, s)
			}
		}
		vid := id
		if len(del) > 0 && sim.vlans[id] {
			add(unbind, "config vlan dhcp_relay del "+strconv.Itoa(id)+" "+strings.Join(del, " "), func(s *swState) {
				var keep []string
				for _, x := range s.vlanRelay[vid] {
					if !containsString(del, x) {
						keep = append(keep, x)
					}
				}
				s.vlanRelay[vid] = keep
			})
		}
		if len(plus) > 0 {
			add(build, "config vlan dhcp_relay add "+strconv.Itoa(id)+" "+strings.Join(plus, " "), func(s *swState) {
				s.vlanRelay[vid] = append(s.vlanRelay[vid], plus...)
			})
		}
		if len(w) > 0 {
			pl.managed.Relays = append(pl.managed.Relays, id)
		}
	}

	// Routes. Every route but the default is a row of the driver's own
	// (several routes to one prefix are its hops). The default route's
	// next hop is the transit gateway, used only while the transit VLAN
	// reaches a port with link: a transit nobody carries would take the
	// switch's own traffic off the LAN into nothing.
	subnets := connected(sim.ifaddrs)
	rows := map[string][]routeHop{}
	transit := ""
	for _, r := range d.StaticRoutes {
		h := routeHop{}
		switch {
		case r.Blackhole:
			h.blackhole = "true"
		case r.VLAN != 0:
			h.ifname = c.l3Iface(r.VLAN)
		default:
			h.nexthop = r.NextHop
		}
		if r.Prefix == "0.0.0.0/0" {
			if h.nexthop == "" {
				note("route %q: a default route without a next hop is not written", r.Name)
				continue
			}
			if transit == "" {
				transit = h.nexthop
			}
			continue
		}
		dup := false
		for _, x := range rows[r.Prefix] {
			dup = dup || x == h
		}
		if !dup {
			rows[r.Prefix] = append(rows[r.Prefix], h)
		}
	}
	if transit != "" {
		iface := ifaceFor(net.ParseIP(transit), subnets)
		switch {
		case iface == "":
			note("default route via %s: no routed network holds it; the switch keeps its management gateway", transit)
			transit = ""
		case !carried(iface, sim):
			note("default route via %s: %s reaches no port with link (allow the Inter-VLAN routing network on the uplink); the switch keeps its management gateway until it does", transit, iface)
			transit = ""
		}
	}
	for _, p := range m.Routes {
		if _, keep := rows[p]; keep {
			continue
		}
		if _, ok := sim.routes["default|"+p]; ok {
			prefix := p
			add(unbind, "sonic-db-cli CONFIG_DB del "+sshrun.ShellQuote(routeKey(p)), func(s *swState) { delete(s.routes, "default|"+prefix) })
		}
	}
	for _, p := range sortedKeysOf(rows) {
		row := rowOf(rows[p])
		pl.managed.Routes = append(pl.managed.Routes, p)
		if sameRow(sim.routes["default|"+p], row) {
			continue
		}
		prefix := p
		add(build, hsetRoute(p, row), func(s *swState) { s.routes["default|"+prefix] = row })
	}
	// The default row: the management hops (every hop but the driver's
	// transit), demoted while a transit carries the default, restored
	// otherwise.
	cur := sim.routes["default|0.0.0.0/0"]
	var mgmt []routeHop
	for _, h := range hopsOf(cur) {
		if h.nexthop != "" && h.nexthop == m.Hop {
			continue
		}
		if h.nexthop != "" && h.nexthop == transit {
			// The transit gateway is the management gateway: the
			// default already goes there.
			transit = ""
		}
		mgmt = append(mgmt, h)
	}
	var next []routeHop
	for _, h := range mgmt {
		switch {
		case transit != "":
			h.distance = l3MgmtDistance
		case h.distance == l3MgmtDistance:
			h.distance = "0"
		}
		next = append(next, h)
	}
	if transit != "" {
		next = append(next, routeHop{nexthop: transit, distance: "0", blackhole: "false"})
		pl.managed.Hop = transit
	}
	switch {
	case len(next) == 0 && cur != nil && m.Hop != "":
		add(unbind, "sonic-db-cli CONFIG_DB del "+sshrun.ShellQuote(routeKey("0.0.0.0/0")), func(s *swState) { delete(s.routes, "default|0.0.0.0/0") })
	case len(next) > 0 && !sameRow(cur, rowOf(next)):
		// The management gateway back on its own goes before a transit
		// address leaves; the transit hop added waits for its address.
		row, phase := rowOf(next), build
		if transit == "" {
			phase = unbind
		}
		add(phase, hsetRoute("0.0.0.0/0", row), func(s *swState) { s.routes["default|0.0.0.0/0"] = row })
	}
	sim.l3Hop = pl.managed.Hop

	// DHCP servers: a config per interface, the unit restarted when it
	// changes or is not running; the driver's other servers stop.
	wantDHCPd := map[string]string{}
	for _, r := range want {
		if r.in.DHCP.Mode != "server" {
			continue
		}
		conf, why := udhcpdConf(r.iface, r.in)
		if why != "" {
			note("network %q: DHCP server not started: %s", r.in.Name, why)
			continue
		}
		wantDHCPd[r.iface] = conf
	}
	if len(wantDHCPd) > 0 && !sim.dhcpdKnown {
		note("DHCP servers requested, but the collector reported no dhcpd section (an older collect.py); not started")
		wantDHCPd = map[string]string{}
	}
	stop := map[string]bool{}
	for _, i := range m.DHCPd {
		stop[i] = true
	}
	for i := range sim.dhcpd {
		stop[i] = true
	}
	for _, i := range sortedKeysOf(wantDHCPd) {
		conf := wantDHCPd[i]
		pl.managed.DHCPd = append(pl.managed.DHCPd, i)
		delete(stop, i)
		u := sim.dhcpd[i]
		if u.sum == sha256Hex(conf) && u.active == "active" {
			continue
		}
		pl.dhcpd = append(pl.dhcpd, dhcpdOp{Iface: i, Conf: conf})
		sim.dhcpd[i] = dhcpdUnit{active: "active", sum: sha256Hex(conf)}
	}
	for _, i := range sortedKeysOf(stop) {
		if _, running := sim.dhcpd[i]; !running {
			continue
		}
		pl.dhcpd = append(pl.dhcpd, dhcpdOp{Iface: i})
		delete(sim.dhcpd, i)
	}
	return pl
}

// rifWait waits (up to 30 s, never failing) until FRR lists no static
// route through iface: the condition `config interface ip remove` checks
// before it removes an interface's last address. vtysh reads stdin, which
// in a batch is the rest of the batch: it gets /dev/null.
func rifWait(iface string) string {
	script := fmt.Sprintf(`for i in $(seq 1 60); do vtysh -c "show ip route vrf all static" </dev/null | grep -qw %s || exit 0; sleep 0.5; done; exit 0`, iface)
	return "sh -c " + sshrun.ShellQuote(script) + " disunified-rif-wait"
}

func sortedInts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isolationRules are the DUI_IP_ACL rules of the routed networks UniFi
// isolates (Isolate Network): every packet between an isolated network
// and another network the switch is connected to is dropped, both ways,
// below the controller's own rules. The transit to the gateway stays open
// (its gateway is the networks' way out and their DNS server), and so
// does the switch's own address in the isolated network (DHCP relay
// replies come to it from a server elsewhere).
func (c *Collector) isolationRules(d devicemodel.DeviceDesired, st *swState) map[string]map[string]string {
	c.mu.Lock()
	hop := c.managedL3.Hop
	c.mu.Unlock()
	var transitIP net.IP
	for _, r := range d.StaticRoutes {
		if r.Prefix == "0.0.0.0/0" && r.NextHop != "" {
			transitIP = net.ParseIP(r.NextHop)
			break
		}
	}
	if transitIP == nil && hop != "" {
		transitIP = net.ParseIP(hop)
	}
	nets := map[string]*net.IPNet{}
	for _, list := range connected(st.ifaddrs) {
		for _, n := range list {
			nets[n.String()] = n
		}
	}
	type iso struct {
		self string
		net  *net.IPNet
	}
	var isolated []iso
	for _, in := range d.L3Interfaces {
		ip, n, err := net.ParseCIDR(in.Address)
		if err != nil || ip.To4() == nil {
			continue
		}
		nets[n.String()] = n
		if in.Isolated {
			isolated = append(isolated, iso{ip.String() + "/32", n})
		}
	}
	if len(isolated) == 0 {
		return nil
	}
	rules := map[string]map[string]string{}
	k := 0
	next := func(r map[string]string) {
		k++
		r["PRIORITY"] = strconv.Itoa(ipACLTop - ipACLRules - k)
		r["IP_TYPE"] = "IPV4ANY"
		rules["ISO_"+strconv.Itoa(k)] = r
	}
	for _, s := range isolated {
		next(map[string]string{"PACKET_ACTION": "FORWARD", "DST_IP": s.self})
	}
	for _, s := range isolated {
		for _, key := range sortedKeysOf(nets) {
			o := nets[key]
			if o.String() == s.net.String() || (transitIP != nil && o.Contains(transitIP)) {
				continue
			}
			next(map[string]string{"PACKET_ACTION": "DROP", "SRC_IP": s.net.String(), "DST_IP": o.String()})
			next(map[string]string{"PACKET_ACTION": "DROP", "SRC_IP": o.String(), "DST_IP": s.net.String()})
		}
	}
	return rules
}
