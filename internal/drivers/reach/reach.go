// Package reach holds a bridged device's identity and reachability when the
// device cannot report them itself: the MAC it is adopted under, its network
// mask, its gateway and the gateway's L2 address (the fields the controller
// reads when it places a device, docs/topology-placement.md), and the address
// it is reported at -- the dialled literal, or what a dialled name resolves
// to now. It is not a driver: it registers nothing. apc-ups introduced it,
// apc-backups and stub take the same options.
//
// The netmask and gateway come from the site's configuration (the
// controller's network list, through SiteNetwork) and the gateway's L2
// address from the bridge's own neighbour table, read only when the bridge
// has an address on the device's subnet. The options `netmask`, `gateway`
// and `gateway_mac` override either (docs/drivers/apc-ups.md §14).
package reach

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Identity is what the device cannot say, from the operator, the site and
// the bridge's own view of the segment.
type Identity struct {
	// Addr is the address dialled (host or host:port), reported as the
	// device's own so the controller has somewhere to place it; MAC is the
	// device's MAC when the operator supplied it.
	Addr string
	MAC  string
	// Netmask, Gateway and GatewayMAC are the operator's overrides of the
	// device's network: the controller places a device by its netmask and
	// gateway MAC (a device without them has no parent in the topology --
	// the lesson that cost a day on the Arista). Empty = derived.
	Netmask    string
	Gateway    string
	GatewayMAC string
	// Resolve turns the dialled host into addresses when it is a name rather
	// than a literal (nil = the system resolver). Dialling the name a lease
	// is published under follows the lease; the resolved address is what is
	// reported.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	// SiteNetwork is the site's network for an address (the bridge sets it
	// from the controller's network list; nil = none, the options are all
	// there is).
	SiteNetwork func(ctx context.Context, ip string) (devicemodel.SiteNetwork, error)
	// Neighbours reads the bridge's own IPv4 neighbour table, address ->
	// MAC, complete entries only (nil = /proc/net/arp); LocalAddrs the
	// bridge's own addresses (nil = net.InterfaceAddrs); Nudge makes the
	// kernel resolve an address it has no entry for (nil = one UDP datagram
	// to the discard port).
	Neighbours func() (map[string]string, error)
	LocalAddrs func() ([]net.Addr, error)
	Nudge      func(ip string)
	// Driver prefixes the log lines ("apc-ups").
	Driver string

	mu       sync.Mutex
	resolved string                             // last address the name resolved to, for change logging
	site     map[string]devicemodel.SiteNetwork // last good site network per address
	gwMACs   map[string]string                  // last neighbour entry per gateway address
	nudged   map[string]time.Time
	said     map[string]string // log line key -> last value logged, so each state is logged once
}

// SetOptions takes the options `mac`, `netmask`, `gateway` and
// `gateway_mac`, and the address dialled.
func (id *Identity) SetOptions(addr string, opts map[string]string) {
	id.Addr = addr
	id.MAC = strings.ToLower(strings.TrimSpace(opts["mac"]))
	id.Netmask = strings.TrimSpace(opts["netmask"])
	id.Gateway = strings.TrimSpace(opts["gateway"])
	id.GatewayMAC = strings.ToLower(strings.TrimSpace(opts["gateway_mac"]))
}

// Fill reports the identity into sys: the MAC when the operator gave one
// (sys.MAC is left for the driver's own fallback otherwise), the address
// with its prefix, the gateway and the gateway's L2 address. A name that
// stops resolving leaves the address unreported rather than stale.
func (id *Identity) Fill(ctx context.Context, sys *devicemodel.System) {
	if id.MAC != "" {
		sys.MAC = id.MAC
	}
	optMAC := strings.ToLower(id.GatewayMAC)
	sys.GatewayMAC = optMAC
	sys.Gateway = id.Gateway
	ip := id.address(ctx)
	if ip == "" {
		return
	}
	site := id.network(ctx, ip)
	prefix := PrefixLen(id.Netmask)
	if prefix == 0 {
		prefix = site.PrefixLen
	} else if site.PrefixLen != 0 && site.PrefixLen != prefix {
		id.sayOnce("netmask", fmt.Sprintf("/%d/%d", prefix, site.PrefixLen), "options.netmask %s overrides the site network's /%d for %s", id.Netmask, site.PrefixLen, ip)
	}
	gw := id.Gateway
	if gw == "" {
		gw = site.Gateway
	} else if site.Gateway != "" && site.Gateway != gw {
		id.sayOnce("gateway", gw+"/"+site.Gateway, "options.gateway %s overrides the site network's gateway %s for %s", gw, site.Gateway, ip)
	}
	sys.Gateway = gw
	seen := id.segmentMAC(ip, prefix, gw)
	switch {
	case optMAC == "":
		sys.GatewayMAC = seen
	case seen != "" && seen != optMAC:
		id.sayOnce("gateway_mac", optMAC+"/"+seen, "options.gateway_mac %s overrides %s, the gateway %s as the bridge's neighbour table has it", optMAC, seen, gw)
	}
	sys.Addresses = []devicemodel.IfAddress{{Iface: "eth0", IP: ip, PrefixLen: prefix}}
}

// GatewayFor is the gateway to give the device when it takes ip: the
// operator's option, else the gateway of the site's network for ip ("" when
// neither says). A driver writing a static address the controller pushed
// without a route takes it from here (the power path's push has none,
// CLAUDE.md §5).
func (id *Identity) GatewayFor(ctx context.Context, ip string) string {
	if id.Gateway != "" {
		return id.Gateway
	}
	return id.network(ctx, ip).Gateway
}

// address is the dialled host, resolved when it is a name; "" when it does
// not resolve.
func (id *Identity) address(ctx context.Context) string {
	host, _, err := net.SplitHostPort(id.Addr)
	if err != nil {
		host = id.Addr
	}
	if host == "" || net.ParseIP(host) != nil {
		return host
	}
	resolve := id.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, h string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, h)
			if err != nil {
				return nil, err
			}
			out := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				out = append(out, a.IP)
			}
			return out, nil
		}
	}
	ips, err := resolve(ctx, host)
	ip := ""
	for _, a := range ips {
		if v4 := a.To4(); v4 != nil {
			ip = v4.String()
			break
		}
	}
	if err != nil || ip == "" {
		log.Printf("%s: %s does not resolve to an IPv4 address (%v); address unreported this cycle", id.Driver, host, err)
		return ""
	}
	id.mu.Lock()
	if id.resolved != "" && id.resolved != ip {
		log.Printf("%s: %s now resolves to %s (was %s): following the lease", id.Driver, host, ip, id.resolved)
	}
	id.resolved = ip
	id.mu.Unlock()
	return ip
}

// network is the site's network for ip. A failed read keeps the last good
// answer for that address: the controller being briefly unreachable is no
// reason to report a device without its netmask.
func (id *Identity) network(ctx context.Context, ip string) devicemodel.SiteNetwork {
	if id.SiteNetwork == nil {
		return devicemodel.SiteNetwork{}
	}
	n, err := id.SiteNetwork(ctx, ip)
	id.mu.Lock()
	last, had := id.site[ip]
	if err == nil && n.PrefixLen > 0 {
		if id.site == nil {
			id.site = map[string]devicemodel.SiteNetwork{}
		}
		id.site[ip] = n
	}
	id.mu.Unlock()
	switch {
	case err != nil && had:
		id.sayOnce("site", "error", "the site's network for %s cannot be read (%v); keeping /%d via %s", ip, err, last.PrefixLen, last.Gateway)
		return last
	case err != nil:
		id.sayOnce("site", "error", "the site's network for %s cannot be read (%v); only the options say what it is", ip, err)
		return devicemodel.SiteNetwork{}
	}
	id.sayOnce("site", fmt.Sprintf("%s/%d", n.Gateway, n.PrefixLen), "%s is on the site network /%d via %s (the controller's network list)", ip, n.PrefixLen, n.Gateway)
	return n
}

// segmentMAC is the gateway's L2 address as the device's segment sees it:
// the bridge's own neighbour entry for the gateway, read only when the
// bridge has an address on the device's subnet, since a bridge on another
// segment knows its own gateway, not the device's. "" when the bridge is not
// on that segment or has not resolved the gateway yet (it is nudged to, and
// the next collect reads it); the last entry seen is kept while one goes
// missing.
func (id *Identity) segmentMAC(ip string, prefix int, gw string) string {
	gwIP := net.ParseIP(gw).To4()
	if gwIP == nil || prefix <= 0 || prefix > 32 {
		return ""
	}
	subnet := &net.IPNet{IP: net.ParseIP(ip).To4(), Mask: net.CIDRMask(prefix, 32)}
	if subnet.IP == nil || !subnet.Contains(gwIP) {
		return ""
	}
	subnet.IP = subnet.IP.Mask(subnet.Mask)
	localAddrs := id.LocalAddrs
	if localAddrs == nil {
		localAddrs = net.InterfaceAddrs
	}
	addrs, err := localAddrs()
	on := false
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && subnet.Contains(n.IP) {
			on = true
			break
		}
	}
	if err != nil || !on {
		id.sayOnce("segment", "off", "the bridge has no address on %s: the gateway's L2 address there cannot be read here, so the device reports options.gateway_mac or none", subnet)
		return ""
	}
	neighbours := id.Neighbours
	if neighbours == nil {
		neighbours = ProcNetARP
	}
	table, err := neighbours()
	if err != nil {
		id.sayOnce("neighbours", err.Error(), "the bridge's neighbour table cannot be read (%v): the gateway's L2 address comes from options.gateway_mac or is not reported", err)
	} else {
		id.sayOnce("neighbours", "", "%s", "the bridge's neighbour table is readable again")
	}
	mac := table[gw]
	id.mu.Lock()
	defer id.mu.Unlock()
	if id.gwMACs == nil {
		id.gwMACs, id.nudged = map[string]string{}, map[string]time.Time{}
	}
	if mac != "" {
		id.gwMACs[gw] = mac
		return mac
	}
	if time.Since(id.nudged[gw]) >= time.Minute {
		id.nudged[gw] = time.Now()
		nudge := id.Nudge
		if nudge == nil {
			nudge = udpNudge
		}
		go nudge(gw)
	}
	return id.gwMACs[gw]
}

// sayOnce logs a line the first time key takes value.
func (id *Identity) sayOnce(key, value, format string, args ...any) {
	id.mu.Lock()
	if id.said == nil {
		id.said = map[string]string{}
	}
	same := id.said[key] == value
	id.said[key] = value
	id.mu.Unlock()
	if !same {
		log.Printf(id.Driver+": "+format, args...)
	}
}

// ProcNetARP reads the kernel's IPv4 neighbour table, complete entries only
// (flag ATF_COM, 0x2); the format is docs/fixtures/bridge-linux-6.12/proc-net-arp.txt.
func ProcNetARP() (map[string]string, error) {
	raw, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return nil, err
	}
	return ParseProcNetARP(raw), nil
}

// ParseProcNetARP parses /proc/net/arp: a header line, then address, HW
// type, flags, HW address, mask, device.
func ParseProcNetARP(raw []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for first := true; sc.Scan(); first = false {
		f := strings.Fields(sc.Text())
		if first || len(f) < 4 {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(f[2], "0x"), 16, 32)
		if err != nil || flags&0x2 == 0 || net.ParseIP(f[0]).To4() == nil {
			continue
		}
		if hw, err := net.ParseMAC(f[3]); err == nil && len(hw) == 6 {
			out[f[0]] = hw.String()
		}
	}
	return out
}

// udpNudge sends one datagram to the discard port, which makes the kernel
// resolve the address first.
func udpNudge(ip string) {
	conn, err := net.DialTimeout("udp4", net.JoinHostPort(ip, "9"), time.Second)
	if err != nil {
		return
	}
	_, _ = conn.Write([]byte{0})
	_ = conn.Close()
}

// PrefixLen turns a dotted netmask into a prefix length; 0 when absent or
// unparseable, which the payload reports as no netmask.
func PrefixLen(mask string) int {
	ip := net.ParseIP(mask)
	if ip == nil || ip.To4() == nil {
		return 0
	}
	ones, _ := net.IPMask(ip.To4()).Size()
	return ones
}
