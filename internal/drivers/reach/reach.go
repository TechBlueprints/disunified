// Package reach holds a bridged device's identity and reachability when the
// device cannot report them itself: the MAC it is adopted under, its network
// mask and the gateway's L2 address (the fields the controller reads when it
// places a device, docs/topology-placement.md), and the address it is
// reported at -- the dialled literal, or what a dialled name resolves to now.
// It is not a driver: it registers nothing. apc-ups introduced the options
// (`mac`, `netmask`, `gateway_mac`); stub takes them the same way.
package reach

import (
	"context"
	"log"
	"net"
	"strings"
	"sync"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Identity is the operator's word for what the device cannot say.
type Identity struct {
	// Addr is the address dialled (host or host:port), reported as the
	// device's own so the controller has somewhere to place it; MAC is the
	// device's MAC when the operator supplied it.
	Addr string
	MAC  string
	// Netmask and GatewayMAC are the device's network as the operator knows
	// it: the controller places a device by its netmask and gateway MAC (a
	// device without them has no parent in the topology -- the lesson that
	// cost a day on the Arista).
	Netmask    string
	GatewayMAC string
	// Resolve turns the dialled host into addresses when it is a name rather
	// than a literal (nil = the system resolver). Dialling the name a lease
	// is published under follows the lease; the resolved address is what is
	// reported.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	// Driver prefixes the log lines ("apc-ups").
	Driver string

	mu       sync.Mutex
	resolved string // last address the name resolved to, for change logging
}

// SetOptions takes the options `mac`, `netmask` and `gateway_mac`, and the
// address dialled.
func (id *Identity) SetOptions(addr string, opts map[string]string) {
	id.Addr = addr
	id.MAC = strings.ToLower(strings.TrimSpace(opts["mac"]))
	id.Netmask = strings.TrimSpace(opts["netmask"])
	id.GatewayMAC = strings.ToLower(strings.TrimSpace(opts["gateway_mac"]))
}

// Fill reports the identity into sys: the MAC when the operator gave one
// (sys.MAC is left for the driver's own fallback otherwise), the gateway's
// L2 address, and the address. A name that stops resolving leaves the
// address unreported rather than stale.
func (id *Identity) Fill(ctx context.Context, sys *devicemodel.System) {
	if id.MAC != "" {
		sys.MAC = id.MAC
	}
	sys.GatewayMAC = strings.ToLower(id.GatewayMAC)
	host, _, err := net.SplitHostPort(id.Addr)
	if err != nil {
		host = id.Addr
	}
	if host == "" {
		return
	}
	ip := host
	if net.ParseIP(host) == nil {
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
		ip = ""
		for _, a := range ips {
			if v4 := a.To4(); v4 != nil {
				ip = v4.String()
				break
			}
		}
		if err != nil || ip == "" {
			log.Printf("%s: %s does not resolve to an IPv4 address (%v); address unreported this cycle", id.Driver, host, err)
			return
		}
		id.mu.Lock()
		if id.resolved != "" && id.resolved != ip {
			log.Printf("%s: %s now resolves to %s (was %s): following the lease", id.Driver, host, ip, id.resolved)
		}
		id.resolved = ip
		id.mu.Unlock()
	}
	sys.Addresses = []devicemodel.IfAddress{{Iface: "eth0", IP: ip, PrefixLen: PrefixLen(id.Netmask)}}
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
