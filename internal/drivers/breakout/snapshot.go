package breakout

import (
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Snapshot is the lane switch as the controller sees it, built from the
// parent's view of the cage alone:
//
//   - ports 1..N: the lanes, each with every field the parent collects for
//     it (state, speed, counters, FEC, the host's LLDP neighbour, the MACs
//     learned on it, LACP); the module is the cage's, so it is reported on
//     port 5 only;
//   - N+1..4 (a 2-lane mode): empty slots, which the Flex XG draws "Not
//     Connected" (docs/breakout-lag.md 6.6);
//   - port 5: the cable -- up while the lane switch is attached to the
//     split cage and the parent has the cage enabled (Uplink), lanes with
//     link or not, at the cage's native speed; counters summed, the
//     cage's module, the parent as its LLDP neighbour.
//
// The identity is the attachment's MAC. The address is the lane switch's
// own link-local one (Address): it has no interface, and reporting the
// parent's made the controller refuse every save of a parent with static
// IP Settings (api.err.StaticIpAlreadyUsedByDevice, naming the lane switch;
// Network 11.0.99, 2026-11-16, docs/drivers/breakout.md 16.10). The
// gateway and its MAC stay the parent's: the segment the device sits on.
func Snapshot(v devicemodel.LaneView, up Uplink) *devicemodel.Snapshot {
	snap := &devicemodel.Snapshot{TakenAt: v.TakenAt, VLANs: slices.Clone(v.VLANs), UplinkHint: UplinkPort}
	ps := v.System
	sys := devicemodel.System{
		Vendor: "disunified", Model: "Lane switch", SKU: v.Mode,
		MAC: v.Child.MAC, Hostname: v.Child.Name, Version: ps.Version,
		Uptime: ps.Uptime, GatewayMAC: ps.GatewayMAC, Gateway: ps.Gateway, DHCP: ps.DHCP,
		ARP: ps.ARP, Timezone: ps.Timezone,
	}
	if ip := Address(v.Child.MAC); ip != "" {
		// One interface, as the stub that was placed reported it: the
		// inform's if_table and uplink name it.
		sys.Addresses = []devicemodel.IfAddress{{Iface: "eth0", IP: ip, PrefixLen: 16}}
	}
	snap.System = sys

	uplink := devicemodel.Port{
		Index: UplinkPort, IfName: v.Cage.IfName, Interfaces: nil, Lanes: 1,
		Media: devicemodel.MediaQSFP28, Present: v.Cage.Present, Optic: v.Cage.Optic,
		FECCapable: v.Cage.FECCapable, FEC: v.Cage.FEC, FECConfig: v.Cage.FECConfig,
		STPState: "disabled", STPRole: "disabled",
	}
	if v.Cage.Media != "" {
		uplink.Media = v.Cage.Media
	}
	native := 0
	for _, s := range v.Cage.SpeedCaps {
		native = max(native, s)
	}
	if native > 0 {
		uplink.SpeedCaps = []int{native}
	}
	chassis := v.ChassisID
	if chassis == "" {
		chassis = ps.MAC
	}
	uplink.Neighbor = &devicemodel.Neighbor{
		ChassisID: chassis, SystemName: ps.Hostname, SystemDesc: ps.Vendor + " " + ps.Model,
		PortID: v.Cage.IfName, PortDescription: v.CageAlias, IsBridge: true,
	}
	if len(ps.Addresses) > 0 {
		uplink.Neighbor.ManagementIP = ps.Addresses[0].IP
	}

	for i, l := range v.Lanes {
		p := l
		p.Index = i + 1
		p.Optic = nil
		p.Present = v.Cage.Present || p.Up
		p.Lanes = 1
		if len(p.Interfaces) == 0 {
			p.Interfaces = []string{p.IfName}
		}
		p.MACs = nil
		for _, m := range l.MACs {
			m.PortIndex = p.Index
			p.MACs = append(p.MACs, m)
			snap.MACTable = append(snap.MACTable, m)
		}
		if p.MirrorFrom < 0 || p.MirrorFrom > len(v.Lanes) {
			p.MirrorFrom = 0
		}
		p.Name = p.IfName
		if p.Description != "" {
			p.Name = p.Description
		}
		snap.Ports = append(snap.Ports, p)

		uplink.Counters.Add(p.Counters)
		uplink.Health.FECCorrected += p.Health.FECCorrected
		uplink.Health.FECUncorrected += p.Health.FECUncorrected
		uplink.Health.HasFECCounters = uplink.Health.HasFECCounters || p.Health.HasFECCounters
		uplink.MTU = max(uplink.MTU, p.MTU)
	}
	for i := len(v.Lanes) + 1; i <= maxLanes; i++ {
		snap.Ports = append(snap.Ports, devicemodel.Port{
			Index: i, Lanes: 1, Media: devicemodel.MediaSFP28, Enabled: true,
			STPState: "disabled", STPRole: "disabled",
			// The lane comes back when the cage is re-split to four
			// lanes: its controller config is kept, not seeded disabled
			// as a free slot's (breakout.md 16.15).
			Reserved: true,
		})
	}
	// The cable is the lane switch's link to its parent: it is up while
	// the lane switch is attached to the split cage and the parent has the
	// cage enabled, whatever the lanes' far ends do (a lane switch whose
	// lanes have no far end is online through it, and five dark ports said
	// otherwise). Its drops are its own (Uplink.Drops: the cage disabled on
	// the parent, a detach), never a lane's: a lane's flaps are the lane's
	// port's, and counting them here raised a link-flap alert for the
	// uplink (CLAUDE.md 5).
	uplink.Up, uplink.Enabled = up.Up, !v.CageDisabled
	uplink.Health.LinkChanges = 2 * up.Drops
	if uplink.Up {
		uplink.Health.LinkChanges++
	}
	if uplink.Up {
		uplink.SpeedMbps, uplink.FullDuplex = native, true
		uplink.STPState, uplink.STPRole = "forwarding", "root"
	}
	uplink.Name = v.CageAlias
	if uplink.Name == "" {
		uplink.Name = uplink.IfName
	}
	snap.Ports = append(snap.Ports, uplink)
	slices.SortFunc(snap.MACTable, func(a, b devicemodel.MACEntry) int {
		if c := strings.Compare(a.MAC, b.MAC); c != 0 {
			return c
		}
		return a.VLAN - b.VLAN
	})
	return snap
}

// Uplink is the cable's own state, which the lane switch keeps across
// collects (Device.Collect): Up while attached to the split cage and the
// cage is enabled on the parent, Drops the times it went down since the
// bridge started.
type Uplink struct {
	Up    bool
	Drops uint64
}

// normMAC is a MAC in lower-case colon form.
func normMAC(s string) (string, error) {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil || len(hw) != 6 {
		return "", fmt.Errorf("%q is not a 6-byte MAC", s)
	}
	return hw.String(), nil
}

// Address is the IPv4 address a lane switch reports: link-local
// (169.254.0.0/16, RFC 3927), made from the last two octets of its MAC --
// 169.254.<1 + fifth octet mod 254>.<sixth octet, 1..254> -- so it is
// stable, differs per cage of one parent (the sixth octet is the cage
// number for a derived MAC) and names no routable host. "" for a MAC that
// does not parse.
func Address(mac string) string {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return ""
	}
	last := int(hw[5])
	if last < 1 {
		last = 1
	}
	if last > 254 {
		last = 254
	}
	return fmt.Sprintf("169.254.%d.%d", 1+int(hw[4])%254, last)
}
