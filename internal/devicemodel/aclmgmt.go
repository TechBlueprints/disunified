package devicemodel

import (
	"fmt"
	"net"
)

// IPRulesCutManagement says which of the controller's IP rules would drop
// the bridge's own TCP connection to the device, "" if none would. A
// driver that binds the rules ingress on every port, the uplink included,
// holds such a set: it would cut the device off from the bridge, and from
// the push that would remove the rule again.
//
// from is the bridge's address as the device sees it ("" = unknown: every
// source counts, and an Allow limited to some sources is not taken to
// cover the bridge). dial is the device's addresses the bridge connects
// to, port the TCP port it connects on. The first rule that matches
// decides, so an earlier Allow that covers the bridge makes a later Block
// harmless. The bridge's source port is ephemeral: a rule naming one
// source port is taken not to match it.
//
// The sonic driver keeps its own copy (cutsManagement, SSH port 22),
// written first; this one is the same check with the port as a parameter.
func IPRulesCutManagement(acls []IPACL, from string, dial []string, port int) string {
	bridge := net.ParseIP(from).To4()
	covers := func(e IPACLEnd, p int, ip net.IP) bool {
		if e.CIDR != "" {
			_, n, err := net.ParseCIDR(e.CIDR)
			if err != nil || !n.Contains(ip) {
				return false
			}
		}
		switch {
		case e.Port != 0:
			return e.Port == p
		case e.PortMin != 0 || e.PortMax != 0:
			return e.PortMin <= p && p <= e.PortMax
		}
		return true
	}
	for _, addr := range dial {
		ip := net.ParseIP(addr).To4()
		if ip == nil {
			continue
		}
		for i, a := range acls {
			if a.Protocol != "" && a.Protocol != "tcp" {
				continue
			}
			if a.Src.Port != 0 || !covers(a.Dst, port, ip) {
				continue
			}
			if bridge != nil && !covers(IPACLEnd{CIDR: a.Src.CIDR}, 0, bridge) {
				continue
			}
			if a.Permit {
				if bridge != nil || a.Src.CIDR == "" {
					break // the first match decides: this Allow lets the bridge in
				}
				continue // it may not cover the bridge, whose address is unknown
			}
			return fmt.Sprintf("rule %d (%q) would block the bridge's connection to this device (%s, TCP %d) on every port, the uplink included", i+1, a.Name, ip, port)
		}
	}
	return ""
}
