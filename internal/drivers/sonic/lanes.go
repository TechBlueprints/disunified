package sonic

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The parent half of a lane switch (devicemodel.LaneProvider;
// docs/drivers/breakout.md). A split cage's lanes are kept as ports of
// their own beside the fold (model.go), a lane switch reads them from the
// last collect (Lanes) and writes them through this driver's own plan,
// scoped to the cage (ApplyLanes: control.go's plan with laneCage set).
//
// Ownership of a split cage with a lane switch attached ("owned"):
//   - the lane switch: each lane's admin state, name, VLANs, lane speed,
//     FEC, MTU, storm control, aggregates (PortChannel<100*cage+lag>) and
//     mirror sessions between its lanes;
//   - this device: the cage's speed (the breakout: split, re-split, join),
//     the cage's admin state (disabled = every lane down), and what the
//     driver computes switch-wide from its own ports -- the STP port
//     settings disunified-l2d takes (l2.go: the cage's, on every bridge
//     port its lanes make, a lane aggregate included), isolation, port
//     security, the egress limit and LLDP-MED, which still apply to every
//     lane of the cage as before.
// A join removes the lane switch's aggregates and mirror sessions first and
// hands the cage back whole; a split gives the new lanes the cage's intent,
// which the lane switch then seeds the controller from.

// lanePCBase is a cage's first lane-switch PortChannel id: the lane
// switch's aggregate headed by its lane L is PortChannel<100*cage+L>. The
// device's own aggregates are PortChannel<lowest member's index>, below
// 100 on every SONiC switch this driver knows (the DX010 has 32 cages), so
// either side tells the other's by the number alone.
func lanePCBase(cage int) int { return 100 * cage }

// laneMACs fills each split cage's lanes with the FDB entries learned on
// them (a lane aggregate's on its lowest member), PortIndex = the lane.
func laneMACs(db map[string]table, ports []cagePort, native int, linkDown func(string) bool) {
	key := map[string]int{} // interface -> cage*1000 + lane
	for _, p := range ports {
		for _, l := range p.lanes {
			key[l.Interfaces[0]] = p.Index*1000 + l.Index
		}
	}
	if len(key) == 0 {
		return
	}
	for pc, k := range lagPorts(db, key) {
		key[pc] = k
	}
	for _, m := range buildMACTable(db["fdb"], key, native, linkDown) {
		if m.PortIndex == 0 {
			continue
		}
		cage, lane := m.PortIndex/1000, m.PortIndex%1000
		for i := range ports {
			if ports[i].Index != cage {
				continue
			}
			for j := range ports[i].lanes {
				if ports[i].lanes[j].Index == lane {
					m.PortIndex = lane
					ports[i].lanes[j].MACs = append(ports[i].lanes[j].MACs, m)
				}
			}
		}
	}
}

// reportChildren is the parent's report of an attached lane switch on its
// split cage: the child's MAC in the cage's MAC table and the switch's, the
// child as the cage's LLDP neighbour (the hosts' own neighbours are the
// lanes', on the lane switch), the cage at its native speed (the cable's
// capacity: the folded lane speed would read as a 25G link to a switch)
// and in the cage's own form (its speed is the breakout control, and the
// controller takes the join's 100G or 40G with a FEC only from a QSFP28;
// the lanes' FEC is set on the lane switch's SFP28 ports:
// docs/breakout-lag.md 7), and no aggregate or mirror of the cage's own
// (those are the lanes').
// Either entry places the child (docs/breakout-lag.md 6.6); both are sent,
// as informloop's children: knob sends them.
func reportChildren(ports []cagePort, macs []devicemodel.MACEntry, children map[int]devicemodel.Attachment, now time.Time) []devicemodel.MACEntry {
	for i := range ports {
		a, ok := children[ports[i].Index]
		if !ok || len(ports[i].lanes) < 2 {
			continue
		}
		p := &ports[i].Port
		p.Neighbor = &devicemodel.Neighbor{ChassisID: a.MAC, SystemName: a.Name, PortID: "eth0", IsBridge: true, Child: true}
		for _, s := range p.SpeedCaps {
			if p.Up && s > p.SpeedMbps {
				p.SpeedMbps = s
			}
		}
		p.Media = cageForm(p.SpeedCaps)
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

// lockApply takes the write lock: one write (or collect) at a time, so two
// plans never interleave their batches or diff against a state the other
// is changing. It gives up when ctx does. Every write that touches what a
// plan reads or writes takes it, and so do the address move (it swaps the
// runner and saves the config) and the reboot (it waits for a batch under
// way). Three stay outside on purpose: the fan mode (pmon's fancontrol
// files), the SSH keys (the login user's authorized_keys) and locate (an
// LED, and its blink runs between calls) share nothing with a plan.
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
	if cage < 1 || cage > 99 {
		return fmt.Errorf("sonic: cage %d: a lane switch's aggregates are PortChannel<100*cage+lane>, so the cage must be 1..99", cage)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last != nil && !slices.ContainsFunc(c.last.Ports, func(p devicemodel.Port) bool { return p.Index == cage }) {
		return fmt.Errorf("sonic: %s has no port %d", c.host, cage)
	}
	if c.children == nil {
		c.children = map[int]devicemodel.Attachment{}
	}
	c.children[cage] = a
	return nil
}

// Lanes implements devicemodel.LaneProvider: the cage's lanes as of the
// last collect, a lane aggregate's id as the lane switch's own (its lowest
// member's lane number), any other PortChannel by name only.
func (c *Collector) Lanes(cage int) (devicemodel.LaneView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap, st := c.last, c.state
	if snap == nil || st == nil {
		return devicemodel.LaneView{}, fmt.Errorf("sonic: no snapshot yet")
	}
	i := slices.IndexFunc(snap.Ports, func(p devicemodel.Port) bool { return p.Index == cage })
	if i < 0 {
		return devicemodel.LaneView{}, fmt.Errorf("sonic: %s has no port %d", c.host, cage)
	}
	lanes := c.lanes[cage]
	if len(lanes) < 2 {
		return devicemodel.LaneView{}, fmt.Errorf("sonic: cage %d of %s is not split", cage, c.host)
	}
	v := devicemodel.LaneView{
		TakenAt: snap.TakenAt, System: snap.System, Cage: snap.Ports[i], CageAlias: c.aliases[cage],
		ChassisID: c.chassisID, VLANs: slices.Clone(snap.VLANs), CageDisabled: c.cageDown[cage], Child: c.children[cage],
	}
	// The lane switch's uplink is the cable in the cage, so the view keeps
	// the cage's own form: the device's port reports its lanes' (model.go).
	v.Cage.Media = cageForm(v.Cage.SpeedCaps)
	var modeSpeeds []int
	if parent, _, ok := st.platformCage(cage); ok {
		v.Mode = st.brkout[parent]
		if m, ok := parseMode(v.Mode); ok {
			modeSpeeds = m.speeds
			sort.Ints(modeSpeeds)
		}
	}
	base := lanePCBase(cage)
	for _, l := range lanes {
		p := l.Port
		p.MACs = slices.Clone(l.MACs)
		if n := lagID(p.LAG); p.LAG != "" && n > base && n < base+100 {
			p.LAGID = n - base
		} else {
			p.LAGID = 0
		}
		if len(p.SpeedCaps) == 0 {
			p.SpeedCaps = modeSpeeds
		}
		v.Lanes = append(v.Lanes, p)
		v.LaneAliases = append(v.LaneAliases, l.alias)
	}
	return v, nil
}

// ApplyLanes implements devicemodel.LaneProvider: the plan of ApplyPorts,
// scoped to the cage's lanes, then the switch-wide applies that follow a
// port batch (the ACL tables bind every port and PortChannel, the reserved
// groups follow VLAN membership).
func (c *Collector) ApplyLanes(ctx context.Context, cage int, desired []devicemodel.PortDesired) (int, error) {
	unlock, err := c.lockApply(ctx)
	if err != nil {
		return 0, fmt.Errorf("sonic apply (cage %d lanes): %w", cage, err)
	}
	defer unlock()
	pl, err := c.planFor(cage, desired, false)
	if err != nil {
		return 0, err
	}
	n, err := c.runPlan(ctx, pl, fmt.Sprintf("cage %d lanes", cage))
	if err != nil || n == 0 {
		return n, err
	}
	// A lane aggregate made or dissolved is a new kernel bridge port: l2d
	// takes the cage's STP settings for it now, not at this device's next
	// push (l2SetPorts).
	if err := c.l2Refresh(ctx); err != nil {
		return n, err
	}
	if _, err := c.applyACL(ctx); err != nil {
		return n, err
	}
	if _, err := c.applyIGMP(ctx); err != nil {
		return n, err
	}
	return n, nil
}

// l2Refresh derives l2d's per-port intent again from this device's last
// port push against the state as it is now, and writes it if it changed.
// Nothing before this device's first push: the intent is its own.
func (c *Collector) l2Refresh(ctx context.Context) error {
	c.mu.Lock()
	st, desired, set := c.state, c.l2.desired, c.l2.portsSet
	c.mu.Unlock()
	if st == nil || st.stpFeature || !set {
		return nil
	}
	c.l2SetPorts(desired, st)
	_, err := c.applyL2(ctx)
	return err
}

// PlanLanes implements devicemodel.LaneProvider (the first-push hold).
func (c *Collector) PlanLanes(cage int, desired []devicemodel.PortDesired) []int {
	pl, err := c.planFor(cage, desired, true)
	if err != nil {
		return nil
	}
	out := make([]int, 0, len(pl.changed))
	for idx := range pl.changed {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}

// noteCageDown records, from this device's own push, which cages with a
// lane switch the controller has disabled: every lane of such a cage is
// down whatever the lane switch asks (ApplyLanes reads it).
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

// laneSpeed is a lane's speed: forced, on that lane only, within the
// speeds the lane runs; auto (0) leaves the lane at what it runs. A lane
// never splits or joins its cage: that is the cage's speed on the parent.
func (pl *plan) laneSpeed(p devicemodel.Port, d devicemodel.PortDesired) {
	want := d.SpeedMbps
	if want == 0 {
		return
	}
	ln := lanesOf(p)[0]
	if !containsInt(p.SpeedCaps, want) {
		pl.note("lane %d (%s) of cage %d: %d Mb/s is not among the lane's speeds %v; a lane does not split or join its cage (that is the cage's speed on the switch itself); speed not written", d.Index, ln, pl.laneCage, want, p.SpeedCaps)
		return
	}
	if isTrue(pl.sim.field(ln, "autoneg")) {
		pl.add(d.Index, fmt.Sprintf("config interface autoneg %s disabled", ln), func(s *swState) { s.setField(ln, "autoneg", "off") })
	}
	if atoi(pl.sim.field(ln, "speed")) != want {
		v := strconv.Itoa(want)
		pl.add(d.Index, fmt.Sprintf("config interface speed %s %s", ln, v), func(s *swState) { s.setField(ln, "speed", v) })
	}
}

// dissolveLanes removes a lane switch's aggregates and lane mirror
// sessions on cage idx ahead of a breakout that replaces its lanes (members
// out, mirrors, then each PortChannel's VLANs and ACL bindings and the
// PortChannel itself): SONiC's
// -f drops a member silently and leaves its PortChannel behind, and the
// breakout is otherwise refused for a cage in an aggregate (breakoutBlocked).
// The join is the operator's decision on the switch itself, so the lane
// switch's own groups are this plan's to undo.
func (pl *plan) dissolveLanes(idx, cage int, write func(int, string, func(*swState))) {
	base := lanePCBase(cage)
	inRange := func(n int) bool { return n > base && n < base+100 }
	emptied := map[string]bool{}
	for _, ln := range pl.sim.cagePorts(cage) {
		lane := ln
		if pc := pl.sim.lagOf(lane); pc != "" && inRange(lagID(pc)) {
			write(idx, fmt.Sprintf("config portchannel member del %s %s", pc, lane), func(s *swState) { delete(s.lags[pc], lane) })
			emptied[pc] = true
		}
	}
	var names []string
	for name := range pl.sim.mirrors {
		if n, err := strconv.Atoi(strings.TrimPrefix(name, "dui")); err == nil && strings.HasPrefix(name, "dui") && inRange(n) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		nm := name
		write(idx, "config mirror_session remove "+nm, func(s *swState) { delete(s.mirrors, nm) })
	}
	var pcs []string
	for pc := range emptied {
		if len(pl.sim.lags[pc]) == 0 {
			pcs = append(pcs, pc)
		}
	}
	sort.Strings(pcs)
	// The driver's ACL tables bind every PortChannel: each leaves them
	// before it goes. `config portchannel del` does not look at ACL_TABLE,
	// so a table would keep naming it: orchagent then holds the LAG
	// ("Failed to remove ref count 1 LAG"), and the breakout after it is
	// refused by sonic-yang ("Invalid value ... in \"ports\" element"),
	// both seen live on the DX010 (sonic.md §11.25).
	if len(pcs) > 0 {
		pl.aclUnbind(idx, pcs, "is to be deleted", nil) // its lanes go with the breakout that follows
	}
	for _, pc := range pcs {
		name := pc
		pl.stripVLANs(idx, name)
		write(idx, "config portchannel del "+name, func(s *swState) {
			delete(s.lags, name)
			delete(s.members, name)
			delete(s.lagMTU, name)
			delete(s.lagLACP, name)
		})
	}
	if len(pcs) > 0 || len(names) > 0 {
		pl.note("port %d: joining the cage removes its lane switch's aggregates %v and mirror sessions %v first", idx, pcs, names)
	}
}

var _ devicemodel.LaneProvider = (*Collector)(nil)
