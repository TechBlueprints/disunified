package sonic

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// The FixtureRunner's device-level writes (§6.3, §6.4), applied to the
// capture's CONFIG_DB sections -- and, for an address or the default
// route, to its `ip -j` sections, so the next Collect sees what the kernel
// would.

func (f *FixtureRunner) applyDevice(w []string) error {
	at := func(i int) string {
		if i < len(w) {
			return w[i]
		}
		return ""
	}
	switch {
	case w[1] == "ntp":
		k := "NTP_SERVER|" + at(3)
		if net.ParseIP(at(3)) == nil {
			return fmt.Errorf("Invalid IP address %q", at(3))
		}
		return f.update("cfg_ntp", func(t table) error {
			_, ok := t[k]
			switch at(2) {
			case "add":
				if ok {
					return fmt.Errorf("%s is already configured", at(3))
				}
				t[k] = map[string]string{"association_type": "server", "iburst": "on", "version": "4"}
			case "del":
				if !ok {
					return fmt.Errorf("%s not configured", at(3))
				}
				delete(t, k)
			default:
				return fmt.Errorf("unmodelled ntp %s", at(2))
			}
			return nil
		})

	case w[1] == "syslog":
		ip := at(3)
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("Invalid IP address %q", ip)
		}
		k := "SYSLOG_SERVER|" + ip
		return f.update("cfg_syslog", func(t table) error {
			_, ok := t[k]
			switch at(2) {
			case "add":
				if ok {
					return fmt.Errorf("Syslog server %s already exists", ip)
				}
				fields := map[string]string{}
				for i := 4; i+1 < len(w); i++ {
					if w[i] == "--port" || w[i] == "-p" {
						fields["port"] = w[i+1]
					}
				}
				t[k] = fields
			case "del":
				if !ok {
					return fmt.Errorf("Syslog server %s does not exist", ip)
				}
				delete(t, k)
			default:
				return fmt.Errorf("unmodelled syslog %s", at(2))
			}
			return nil
		})

	case w[1] == "snmp":
		name := at(4)
		k := "SNMP_COMMUNITY|" + name
		return f.update("cfg_snmp", func(t table) error {
			_, ok := t[k]
			switch at(3) {
			case "add":
				if ok {
					return fmt.Errorf("SNMP community %s is already configured", name)
				}
				if at(5) != "RO" && at(5) != "RW" {
					return fmt.Errorf("community type must be RO or RW")
				}
				t[k] = map[string]string{} // the capture keeps key names only
			case "del":
				if !ok {
					return fmt.Errorf("SNMP community %s is not configured", name)
				}
				delete(t, k)
			default:
				return fmt.Errorf("unmodelled snmp community %s", at(3))
			}
			return nil
		})

	case w[1] == "dns":
		k := "DNS_NAMESERVER|" + at(4)
		return f.update("cfg_dns", func(t table) error {
			_, ok := t[k]
			switch at(3) {
			case "add":
				if ok {
					return fmt.Errorf("nameserver %s already exists", at(4))
				}
				t[k] = map[string]string{}
			case "del":
				if !ok {
					return fmt.Errorf("nameserver %s does not exist", at(4))
				}
				delete(t, k)
			}
			return nil
		})

	case w[1] == "spanning-tree":
		if _, ok := f.table("cfg_feature")["FEATURE|stp"]; !ok {
			return fmt.Errorf("STP is not available on this image")
		}
		return f.update("cfg_stp", func(t table) error {
			g, ok := t["STP|GLOBAL"]
			switch at(2) {
			case "enable":
				if ok {
					return fmt.Errorf("PVST is already configured")
				}
				t["STP|GLOBAL"] = map[string]string{"mode": at(3), "priority": "32768", "hello_time": "2", "forward_delay": "15", "max_age": "20"}
			case "disable":
				delete(t, "STP|GLOBAL")
			case "priority":
				if !ok {
					return fmt.Errorf("STP is not enabled")
				}
				g["priority"] = at(3)
			default:
				return fmt.Errorf("unmodelled spanning-tree %s", at(2))
			}
			return nil
		})

	case w[1] == "route":
		// route add|del prefix 0.0.0.0/0 nexthop <gw>
		if at(3) != "prefix" || at(5) != "nexthop" {
			return fmt.Errorf("unmodelled route form")
		}
		k := "STATIC_ROUTE|default|" + at(4)
		gw := at(6)
		err := f.update("cfg_route", func(t table) error {
			r := t[k]
			hops := splitHops(r["nexthop"])
			switch at(2) {
			case "add":
				for _, h := range hops {
					if h == gw {
						return fmt.Errorf("route %s via %s already exists", at(4), gw)
					}
				}
				hops = append(hops, gw)
			case "del":
				var keep []string
				for _, h := range hops {
					if h != gw {
						keep = append(keep, h)
					}
				}
				if len(keep) == len(hops) {
					return fmt.Errorf("route %s via %s does not exist", at(4), gw)
				}
				hops = keep
			}
			if len(hops) == 0 {
				delete(t, k)
				return nil
			}
			if r == nil {
				r = map[string]string{"blackhole": "false", "distance": "0", "ifname": "", "nexthop-vrf": ""}
				t[k] = r
			}
			r["nexthop"] = strings.Join(hops, ",")
			return nil
		})
		if err == nil && at(4) == "0.0.0.0/0" {
			f.syncKernelDefault()
		}
		return err

	case w[1] == "interface" && w[2] == "ip":
		// interface ip add|remove <iface> <ip/len>
		iface, cidr := at(4), at(5)
		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil || ip.To4() == nil {
			return fmt.Errorf("Invalid ip address %q", cidr)
		}
		plen, _ := ipnet.Mask.Size()
		var sec, key string
		switch {
		case strings.HasPrefix(iface, "Vlan"):
			sec, key = "cfg_mgmt", "VLAN_INTERFACE|"+iface+"|"+cidr
		case iface == "eth0":
			sec, key = "cfg_mgmt", "MGMT_INTERFACE|eth0|"+cidr
		default:
			sec, key = "cfg_mgmt", "INTERFACE|"+iface+"|"+cidr
		}
		if err := f.update(sec, func(t table) error {
			_, ok := t[key]
			switch at(3) {
			case "add":
				if ok {
					return fmt.Errorf("%s already has %s", iface, cidr)
				}
				t[key] = map[string]string{"NULL": "NULL"}
			case "remove":
				if !ok {
					return fmt.Errorf("%s has no %s", iface, cidr)
				}
				delete(t, key)
			default:
				return fmt.Errorf("unmodelled interface ip %s", at(3))
			}
			return nil
		}); err != nil {
			return err
		}
		return f.kernelAddress(iface, ip.String(), plen, at(3) == "add")
	}
	return fmt.Errorf("unmodelled write: %s", strings.Join(w, " "))
}

func splitHops(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// section returns a section's body; setSection replaces it.
func (f *FixtureRunner) section(name string) string { return sections(f.Fixture)[name] }

func (f *FixtureRunner) setSection(name, body string) {
	head := "@@@ " + name + "\n"
	i := strings.Index(f.Fixture, head)
	if i < 0 {
		return
	}
	rest := f.Fixture[i+len(head):]
	j := strings.Index(rest, "@@@ ")
	if j < 0 {
		return
	}
	f.Fixture = f.Fixture[:i] + head + strings.TrimRight(body, "\n") + "\n" + rest[j:]
}

// kernelAddress adds or removes an IPv4 address in the capture's `ip -j
// addr` section, as the kernel would once SONiC applied it.
func (f *FixtureRunner) kernelAddress(iface, ip string, plen int, add bool) error {
	var links []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(f.section("addr"))), &links); err != nil {
		return fmt.Errorf("addr section: %w", err)
	}
	for _, l := range links {
		if l["ifname"] != iface {
			continue
		}
		infos, _ := l["addr_info"].([]any)
		var kept []any
		for _, a := range infos {
			m, _ := a.(map[string]any)
			if m["family"] == "inet" && m["local"] == ip {
				continue
			}
			kept = append(kept, a)
		}
		if add {
			kept = append(kept, map[string]any{"family": "inet", "local": ip, "prefixlen": plen, "scope": "global", "label": iface})
		}
		l["addr_info"] = kept
	}
	b, err := json.Marshal(links)
	if err != nil {
		return err
	}
	f.setSection("addr", string(b))
	return nil
}

// syncKernelDefault points the capture's FRR-installed default route at the
// static route's (first) next hop.
func (f *FixtureRunner) syncKernelDefault() {
	hops := splitHops(f.table("cfg_route")["STATIC_ROUTE|default|0.0.0.0/0"]["nexthop"])
	var routes []map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(f.section("route"))), &routes) != nil {
		return
	}
	var kept []map[string]any
	for _, r := range routes {
		if r["protocol"] == "196" {
			if len(hops) == 0 {
				continue
			}
			r["gateway"] = hops[0]
		}
		kept = append(kept, r)
	}
	b, err := json.Marshal(kept)
	if err == nil {
		f.setSection("route", string(b))
	}
}

// applyKeys applies an authorized_keys rewrite: the block between the
// markers in the script's here-document becomes the capture's
// authorized_keys section (empty when the script writes no block).
func (f *FixtureRunner) applyKeys(script string) error {
	if !strings.Contains(script, `mv -f "$tmp" "$f"`) || !strings.Contains(script, "awk ") {
		return fmt.Errorf("not an atomic authorized_keys rewrite")
	}
	var keys []string
	in := false
	for _, line := range strings.Split(script, "\n") {
		switch {
		case line == keysBegin:
			in = true
		case line == keysEnd:
			in = false
		case in:
			keys = append(keys, line)
		}
	}
	f.setSection("authorized_keys", strings.Join(keys, "\n"))
	return nil
}
