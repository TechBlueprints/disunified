package aristaeos

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The parent half of a lane switch (devicemodel.LaneProvider;
// docs/drivers/breakout.md §7b). A split cage's lanes are kept as ports of
// their own beside the fold: the collect runs every per-interface reading a
// second time with laneKey, each lane its own port (buildLanes). A lane
// switch reads them from the last collect (Lanes) and writes them through
// this driver's own plan, scoped to the cage (ApplyLanes: plan with a
// planScope naming the cage).
//
// Ownership of a split cage with a lane switch attached, as on sonic:
//   - the lane switch: each lane's admin state, name, VLANs, FEC, storm
//     control, aggregates (Port-Channel<laneBase(cage)+lag>) and mirror
//     sessions between its lanes (session laneBase(cage)+destination lane);
//   - this device: the cage's speed (the breakout: split, join, the lane
//     speed every lane runs), the cage's admin state (disabled = every lane
//     down), and the cage's STP port settings (BPDU guard, edge), still
//     written to every lane: the lane switch claims none of them, so the
//     controller pushes it none.
// A join removes the lane switch's channel groups and mirror sessions first.

// Lane switches' channel groups and monitor sessions on EOS. 4.26.14M takes
// `channel-group <1-2000>` (live 2026-11-15: 2000 accepted, 2001 and 5402
// "Invalid input"), so the spec's 100*cage+lag (5401 for cage 54) cannot
// be used here: cage C's are laneBase(C)+1..+9, 1001..1999 for cages 0..99.
// The device's own aggregates and sessions are numbered by a port index
// (below 1000 on any EOS switch with fewer than 1000 ports), so either
// side tells the other's by the number alone. Monitor sessions are names
// on EOS ("1542" accepted the same day) and follow the same rule.
const (
	laneGroupFloor = 1000
	laneGroupCeil  = 2000
	laneGroupSpan  = 10
)

func laneBase(cage int) int { return laneGroupFloor + laneGroupSpan*cage }

// ownSession: a monitor session of the device's own (fold) ports.
func ownSession(n int) bool { return n < laneGroupFloor }

// laneSession: a monitor session of some cage's lane switch.
func laneSession(n int) bool { return n >= laneGroupFloor && n < laneGroupCeil }

// ifKey maps an EOS interface name to the port it is read into: the
// front-panel slot for the fold (parseEthName), or a lane port of its own.
type ifKey func(name string) (idx, lane int, ok bool)

// laneIdx is a lane's port index while the lanes are built (cage*100 +
// EOS lane number); buildLanes renumbers them 1..N per cage.
func laneIdx(cage, lane int) int { return cage*100 + lane }

// buildLanes reads the lanes of every split cage of fold, each a port of
// its own, by running build with a key that gives every lane its own index.
// The result is by cage, lanes in EOS lane order, Index 1..N.
func (c *Collector) buildLanes(fold []devicemodel.Port, build func(ifKey, func(int) bool, *devicemodel.System) ([]devicemodel.Port, []devicemodel.MACEntry, error)) (map[int][]devicemodel.Port, error) {
	split := map[int]bool{}
	for _, p := range fold {
		if p.Lanes > 1 {
			split[p.Index] = true
		}
	}
	if len(split) == 0 {
		return nil, nil
	}
	key := func(name string) (int, int, bool) {
		slot, lane, ok := parseEthName(name)
		if !ok || lane < 1 || !split[slot] {
			return 0, 0, false
		}
		return laneIdx(slot, lane), 1, true
	}
	var scratch devicemodel.System
	ports, _, err := build(key, laneSession, &scratch)
	if err != nil {
		return nil, err
	}
	byCage := map[int][]devicemodel.Port{}
	for _, p := range ports {
		byCage[p.Index/100] = append(byCage[p.Index/100], p)
	}
	out := map[int][]devicemodel.Port{}
	for cage, lanes := range byCage {
		sort.Slice(lanes, func(i, j int) bool { return lanes[i].Index < lanes[j].Index })
		pos := map[int]int{} // laneIdx -> 1..N
		for i, l := range lanes {
			pos[l.Index] = i + 1
		}
		speed := 0 // the one speed the cage's lanes run (EOS: a cage property)
		for _, l := range lanes {
			if speed == 0 {
				speed = l.AdminSpeed
			}
		}
		base := laneBase(cage)
		for i := range lanes {
			l := &lanes[i]
			l.MirrorFrom = pos[l.MirrorFrom] // 0 for a source outside the cage
			for j := range l.MACs {
				l.MACs[j].PortIndex = i + 1
			}
			l.Index = i + 1
			l.Lanes = 1
			l.Optic = nil // the module sits in the cage: the lane switch reports it on its cable port
			if n := l.LAGID; n > base && n < base+laneGroupSpan {
				l.LAGID = n - base
			} else {
				l.LAGID = 0
			}
			s := speed
			if s == 0 {
				s = l.SpeedMbps
			}
			if s > 0 {
				l.SpeedCaps = []int{s}
			}
			l.Media = devicemodel.MediaSFP28
			if s >= 50000 {
				l.Media = devicemodel.MediaQSFP28
			}
		}
		out[cage] = lanes
	}
	return out, nil
}

// flatLanes is every lane of lanes, for the readings run once over both the
// fold and the lanes (the ACL counters, the PHY text).
func flatLanes(lanes map[int][]devicemodel.Port) []devicemodel.Port {
	cages := make([]int, 0, len(lanes))
	for k := range lanes {
		cages = append(cages, k)
	}
	sort.Ints(cages)
	var out []devicemodel.Port
	for _, k := range cages {
		out = append(out, lanes[k]...)
	}
	return out
}

// unflatLanes writes flat (flatLanes' order, after the readings) back.
func unflatLanes(lanes map[int][]devicemodel.Port, flat []devicemodel.Port) {
	cages := make([]int, 0, len(lanes))
	for k := range lanes {
		cages = append(cages, k)
	}
	sort.Ints(cages)
	i := 0
	for _, k := range cages {
		n := len(lanes[k])
		lanes[k] = flat[i : i+n]
		i += n
	}
}

// finishLanes completes the fold once the lanes are read: a cage in a lane
// switch's channel group keeps the group's name but no aggregate id of the
// device's own (its plan leaves it alone), and an attached lane switch is
// reported on its split cage (reportChildren).
func (c *Collector) finishLanes(snap *devicemodel.Snapshot) {
	for i := range snap.Ports {
		if snap.Ports[i].LAGID >= laneGroupFloor {
			snap.Ports[i].LAGID = 0
		}
	}
	c.mu.Lock()
	children := c.children
	c.mu.Unlock()
	snap.MACTable = reportChildren(snap.Ports, snap.MACTable, children, snap.TakenAt)
}

// reportChildren is the parent's report of an attached lane switch on its
// split cage, as sonic's: the child's MAC in the cage's MAC table and the
// switch's, the child as the cage's LLDP neighbour (the hosts' neighbours
// are the lanes', on the lane switch), the cage at its native speed (the
// highest it claims: the folded lane speed would read as a 25G link to a
// switch), and no aggregate or mirror of the cage's own.
func reportChildren(ports []devicemodel.Port, macs []devicemodel.MACEntry, children map[int]devicemodel.Attachment, now time.Time) []devicemodel.MACEntry {
	for i := range ports {
		p := &ports[i]
		a, ok := children[p.Index]
		if !ok || p.Lanes < 2 {
			continue
		}
		p.Neighbor = &devicemodel.Neighbor{ChassisID: a.MAC, SystemName: a.Name, PortID: "eth0", IsBridge: true, Child: true}
		for _, s := range p.SpeedCaps {
			if p.Up && s > p.SpeedMbps {
				p.SpeedMbps = s
			}
		}
		p.LAG, p.LAGID, p.LACP, p.MirrorFrom = "", 0, nil, 0
		vlan := p.VLAN.NativeVLAN
		if vlan == 0 {
			vlan = 1
		}
		e := devicemodel.MACEntry{MAC: a.MAC, VLAN: vlan, PortIndex: p.Index, LastSeen: now}
		has := func(m devicemodel.MACEntry) bool { return m.MAC == a.MAC }
		if !slices.ContainsFunc(p.MACs, has) {
			p.MACs = append(p.MACs, e)
		}
		if !slices.ContainsFunc(macs, has) {
			macs = append(macs, e)
		}
	}
	return macs
}

// lockApply takes the write lock: one write (or collect) at a time, so the
// device's loop and a lane switch's never interleave their batches or diff
// against a state the other is changing. It gives up when ctx does.
func (c *Collector) lockApply(ctx context.Context) (func(), error) {
	c.applyOnce.Do(func() { c.applySem = make(chan struct{}, 1) })
	select {
	case c.applySem <- struct{}{}:
		return func() { <-c.applySem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for another write to this switch: %w", ctx.Err())
	}
}

// AttachLaneSwitch implements devicemodel.LaneProvider.
func (c *Collector) AttachLaneSwitch(cage int, a devicemodel.Attachment) error {
	if cage < 1 || laneBase(cage)+laneGroupSpan-1 > laneGroupCeil-1 {
		return fmt.Errorf("arista-eos: cage %d: a lane switch's channel groups are %d+10*cage+lag and EOS stops at %d, so the cage must be 1..%d", cage, laneGroupFloor, laneGroupCeil, (laneGroupCeil-laneGroupFloor)/laneGroupSpan-1)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last != nil && !slices.ContainsFunc(c.last.Ports, func(p devicemodel.Port) bool { return p.Index == cage }) {
		return fmt.Errorf("arista-eos: the switch has no port %d", cage)
	}
	// A new map each time: finishLanes reads the old one outside the lock,
	// and an attach can come from the lane switch's loop while this device
	// collects (an adoption, docs/drivers/breakout.md 16).
	next := make(map[int]devicemodel.Attachment, len(c.children)+1)
	for k, v := range c.children {
		next[k] = v
	}
	next[cage] = a
	c.children = next
	return nil
}

// DetachLaneSwitch implements devicemodel.LaneProvider: the inverse of
// AttachLaneSwitch. The cage is the fold again from the next collect and
// the next plan; the lane switch's VLAN list and its view of the cage's
// disabled state go with it (docs/drivers/breakout.md 7c for what it left).
func (c *Collector) DetachLaneSwitch(cage int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.children[cage]; ok {
		// finishLanes reads the map outside the lock: replace it, never
		// change the one a collect may be holding.
		next := make(map[int]devicemodel.Attachment, len(c.children))
		for k, v := range c.children {
			if k != cage {
				next[k] = v
			}
		}
		c.children = next
	}
	delete(c.cageDown, cage)
	delete(c.laneVLANs, cage)
}

// SplitCages implements devicemodel.LaneProvider.
func (c *Collector) SplitCages() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		return nil
	}
	var out []int
	for _, p := range c.last.Ports {
		if p.Lanes > 1 && len(c.lanes[p.Index]) > 1 {
			out = append(out, p.Index)
		}
	}
	slices.Sort(out)
	return out
}

// LocalChassisID is what the switch sends as its own LLDP chassis id (`show
// lldp local-info`, read at start).
func (c *Collector) LocalChassisID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chassisID
}

// Lanes implements devicemodel.LaneProvider: the cage's lanes as of the
// last collect.
func (c *Collector) Lanes(cage int) (devicemodel.LaneView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.last
	if snap == nil {
		return devicemodel.LaneView{}, fmt.Errorf("arista-eos: no snapshot yet")
	}
	i := slices.IndexFunc(snap.Ports, func(p devicemodel.Port) bool { return p.Index == cage })
	if i < 0 {
		return devicemodel.LaneView{}, fmt.Errorf("arista-eos: the switch has no port %d", cage)
	}
	lanes := c.lanes[cage]
	if len(lanes) < 2 {
		return devicemodel.LaneView{}, fmt.Errorf("arista-eos: cage %d (%s) is not split", cage, snap.Ports[i].IfName)
	}
	v := devicemodel.LaneView{
		TakenAt: snap.TakenAt, System: snap.System, Cage: snap.Ports[i],
		ChassisID: c.chassisID, VLANs: slices.Clone(snap.VLANs), CageDisabled: c.cageDown[cage], Child: c.children[cage],
		Mode: laneMode(lanes),
	}
	for _, l := range lanes {
		p := l
		p.MACs = slices.Clone(l.MACs)
		v.Lanes = append(v.Lanes, p)
		v.LaneAliases = append(v.LaneAliases, "") // EOS has no faceplate alias but the name
	}
	return v, nil
}

// laneMode names the cage's layout as EOS runs it: "4x25G".
func laneMode(lanes []devicemodel.Port) string {
	if len(lanes) == 0 || len(lanes[0].SpeedCaps) == 0 {
		return ""
	}
	return fmt.Sprintf("%dx%dG", len(lanes), lanes[0].SpeedCaps[0]/1000)
}

// lanePlan is the plan of ApplyPorts scoped to a cage's lanes.
func (c *Collector) lanePlan(cage int, desired []devicemodel.PortDesired) (*devicemodel.Snapshot, planResult, error) {
	c.mu.Lock()
	snap, lanes, down := c.last, c.lanes[cage], c.cageDown[cage]
	c.mu.Unlock()
	if snap == nil {
		return nil, planResult{}, fmt.Errorf("arista-eos: no snapshot yet")
	}
	if len(lanes) < 2 {
		return nil, planResult{}, fmt.Errorf("arista-eos: cage %d is not split", cage)
	}
	scoped := *snap
	scoped.Ports = lanes
	return &scoped, c.plan(&scoped, desired, planScope{cage: cage, cageDown: down}), nil
}

// ApplyLanes implements devicemodel.LaneProvider: the plan of ApplyPorts,
// scoped to the cage's lanes, as one batch.
func (c *Collector) ApplyLanes(ctx context.Context, cage int, desired []devicemodel.PortDesired) (int, error) {
	unlock, err := c.lockApply(ctx)
	if err != nil {
		return 0, fmt.Errorf("eos apply (cage %d lanes): %w", cage, err)
	}
	defer unlock()
	_, pr, err := c.lanePlan(cage, desired)
	if err != nil {
		return 0, err
	}
	c.logNotes(pr.notes)
	if pr.changed == 0 {
		return 0, nil
	}
	cmds := append(append([]string{"enable", "configure"}, pr.cmds...), "end", "write memory")
	if c.Log != nil {
		c.Log.Printf("eos apply (cage %d lanes): %s", cage, strings.Join(pr.cmds, " | "))
	}
	if err := c.t.Configure(ctx, cmds); err != nil {
		return 0, fmt.Errorf("eos apply (cage %d lanes, %d changes): %w", cage, pr.changed, err)
	}
	c.mu.Lock()
	if l := c.lanes[cage]; len(l) > 0 {
		marked := slices.Clone(l)
		markApplied(marked, desired, laneBase(cage), nil)
		c.lanes[cage] = marked
	}
	c.markPortChannels(pr.pcVLAN)
	c.forgetPortChannels(pr.removed)
	c.mu.Unlock()
	return pr.changed, nil
}

// PlanLanes implements devicemodel.LaneProvider (the first-push hold).
func (c *Collector) PlanLanes(cage int, desired []devicemodel.PortDesired) []int {
	_, pr, err := c.lanePlan(cage, desired)
	if err != nil {
		return nil
	}
	return pr.ports
}

// noteCageDown records, from this device's own push, which cages with a
// lane switch the controller has disabled: every lane of such a cage is
// down whatever the lane switch asks.
func (c *Collector) noteCageDown(desired []devicemodel.PortDesired) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range desired {
		if _, ok := c.children[d.Index]; !ok {
			continue
		}
		if c.cageDown == nil {
			c.cageDown = map[int]bool{}
		}
		c.cageDown[d.Index] = !d.Enabled
	}
}

// logNotes logs each of a plan's notes the first time it is seen: the loop
// re-applies every cycle, and a refusal does not change until the push does.
func (c *Collector) logNotes(notes []string) {
	c.mu.Lock()
	var fresh []string
	for _, n := range notes {
		if c.noted == nil {
			c.noted = map[string]bool{}
		}
		if !c.noted[n] {
			c.noted[n] = true
			fresh = append(fresh, n)
		}
	}
	c.mu.Unlock()
	for _, n := range fresh {
		c.logf("eos apply: %s", n)
	}
}

var _ devicemodel.LaneProvider = (*Collector)(nil)
