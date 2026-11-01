package informloop

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// DHCP guarding and the switch ACL rules (docs/drivers/sonic.md §11.7):
// the push's switch.dhcp_guard.* and switch.acl.* groups in the neutral
// model.

// desiredGuards are the trusted DHCP servers while snooping is on.
func desiredGuards(cfg *unificfg.Config) []devicemodel.DHCPGuard {
	if cfg.DHCPSnoop == nil || !*cfg.DHCPSnoop {
		return nil
	}
	var out []devicemodel.DHCPGuard
	for _, g := range cfg.DHCPGuards {
		out = append(out, devicemodel.DHCPGuard{VLAN: g.VLAN, Server: g.Server})
	}
	return out
}

// desiredACLs fills the ACL rules of d. A rule whose shape no capture has
// shown (unificfg's Uncaptured) is not guessed at: every rule of its kind is
// held (IPACLSet/MACACLSet false, the device keeps what it has), because the
// rules are ordered and leaving one out changes what the others match. The
// reasons are returned for the log.
func desiredACLs(cfg *unificfg.Config, d *devicemodel.DeviceDesired) (held []string) {
	d.IPACLSet, d.IPACLs = true, []devicemodel.IPACL{}
	for _, a := range cfg.IPACLs {
		if why := a.Uncaptured(); why != "" {
			held = append(held, fmt.Sprintf("IP rule %q: %s", a.Name, why))
			d.IPACLSet = false
			continue
		}
		proto := ""
		if a.Type != "ip" {
			proto = a.Type
		}
		d.IPACLs = append(d.IPACLs, devicemodel.IPACL{Name: a.Name, Permit: a.Permit, Protocol: proto, Src: ipEnd(a.Src), Dst: ipEnd(a.Dst)})
	}
	if !d.IPACLSet {
		d.IPACLs = nil
	}
	d.MACACLSet, d.MACACLs = true, []devicemodel.MACACL{}
	for _, a := range cfg.MACACLs {
		if why := a.Uncaptured(); why != "" {
			held = append(held, fmt.Sprintf("MAC rule %q: %s", a.Name, why))
			d.MACACLSet = false
			continue
		}
		d.MACACLs = append(d.MACACLs, devicemodel.MACACL{Name: a.Name, Permit: a.Permit, VLAN: a.VLAN, Src: macEnd(a.Src), Dst: macEnd(a.Dst)})
	}
	if !d.MACACLSet {
		d.MACACLs = nil
	}
	return held
}

// ipEnd is one side of a captured IP rule: port_range 1-65535 is the UI's
// "no port", so it matches any port.
func ipEnd(m unificfg.IPMatch) devicemodel.IPACLEnd {
	e := devicemodel.IPACLEnd{CIDR: m.IP, Port: m.Port}
	if lo, hi, ok := strings.Cut(m.PortRange, "-"); ok && m.PortRange != unificfg.AnyPort {
		e.PortMin, _ = strconv.Atoi(lo)
		e.PortMax, _ = strconv.Atoi(hi)
	}
	return e
}

// macEnd is one side of a MAC rule; "any" under the zero mask is any.
func macEnd(m unificfg.MACMatch) devicemodel.MACACLEnd {
	if m.MAC == "any" {
		return devicemodel.MACACLEnd{}
	}
	return devicemodel.MACACLEnd{MAC: strings.ToLower(m.MAC), Mask: strings.ToLower(m.Mask)}
}

// noteHeldACLs logs the held rules when they change (desiredDevice runs on
// every apply and reconcile).
func (l *Loop) noteHeldACLs(held []string) {
	note := strings.Join(held, "; ")
	if note == l.aclHeld {
		return
	}
	l.aclHeld = note
	if note == "" {
		l.cfg.Logger.Printf("[%s] switch ACL rules: every rule in the push is translated again", l.desc.MAC)
		return
	}
	l.cfg.Logger.Printf("[%s] switch ACL rules HELD: %s -- the device keeps the rules it has of that kind; a rule shape is translated only once a push of it has been captured (docs/drivers/sonic.md §11.7.2)", l.desc.MAC, note)
}
