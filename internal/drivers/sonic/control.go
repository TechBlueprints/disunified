package sonic

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// The port controller (docs/drivers/sonic.md §6.2): every write is a
// `sudo config …` command (or `sudo sonic-db-cli` for a description, which
// has no CLI on 202505), diffed against CONFIG_DB as the last collect read
// it, run as one `bash -s` batch under `set -e`, and closed with
// `sudo config save -y` so a reboot keeps the controller's intent.
//
// Order within one apply: mirror removals, LAG leaves, per-port state, VLAN
// membership, LAG joins and the LAG's own membership, mirror adds, save.

// defaultMTU is SONiC's own port and PortChannel MTU.
const defaultMTU = 9100

// lacpWanted is the LACP setting of every PortChannel the driver manages:
// fast rate (1 s LACPDUs) and min-links 1, as UniFi's own SONiC switches
// run theirs (docs/drivers/sonic.md §9). `config portchannel add` defaults
// to slow rate.
var lacpWanted = lacp{fastRate: "true", minLinks: "1"}

// op is one planned write: the command, the port it belongs to (0 = none),
// and its effect on CONFIG_DB, applied to the plan's copy as it is planned
// and to the collector's state once the batch has run.
type op struct {
	cmd    string
	idx    int
	effect func(*swState)
}

type plan struct {
	sim     *swState
	ports   map[int]devicemodel.Port
	native  int
	site    []int // SONiC VLAN ids "Allow All" tags, from EnsureVLANs
	ops     []op
	changed map[int]bool
	// own are the ports written only for the driver's own reading of the
	// push -- the FEC and the speed a module calls for where UniFi says
	// auto (§10.3, §10.9).
	own map[int]bool
	// controllerOnly plans without those writes at all (PlanPorts): the
	// first-push hold asks whether the controller's config would change a
	// port, and a module-driven join, planned, would leave the rejoined
	// port's state to re-apply on every cycle.
	controllerOnly bool
	notes          []string // refusals, logged once each by ApplyPorts
	// bridgeWait: an interface that leaves its last VLAN waits for its
	// bridge port to go before anything else uses it (bridgePortWait): an
	// image with l2mcd running and without the kit's sonic-swss 0004
	// (§8.15).
	bridgeWait bool
	appl       []applOp // APPL_DB writes, run together at the end (portfeat.go)

	// The lane scope (ApplyLanes, lanes.go): ports are one cage's lanes
	// keyed by lane number, aggregates and mirror sessions are numbered
	// from pcBase, and cageDown is the cage disabled in the device's own
	// config. laneCage 0 is the device's own plan, whose owned cages are
	// the split ones a lane switch sets the lanes of.
	laneCage int
	pcBase   int
	cageDown bool
	owned    map[int]bool
}

// pc is the PortChannel of the plan's aggregate lag.
func (pl *plan) pc(lag int) string { return pcName(pl.pcBase + lag) }

// mine: pc is in this plan's range -- the device's own aggregates (ids
// below 100) or, in the lane scope, this cage's lane-switch aggregates.
// Any other PortChannel is left as it is.
func (pl *plan) mine(pc string) bool {
	n := lagID(pc)
	if pl.laneCage == 0 {
		return n > 0 && n < lanePCBase(1)
	}
	return n > pl.pcBase && n < pl.pcBase+100
}

func (pl *plan) add(idx int, cmd string, effect func(*swState)) {
	pl.ops = append(pl.ops, op{cmd: "sudo " + cmd, idx: idx, effect: effect})
	if effect != nil {
		effect(pl.sim)
	}
	if idx > 0 {
		pl.changed[idx] = true
	}
}

// addOwn is add for a write the controller did not ask for (plan.own).
func (pl *plan) addOwn(idx int, cmd string, effect func(*swState)) {
	pl.ops = append(pl.ops, op{cmd: "sudo " + cmd, idx: idx, effect: effect})
	if effect != nil {
		effect(pl.sim)
	}
	pl.own[idx] = true
}

func (pl *plan) note(format string, args ...any) {
	pl.notes = append(pl.notes, fmt.Sprintf(format, args...))
}

func pcName(lag int) string { return "PortChannel" + strconv.Itoa(lag) }

func lanesOf(p devicemodel.Port) []string {
	if len(p.Interfaces) > 0 {
		return p.Interfaces
	}
	return []string{p.IfName}
}

// newPlan diffs desired against the collector's state without writing;
// controllerOnly leaves out the module-driven writes (plan.controllerOnly).
func (c *Collector) newPlan(desired []devicemodel.PortDesired, controllerOnly bool) (*plan, error) {
	return c.planFor(0, desired, controllerOnly)
}

// planFor is newPlan for the device's own ports (cage 0) or, for a lane
// switch, for one split cage's lanes (lanes.go).
func (c *Collector) planFor(cage int, desired []devicemodel.PortDesired, controllerOnly bool) (*plan, error) {
	caps := c.Capabilities()
	c.mu.Lock()
	snap, st, site := c.last, c.state, append([]int(nil), c.siteVLANs...)
	if c.siteKnown && site == nil {
		site = []int{} // the push names no VLAN but 1: known, not missing
	}
	if cage > 0 {
		// A lane switch's Allow All tags what its own push lists; before
		// its first EnsureVLANs (a held first push) that is unknown, and
		// the device's own push says nothing about the lanes.
		site = nil
		if l, ok := c.laneVLANs[cage]; ok {
			site = append([]int{}, l...)
		}
	}
	lanes := c.lanes[cage]
	down := c.cageDown[cage]
	var attached []int
	for idx := range c.children {
		attached = append(attached, idx)
	}
	c.mu.Unlock()
	if snap == nil || st == nil {
		return nil, fmt.Errorf("sonic apply: no snapshot yet")
	}
	c.mu.Lock()
	fixed := c.l2mcFixed
	c.mu.Unlock()
	pl := &plan{sim: st.clone(), ports: map[int]devicemodel.Port{}, native: c.NativeVLAN, site: site, changed: map[int]bool{}, own: map[int]bool{}, controllerOnly: controllerOnly, owned: map[int]bool{}}
	pl.bridgeWait = st.l2mcKnown && st.l2mcFeature == "enabled" && !fixed
	if cage == 0 {
		for _, p := range snap.Ports {
			pl.ports[p.Index] = p
		}
		for _, idx := range attached {
			if len(st.cagePorts(idx)) > 1 {
				pl.owned[idx] = true
			}
		}
	} else {
		var names []string
		for _, l := range lanes {
			names = append(names, l.Interfaces[0])
		}
		if len(names) < 2 || !slices.Equal(names, st.cagePorts(cage)) {
			return nil, fmt.Errorf("sonic apply: cage %d is not split as the last collect saw it (lanes %v, CONFIG_DB %v)", cage, names, st.cagePorts(cage))
		}
		var optic *devicemodel.Optic
		for _, p := range snap.Ports {
			if p.Index == cage {
				optic = p.Optic
			}
		}
		for _, l := range lanes {
			p := l.Port
			p.Optic = optic // the module FEC where the lane switch says auto
			if len(p.SpeedCaps) == 0 {
				if parent, _, ok := st.platformCage(cage); ok {
					if m, ok := parseMode(st.brkout[parent]); ok {
						p.SpeedCaps = m.speeds
					}
				}
			}
			pl.ports[p.Index] = p
		}
		pl.laneCage, pl.pcBase, pl.cageDown = cage, lanePCBase(cage), down
	}
	if pl.site == nil {
		for id := range st.vlans {
			if id != pl.native {
				pl.site = append(pl.site, id)
			}
		}
		sort.Ints(pl.site)
	}

	// Ports the switch has, minus the ones the native-id collision refuses.
	var ds []devicemodel.PortDesired
	for _, d := range desired {
		if _, ok := pl.ports[d.Index]; !ok {
			continue // the claimed model has more ports than the switch
		}
		if bad := pl.collides(d); bad != 0 {
			pl.note("port %d: VLAN %d is this switch's native_vlan (UniFi's VLAN 1 is Vlan%d here); port not written -- set options.native_vlan to an id no UniFi network uses", d.Index, bad, pl.native)
			continue
		}
		switch {
		case pl.laneCage > 0 && d.Routed:
			pl.note("lane %d of cage %d: a routed lane is not something this driver writes; its VLANs are left as they are", d.Index, pl.laneCage)
			d.Routed, d.VLANSet = false, false
		case pl.owned[d.Index]:
			// The lanes' aggregates and mirrors are the lane switch's.
			if d.LAG > 0 || d.MirrorSource > 0 || d.Routed {
				pl.note("port %d: a lane switch sets this cage's lanes; the cage's own aggregate, mirror and routing are not applied", d.Index)
			}
			d.LAG, d.MirrorSource, d.Routed = 0, 0, false
		}
		if d.VLANSet && d.NativeVLAN != 1 && containsInt(d.TaggedVLANs, 1) {
			// VLAN 1 is Vlan<native_vlan> as an untagged member; tagging it
			// would put the stand-in id on the wire.
			pl.note("port %d: VLAN 1 tagged (native VLAN %d) cannot be expressed on SONiC, where UniFi's VLAN 1 is Vlan%d untagged; the port's VLANs are left as they are", d.Index, d.NativeVLAN, pl.native)
			d.VLANSet = false
		}
		ds = append(ds, d)
	}

	pl.mirrorRemovals(ds)
	pl.lagLeaves(ds)
	for _, d := range ds {
		pl.portState(d)
	}
	for _, d := range ds {
		if d.LAG > 0 || d.MirrorSource > 0 {
			continue // VLANs live on the PortChannel; a mirror destination has none
		}
		if pl.owned[d.Index] {
			continue // the lane switch's
		}
		lanes := lanesOf(pl.ports[d.Index])
		if d.Routed && len(lanes) > 1 {
			pl.note("port %d: a routed port on a split cage (%d ports) is not something this driver writes; its VLANs are left as they are", d.Index, len(lanes))
			continue
		}
		for _, lane := range lanes {
			if pl.foreignLAG(lane, "") {
				continue // its VLANs are its PortChannel's, which this plan does not own
			}
			pl.syncVLANs(d.Index, lane, d)
		}
	}
	pl.lagJoins(ds)
	pl.mirrorAdds(ds)
	if pl.laneCage == 0 {
		// Both are switch-wide: the VLANs' MTU follows the device's own
		// ports, and isolation, port security, the egress limit and
		// LLDP-MED are computed over every port the device has.
		pl.vlanMTUs(ds)
		pl.portFeatures(ds, caps)
	}
	if len(pl.owned) > 0 && pl.laneCage == 0 {
		var owned []int
		for idx := range pl.owned {
			owned = append(owned, idx)
		}
		sort.Ints(owned)
		pl.note("ports %v: a lane switch sets these cages' lanes; the cage keeps its speed (the breakout) and its admin state, its other settings are not applied to the lanes", owned)
	}
	return pl, nil
}

// vlanMTUs brings the VLANs the driver manages -- the site's and the
// native stand-in -- to the ports' MTU when the push sets one (jumbo; the
// lowest pushed MTU should ports ever differ). SONiC creates every VLAN at
// 9100 and leaves it there: the VLAN table's mtu reaches APPL_DB and the
// router interface (orchagent), but vlanmgrd never applies it to the
// kernel netdev (a TODO in 202505 and 202511), so the switch's own TCP
// over Vlan<id> ran at MSS 9048 behind 1500-byte ports and lost every full
// segment -- the on-switch bridge's inform never got out (live, 2026-11-02,
// docs/drivers/sonic.md §12). Both are written: the CONFIG_DB field, which
// config save keeps, and the kernel netdev through `ip link set`, capped at
// the Bridge's MTU (a VLAN device cannot exceed its parent), which a swss
// restart or a reboot resets and the next apply restores. A netdev the
// last collect did not see (a VLAN made this cycle) waits for the next.
func (pl *plan) vlanMTUs(ds []devicemodel.PortDesired) {
	want := 0
	for _, d := range ds {
		if d.MTU > 0 && (want == 0 || d.MTU < want) {
			want = d.MTU
		}
	}
	if want == 0 {
		return
	}
	kernel := want
	if b := pl.sim.netMTU["Bridge"]; b > 0 && b < kernel {
		kernel = b
	}
	ids := append([]int{pl.native}, pl.site...)
	sort.Ints(ids)
	for _, id := range ids {
		if !pl.sim.vlans[id] {
			continue
		}
		dev := "Vlan" + strconv.Itoa(id)
		if cur := atoi(pl.sim.vlanMTU[id]); cur != want && (cur != 0 || want != defaultMTU) {
			m := strconv.Itoa(want)
			pl.add(0, "sonic-db-cli CONFIG_DB hset "+sshrun.ShellQuote("VLAN|"+dev)+" mtu "+m, func(s *swState) { s.vlanMTU[id] = m })
		}
		if cur := pl.sim.netMTU[dev]; cur > 0 && cur != kernel {
			pl.add(0, fmt.Sprintf("ip link set dev %s mtu %d", dev, kernel), func(s *swState) { s.netMTU[dev] = kernel })
		}
	}
}

// collides: the native-id hazard of §5.2a. A desired port that names
// native_vlan itself (tagged, or as its native) would be read back as VLAN 1.
func (pl *plan) collides(d devicemodel.PortDesired) int {
	if !d.VLANSet {
		return 0
	}
	if d.NativeVLAN == pl.native {
		return pl.native
	}
	for _, v := range d.TaggedVLANs {
		if v == pl.native {
			return pl.native
		}
	}
	return 0
}

// sonicID maps a UniFi VLAN id onto the switch's: 1 is Vlan<native_vlan>.
func (pl *plan) sonicID(id int) int {
	if id == 1 {
		return pl.native
	}
	return id
}

// --- mirror ---

func (pl *plan) mirrorSource(d devicemodel.PortDesired) (string, bool) {
	src, ok := pl.ports[d.MirrorSource]
	if !ok || d.MirrorSource == d.Index {
		return "", false
	}
	return strings.Join(lanesOf(src), ","), true
}

func (pl *plan) mirrorRemovals(ds []devicemodel.PortDesired) {
	for _, d := range ds {
		if pl.owned[d.Index] {
			continue // a lane mirror session is the lane switch's
		}
		dst := lanesOf(pl.ports[d.Index])[0]
		cur := pl.sim.mirrorTo(dst)
		if cur == "" {
			continue
		}
		if d.MirrorSource > 0 {
			if src, ok := pl.mirrorSource(d); ok && pl.sim.mirrors[cur]["src_port"] == src {
				continue // already as wanted
			}
		}
		name := cur
		pl.add(d.Index, "config mirror_session remove "+name, func(s *swState) { delete(s.mirrors, name) })
	}
}

func (pl *plan) mirrorAdds(ds []devicemodel.PortDesired) {
	for _, d := range ds {
		if d.MirrorSource == 0 {
			continue
		}
		src, ok := pl.mirrorSource(d)
		if !ok {
			pl.note("port %d: mirror source port %d does not exist on this switch; not mirrored", d.Index, d.MirrorSource)
			continue
		}
		dst := lanesOf(pl.ports[d.Index])[0]
		if cur := pl.sim.mirrorTo(dst); cur != "" && pl.sim.mirrors[cur]["src_port"] == src {
			continue
		}
		if pl.sim.portMirrorUnsupported() {
			pl.note("port %d: mirroring port %d is not possible on this switch: its SAI reports that a port cannot be attached to a mirror session (STATE_DB SWITCH_CAPABILITY PORT_INGRESS/EGRESS_MIRROR_CAPABLE false), and SONiC then creates the session without its source port; not mirrored", d.Index, d.MirrorSource)
			continue
		}
		name := "dui" + strconv.Itoa(pl.pcBase+d.Index)
		if _, taken := pl.sim.mirrors[name]; taken {
			pl.add(d.Index, "config mirror_session remove "+name, func(s *swState) { delete(s.mirrors, name) })
		}
		// SONiC refuses a destination with VLAN config.
		pl.stripVLANs(d.Index, dst)
		set := func(s *swState) {
			s.mirrors[name] = map[string]string{"type": "SPAN", "dst_port": dst, "src_port": src, "direction": "BOTH"}
		}
		pl.add(d.Index, fmt.Sprintf("config mirror_session span add %s %s %s both", name, dst, src), set)
	}
}

// --- LAG ---

func (pl *plan) lagLeaves(ds []devicemodel.PortDesired) {
	for _, d := range ds {
		if pl.owned[d.Index] {
			continue // the lanes' aggregates are the lane switch's
		}
		for _, lane := range lanesOf(pl.ports[d.Index]) {
			cur := pl.sim.lagOf(lane)
			if cur == "" || (d.LAG > 0 && cur == pl.pc(d.LAG)) {
				continue
			}
			if !pl.mine(cur) {
				pl.note("port %d: %s is in %s, an aggregate this config does not own (a lane switch's, the switch's own, or one made by hand); left as it is", d.Index, lane, cur)
				continue
			}
			pc, ln := cur, lane
			pl.add(d.Index, fmt.Sprintf("config portchannel member del %s %s", pc, ln), func(s *swState) { delete(s.lags[pc], ln) })
		}
	}
}

func (pl *plan) lagJoins(ds []devicemodel.PortDesired) {
	byLAG := map[int][]devicemodel.PortDesired{}
	var ids []int
	for _, d := range ds {
		if d.LAG > 0 {
			if byLAG[d.LAG] == nil {
				ids = append(ids, d.LAG)
			}
			byLAG[d.LAG] = append(byLAG[d.LAG], d)
		}
	}
	sort.Ints(ids)
	// `config portchannel member add` refuses a port an ACL table binds
	// (get_port_acl_binding; the DX010, 2026-11-02), and the driver's own
	// tables bind every port: the joining lanes leave them first, and
	// applyACL binds the PortChannel once the batch has made it (acl.go).
	var joining []string
	joinTo := map[string][]string{}
	for _, lag := range ids {
		for _, d := range byLAG[lag] {
			for _, lane := range lanesOf(pl.ports[d.Index]) {
				if !pl.sim.lags[pl.pc(lag)][lane] && !pl.foreignLAG(lane, pl.pc(lag)) {
					joining = append(joining, lane)
					joinTo[lane] = []string{pl.pc(lag)}
				}
			}
		}
	}
	if len(joining) > 0 {
		pl.aclUnbind(byLAG[ids[0]][0].Index, joining, "is to join an aggregate", joinTo)
	}
	for _, lag := range ids {
		pc := pl.pc(lag)
		members := byLAG[lag]
		for _, d := range members {
			for _, lane := range lanesOf(pl.ports[d.Index]) {
				if pl.sim.lags[pc][lane] || pl.foreignLAG(lane, pc) {
					continue // already in, or held by an aggregate lagLeaves left alone
				}
				ln := lane
				laneMTU := pl.sim.field(ln, "mtu")
				if _, exists := pl.sim.lags[pc]; !exists {
					// A new PortChannel is 9100; `config interface mtu` refuses
					// a PortChannel ("Invalid port"), so its MTU is set in
					// CONFIG_DB (APPL_DB LAG_TABLE follows) before anyone joins.
					pl.add(d.Index, "config portchannel add "+pc+" --fast-rate "+lacpWanted.fastRate+" --min-links "+lacpWanted.minLinks, func(s *swState) {
						s.lags[pc] = map[string]bool{}
						s.lagMTU[pc] = strconv.Itoa(defaultMTU)
						s.lagLACP[pc] = lacpWanted
					})
				}
				// SONiC refuses a member whose MTU differs from the PortChannel's.
				switch {
				case laneMTU == "" || laneMTU == pl.sim.lagMTU[pc]:
				case len(pl.sim.lags[pc]) == 0:
					pl.setLAGMTU(d.Index, pc, laneMTU)
				default:
					m := pl.sim.lagMTU[pc]
					pl.add(d.Index, fmt.Sprintf("config interface mtu %s %s", ln, m), func(s *swState) { s.setField(ln, "mtu", m) })
				}
				// SONiC refuses a member with VLAN config.
				pl.stripVLANs(d.Index, lane)
				pl.add(d.Index, fmt.Sprintf("config portchannel member add %s %s", pc, ln), func(s *swState) { s.lags[pc][ln] = true })
			}
		}
		if _, exists := pl.sim.lags[pc]; exists {
			pl.setLAGLACP(members[0].Index, pc)
			// The PortChannel carries its members' VLAN intent (the first's).
			pl.syncVLANs(members[0].Index, pc, members[0])
		}
	}
	// A PortChannel this apply emptied goes, its VLANs and the ACL tables
	// binding it first (sonic.md §11.25: `config portchannel del` leaves a
	// table naming it, orchagent then keeps the LAG, and a breakout's YANG
	// check refuses the dangling name). So does a
	// memberless one the driver made and nobody wants: a batch that made
	// it and then failed at the member add (the ACL refusal above, before
	// the unbind) left exactly that on the DX010 (2026-11-02).
	wanted := map[string]bool{}
	for _, lag := range ids {
		wanted[pl.pc(lag)] = true
	}
	var pcs []string
	for pc, m := range pl.sim.lags {
		if len(m) == 0 && !wanted[pc] && pl.mine(pc) && (pl.emptiedHere(pc) || pl.ownOrphan(pc)) {
			pcs = append(pcs, pc)
		}
	}
	sort.Strings(pcs)
	if len(pcs) > 0 {
		// An operator's table that bound the PortChannel binds the
		// members it had instead (they left it in this batch).
		back := map[string][]string{}
		for _, pc := range pcs {
			back[pc] = pl.leftMembers(pc)
		}
		pl.aclUnbind(0, pcs, "is to be deleted", back)
	}
	for _, pc := range pcs {
		name := pc
		pl.stripVLANs(0, name)
		pl.add(0, "config portchannel del "+name, func(s *swState) {
			delete(s.lags, name)
			delete(s.members, name)
			delete(s.lagMTU, name)
			delete(s.lagLACP, name)
		})
	}
}

// foreignLAG: lane is in a PortChannel other than pc that this plan does
// not own, which SONiC would refuse to move it out of by a member add.
func (pl *plan) foreignLAG(lane, pc string) bool {
	cur := pl.sim.lagOf(lane)
	return cur != "" && cur != pc && !pl.mine(cur)
}

// setLAGMTU sets a PortChannel's MTU in CONFIG_DB (probed: APPL_DB
// LAG_TABLE follows; `config interface mtu PortChannel<n>` is "Invalid
// port").
func (pl *plan) setLAGMTU(idx int, pc, mtu string) {
	if pl.sim.lagMTU[pc] == mtu {
		return
	}
	pl.add(idx, "sonic-db-cli CONFIG_DB hset "+sshrun.ShellQuote("PORTCHANNEL|"+pc)+" mtu "+mtu, func(s *swState) { s.lagMTU[pc] = mtu })
}

// setLAGLACP brings a PortChannel made without lacpWanted (by hand, or by
// the driver before it passed the options) in line, once, in CONFIG_DB.
// teammgrd reads fast_rate and min_links only when it creates the team, so
// the running LAG takes them at its next start (reboot, `config reload`,
// teamd restart); the saved config holds them from now on.
func (pl *plan) setLAGLACP(idx int, pc string) {
	if pl.sim.lagLACP[pc] == lacpWanted {
		return
	}
	pl.add(idx, "sonic-db-cli CONFIG_DB hset "+sshrun.ShellQuote("PORTCHANNEL|"+pc)+" fast_rate "+lacpWanted.fastRate+" min_links "+lacpWanted.minLinks,
		func(s *swState) { s.lagLACP[pc] = lacpWanted })
}

// memberMTUWritten: this plan moved pc's MTU to m, so every member's own
// MTU follows it (the second member of a LAG sees the PortChannel already
// moved by the first).
func (pl *plan) memberMTUWritten(pc, m string) bool {
	want := "sonic-db-cli CONFIG_DB hset " + sshrun.ShellQuote("PORTCHANNEL|"+pc) + " mtu " + m
	for _, o := range pl.ops {
		if strings.HasSuffix(o.cmd, want) {
			return true
		}
	}
	return false
}

// ownOrphan: pc has no members and the LACP settings only the driver's
// `config portchannel add` writes (fast-rate true, min-links 1); a
// PortChannel someone made by hand keeps SONiC's defaults and is left.
func (pl *plan) ownOrphan(pc string) bool {
	return len(pl.sim.lags[pc]) == 0 && pl.sim.lagLACP[pc] == lacpWanted
}

// emptiedHere: a member del for pc is among the planned writes.
// leftMembers are the interfaces this plan took out of pc, still ports.
func (pl *plan) leftMembers(pc string) []string {
	var out []string
	for _, o := range pl.ops {
		if lane, ok := strings.CutPrefix(o.cmd, "sudo config portchannel member del "+pc+" "); ok && pl.sim.port[lane] != nil {
			out = append(out, lane)
		}
	}
	return out
}

func (pl *plan) emptiedHere(pc string) bool {
	for _, o := range pl.ops {
		if strings.HasPrefix(o.cmd, "sudo config portchannel member del "+pc+" ") {
			return true
		}
	}
	return false
}

// --- VLANs ---

// wantVLANs is the membership d asks of an interface, in SONiC ids:
// UniFi's native 1 is an untagged member of Vlan<native_vlan>; "Allow All"
// tags every VLAN the controller lists; "Block All" is the native only.
func (pl *plan) wantVLANs(idx int, d devicemodel.PortDesired) (map[int]bool, bool) {
	if d.Routed {
		// A routed port is a port in no VLAN (its address is l3.go's).
		return map[int]bool{}, true
	}
	if !d.VLANSet || (d.NativeVLAN == 0 && !d.TaggedAll && len(d.TaggedVLANs) == 0) {
		return nil, false
	}
	want := map[int]bool{}
	untagged := 0
	if d.NativeVLAN > 0 {
		untagged = pl.sonicID(d.NativeVLAN)
		want[untagged] = false
	}
	var tagged []int
	if d.TaggedAll {
		tagged = pl.site
	} else {
		for _, v := range d.TaggedVLANs {
			tagged = append(tagged, pl.sonicID(v))
		}
	}
	for _, id := range tagged {
		if id == untagged || id <= 1 || id > 4094 || (d.TaggedAll && id == pl.native) {
			continue
		}
		want[id] = true
	}
	for id := range want {
		if !pl.sim.vlans[id] {
			pl.note("port %d: VLAN %d does not exist on the switch; membership skipped", idx, id)
			delete(want, id)
		}
	}
	return want, true
}

// syncVLANs diffs an interface's memberships: removals first (tagged, then
// a changed untagged one, so a native change is del then add -u), then the
// untagged add, then the tagged adds. Several VLANs go in one command with
// -m, and "every VLAN but these" with -e (§6.2, corrections after pass 1):
// a config call costs 200-250 ms, and Allow All on 31 ports one membership
// at a time would blow the loop's 20 s apply budget.
func (pl *plan) syncVLANs(idx int, iface string, d devicemodel.PortDesired) {
	want, ok := pl.wantVLANs(idx, d)
	if !ok {
		return
	}
	if !d.Routed && len(want) > 0 && pl.sim.routerIface(iface) {
		// `config vlan member add` refuses a router interface ("is a
		// router interface!"), which under set -e would fail the batch: a
		// routed port returning to switching keeps its VLANs off until
		// the L3 apply has removed its address (it runs first, l3.go).
		pl.note("port %d: %s still carries an address (a routed port returning to switching); its VLANs wait for the address to go", idx, iface)
		return
	}
	have := pl.sim.members[iface]
	if pl.sim.bridgeDown[iface] && !sameVLANs(have, want) {
		// Every membership change would land on (or keep retrying against)
		// the bridge port orchagent refused to remove (§8.15).
		pl.note("port %d: %s's bridge port is held admin down in the chip (orchagent refused its removal: L2MC references, docs/drivers/sonic.md §8.15); its VLANs are left as they are until a swss restart or a reboot clears it", idx, iface)
		return
	}
	var delTagged, addTagged []int
	delUntagged, addUntagged, untaggedAfter := 0, 0, 0
	for id, tagged := range have {
		if w, keep := want[id]; keep && w == tagged {
			if !tagged {
				untaggedAfter = id
			}
			continue
		}
		if tagged {
			delTagged = append(delTagged, id)
		} else {
			delUntagged = id
		}
	}
	for id, tagged := range want {
		if h, ok := have[id]; ok && h == tagged {
			continue
		}
		if tagged {
			addTagged = append(addTagged, id)
		} else {
			addUntagged, untaggedAfter = id, id
		}
	}
	sort.Ints(delTagged)
	sort.Ints(addTagged)
	pl.vlanMembers(idx, iface, "del", delTagged, untaggedAfter, delUntagged)
	if delUntagged > 0 {
		pl.vlanMembers(idx, iface, "del", []int{delUntagged}, 0, 0)
	}
	if addUntagged > 0 {
		vid, ifc := addUntagged, iface
		pl.add(idx, fmt.Sprintf("config vlan member add -u %d %s", vid, ifc), func(s *swState) { s.setMember(ifc, vid, false) })
	}
	pl.vlanMembers(idx, iface, "add", addTagged, untaggedAfter, 0)
}

// vlanMembers adds (tagged) or deletes the memberships ids of one interface
// in one command: the plain form for one VLAN, `-m a,b,c` for several, and
// `-e -m <native>[,<untagged>]` when ids are exactly every VLAN on the
// switch except the native one and the port's untagged one -- Allow All, or
// dropping every tagged membership. -e takes whatever VLANs exist at that
// moment, so a switch holding a VLAN outside ids gets the explicit list.
// alsoGoing is an untagged VLAN being deleted right after (it may be
// excluded too).
func (pl *plan) vlanMembers(idx int, iface, verb string, ids []int, untagged, alsoGoing int) {
	if len(ids) == 0 {
		return
	}
	in := map[int]bool{}
	for _, id := range ids {
		in[id] = true
	}
	allowed := map[int]bool{pl.native: true, untagged: true, alsoGoing: true}
	var except []int
	exceptOK := len(ids) > 1
	for id := range pl.sim.vlans {
		if in[id] {
			continue
		}
		if !allowed[id] {
			exceptOK = false
		}
		except = append(except, id)
	}
	sort.Ints(except)
	if !containsInt(except, pl.native) {
		exceptOK = false // -e needs at least the native excluded; nothing else qualifies
	}
	var form string
	switch {
	case exceptOK:
		form = "-e -m " + joinInts(except)
	case len(ids) > 1:
		form = "-m " + joinInts(ids)
	default:
		form = strconv.Itoa(ids[0])
	}
	ifc, list, add := iface, append([]int(nil), ids...), verb == "add"
	if !add {
		// The CLI refuses to take a port out of a VLAN while a static group
		// names it there ("remove all associated static Layer 2 multicast
		// group entries first"); applyIGMP rewrites the groups after the
		// batch.
		for _, id := range list {
			for _, k := range pl.sim.staticsOf(ifc, id) {
				key := k
				pl.add(idx, "sonic-db-cli CONFIG_DB del "+sshrun.ShellQuote(key), func(s *swState) { delete(s.l2mc, key) })
			}
		}
	}
	pl.add(idx, fmt.Sprintf("config vlan member %s %s %s", verb, form, ifc), func(s *swState) {
		for _, id := range list {
			if add {
				s.setMember(ifc, id, true)
			} else {
				s.delMember(ifc, id)
			}
		}
	})
	if !add && pl.bridgeWait && len(pl.sim.members[ifc]) == 0 {
		// The interface's last VLAN went: orchagent removes its bridge port,
		// and without sonic-swss 0004 it refuses while L2MC members still
		// name the port -- after setting it admin down -- and the next VLAN
		// added reuses it as it is (§8.15). Nothing else uses the interface
		// until the bridge port is gone.
		w := bridgePortWait(ifc)
		pl.ops = append(pl.ops, op{cmd: w, idx: idx})
	}
}

// sameVLANs: an interface's memberships are what the plan wants.
func sameVLANs(have, want map[int]bool) bool {
	if len(have) != len(want) {
		return false
	}
	for id, tagged := range want {
		if h, ok := have[id]; !ok || h != tagged {
			return false
		}
	}
	return true
}

func joinInts(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

// stripVLANs removes every membership of an interface (before it joins a
// LAG, becomes a mirror destination, or a PortChannel is deleted).
func (pl *plan) stripVLANs(idx int, iface string) {
	pl.vlanMembers(idx, iface, "del", pl.sim.memberIDs(iface), 0, 0)
}

// --- per-port state ---

func (pl *plan) portState(d devicemodel.PortDesired) {
	p := pl.ports[d.Index]
	if pl.laneCage > 0 {
		pl.laneSpeed(p, d)
	} else {
		pl.speed(p, d, lanesOf(p))
	}
	// A breakout replaced the cage's interfaces: the rest of its intent goes
	// on the new ones (admin state, name, FEC, MTU, storm control, STP) --
	// a cage a lane switch owned too, which the breakout handed back.
	lanes := lanesOf(pl.ports[d.Index])
	owned := pl.owned[d.Index]
	want := d.Enabled && !pl.cageDown
	for _, lane := range lanes {
		ln := lane
		if owned {
			// The cage's admin state is the one control it keeps over its
			// lanes besides the breakout: disabled takes every lane down,
			// enabled leaves each lane to the lane switch.
			if !d.Enabled && pl.sim.field(ln, "admin_status") == "up" {
				pl.add(d.Index, "config interface shutdown "+ln, func(s *swState) { s.setField(ln, "admin_status", "down") })
			}
			pl.stpOps(d, ln)
			continue
		}
		if enabled := pl.sim.field(ln, "admin_status") == "up"; enabled != want {
			if want {
				pl.add(d.Index, "config interface startup "+ln, func(s *swState) { s.setField(ln, "admin_status", "up") })
			} else {
				pl.add(d.Index, "config interface shutdown "+ln, func(s *swState) { s.setField(ln, "admin_status", "down") })
			}
		}
		if want := sanitizeDescription(d.Description); want != pl.sim.field(ln, "description") {
			key := sshrun.ShellQuote("PORT|" + ln)
			if want == "" {
				// hset '' first so APPL_DB follows; a bare hdel leaves its copy.
				pl.add(d.Index, "sonic-db-cli CONFIG_DB hset "+key+" description ''", nil)
				pl.add(d.Index, "sonic-db-cli CONFIG_DB hdel "+key+" description", func(s *swState) { s.setField(ln, "description", "") })
			} else {
				pl.add(d.Index, "sonic-db-cli CONFIG_DB hset "+key+" description "+sshrun.ShellQuote(want), func(s *swState) { s.setField(ln, "description", want) })
			}
		}
		// FEC: as pushed; where UniFi says auto (no port.N.fec), what the
		// cage's module calls for (§10.3).
		fec, write := d.FEC, pl.add
		if fec == nil {
			fec, write = pl.moduleFEC(d.Index, ln), pl.addOwn
		} else if sup := pl.sim.fecs[ln]; len(sup) > 0 && !sup[fecKeyword(*fec)] {
			// portconfig refuses a mode outside supported_fecs ("fec fc is
			// not in ['none', 'rs']", exit 1), which under set -e would fail
			// the whole batch and leave the rest of the push unapplied, as
			// FC-FEC on a QSFP28 cage of the DX010 did (§11.8).
			pl.note("port %d: the controller asks for FEC %s, which %s does not support (%s); FEC left as it is", d.Index, fecKeyword(*fec), ln, strings.Join(sortedSet(sup), ","))
			fec = nil
		}
		if fec != nil && *fec != fecOf(pl.sim.field(ln, "fec")) {
			if kw := fecKeyword(*fec); kw != "" && !(kw == "none" && pl.sim.field(ln, "fec") == "") {
				write(d.Index, fmt.Sprintf("config interface fec %s %s", ln, kw), func(s *swState) { s.setField(ln, "fec", kw) })
			}
		}
		// Jumbo is switch-wide in UniFi and a port MTU on SONiC: the pushed
		// switch.mtu (9216, accepted by SONiC) when on, 1500 when off. A LAG
		// member runs at its PortChannel's MTU, and SONiC refuses `config
		// interface mtu` on a member whatever the value ("Error:
		// 'interface_name' is in portchannel!", probed): the PortChannel moves
		// through CONFIG_DB and the member's own PORT mtu with it, by hset
		// too (APPL_DB follows, probed). A member whose PortChannel already
		// runs the wanted MTU is satisfied.
		if d.MTU > 0 {
			want := d.MTU
			m := strconv.Itoa(want)
			if pc := pl.sim.lagOf(ln); pc != "" && !pl.mine(pc) {
				// A member of an aggregate this plan does not own runs at that
				// PortChannel's MTU, which is its owner's.
			} else if pc != "" {
				if atoi(pl.sim.lagMTU[pc]) != want {
					pl.setLAGMTU(d.Index, pc, m)
					if atoi(pl.sim.field(ln, "mtu")) != want {
						pl.add(d.Index, "sonic-db-cli CONFIG_DB hset "+sshrun.ShellQuote("PORT|"+ln)+" mtu "+m, func(s *swState) { s.setField(ln, "mtu", m) })
					}
				} else if pl.memberMTUWritten(pc, m) && atoi(pl.sim.field(ln, "mtu")) != want {
					pl.add(d.Index, "sonic-db-cli CONFIG_DB hset "+sshrun.ShellQuote("PORT|"+ln)+" mtu "+m, func(s *swState) { s.setField(ln, "mtu", m) })
				}
			} else if atoi(pl.sim.field(ln, "mtu")) != want {
				pl.add(d.Index, fmt.Sprintf("config interface mtu %s %s", ln, m), func(s *swState) { s.setField(ln, "mtu", m) })
			}
		}
		pl.stormOps(d, ln)
		if pl.laneCage == 0 {
			// A lane's STP port settings are its cage's (l2.go keys them by
			// the cage; the PVST path the same).
			pl.stpOps(d, ln)
		}
	}
}

// moduleFEC is the FEC the module in idx's cage calls for at ln's
// configured speed (§10.3), nil for an empty cage, a module or speed the
// table does not know, or a mode STATE_DB's supported_fecs leaves out.
func (pl *plan) moduleFEC(idx int, ln string) *devicemodel.FEC {
	o := pl.ports[idx].Optic
	if o == nil || pl.controllerOnly {
		return nil
	}
	speed := atoi(pl.sim.field(ln, "speed"))
	f, ok := moduleFEC(o.MediaType, speed)
	if !ok {
		pl.note("port %d: no FEC known for the module %q at %d Mb/s on %s; FEC left as it is (set it in the UI to choose one)", idx, o.MediaType, speed, ln)
		return nil
	}
	if sup := pl.sim.fecs[ln]; len(sup) > 0 && !sup[fecKeyword(f)] {
		pl.note("port %d: the module %q calls for FEC %s, which %s does not support (%s); FEC left as it is", idx, o.MediaType, fecKeyword(f), ln, strings.Join(sortedSet(sup), ","))
		return nil
	}
	return &f
}

func fecKeyword(f devicemodel.FEC) string {
	switch f {
	case devicemodel.FECRS:
		return "rs"
	case devicemodel.FECFC:
		return "fc"
	case devicemodel.FECDisabled:
		return "none"
	}
	return ""
}

func isCopper(m devicemodel.Media) bool {
	switch m {
	case devicemodel.MediaCopper1G, devicemodel.MediaCopper2G5, devicemodel.MediaCopper10G:
		return true
	}
	return false
}

// speed: an explicit speed is forced (autoneg off, then the speed) on every
// lane; "auto" is autoneg on a copper port only -- a QSFP cage holding a
// module the table knows runs what the module runs (moduleSpeed), any other
// is set to its native (highest) speed, or left split. A speed that would
// split or join a cage is a breakout (breakout.go), which needs the
// platform's platform.json.
func (pl *plan) speed(p devicemodel.Port, d devicemodel.PortDesired, lanes []string) {
	want := d.SpeedMbps
	split := len(lanes) > 1
	if want == 0 {
		if isCopper(p.Media) {
			for _, lane := range lanes {
				ln := lane
				if !isTrue(pl.sim.field(ln, "autoneg")) {
					pl.add(d.Index, fmt.Sprintf("config interface autoneg %s enabled", ln), func(s *swState) { s.setField(ln, "autoneg", "on") })
				}
			}
			return
		}
		if pl.moduleSpeed(p, d, lanes) {
			return
		}
		if split {
			return // auto on a split cage would be a join: leave it
		}
		for _, s := range p.SpeedCaps {
			if s > want {
				want = s
			}
		}
		if want == 0 {
			return
		}
	}
	if parent, pp, ok := pl.sim.platformCage(p.Index); ok && len(pp.BreakoutModes) > 0 {
		pl.breakoutSpeed(p, d, laneCfg{speed: want}, parent, pp, pl.add)
		return
	}
	serdes := 0
	for _, lane := range lanes {
		serdes += laneCount(pl.sim.field(lane, "lanes"))
	}
	if (!split && serdes >= 4 && want <= 25000) || (split && want >= 40000) {
		if !pl.sim.platformJSON {
			pl.note("port %d: %d Mb/s needs a breakout (split or join of the cage), and this platform has no platform.json (dynamic port breakout unavailable); speed not written", d.Index, want)
		} else {
			pl.note("port %d: %d Mb/s needs a breakout, and platform.json lists no breakout modes for this cage; speed not written", d.Index, want)
		}
		return
	}
	if len(p.SpeedCaps) > 0 && !containsInt(p.SpeedCaps, want) {
		pl.note("port %d: %d Mb/s is not among the port's speeds %v; speed not written", d.Index, want, p.SpeedCaps)
		return
	}
	for _, lane := range lanes {
		ln := lane
		if isTrue(pl.sim.field(ln, "autoneg")) {
			pl.add(d.Index, fmt.Sprintf("config interface autoneg %s disabled", ln), func(s *swState) { s.setField(ln, "autoneg", "off") })
		}
		if atoi(pl.sim.field(ln, "speed")) != want {
			v := strconv.Itoa(want)
			pl.add(d.Index, fmt.Sprintf("config interface speed %s %s", ln, v), func(s *swState) { s.setField(ln, "speed", v) })
		}
	}
}

// moduleSpeed is "auto" on a cage holding a module the table knows
// (§10.9): a configuration the module runs stays; any other becomes the
// module's native one, by breakout where platform.json has the mode, else
// as a lane speed -- the driver's own writes (plan.own), none of them in a
// controller-only plan. It reports whether the module decided; false for
// an empty cage or an unknown module, which keep the rule from before.
func (pl *plan) moduleSpeed(p devicemodel.Port, d devicemodel.PortDesired, lanes []string) bool {
	if p.Optic == nil {
		return false
	}
	runs, ok := moduleRuns(p.Optic.MediaType)
	if !ok {
		pl.note("port %d: speed auto, and the module %q is not in the speed table; the cage is treated as empty", d.Index, p.Optic.MediaType)
		return false
	}
	if pl.controllerOnly {
		return true
	}
	if pl.owned[p.Index] {
		// A lane switch sets the lane speeds, which may differ: the module
		// decides the mode only, kept while it runs that many ports.
		for _, r := range runs {
			if r.ports == len(lanes) {
				return true
			}
		}
	}
	cur := laneCfg{ports: len(lanes), speed: atoi(pl.sim.field(lanes[0], "speed"))}
	for _, ln := range lanes[1:] {
		if atoi(pl.sim.field(ln, "speed")) != cur.speed {
			cur.speed = -1 // lanes apart: nothing a module runs
		}
	}
	for _, r := range runs {
		if r == cur {
			return true
		}
	}
	native := runs[0]
	if parent, pp, ok := pl.sim.platformCage(p.Index); ok && len(pp.BreakoutModes) > 0 {
		pl.breakoutSpeed(p, d, native, parent, pp, pl.addOwn)
		return true
	}
	if native.ports != len(lanes) || (len(p.SpeedCaps) > 0 && !containsInt(p.SpeedCaps, native.speed)) {
		pl.note("port %d: the module %q runs %v, which this cage cannot be set to without platform.json (dynamic port breakout); speed not written", d.Index, p.Optic.MediaType, runs)
		return true
	}
	for _, lane := range lanes {
		ln := lane
		if isTrue(pl.sim.field(ln, "autoneg")) {
			pl.addOwn(d.Index, fmt.Sprintf("config interface autoneg %s disabled", ln), func(s *swState) { s.setField(ln, "autoneg", "off") })
		}
		if atoi(pl.sim.field(ln, "speed")) != native.speed {
			v := strconv.Itoa(native.speed)
			pl.addOwn(d.Index, fmt.Sprintf("config interface speed %s %s", ln, v), func(s *swState) { s.setField(ln, "speed", v) })
		}
	}
	return true
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// stormOps: percent of the configured speed to kbps, three types; a type
// whose rate changes is deleted and added again.
func (pl *plan) stormOps(d devicemodel.PortDesired, ln string) {
	speed := atoi(pl.sim.field(ln, "speed"))
	want := stormWant(d.StormCtrl, speed)
	have := pl.sim.storm[ln]
	for _, t := range stormTypes {
		h, hasH := have[t]
		w, hasW := want[t]
		if hasH == hasW && h == w {
			continue
		}
		typ := t
		if hasH {
			pl.add(d.Index, fmt.Sprintf("config interface storm-control del %s %s", ln, typ), func(s *swState) { delete(s.storm[ln], typ) })
		}
		if hasW {
			kbps := w
			pl.add(d.Index, fmt.Sprintf("config interface storm-control add %s %s %d", ln, typ, kbps), func(s *swState) {
				if s.storm[ln] == nil {
					s.storm[ln] = map[string]int{}
				}
				s.storm[ln][typ] = kbps
			})
		}
	}
}

// stpOps: BPDU guard and "STP disabled on the port" (portfast on and STP
// off for the interface), only where the image has the STP daemon.
func (pl *plan) stpOps(d devicemodel.PortDesired, ln string) {
	cur := pl.sim.stp[ln]
	bpdu := isTrue(cur["bpdu_guard"])
	edge := isTrue(cur["portfast"]) && cur["enabled"] == "false"
	if bpdu == d.BPDUGuard && edge == d.STPEdge {
		return
	}
	if !pl.sim.stpFeature {
		return // disunified-l2d takes the port's STP settings (l2.go)
	}
	if bpdu != d.BPDUGuard {
		v := map[bool]string{true: "enable", false: "disable"}[d.BPDUGuard]
		b := strconv.FormatBool(d.BPDUGuard)
		pl.add(d.Index, fmt.Sprintf("config spanning-tree interface bpdu_guard %s %s", v, ln), func(s *swState) { s.setSTP(ln, "bpdu_guard", b) })
	}
	if edge != d.STPEdge {
		if d.STPEdge {
			pl.add(d.Index, "config spanning-tree interface portfast enable "+ln, func(s *swState) { s.setSTP(ln, "portfast", "true") })
			pl.add(d.Index, "config spanning-tree interface disable "+ln, func(s *swState) { s.setSTP(ln, "enabled", "false") })
		} else {
			pl.add(d.Index, "config spanning-tree interface portfast disable "+ln, func(s *swState) { s.setSTP(ln, "portfast", "false") })
			pl.add(d.Index, "config spanning-tree interface enable "+ln, func(s *swState) { s.setSTP(ln, "enabled", "true") })
		}
	}
}

// sanitizeDescription keeps a port name to one printable line of at most
// 64 characters without quotes or backslashes: it travels through a shell
// and into CONFIG_DB.
func sanitizeDescription(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '\'' || r == '"' || r == '\\' || r == '`' {
			continue
		}
		if n == 64 {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// --- the interfaces ---

// ApplyPorts implements devicemodel.Controller.
func (c *Collector) ApplyPorts(ctx context.Context, desired []devicemodel.PortDesired) (int, error) {
	unlock, err := c.lockApply(ctx)
	if err != nil {
		return 0, fmt.Errorf("sonic apply: %w", err)
	}
	defer unlock()
	n, err := c.applyPorts(ctx, desired)
	if err != nil {
		return n, err
	}
	// The ports' STP and loop-protection settings go to disunified-l2d
	// after the ports themselves (an aggregate the batch made is its
	// bridge port now).
	c.mu.Lock()
	st := c.state
	c.mu.Unlock()
	if st != nil && !st.stpFeature {
		c.l2SetPorts(desired, st)
		if !st.psample && stormUnicastAsked(desired) {
			c.warnOnce("storm unicast", "unknown-unicast storm control is applied, but this image cannot detect an unknown-unicast storm: it has no linux-bcm-genl (the image kit's sonic-buildimage 0014), so the SAI cannot sample a port; anomaly 4 is not reported (docs/drivers/sonic.md §11.24)")
		}
		if _, err := c.applyL2(ctx); err != nil {
			return n, err
		}
	}
	// The ACL tables bind to every port and PortChannel: a breakout or an
	// aggregate this batch made changes that list (acl.go).
	if _, err := c.applyACL(ctx); err != nil {
		return n, err
	}
	// The reserved groups' static members follow VLAN membership, which the
	// batch may have changed (igmp.go).
	if _, err := c.applyIGMP(ctx); err != nil {
		return n, err
	}
	// A routed port's address waits until the port is in no VLAN, which
	// this batch may just have made it (l3.go): the device-wide apply ran
	// before it.
	if _, err := c.applyL3(ctx); err != nil {
		return n, err
	}
	// Last, a VLAN no push names any more, now that the ports, the IGMP
	// rows and a routed network's address have left it (vlan_prune.go).
	// Only with ports to write: control.ports off writes no VLAN either.
	if len(desired) > 0 {
		pruned, err := c.pruneVLANs(ctx)
		if err != nil {
			return n, err
		}
		if pruned > 0 {
			// The DHCP guard's rules are per VLAN on the switch (acl.go):
			// derived again without the deleted ones.
			c.mu.Lock()
			d, st := c.aclFrom, c.state
			c.mu.Unlock()
			if d != nil && st != nil {
				c.aclSet(*d, st)
				if _, err := c.applyACL(ctx); err != nil {
					return n, err
				}
			}
		}
	}
	return n, nil
}

func (c *Collector) applyPorts(ctx context.Context, desired []devicemodel.PortDesired) (int, error) {
	c.noteCageDown(desired)
	pl, err := c.newPlan(desired, false)
	if err != nil {
		return 0, err
	}
	return c.runPlan(ctx, pl, "")
}

// runPlan logs the plan's notes, runs its batch and makes its copy of
// CONFIG_DB the state. what names a scoped plan in the log ("" = ports).
func (c *Collector) runPlan(ctx context.Context, pl *plan, what string) (int, error) {
	for _, n := range pl.notes {
		c.warnOnce("apply: "+n, "%s", n)
	}
	if len(pl.ops) == 0 {
		return 0, nil
	}
	cmds := make([]string, 0, len(pl.ops)+1)
	for _, o := range pl.ops {
		cmds = append(cmds, o.cmd)
	}
	cmds = append(cmds, "sudo config save -y")
	n := len(pl.changed)
	for idx := range pl.own {
		if !pl.changed[idx] {
			n++
		}
	}
	label := fmt.Sprintf("%d ports", n)
	if what != "" {
		label = fmt.Sprintf("%s, %d changed", what, n)
	}
	c.Log.Printf("sonic apply (%s): %s", label, strings.ReplaceAll(strings.Join(loggedCmds(cmds), " ; "), applCommand, applLogged))
	if err := c.runBatch(ctx, cmds); err != nil {
		return 0, fmt.Errorf("sonic apply (%s): %w", label, err)
	}
	c.mu.Lock()
	c.state = pl.sim
	c.mu.Unlock()
	return n, nil
}

// PlanPorts implements devicemodel.Planner: the ports ApplyPorts would
// write for the controller's config -- planned without the FEC and the
// speed a module calls for where UniFi says auto (plan.controllerOnly),
// which no controller setting can match.
func (c *Collector) PlanPorts(desired []devicemodel.PortDesired) []int {
	pl, err := c.newPlan(desired, true)
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

// EnsureVLANs implements devicemodel.VLANController: it creates the
// controller's VLANs that are missing (UniFi's 1 is Vlan<native_vlan>) and
// remembers the list for "Allow All" and for pruneVLANs, which deletes the
// ones the push stops naming after the ports have left them. A list that
// contains native_vlan itself is refused whole: that id is the stand-in
// for VLAN 1.
func (c *Collector) EnsureVLANs(ctx context.Context, ids []int) (int, error) {
	return c.ensureVLANs(ctx, 0, ids)
}

// EnsureLaneVLANs implements devicemodel.LaneVLANController: an attached
// lane switch's own list, kept apart from the device's (its push names
// what its lanes carry), created like the device's and kept by the prune.
func (c *Collector) EnsureLaneVLANs(ctx context.Context, cage int, ids []int) (int, error) {
	if cage <= 0 {
		return 0, fmt.Errorf("sonic ensure lane vlans: cage %d", cage)
	}
	return c.ensureVLANs(ctx, cage, ids)
}

// ensureVLANs is EnsureVLANs for the device's own push (cage 0) or a lane
// switch's.
func (c *Collector) ensureVLANs(ctx context.Context, cage int, ids []int) (int, error) {
	unlock, err := c.lockApply(ctx)
	if err != nil {
		return 0, fmt.Errorf("sonic ensure vlans: %w", err)
	}
	defer unlock()
	for _, id := range ids {
		if id == c.NativeVLAN {
			return 0, fmt.Errorf("sonic: the controller has a network on VLAN %d, which is this switch's native_vlan (UniFi's VLAN 1 is Vlan%d here); nothing written -- set options.native_vlan to an id no UniFi network uses", id, c.NativeVLAN)
		}
	}
	c.mu.Lock()
	st := c.state
	c.mu.Unlock()
	if st == nil {
		return 0, fmt.Errorf("sonic ensure vlans: no snapshot yet")
	}
	seen := map[int]bool{}
	var site, missing []int
	for _, id := range ids {
		sid := id
		if id == 1 {
			sid = c.NativeVLAN
		}
		if sid < 2 || sid > 4094 || seen[sid] {
			continue
		}
		seen[sid] = true
		if sid != c.NativeVLAN {
			site = append(site, sid)
		}
		if !st.vlans[sid] {
			missing = append(missing, sid)
		}
	}
	sort.Ints(site)
	sort.Ints(missing)
	c.mu.Lock()
	if cage == 0 {
		c.siteVLANs, c.siteKnown = site, true
	} else {
		if c.laneVLANs == nil {
			c.laneVLANs = map[int][]int{}
		}
		c.laneVLANs[cage] = append([]int{}, site...)
	}
	c.mu.Unlock()
	if len(missing) == 0 {
		return 0, nil
	}
	add := "sudo config vlan add " + strconv.Itoa(missing[0])
	if len(missing) > 1 {
		add = "sudo config vlan add -m " + joinInts(missing)
	}
	cmds := []string{add}
	// A new VLAN is 9100; behind ports at another MTU (jumbo off) it takes
	// theirs from the start (vlanMTUs; the kernel netdev follows on the
	// next apply, once a collect has seen it).
	mtu := 0
	c.mu.Lock()
	if c.last != nil {
		for _, p := range c.last.Ports {
			if p.MTU > 0 && (mtu == 0 || p.MTU < mtu) {
				mtu = p.MTU
			}
		}
	}
	c.mu.Unlock()
	if mtu > 0 && mtu != defaultMTU {
		for _, id := range missing {
			cmds = append(cmds, fmt.Sprintf("sudo sonic-db-cli CONFIG_DB hset %s mtu %d", sshrun.ShellQuote("VLAN|Vlan"+strconv.Itoa(id)), mtu))
		}
	}
	cmds = append(cmds, "sudo config save -y")
	c.Log.Printf("sonic: creating VLANs %v", missing)
	if err := c.runBatch(ctx, cmds); err != nil {
		return 0, fmt.Errorf("sonic create vlans %v: %w", missing, err)
	}
	c.mu.Lock()
	if c.state == st {
		next := st.clone()
		for _, id := range missing {
			next.vlans[id] = true
			if mtu > 0 && mtu != defaultMTU {
				next.vlanMTU[id] = strconv.Itoa(mtu)
			}
		}
		c.state = next
	}
	c.mu.Unlock()
	return len(missing), nil
}

// SwitchesJumbo implements devicemodel.JumboSwitcher: jumbo off is a 1500
// port MTU on SONiC.
func (c *Collector) SwitchesJumbo() bool { return true }

// batchCommand runs a write batch: one shell, the commands on stdin, the
// first failure stops it.
const batchCommand = "bash -s"

func (c *Collector) runBatch(ctx context.Context, cmds []string) error {
	out, err := c.run(ctx, batchCommand, "set -e\n"+strings.Join(cmds, "\n")+"\necho "+batchDone+"\n")
	if err != nil {
		return err
	}
	if !strings.Contains(out, batchDone) {
		return fmt.Errorf("bash -s: the batch ended before its last line without an error (a command read the rest of it from stdin); what followed did not run")
	}
	return nil
}

// batchDone is echoed by a batch's last line. bash -s reads the batch from
// stdin, which every command in it inherits: one that reads stdin swallows
// the rest and bash exits 0 at the end of input (live, 2026-11-07: vtysh
// in the RIF wait took an L3 batch's address removals and its save). A
// batch whose output lacks the marker did not run to the end.
const batchDone = "disunified-batch-done"
