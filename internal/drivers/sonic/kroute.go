package sonic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// The kernel's IPv4 default route (docs/drivers/sonic.md §6.4.1).
//
// The default route the driver configures (STATIC_ROUTE, address.go and
// l3.go) reaches the kernel through zebra. On 202511 zebra installs routes
// through kernel nexthop objects (`zebra nexthop kernel enable`, the
// template's default since sonic-buildimage #25117; 202505 forced it off,
// #23459). When an interface loses carrier, the kernel deletes every
// nexthop object on it and every route using one, and tells nobody (6.12's
// nexthop_flush_dev passes no netlink info: no RTM_DELNEXTHOP, no
// RTM_DELROUTE). zebra re-creates the nexthop objects when the interface
// comes back (zebra_interface_nhg_reinstall) but not the routes, unless it
// re-resolved them while the interface was down. On the DX010 the kernel
// Bridge lost carrier for 90 ms at boot, when disunified-l2d started
// spanning tree (every port blocking until the dummy port, an admin edge,
// forwarded), 6 s after zebra had installed the default: FRR reported it
// selected, installed and offloaded for 11 h while the kernel had none and
// the switch reached nothing off its own subnet (2026-11-15). Upstream FRR
// has the quick flap open (FRRouting/frr#20747).
//
// Two writes, both idempotent:
//
//   - The cause: DEVICE_METADATA|localhost zebra_nexthop disabled, SONiC's
//     own switch for it (zebra.conf.j2 renders `no zebra nexthop kernel
//     enable` at the bgp container's start), saved; and the same line in
//     the running zebra, which takes it at once for every route it installs
//     after. A route with inline nexthops survives a carrier loss in the
//     kernel (marked linkdown, cleared on carrier up), whether or not zebra
//     sees the flap.
//   - The repair: when FRR selects and installs 0.0.0.0/0 and the kernel's
//     main table has no default through any of its nexthops on two collects
//     in a row, or still has it through a nexthop object once zebra no
//     longer uses them, the running config's own `ip route 0.0.0.0/0 ...`
//     lines are taken out and put back in one vtysh call. Each -c line is
//     committed on its own; a `vtysh -f` file holding both is one mgmtd
//     transaction that changes nothing (live, 2026-11-15). The route leaves
//     the chip for the few milliseconds between the two lines.
//
// The kernel half of the zebra line is lost with the bgp container (a
// restart renders zebra.conf from the template, which then has it); the
// next cycle writes it again if not.

// kroute4 is collect.py's frr_route section and the kernel's IPv4 default
// routes (`ip -j route show default`).
type kroute4 struct {
	// known: collect.py printed frr_route (captures before 2026-11-15 lack
	// it) and the bgp container answered.
	known bool
	// frr are FRR's entries for 0.0.0.0/0; static the running config's own
	// lines for it ("ip route 0.0.0.0/0 <hop> ...").
	frr    []frrRoute
	static []string
	// kernelNH: zebra installs routes through kernel nexthop objects.
	kernelNH bool
	kernel   []kernelRoute
}

type frrRoute struct {
	Protocol  string   `json:"protocol"`
	Selected  bool     `json:"selected"`
	Installed bool     `json:"installed"`
	Nexthops  []frrHop `json:"nexthops"`
}

type frrHop struct {
	IP        string `json:"ip"`
	Interface string `json:"interface"`
	Active    bool   `json:"active"`
	FIB       bool   `json:"fib"`
}

// kernelRoute is one route of `ip -j route show default`: a single
// gateway, or the gateways of a multipath route.
type kernelRoute struct {
	Dst      string `json:"dst"`
	Gateway  string `json:"gateway"`
	Dev      string `json:"dev"`
	Protocol string `json:"protocol"`
	NHID     int    `json:"nhid"`
	Nexthops []struct {
		Gateway string `json:"gateway"`
		Dev     string `json:"dev"`
	} `json:"nexthops"`
}

func parseKRoute4(frrBody, routeBody string) kroute4 {
	var k kroute4
	body := strings.TrimSpace(frrBody)
	if body == "" || body == "error" {
		return k
	}
	var sec struct {
		Route          []frrRoute `json:"route"`
		Static         []string   `json:"static"`
		KernelNexthops *bool      `json:"kernel_nexthops"`
	}
	if json.Unmarshal([]byte(body), &sec) != nil || sec.KernelNexthops == nil {
		return k
	}
	if json.Unmarshal([]byte(strings.TrimSpace(routeBody)), &k.kernel) != nil {
		return kroute4{}
	}
	k.known, k.frr, k.static, k.kernelNH = true, sec.Route, sec.Static, *sec.KernelNexthops
	return k
}

// installed is FRR's selected, installed default, or nil.
func (k kroute4) installed() *frrRoute {
	for i := range k.frr {
		if k.frr[i].Selected && k.frr[i].Installed {
			return &k.frr[i]
		}
	}
	return nil
}

// kernelVia: the kernel's default through one of r's active hops, and
// whether that route uses a nexthop object.
func (k kroute4) kernelVia(r *frrRoute) (found, nhid bool) {
	if r == nil {
		return false, false
	}
	hop := map[string]bool{}
	for _, h := range r.Nexthops {
		if h.Active && h.IP != "" {
			hop[h.IP] = true
		}
	}
	for _, kr := range k.kernel {
		if kr.Dst != "default" {
			continue
		}
		gws := []string{kr.Gateway}
		for _, n := range kr.Nexthops {
			gws = append(gws, n.Gateway)
		}
		for _, g := range gws {
			if g != "" && hop[g] {
				return true, kr.NHID != 0
			}
		}
	}
	return false, false
}

// frrHops is r's active hops for a log line.
func frrHops(r *frrRoute) string {
	var s []string
	for _, h := range r.Nexthops {
		if h.Active {
			s = append(s, h.IP+" on "+h.Interface)
		}
	}
	return strings.Join(s, ", ")
}

const zebraNoKernelNH = "no zebra nexthop kernel enable"

// vtyshCommand runs config lines through the bgp container's vtysh, one -c
// each (each committed on its own); stdin is never read.
func vtyshCommand(lines []string) string {
	cmd := "docker exec bgp vtysh -c 'configure terminal'"
	for _, l := range lines {
		cmd += " -c " + sshrun.ShellQuote(l)
	}
	return cmd + " </dev/null"
}

// planKernelRoute is what applyKernelRoute writes for k and DEVICE_METADATA
// meta, after misses collects in a row without the kernel's default: the
// batch (CONFIG_DB, save), the vtysh lines, and why the route is re-installed
// ("" when it is not).
func planKernelRoute(k kroute4, meta map[string]string, misses int) (batch, vtysh []string, why string) {
	if !k.known {
		return nil, nil, ""
	}
	if meta["zebra_nexthop"] != "disabled" {
		batch = append(batch, "sudo sonic-db-cli CONFIG_DB hset 'DEVICE_METADATA|localhost' zebra_nexthop disabled", "sudo config save -y")
	}
	if k.kernelNH {
		vtysh = append(vtysh, zebraNoKernelNH)
	}
	r := k.installed()
	if r == nil || len(k.static) == 0 {
		return batch, vtysh, ""
	}
	found, nhid := k.kernelVia(r)
	switch {
	case !found && misses >= 2:
		why = fmt.Sprintf("the kernel has no IPv4 default route while FRR reports 0.0.0.0/0 via %s selected and installed (%d collects in a row)", frrHops(r), misses)
	case found && nhid:
		why = fmt.Sprintf("the kernel's IPv4 default route via %s uses a nexthop object, which a carrier loss deletes without a word to zebra", frrHops(r))
	default:
		return batch, vtysh, ""
	}
	for _, l := range k.static {
		vtysh = append(vtysh, "no "+l)
	}
	vtysh = append(vtysh, k.static...)
	return batch, vtysh, why
}

// applyKernelRoute keeps zebra off kernel nexthop objects and re-installs
// the IPv4 default route the kernel lost. The caller holds the apply lock.
func (c *Collector) applyKernelRoute(ctx context.Context) (int, error) {
	c.mu.Lock()
	st := c.state
	if st == nil {
		c.mu.Unlock()
		return 0, nil
	}
	if st != c.krSeen {
		c.krSeen = st
		r := st.kr.installed()
		if found, _ := st.kr.kernelVia(r); st.kr.known && r != nil && !found {
			c.krMisses++
		} else {
			c.krMisses = 0
		}
	}
	misses := c.krMisses
	c.mu.Unlock()
	if !st.kr.known {
		return 0, nil
	}
	if r := st.kr.installed(); r != nil && len(st.kr.static) == 0 {
		if found, _ := st.kr.kernelVia(r); !found && misses >= 2 {
			c.warnOnce("kroute-nostatic", "sonic: the kernel has no IPv4 default route while FRR reports 0.0.0.0/0 via %s installed (%s), and FRR's running config has no static line for it to re-install", frrHops(r), r.Protocol)
		}
	}
	batch, vtysh, why := planKernelRoute(st.kr, st.meta, misses)
	n := 0
	if len(batch) > 0 {
		c.Log.Printf("sonic route: zebra off kernel nexthop objects at the bgp container's next start: %s", strings.Join(batch, " ; "))
		if err := c.runBatch(ctx, batch); err != nil {
			return 0, fmt.Errorf("sonic: zebra_nexthop: %w", err)
		}
		n++
		c.mu.Lock()
		if c.state == st {
			next := st.clone()
			next.meta["zebra_nexthop"] = "disabled"
			c.state, c.krSeen = next, next
		}
		c.mu.Unlock()
	}
	if len(vtysh) == 0 {
		return n, nil
	}
	if why != "" {
		c.Log.Printf("sonic route: %s; re-installing it through zebra", why)
	}
	cmd := vtyshCommand(vtysh)
	c.Log.Printf("sonic route: %s", cmd)
	if _, err := c.run(ctx, cmd, ""); err != nil {
		return n, fmt.Errorf("sonic: zebra: %w", err)
	}
	n++
	c.mu.Lock()
	if cur := c.state; cur != nil {
		next := cur.clone()
		next.kr.kernelNH = false
		if why != "" {
			// The next collect reads what the kernel has; until then the
			// route counts as there, so a second apply writes nothing.
			next.kr = withKernelDefault(next.kr)
			c.krMisses = 0
		}
		c.state, c.krSeen = next, next
	}
	c.mu.Unlock()
	return n, nil
}

// withKernelDefault is k with FRR's installed default in the kernel through
// inline nexthops, as zebra installs it once it no longer uses nexthop
// objects.
func withKernelDefault(k kroute4) kroute4 {
	r := k.installed()
	if r == nil {
		return k
	}
	var kept []kernelRoute
	for _, kr := range k.kernel {
		if kr.Dst != "default" {
			kept = append(kept, kr)
		}
	}
	for _, h := range r.Nexthops {
		if h.Active {
			kept = append(kept, kernelRoute{Dst: "default", Gateway: h.IP, Dev: h.Interface, Protocol: "196"})
			break
		}
	}
	k.kernel = kept
	return k
}
