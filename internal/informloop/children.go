package informloop

import (
	"fmt"
	"slices"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Child is a device reported below one of this device's ports (config
// `children:`). The controller places a bridged device by the upstream
// switch reporting its MAC on a port (docs/topology-placement.md), so the
// port's MAC table carries the child's MAC and, when the port has no LLDP
// neighbour of its own, the child is reported as the neighbour -- the parent
// half of a breakout cage's lane switch (docs/breakout-lag.md §6.4).
// Reported values only: nothing is written to the device.
type Child struct {
	Port int
	MAC  string // lower-case colon form
	Name string // the neighbour's system name
}

// withChildren returns snap with the declared children reported on their
// ports. The driver's snapshot is not modified: a copy carries the entries.
func (l *Loop) withChildren(snap *devicemodel.Snapshot) *devicemodel.Snapshot {
	if snap == nil || len(l.cfg.Children) == 0 {
		return snap
	}
	s := *snap
	s.Ports = slices.Clone(snap.Ports)
	at := s.TakenAt
	if at.IsZero() {
		at = time.Now()
	}
	for _, c := range l.cfg.Children {
		i := slices.IndexFunc(s.Ports, func(p devicemodel.Port) bool { return p.Index == c.Port })
		if i < 0 {
			l.noteChild(c.Port, fmt.Sprintf("child %s (%s): port %d is not in the device's port table; not reported", c.Name, c.MAC, c.Port))
			continue
		}
		p := &s.Ports[i]
		vlan := p.VLAN.NativeVLAN
		if vlan == 0 {
			vlan = 1
		}
		e := devicemodel.MACEntry{MAC: c.MAC, VLAN: vlan, PortIndex: c.Port, LastMove: at}
		has := func(m devicemodel.MACEntry) bool { return m.MAC == c.MAC }
		if !slices.ContainsFunc(p.MACs, has) {
			p.MACs = append(slices.Clip(p.MACs), e)
		}
		if !slices.ContainsFunc(s.MACTable, has) {
			s.MACTable = append(slices.Clip(s.MACTable), e)
		}
		switch {
		case p.Neighbor == nil:
			p.Neighbor = &devicemodel.Neighbor{ChassisID: c.MAC, SystemName: c.Name, PortID: "eth0", IsBridge: true, Child: true}
			l.noteChild(c.Port, "")
		case !p.Neighbor.Child:
			l.noteChild(c.Port, fmt.Sprintf("child %s (%s): port %d already has an LLDP neighbour (%s %s); the child is reported in its MAC table only", c.Name, c.MAC, c.Port, p.Neighbor.SystemName, p.Neighbor.ChassisID))
		}
	}
	return &s
}

// noteChild logs a child's condition once, and again only when it changes
// ("" = the normal case, not logged).
func (l *Loop) noteChild(port int, msg string) {
	if l.childNoted == nil {
		l.childNoted = map[int]string{}
	}
	if l.childNoted[port] == msg {
		return
	}
	l.childNoted[port] = msg
	if msg != "" {
		l.cfg.Logger.Printf("[%s] %s", l.desc.MAC, msg)
	}
}
