package informloop

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// UniFi's Layer 3 switching (docs/drivers/sonic.md §11.13): the push's
// switch.routing_interface.* groups in the neutral model.

// desiredL3 fills the routed networks and routes of d from the push, and
// returns what it could not translate, for the log. A push without
// switch.routing_enabled leaves the device's routing alone.
func desiredL3(cfg *unificfg.Config, d *devicemodel.DeviceDesired) (notes []string) {
	r := cfg.Routing
	if r == nil {
		return nil
	}
	d.L3Set = true
	d.L3Interfaces, d.StaticRoutes = []devicemodel.L3Interface{}, []devicemodel.StaticRoute{}
	if !r.Enabled {
		return nil
	}
	vlanOf := map[int]int{}
	for _, ri := range r.Interfaces {
		vlanOf[ri.ID] = ri.VLAN
	}
	for _, ri := range r.Interfaces {
		for _, u := range ri.Unmodelled {
			notes = append(notes, fmt.Sprintf("network %q: %s not translated", ri.Name, u))
		}
		addr, ok := withPrefix(ri.IP, ri.Mask)
		if !ok || ri.VLAN < 1 || ri.VLAN > 4094 {
			notes = append(notes, fmt.Sprintf("network %q: VLAN %d, address %s mask %s is not a routed interface; left out", ri.Name, ri.VLAN, ri.IP, ri.Mask))
			continue
		}
		in := devicemodel.L3Interface{Name: ri.Name, VLAN: ri.VLAN, Address: addr, Isolated: ri.Purpose == "guest"}
		switch ri.OpMode {
		case "manual", "":
		case "dhcp_server":
			s := ri.DHCPServer
			in.DHCP = devicemodel.L3DHCP{Mode: "server", Start: s.Start, End: s.End, Router: s.Gateway, DNS: s.DNS, LeaseSecs: s.LeaseTime}
		case "dhcp_relay":
			in.DHCP = devicemodel.L3DHCP{Mode: "relay", Relays: ri.Relays}
		default:
			notes = append(notes, fmt.Sprintf("network %q: DHCP mode %q not translated; no DHCP service there", ri.Name, ri.OpMode))
		}
		d.L3Interfaces = append(d.L3Interfaces, in)
	}
	seen := map[devicemodel.StaticRoute]bool{}
	for _, ri := range r.Interfaces {
		for _, rt := range ri.Routes {
			for _, u := range rt.Unmodelled {
				notes = append(notes, fmt.Sprintf("route %q: %s not translated", rt.Name, u))
			}
			prefix, ok := withPrefix(rt.Prefix, rt.Mask)
			if !ok {
				notes = append(notes, fmt.Sprintf("route %q: %s mask %s is not a prefix; left out", rt.Name, rt.Prefix, rt.Mask))
				continue
			}
			_, n, _ := net.ParseCIDR(prefix)
			s := devicemodel.StaticRoute{Name: rt.Name, Prefix: n.String()}
			switch {
			case rt.Gateway == "Null0":
				s.Blackhole = true
			case rt.Gateway != "":
				if ip := net.ParseIP(rt.Gateway); ip == nil || ip.To4() == nil {
					notes = append(notes, fmt.Sprintf("route %q: next hop %q is not an IPv4 address; left out", rt.Name, rt.Gateway))
					continue
				}
				s.NextHop = rt.Gateway
			case vlanOf[rt.Interface] != 0:
				s.VLAN = vlanOf[rt.Interface]
			default:
				notes = append(notes, fmt.Sprintf("route %q: no next hop and interface %d is no routed network; left out", rt.Name, rt.Interface))
				continue
			}
			key := s
			key.Name = ""
			if seen[key] {
				continue
			}
			seen[key] = true
			d.StaticRoutes = append(d.StaticRoutes, s)
		}
	}
	sort.SliceStable(d.StaticRoutes, func(i, j int) bool { return d.StaticRoutes[i].Prefix < d.StaticRoutes[j].Prefix })
	return notes
}

// withPrefix is a.b.c.d with a dotted mask as a.b.c.d/n; false for anything
// else (an IPv4 address and a contiguous mask).
func withPrefix(ip, mask string) (string, bool) {
	a := net.ParseIP(ip).To4()
	m := net.ParseIP(mask).To4()
	if a == nil || m == nil {
		return "", false
	}
	ones, bits := net.IPv4Mask(m[0], m[1], m[2], m[3]).Size()
	if bits != 32 {
		return "", false
	}
	return fmt.Sprintf("%s/%d", a, ones), true
}

// noteL3 logs what the push's routing could not translate, when that
// changes (desiredDevice runs on every apply and reconcile).
func (l *Loop) noteL3(notes []string) {
	note := strings.Join(notes, "; ")
	if note == l.l3Held {
		return
	}
	l.l3Held = note
	if note != "" {
		l.cfg.Logger.Printf("[%s] L3 routing: %s", l.desc.MAC, note)
	}
}

// desiredBGP fills the switch's BGP router from the push (switch.bgp.1.*,
// docs/drivers/sonic.md §11.14): the uploaded FRR file while the router is
// enabled, nothing when it is off or the push names none. Every push to a
// switch claiming L3 routing carries switch.routing_enabled, so a push with
// it and no switch.bgp means no BGP router. A file that does not decode
// leaves the device's BGP alone.
func desiredBGP(cfg *unificfg.Config, d *devicemodel.DeviceDesired) (notes []string) {
	b := cfg.BGP
	if b == nil {
		d.BGPSet = cfg.Routing != nil
		return nil
	}
	if b.DecodeErr != nil {
		return []string{fmt.Sprintf("BGP: the configuration file is not base64 (%v); the switch's BGP left as it is", b.DecodeErr)}
	}
	d.BGPSet = true
	if b.Enabled {
		d.BGPConfig = b.Config
	}
	return nil
}

// desiredOSPF fills the switch's OSPF router from the push (switch.ospf.*
// and each routed network's ospf.*, docs/drivers/sonic.md §11.14). A push
// with switch.routing_enabled and no switch.ospf means no OSPF router.
func desiredOSPF(cfg *unificfg.Config, d *devicemodel.DeviceDesired) (notes []string) {
	o := cfg.OSPF
	if o == nil {
		d.OSPFSet = cfg.Routing != nil
		return nil
	}
	for _, u := range o.Unmodelled {
		notes = append(notes, fmt.Sprintf("OSPF: %s not translated", u))
	}
	redist := func(what, v string) string {
		switch v {
		case "e1", "e2":
			return v
		case "none", "":
			return ""
		}
		notes = append(notes, fmt.Sprintf("OSPF: %s redistribution %q not translated; not redistributed", what, v))
		return ""
	}
	r := &devicemodel.OSPFRouter{RouterID: o.RouterID, AnnounceDefault: o.AnnounceDefault,
		RedistStatic: redist("static", o.RedistStatic), RedistConnected: redist("connected", o.RedistConnected)}
	for _, a := range o.Areas {
		r.Areas = append(r.Areas, devicemodel.OSPFArea{ID: a.ID, Type: a.Type})
	}
	if cfg.Routing != nil {
		for _, ri := range cfg.Routing.Interfaces {
			oi := ri.OSPF
			if oi == nil {
				continue
			}
			for _, u := range oi.Unmodelled {
				notes = append(notes, fmt.Sprintf("OSPF on %q: %s not translated", ri.Name, u))
			}
			in := devicemodel.OSPFInterface{Name: ri.Name, VLAN: ri.VLAN, Area: oi.Area, Passive: oi.Passive,
				Cost: oi.Cost, Priority: oi.Priority, Hello: oi.Hello, Dead: oi.Dead}
			switch oi.Auth {
			case "none", "":
			case "plaintext":
				in.Auth, in.Password = "plaintext", oi.Password
			case "md5":
				in.Auth = "md5"
				for _, k := range oi.MD5Keys {
					in.MD5Keys = append(in.MD5Keys, devicemodel.OSPFKey{ID: k.ID, Key: k.Key})
				}
			default:
				notes = append(notes, fmt.Sprintf("OSPF on %q: authentication %q not translated; the network left out of OSPF", ri.Name, oi.Auth))
				continue
			}
			r.Interfaces = append(r.Interfaces, in)
		}
	}
	d.OSPFSet, d.OSPF = true, r
	return notes
}
