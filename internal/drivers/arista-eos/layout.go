package aristaeos

import (
	"maps"
	"slices"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/modulelayout"
)

// What a module runs, where the controller says auto
// (docs/module-layout.md §3, docs/drivers/arista-eos.md §14).
//
// On a QSFP28 cage the controller pushes no speed for, holding a module
// internal/modulelayout knows, the cage keeps a layout the module runs and
// otherwise takes the module's native one as a forced speed: a different
// port count is a split or join on lane 1 (`speed forced 25gfull` /
// `100gfull`), the same port count a lane speed on every lane. An optical
// port is never set to auto speed. These are the driver's own writes: the
// controller-only plans (PlanPorts, PlanLanes) leave them out.

// moduleOf is the cage's module as the layout table keys it: its EEPROM
// codes where they were read and are not stale, else the media type for
// the two strings a capture of this switch shows (fec.go's moduleMedium
// reads the same). `100GBASE-CR4` stands for the code that runs every
// copper layout (CA-25G-L), so it never changes a split or a whole cage;
// a CA-S or CA-N cable is corrected once its codes are read.
func moduleOf(in fecInputs, cage int, mediaType string) (modulelayout.Module, bool) {
	if codes := in.codes[cage]; codes.ok && !in.stale {
		return modulelayout.Module{Ethernet: codes.ethernet, Extended: codes.extended}, true
	}
	switch mediaType {
	case "100GBASE-CWDM4":
		return modulelayout.Extended(0x06), true
	case "100GBASE-CR4":
		return modulelayout.Extended(0x0B), true
	}
	return modulelayout.Module{}, false
}

// laneSpeeds is each cage's lanes' admin speeds, by EOS lane number, from
// `show interfaces transceiver properties` (0 for "auto").
func laneSpeeds(props showInterfacesTransceiverProperties) map[int]map[int]int {
	out := map[int]map[int]int{}
	for name, e := range props.Interfaces {
		slot, lane, ok := parseEthName(name)
		if !ok {
			continue
		}
		if out[slot] == nil {
			out[slot] = map[int]int{}
		}
		out[slot][lane] = adminSpeedMbps(e.AdminSpeed.Speed)
	}
	return out
}

// currentLayout is the cage's layout: its lane count at its lanes' admin
// speed. ok is false for lanes at different speeds, which match nothing.
// The fold's LanesDiverge is not the test: VLANs that differ between the
// lanes set it too. A whole cage with no speed line (admin speed auto, 0)
// is one port at EOS's own speed: it satisfies any one-port layout.
func currentLayout(p devicemodel.Port, speeds map[int]int) (l modulelayout.Layout, ok bool) {
	l = modulelayout.Layout{Ports: max(p.Lanes, 1), SpeedMbps: p.AdminSpeed}
	for _, n := range laneNames(p) {
		_, lane, _ := parseEthName(n)
		if sp, seen := speeds[lane]; seen && sp != l.SpeedMbps {
			return l, false
		}
	}
	return l, true
}

// layoutRuns reports whether cur is one of runs; a speedless whole cage
// is any one-port layout.
func layoutRuns(runs []modulelayout.Layout, cur modulelayout.Layout) bool {
	for _, r := range runs {
		if r == cur || (r.Ports == 1 && cur.Ports == 1 && cur.SpeedMbps == 0) {
			return true
		}
	}
	return false
}

// laneCountAt is how many interfaces a cage has after `speed forced` at
// mbps on lane 1: 1 at 40G and 100G, 2 at 50G (EOS names them /1 and /3),
// 4 at 25G and 10G.
func laneCountAt(mbps int) int {
	switch {
	case mbps == 50000:
		return 2
	case mbps >= 40000:
		return 1
	}
	return 4
}

// moduleSpeed is the speed the module rule forces on cage p for d, or 0
// for none: the controller pushed a speed, the cage is not a QSFP28 cage,
// no module is seated, the module is not one the table knows (noted), the
// cage runs a layout the module runs, or the cage is an aggregate member
// or a mirror port (noted). owned is a cage with a lane switch attached:
// the module decides the port count only (the lanes' own settings, which
// may differ, are the lane switch's).
func moduleSpeed(in fecInputs, speeds map[int]int, p devicemodel.Port, d devicemodel.PortDesired, owned, mirrored bool, note func(string, ...any)) int {
	if d.SpeedMbps > 0 || p.Media != devicemodel.MediaQSFP28 || p.Optic == nil || !p.Present {
		return 0
	}
	media := p.Optic.MediaType
	if media == "" {
		media = in.types[laneNames(p)[0]]
	}
	m, ok := moduleOf(in, p.Index, media)
	runs, known := modulelayout.Runs(m)
	if !ok || !known {
		note("port %d: speed auto, and the module %q is not in the layout table (docs/module-layout.md); the cage is left as it is", d.Index, media)
		return 0
	}
	if owned {
		for _, r := range runs {
			if r.Ports == max(p.Lanes, 1) {
				return 0
			}
		}
	} else if cur, ok := currentLayout(p, speeds); ok && layoutRuns(runs, cur) {
		return 0
	}
	native := runs[0]
	if speedKeyword(native.SpeedMbps) == "" {
		return 0
	}
	switch {
	case p.LAG != "" || d.LAG > 0:
		note("port %d: the module %q runs %v, and the cage is in an aggregate; its layout is left as it is", d.Index, media, runs)
		return 0
	case mirrored:
		note("port %d: the module %q runs %v, and the cage is a mirror source or destination; its layout is left as it is", d.Index, media, runs)
		return 0
	}
	return native.SpeedMbps
}

// mirrorPorts are the ports in a mirror session, as the switch has them
// and as desired asks.
func mirrorPorts(ports []devicemodel.Port, desired []devicemodel.PortDesired) map[int]bool {
	out := map[int]bool{}
	for _, p := range ports {
		if p.MirrorFrom > 0 {
			out[p.Index], out[p.MirrorFrom] = true, true
		}
	}
	for _, d := range desired {
		if d.MirrorSource > 0 {
			out[d.Index], out[d.MirrorSource] = true, true
		}
	}
	return out
}

// PlanLayout implements devicemodel.LayoutPlanner: the ports whose lane
// count ApplyPorts(desired) would change, the module rule's and a pushed
// speed's alike, so the loop does not take a cage the apply split or
// joined for a new port.
func (c *Collector) PlanLayout(desired []devicemodel.PortDesired) []int {
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	if snap == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(c.plan(snap, desired, planScope{}).layout))
}

var _ devicemodel.LayoutPlanner = (*Collector)(nil)
