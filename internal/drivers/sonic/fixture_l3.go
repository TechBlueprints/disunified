package sonic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
)

// The FixtureRunner's model of the L3 writes (l3.go): STATIC_ROUTE rows
// written whole (bgpcfgd refuses a row whose lists differ in length), the
// DHCP relay CLI on the VLAN's dhcp_servers list (the 202511 plugin
// refuses a destination already there, one not there, and a VLAN that
// does not exist), the driver's DHCP server script, and the CLI's refusal
// of an address on a VLAN interface whose VLAN does not exist.

func (f *FixtureRunner) l3Write(w []string) (bool, error) {
	at := func(i int) string {
		if i < len(w) {
			return w[i]
		}
		return ""
	}
	switch {
	case len(w) >= 4 && w[0] == "sonic-db-cli" && w[1] == "CONFIG_DB" && strings.HasPrefix(w[3], "STATIC_ROUTE|"):
		return true, f.staticRouteWrite(w[2], w[3], w[4:])
	case len(w) >= 6 && w[0] == "config" && w[1] == "vlan" && w[2] == "dhcp_relay":
		return true, f.relayWrite(w[3], w[4], w[5:])
	case len(w) == 5 && w[0] == "python3" && w[1] == "-c" && w[3] == "disunified-dhcpd":
		return true, f.dhcpdWrite(w[4])
	case len(w) == 4 && w[0] == "sh" && w[1] == "-c" && w[3] == "disunified-rif-wait":
		return true, nil // FRR follows CONFIG_DB at once here
	case len(w) == 6 && w[0] == "config" && w[1] == "interface" && w[2] == "ip" && w[3] == "remove":
		if why := f.rifBound(w[4], w[5]); why != "" {
			return true, fmt.Errorf("Error: Cannot remove the last IP entry of interface %s. A static ip route is still bound to the RIF.%s", w[4], why)
		}
	case len(w) == 6 && w[0] == "config" && w[1] == "interface" && w[2] == "ip" && w[3] == "add" && strings.HasPrefix(w[4], "Vlan"):
		if _, ok := f.table("cfg_vlan")["VLAN|"+at(4)]; !ok {
			return true, fmt.Errorf("Error: Interface name %s is invalid. Please enter a valid interface name!!", at(4))
		}
	}
	return false, nil
}

func (f *FixtureRunner) staticRouteWrite(op, key string, fv []string) error {
	err := f.update("cfg_route", func(t table) error {
		switch op {
		case "del":
			if _, ok := t[key]; !ok {
				return fmt.Errorf("del of a missing %s", key)
			}
			delete(t, key)
			return nil
		case "hset":
			if len(fv) == 0 || len(fv)%2 != 0 {
				return fmt.Errorf("hset %s: odd field list", key)
			}
			r := t[key]
			if r == nil {
				r = map[string]string{}
				t[key] = r
			}
			for i := 0; i < len(fv); i += 2 {
				r[fv[i]] = fv[i+1]
			}
			n := -1
			for _, k := range []string{"nexthop", "ifname", "distance", "blackhole", "nexthop-vrf"} {
				v, ok := r[k]
				if !ok {
					continue
				}
				if l := len(strings.Split(v, ",")); n == -1 {
					n = l
				} else if l != n {
					return fmt.Errorf("bgpcfgd: Lists of next-hop attribute have different sizes in %s", key)
				}
			}
			return nil
		}
		return fmt.Errorf("unmodelled STATIC_ROUTE %s", op)
	})
	if err == nil && key == "STATIC_ROUTE|default|0.0.0.0/0" {
		f.syncKernelDefault()
	}
	return err
}

func (f *FixtureRunner) relayWrite(op, vid string, ips []string) error {
	return f.update("cfg_vlan", func(t table) error {
		k := "VLAN|Vlan" + vid
		r, ok := t[k]
		if !ok {
			return fmt.Errorf("Error: Vlan%s doesn't exist", vid)
		}
		cur := splitHops(r["dhcp_servers@"])
		for _, ip := range ips {
			has := containsString(cur, ip)
			switch {
			case op == "add" && has:
				return fmt.Errorf("Error: %s is already a DHCP relay destination for Vlan%s", ip, vid)
			case op == "del" && !has:
				return fmt.Errorf("Error: %s is not a DHCP relay destination for Vlan%s", ip, vid)
			case op == "add":
				cur = append(cur, ip)
			case op == "del":
				var keep []string
				for _, x := range cur {
					if x != ip {
						keep = append(keep, x)
					}
				}
				cur = keep
			default:
				return fmt.Errorf("unmodelled dhcp_relay %s", op)
			}
		}
		if len(cur) == 0 {
			delete(r, "dhcp_servers@")
		} else {
			r["dhcp_servers@"] = strings.Join(cur, ",")
		}
		return nil
	})
}

// dhcpdWrite applies the driver's DHCP server ops to the capture's dhcpd
// section, as the next collect would read it. DHCPdConf records each
// written config by interface.
func (f *FixtureRunner) dhcpdWrite(arg string) error {
	var ops []dhcpdOp
	if err := json.Unmarshal([]byte(arg), &ops); err != nil {
		return err
	}
	units := map[string]string{}
	for _, line := range strings.Split(f.section("dhcpd"), "\n") {
		if fs := strings.Fields(line); len(fs) == 3 {
			units[fs[0]] = line
		}
	}
	if f.DHCPdConf == nil {
		f.DHCPdConf = map[string]string{}
	}
	for _, o := range ops {
		if o.Conf == "" {
			delete(units, o.Iface)
			delete(f.DHCPdConf, o.Iface)
			continue
		}
		if !strings.Contains(o.Conf, "interface "+o.Iface+"\n") {
			return fmt.Errorf("udhcpd config for %s names another interface", o.Iface)
		}
		h := sha256.Sum256([]byte(o.Conf))
		units[o.Iface] = o.Iface + " active " + hex.EncodeToString(h[:])
		f.DHCPdConf[o.Iface] = o.Conf
	}
	names := make([]string, 0, len(units))
	for n := range units {
		names = append(names, n)
	}
	sort.Strings(names)
	var lines []string
	for _, n := range names {
		lines = append(lines, units[n])
	}
	f.UseSection("dhcpd", strings.Join(lines, "\n"))
	return nil
}

// rifBound models the CLI's check before it removes an interface's last
// address: a static route FRR would list through the interface -- one out
// of it, or via a next hop in its subnet -- refuses the removal. "" when
// the removal may go ahead.
func (f *FixtureRunner) rifBound(iface, cidr string) string {
	var nets []*net.IPNet
	for k := range f.table("cfg_mgmt") {
		parts := strings.Split(k, "|")
		if len(parts) != 3 || parts[1] != iface {
			continue
		}
		if parts[2] != cidr {
			return "" // not the last address
		}
		if _, n, err := net.ParseCIDR(parts[2]); err == nil {
			nets = append(nets, n)
		}
	}
	for k, r := range f.table("cfg_route") {
		for _, h := range hopsOf(r) {
			ip := net.ParseIP(h.nexthop)
			if h.ifname == iface {
				return " (" + k + ")"
			}
			for _, n := range nets {
				if ip != nil && n.Contains(ip) {
					return " (" + k + ")"
				}
			}
		}
	}
	return ""
}
