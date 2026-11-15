package aristaeos

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// ApplyPorts implements devicemodel.Controller: it compares each desired
// port with the last collected snapshot and writes only the differences,
// as one eAPI/SSH batch:
//
//	enable / configure / interface EthernetN / [no] shutdown / [no] description X / ... / end / write memory
//
// eAPI sessions start at privilege 1 whatever the user's level, so `enable`
// leads. A breakout slot writes to every lane. `write memory` persists the
// running config so a switch reboot keeps the controller's intent.
func (c *Collector) ApplyPorts(ctx context.Context, desired []devicemodel.PortDesired) (int, error) {
	unlock, err := c.lockApply(ctx)
	if err != nil {
		return 0, fmt.Errorf("eos apply: %w", err)
	}
	defer unlock()
	c.noteCageDown(desired)
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	if snap == nil {
		return 0, fmt.Errorf("eos apply: no snapshot yet")
	}
	pr := c.plan(snap, desired, planScope{})
	c.logNotes(pr.notes)
	if pr.changed == 0 {
		return 0, nil
	}
	cmds := append(append([]string{"enable", "configure"}, pr.cmds...), "end", "write memory")
	if c.Log != nil {
		c.Log.Printf("eos apply: %s", strings.Join(pr.cmds, " | "))
	}
	if err := c.t.Configure(ctx, cmds); err != nil {
		return 0, fmt.Errorf("eos apply (%d ports): %w", pr.changed, err)
	}
	// The snapshot is now stale for the ports just written; the next Collect
	// refreshes it. Mark it so a second ApplyPorts before that does not
	// re-issue the same commands.
	c.mu.Lock()
	// The admin speed is read with the startup commands (refreshStatic):
	// without a refresh the next collect would read the old speed back, and
	// every reconcile until the media refresh, 30 polls on, would write the
	// speed again.
	for _, cmd := range pr.cmds {
		if strings.HasPrefix(cmd, "speed ") {
			c.refresh = true
			break
		}
	}
	if c.last == snap {
		updated := *snap
		updated.Ports = append([]devicemodel.Port(nil), snap.Ports...)
		children := c.childCages(snap)
		markApplied(updated.Ports, desired, 0, children)
		c.last = &updated
		// A lane switch's cage took its admin state on every lane.
		for _, d := range desired {
			if l := c.lanes[d.Index]; children[d.Index] && len(l) > 0 {
				marked := slices.Clone(l)
				for i := range marked {
					marked[i].Enabled = d.Enabled
				}
				c.lanes = maps.Clone(c.lanes)
				c.lanes[d.Index] = marked
			}
		}
		c.markPortChannels(pr.pcVLAN)
		c.forgetPortChannels(pr.removed)
		if pr.joins != nil {
			lanes := map[int][]devicemodel.Port{}
			for k, v := range c.lanes {
				if !pr.joins[k] {
					lanes[k] = v
				}
			}
			c.lanes = lanes
		}
	}
	c.mu.Unlock()
	return pr.changed, nil
}

// markApplied records desired on ports after a batch (Index = the port's,
// or the lane's in a lane scope; base = the scope's channel-group base). A
// cage with a lane switch (child) took only its admin state and speed.
func markApplied(ports []devicemodel.Port, desired []devicemodel.PortDesired, base int, child map[int]bool) {
	for i := range ports {
		for _, d := range desired {
			if ports[i].Index != d.Index {
				continue
			}
			p := &ports[i]
			p.Enabled = d.Enabled
			if d.SpeedMbps > 0 || isCopperMedia(p.Media) {
				if base == 0 {
					p.AdminSpeed = d.SpeedMbps
				}
			}
			if child[d.Index] {
				continue
			}
			p.Description = d.Description
			if d.VLANSet {
				p.VLAN = desiredVLAN(d)
				p.LanesDiverge = false
			}
			if d.FEC != nil {
				p.FECConfig = *d.FEC
			}
			p.StormCtrl = d.StormCtrl
			if base == 0 {
				p.BPDUGuard = d.BPDUGuard
				p.STPEdge = d.STPEdge
			}
			if p.LAG != "" && p.LAGID == 0 {
				continue // another scope's aggregate: left alone
			}
			if p.LAGID != d.LAG {
				p.LACP = nil // read again at the next collect
			}
			p.LAGID = d.LAG
			if d.LAG > 0 {
				p.LAG = "Port-Channel" + strconv.Itoa(base+d.LAG)
			} else {
				p.LAG = ""
			}
			p.MirrorFrom = d.MirrorSource
		}
	}
}

// markPortChannels records the Port-Channels a batch wrote (c.mu held).
func (c *Collector) markPortChannels(written map[string]devicemodel.PortVLAN) {
	if len(written) == 0 {
		return
	}
	pcs := make(map[string]devicemodel.PortVLAN, len(c.portChannels)+len(written))
	for k, v := range c.portChannels {
		pcs[k] = v
	}
	for k, v := range written {
		pcs[k] = v
	}
	c.portChannels = pcs
}

// childCages are snap's split cages with a lane switch attached.
func (c *Collector) childCages(snap *devicemodel.Snapshot) map[int]bool {
	out := map[int]bool{}
	for _, p := range snap.Ports {
		if _, ok := c.children[p.Index]; ok && p.Lanes > 1 {
			out[p.Index] = true
		}
	}
	return out
}

// planScope says which ports a plan is for: the device's own (cage 0), or
// the lanes of one split cage for its lane switch (lanes.go).
type planScope struct {
	cage     int
	cageDown bool // the device's own push has the cage disabled: no lane comes up
}

// planResult is a plan: the commands between `configure` and `end`, the
// ports they change, the change count ApplyPorts reports, what the plan
// would not write and why, and the cages a join hands back whole.
type planResult struct {
	cmds    []string
	ports   []int
	changed int
	notes   []string
	joins   map[int]bool
	pcVLAN  map[string]devicemodel.PortVLAN // the Port-Channels written, as the batch leaves them
	removed []string                        // the Port-Channels the batch removes (portchannel.go)
}

// plan computes the batch for desired against snap. PlanPorts and
// ApplyPorts share it, so the loop's hold on a first push and the write
// itself see the same diff; ApplyLanes and PlanLanes share it with a lane
// scope (snap.Ports then the cage's lanes, Index = lane).
func (c *Collector) plan(snap *devicemodel.Snapshot, desired []devicemodel.PortDesired, sc planScope) planResult {
	var pr planResult
	note := func(format string, args ...any) { pr.notes = append(pr.notes, fmt.Sprintf(format, args...)) }
	inLanes := sc.cage > 0
	base := 0
	if inLanes {
		base = laneBase(sc.cage)
	}
	c.mu.Lock()
	channels := c.channelCfg
	child := map[int]bool{}
	childLanes := map[int][]devicemodel.Port{}
	if !inLanes {
		for _, p := range snap.Ports {
			if _, ok := c.children[p.Index]; ok && p.Lanes > 1 {
				child[p.Index] = true
				childLanes[p.Index] = c.lanes[p.Index]
			}
		}
	}
	c.mu.Unlock()

	live := map[int]devicemodel.Port{}
	for _, p := range snap.Ports {
		live[p.Index] = p
	}
	touched := map[int]bool{}
	mirror := mirrorCommands(snap.Ports, desired, base, child, note)
	if len(mirror) > 0 {
		pr.cmds = append(pr.cmds, mirror...)
		pr.changed++
		for _, d := range desired {
			if p, ok := live[d.Index]; ok && p.MirrorFrom != d.MirrorSource && !child[d.Index] {
				touched[d.Index] = true
			}
		}
	}
	lagVLAN := map[int]devicemodel.PortDesired{} // LAG id -> the VLAN intent its Port-Channel carries
	lagPorts := map[int][]int{}                  // LAG id -> its members' indexes
	members := map[string]bool{}                 // the Port-Channels with a member once the batch has run
	pushed := map[int]bool{}
	for _, d := range desired {
		pushed[d.Index] = true
	}
	for _, p := range snap.Ports {
		if !pushed[p.Index] && p.LAG != "" {
			members[p.LAG] = true
		}
	}
	for _, d := range desired {
		p, ok := live[d.Index]
		if !ok {
			continue // claimed model has more ports than the switch
		}
		if inLanes && sc.cageDown && d.Enabled {
			d.Enabled = false // the cage is disabled on the switch itself: every lane down
		}
		var portCmds []string
		if d.Enabled != p.Enabled {
			if d.Enabled {
				portCmds = append(portCmds, "no shutdown")
			} else {
				portCmds = append(portCmds, "shutdown")
			}
		}
		owned := child[d.Index] // the lanes' settings are the lane switch's
		if owned {
			if d.LAG > 0 {
				note("port %d is a lane switch's cage: it cannot join this switch's aggregate %d; not written", d.Index, d.LAG)
			}
			note("port %d: a lane switch sets its lanes; the cage's own name, VLANs, FEC and storm control are not applied", d.Index)
		}
		if !owned && d.Description != p.Description {
			if d.Description == "" {
				portCmds = append(portCmds, "no description")
			} else {
				portCmds = append(portCmds, "description "+sanitizeDescription(d.Description))
			}
		}
		foreign := p.LAG != "" && p.LAGID == 0 // another scope's channel group, or one made by hand
		lag := d.LAG
		if inLanes && lag >= laneGroupSpan {
			note("lane %d of cage %d: aggregate %d is past the %d this switch numbers per cage; not written", d.Index, sc.cage, lag, laneGroupSpan-1)
			lag = 0
		}
		switch {
		case owned:
		case foreign:
			note("port %d (%s) is in %s, which this plan does not own; its aggregate and VLANs are left alone", d.Index, laneNames(p)[0], p.LAG)
		case lag > 0:
			// VLAN config belongs on the Port-Channel; members inherit it.
			if _, seen := lagVLAN[lag]; !seen {
				lagVLAN[lag] = d
			}
			lagPorts[lag] = append(lagPorts[lag], d.Index)
			portCmds = append(portCmds, lagCommands(p, lag, base)...)
		default:
			portCmds = append(portCmds, vlanCommands(p, d)...)
			portCmds = append(portCmds, lagCommands(p, 0, base)...)
		}
		switch {
		case owned || foreign:
			if p.LAG != "" {
				members[p.LAG] = true
			}
		case lag > 0:
			members["Port-Channel"+strconv.Itoa(base+lag)] = true
		}
		if !owned {
			portCmds = append(portCmds, fecCommands(p, d)...)
			portCmds = append(portCmds, stormCommands(p, d)...)
		}
		if !inLanes {
			// STP port settings stay the cage's, on every lane, with a
			// lane switch too: the lane switch claims none of them.
			portCmds = append(portCmds, bpduGuardCommands(p, d)...)
			portCmds = append(portCmds, stpEdgeCommands(p, d)...)
		}
		// Speed on a QSFP cage (verified on the 7160): `speed forced
		// 25gfull`/`10gfull` on lane 1 splits the cage into 4 lanes and
		// `100gfull`/`40gfull` on lane 1 joins them back — but changing the
		// lane speed of an already split cage must reach every lane, or the
		// others go errdisabled "speed-misconfigured". So a join speed goes
		// to lane 1 only; a lane speed goes to every lane that exists.
		// Everything else goes to every lane, and a freshly split cage
		// picks up its VLAN/FEC/storm config on the next reconcile. A lane
		// of a lane switch never sets a speed: every lane of a cage runs
		// one, the cage's on this switch.
		var speedCmds []string
		if inLanes {
			if d.SpeedMbps > 0 && !slices.Contains(p.SpeedCaps, d.SpeedMbps) {
				note("lane %d of cage %d: %d Mb/s asked; on EOS every lane of a cage runs one speed (%v), set on the switch's own port %d; not written", d.Index, sc.cage, d.SpeedMbps, p.SpeedCaps, sc.cage)
			}
		} else {
			speedCmds = speedCommands(p, d)
		}
		var dissolve []string
		if owned && len(speedCmds) > 0 && d.SpeedMbps >= 40000 {
			var gone []string
			dissolve, gone = dissolveLanes(d.Index, childLanes[d.Index])
			pr.removed = append(pr.removed, gone...)
			if len(dissolve) > 0 {
				note("port %d: joining the cage removes its lane switch's aggregates and mirror sessions first", d.Index)
			}
			if pr.joins == nil {
				pr.joins = map[int]bool{}
			}
			pr.joins[d.Index] = true
		}
		if len(portCmds) == 0 && len(speedCmds) == 0 {
			continue
		}
		pr.changed++
		touched[d.Index] = true
		pr.cmds = append(pr.cmds, dissolve...)
		lanes := laneNames(p)
		speedEveryLane := d.SpeedMbps > 0 && d.SpeedMbps < 40000
		for i, ifname := range lanes {
			laneCmds := portCmds
			if i == 0 || speedEveryLane {
				laneCmds = append(append([]string(nil), speedCmds...), portCmds...)
			}
			if len(laneCmds) == 0 {
				continue
			}
			pr.cmds = append(pr.cmds, "interface "+ifname)
			pr.cmds = append(pr.cmds, laneCmds...)
		}
	}
	lags := make([]int, 0, len(lagVLAN))
	for lag := range lagVLAN {
		lags = append(lags, lag)
	}
	sort.Ints(lags)
	for _, lag := range lags {
		d := lagVLAN[lag]
		pc := devicemodel.Port{IfName: "Port-Channel" + strconv.Itoa(base+lag)}
		if pv, ok := c.portChannelVLAN(pc.IfName); ok {
			pc.VLAN = pv
		}
		if v := vlanCommands(pc, d); len(v) > 0 {
			if pr.pcVLAN == nil {
				pr.pcVLAN = map[string]devicemodel.PortVLAN{}
			}
			pr.pcVLAN[pc.IfName] = desiredVLAN(d)
			pr.cmds = append(pr.cmds, "interface "+pc.IfName)
			pr.cmds = append(pr.cmds, v...)
			pr.changed++
			for _, i := range lagPorts[lag] {
				touched[i] = true
			}
		}
	}
	// A Port-Channel of this plan's range that the batch leaves (or the
	// switch already had) without a member goes: EOS keeps it, settings
	// and all, when its last member leaves (portchannel.go).
	lo, hi := 1, 0
	if inLanes {
		lo, hi = base+1, base+laneGroupSpan-1
	} else {
		for _, p := range snap.Ports {
			hi = max(hi, p.Index) // the controller numbers aggregates within the switch's ports
		}
		hi = min(hi, laneGroupFloor-1)
	}
	remove, kept := staleChannels(channels, members, lo, hi)
	for _, n := range remove {
		name := "Port-Channel" + strconv.Itoa(n)
		pr.cmds = append(pr.cmds, "no interface "+name)
		pr.removed = append(pr.removed, name)
		pr.changed++
	}
	for _, n := range slices.Sorted(maps.Keys(kept)) {
		note("Port-Channel%d has no member and carries %q, which this driver never writes: left alone as made by hand", n, kept[n])
	}
	for i := range touched {
		pr.ports = append(pr.ports, i)
	}
	sort.Ints(pr.ports)
	return pr
}

// dissolveLanes removes a lane switch's channel groups and mirror sessions
// on cage idx ahead of the join that replaces its lanes. The join is the
// operator's decision on the switch itself, so the lane switch's own groups
// are this plan's to undo. EOS refuses to remove a session that does not
// exist, so only those the lanes report go. The groups' Port-Channels go
// after their members (EOS keeps an emptied one; portchannel.go): removed
// names them.
func dissolveLanes(idx int, lanes []devicemodel.Port) (cmds, removed []string) {
	base := laneBase(idx)
	for _, l := range lanes {
		if l.MirrorFrom > 0 {
			cmds = append(cmds, "no monitor session "+strconv.Itoa(base+l.Index))
		}
	}
	groups := map[int]bool{}
	for _, l := range lanes {
		if l.LAGID > 0 {
			cmds = append(cmds, "interface "+laneNames(l)[0], "no channel-group")
			groups[base+l.LAGID] = true
		}
	}
	for _, n := range slices.Sorted(maps.Keys(groups)) {
		name := "Port-Channel" + strconv.Itoa(n)
		cmds = append(cmds, "no interface "+name)
		removed = append(removed, name)
	}
	return cmds, removed
}

// PlanPorts implements devicemodel.Planner: the ports ApplyPorts would
// change for desired, without writing. The loop holds the first push after
// adoption while it would change any (the controller is seeded from the
// switch first), so a fresh adoption never resets the switch's ports.
func (c *Collector) PlanPorts(desired []devicemodel.PortDesired) []int {
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	if snap == nil {
		return nil
	}
	return c.plan(snap, desired, planScope{}).ports
}

// speedCommands: an explicit UniFi speed is forced; "auto" is written only
// on copper ports, because optical ports on this platform run forced
// (Ethernet49/1 is `speed forced 100gfull` by hand) and a blanket `speed
// auto` would drop the 100G links. An optical port reverts to auto only via
// the switch CLI.
func speedCommands(p devicemodel.Port, d devicemodel.PortDesired) []string {
	switch {
	case d.SpeedMbps > 0:
		if p.AdminSpeed == d.SpeedMbps && !p.LanesDiverge {
			return nil
		}
		kw := speedKeyword(d.SpeedMbps)
		if kw == "" {
			return nil
		}
		return []string{"speed forced " + kw}
	case isCopperMedia(p.Media) && p.AdminSpeed != 0:
		return []string{"speed auto"}
	}
	return nil
}

// desiredVLAN is the switch-side state d asks for.
func desiredVLAN(d devicemodel.PortDesired) devicemodel.PortVLAN {
	if !d.TaggedAll && len(d.TaggedVLANs) == 0 {
		return devicemodel.PortVLAN{Mode: "access", NativeVLAN: d.NativeVLAN}
	}
	v := devicemodel.PortVLAN{Mode: "trunk", NativeVLAN: d.NativeVLAN, AllowAll: d.TaggedAll}
	if !d.TaggedAll {
		v.Allowed = append([]int(nil), d.TaggedVLANs...)
		if d.NativeVLAN > 0 {
			v.Allowed = append(v.Allowed, d.NativeVLAN)
		}
		v.Allowed = uniqueSorted(v.Allowed)
	}
	return v
}

// vlanCommands diffs the port's 802.1Q state against d. UniFi's "Allow All"
// is an EOS trunk with every VLAN allowed; a custom tagged set is a trunk
// with an explicit allowed list (native included); "Block All" (native
// only) is an access port.
func vlanCommands(p devicemodel.Port, d devicemodel.PortDesired) []string {
	if !d.VLANSet || d.NativeVLAN == 0 {
		return nil
	}
	want := desiredVLAN(d)
	have := p.VLAN
	if !p.LanesDiverge && have.Mode == want.Mode && have.NativeVLAN == want.NativeVLAN && have.AllowAll == want.AllowAll &&
		(want.AllowAll || equalInts(have.Allowed, want.Allowed)) {
		return nil
	}
	var cmds []string
	if want.Mode == "access" {
		cmds = append(cmds, "switchport mode access", "switchport access vlan "+strconv.Itoa(want.NativeVLAN))
		return cmds
	}
	cmds = append(cmds, "switchport mode trunk")
	if want.NativeVLAN == 1 {
		cmds = append(cmds, "no switchport trunk native vlan")
	} else {
		cmds = append(cmds, "switchport trunk native vlan "+strconv.Itoa(want.NativeVLAN))
	}
	if want.AllowAll {
		cmds = append(cmds, "switchport trunk allowed vlan all")
	} else {
		cmds = append(cmds, "switchport trunk allowed vlan "+formatVLANList(want.Allowed))
	}
	return cmds
}

func uniqueSorted(ids []int) []int {
	sort.Ints(ids)
	out := ids[:0]
	for i, v := range ids {
		if i == 0 || v != ids[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// EnsureVLANs creates any of ids missing on the switch. UniFi's site VLAN
// list is the source of truth for existence; VLANs the switch has and UniFi
// does not are left alone.
func (c *Collector) EnsureVLANs(ctx context.Context, ids []int) (int, error) {
	unlock, err := c.lockApply(ctx)
	if err != nil {
		return 0, fmt.Errorf("eos ensure vlans: %w", err)
	}
	defer unlock()
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	if snap == nil {
		return 0, fmt.Errorf("eos ensure vlans: no snapshot yet")
	}
	have := map[int]bool{}
	for _, v := range snap.VLANs {
		have[v] = true
	}
	var missing []int
	for _, id := range ids {
		if id > 0 && id < 4095 && !have[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}
	cmds := []string{"enable", "configure", "vlan " + formatVLANList(missing), "end", "write memory"}
	if err := c.t.Configure(ctx, cmds); err != nil {
		return 0, fmt.Errorf("eos create vlans %v: %w", missing, err)
	}
	c.mu.Lock()
	if c.last == snap {
		updated := *snap
		updated.VLANs = uniqueSorted(append(append([]int(nil), snap.VLANs...), missing...))
		c.last = &updated
	}
	c.mu.Unlock()
	return len(missing), nil
}

// sanitizeDescription keeps an EOS description to one printable line. EOS
// accepts up to 240 characters; the CLI treats a newline as the end of the
// command, so strip control characters rather than risk command injection
// through a port name typed in the UniFi UI.
func sanitizeDescription(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 240 {
		out = out[:240]
	}
	return out
}

// portChannelVLAN reads a Port-Channel's 802.1Q state from the snapshot's
// switchport table (kept per collector for the diff).
func (c *Collector) portChannelVLAN(name string) (devicemodel.PortVLAN, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.portChannels[name]
	return v, ok
}
