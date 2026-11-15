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
//   - port 5: the cable -- up when any lane is, at the cage's native speed,
//     counters summed, the cage's module, the parent as its LLDP neighbour.
//
// The identity is the attachment's MAC; the address and the reachability
// fields are the parent's (one address for two devices was fine, 6.6).
func Snapshot(v devicemodel.LaneView) *devicemodel.Snapshot {
	snap := &devicemodel.Snapshot{TakenAt: v.TakenAt, VLANs: slices.Clone(v.VLANs), UplinkHint: UplinkPort}
	ps := v.System
	sys := devicemodel.System{
		Vendor: "disunified", Model: "Lane switch", SKU: v.Mode,
		MAC: v.Child.MAC, Hostname: v.Child.Name, Version: ps.Version,
		Uptime: ps.Uptime, GatewayMAC: ps.GatewayMAC, Gateway: ps.Gateway, DHCP: ps.DHCP,
		ARP: ps.ARP, Timezone: ps.Timezone,
	}
	if len(ps.Addresses) > 0 {
		// One interface, as the stub that was placed reported it: the
		// inform's if_table and uplink name it.
		a := ps.Addresses[0]
		sys.Addresses = []devicemodel.IfAddress{{Iface: "eth0", IP: a.IP, PrefixLen: a.PrefixLen}}
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

	var drops uint64
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

		uplink.Up = uplink.Up || p.Up
		uplink.Enabled = uplink.Enabled || p.Enabled
		uplink.Counters.Add(p.Counters)
		drops += p.Health.LinkChanges / 2 // transitions from down: half are drops
		uplink.Health.FECCorrected += p.Health.FECCorrected
		uplink.Health.FECUncorrected += p.Health.FECUncorrected
		uplink.Health.HasFECCounters = uplink.Health.HasFECCounters || p.Health.HasFECCounters
		uplink.MTU = max(uplink.MTU, p.MTU)
	}
	for i := len(v.Lanes) + 1; i <= maxLanes; i++ {
		snap.Ports = append(snap.Ports, devicemodel.Port{
			Index: i, Lanes: 1, Media: devicemodel.MediaSFP28, Enabled: true,
			STPState: "disabled", STPRole: "disabled",
		})
	}
	// The cable's drops are its lanes' together; summing the lanes'
	// transitions would count each up lane's rise.
	uplink.Health.LinkChanges = 2 * drops
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

// normMAC is a MAC in lower-case colon form.
func normMAC(s string) (string, error) {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil || len(hw) != 6 {
		return "", fmt.Errorf("%q is not a 6-byte MAC", s)
	}
	return hw.String(), nil
}
