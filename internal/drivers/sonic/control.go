package sonic

import (
	"context"
	"fmt"
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

const (
	jumboMTU  = 9100 // SONiC's default and its jumbo
	normalMTU = 1500 // "jumbo off"
)

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
	notes   []string // refusals, logged once each by ApplyPorts
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

// newPlan diffs desired against the collector's state without writing.
func (c *Collector) newPlan(desired []devicemodel.PortDesired) (*plan, error) {
	c.mu.Lock()
	snap, st, site := c.last, c.state, append([]int(nil), c.siteVLANs...)
	c.mu.Unlock()
	if snap == nil || st == nil {
		return nil, fmt.Errorf("sonic apply: no snapshot yet")
	}
	pl := &plan{sim: st.clone(), ports: map[int]devicemodel.Port{}, native: c.NativeVLAN, site: site, changed: map[int]bool{}}
	for _, p := range snap.Ports {
		pl.ports[p.Index] = p
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
		for _, lane := range lanesOf(pl.ports[d.Index]) {
			pl.syncVLANs(d.Index, lane, d)
		}
	}
	pl.lagJoins(ds)
	pl.mirrorAdds(ds)
	return pl, nil
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
		name := "dui" + strconv.Itoa(d.Index)
		if _, taken := pl.sim.mirrors[name]; taken {
			pl.add(d.Index, "config mirror_session remove "+name, func(s *swState) { delete(s.mirrors, name) })
		}
		// SONiC refuses a destination with VLAN config.
		pl.stripVLANs(d.Index, dst)
		pl.add(d.Index, fmt.Sprintf("config mirror_session span add %s %s %s both", name, dst, src), func(s *swState) {
			s.mirrors[name] = map[string]string{"type": "SPAN", "dst_port": dst, "src_port": src, "direction": "BOTH"}
		})
	}
}

// --- LAG ---

func (pl *plan) lagLeaves(ds []devicemodel.PortDesired) {
	for _, d := range ds {
		for _, lane := range lanesOf(pl.ports[d.Index]) {
			cur := pl.sim.lagOf(lane)
			if cur == "" || (d.LAG > 0 && cur == pcName(d.LAG)) {
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
	for _, lag := range ids {
		pc := pcName(lag)
		members := byLAG[lag]
		for _, d := range members {
			for _, lane := range lanesOf(pl.ports[d.Index]) {
				if pl.sim.lags[pc][lane] {
					continue
				}
				if _, exists := pl.sim.lags[pc]; !exists {
					pl.add(d.Index, "config portchannel add "+pc, func(s *swState) { s.lags[pc] = map[string]bool{} })
				}
				// SONiC refuses a member with VLAN config.
				pl.stripVLANs(d.Index, lane)
				ln := lane
				pl.add(d.Index, fmt.Sprintf("config portchannel member add %s %s", pc, ln), func(s *swState) { s.lags[pc][ln] = true })
			}
		}
		// The PortChannel carries its members' VLAN intent (the first's).
		if _, exists := pl.sim.lags[pc]; exists {
			pl.syncVLANs(members[0].Index, pc, members[0])
		}
	}
	// A PortChannel this apply emptied goes, its VLANs first.
	wanted := map[string]bool{}
	for _, lag := range ids {
		wanted[pcName(lag)] = true
	}
	var pcs []string
	for pc, m := range pl.sim.lags {
		if len(m) == 0 && !wanted[pc] && pl.emptiedHere(pc) {
			pcs = append(pcs, pc)
		}
	}
	sort.Strings(pcs)
	for _, pc := range pcs {
		name := pc
		pl.stripVLANs(0, name)
		pl.add(0, "config portchannel del "+name, func(s *swState) { delete(s.lags, name); delete(s.members, name) })
	}
}

// emptiedHere: a member del for pc is among the planned writes.
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
	have := pl.sim.members[iface]
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
	pl.add(idx, fmt.Sprintf("config vlan member %s %s %s", verb, form, ifc), func(s *swState) {
		for _, id := range list {
			if add {
				s.setMember(ifc, id, true)
			} else {
				s.delMember(ifc, id)
			}
		}
	})
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
	lanes := lanesOf(p)
	pl.speed(p, d, lanes)
	for _, lane := range lanes {
		ln := lane
		if enabled := pl.sim.field(ln, "admin_status") == "up"; enabled != d.Enabled {
			if d.Enabled {
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
		if d.FEC != nil && *d.FEC != fecOf(pl.sim.field(ln, "fec")) {
			if kw := fecKeyword(*d.FEC); kw != "" && !(kw == "none" && pl.sim.field(ln, "fec") == "") {
				pl.add(d.Index, fmt.Sprintf("config interface fec %s %s", ln, kw), func(s *swState) { s.setField(ln, "fec", kw) })
			}
		}
		// Jumbo is switch-wide in UniFi and a port MTU on SONiC. A LAG
		// member takes its PortChannel's MTU and refuses its own.
		if d.Jumbo != nil && d.LAG == 0 && pl.sim.lagOf(ln) == "" {
			want := normalMTU
			if *d.Jumbo {
				want = jumboMTU
			}
			if atoi(pl.sim.field(ln, "mtu")) != want {
				m := strconv.Itoa(want)
				pl.add(d.Index, fmt.Sprintf("config interface mtu %s %s", ln, m), func(s *swState) { s.setField(ln, "mtu", m) })
			}
		}
		pl.stormOps(d, ln)
		pl.stpOps(d, ln)
	}
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
// lane; "auto" is autoneg on a copper port only -- a QSFP cage is set to its
// native (highest) speed instead. A speed that would split or join a cage
// is a breakout, which needs the platform's platform.json.
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
	serdes := 0
	for _, lane := range lanes {
		serdes += laneCount(pl.sim.field(lane, "lanes"))
	}
	if (!split && serdes >= 4 && want <= 25000) || (split && want >= 40000) {
		if !pl.sim.platformJSON {
			pl.note("port %d: %d Mb/s needs a breakout (split or join of the cage), and this platform has no platform.json (dynamic port breakout unavailable); speed not written", d.Index, want)
		} else {
			pl.note("port %d: %d Mb/s needs a breakout, which this driver does not write yet; speed not written", d.Index, want)
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
		pl.note("port %d: BPDU guard / STP-disabled requested, but this image has no STP daemon (no FEATURE|stp); not written", d.Index)
		return
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
	pl, err := c.newPlan(desired)
	if err != nil {
		return 0, err
	}
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
	c.Log.Printf("sonic apply (%d ports): %s", len(pl.changed), strings.Join(cmds, " ; "))
	if err := c.runBatch(ctx, cmds); err != nil {
		return 0, fmt.Errorf("sonic apply (%d ports): %w", len(pl.changed), err)
	}
	c.mu.Lock()
	c.state = pl.sim
	c.mu.Unlock()
	return len(pl.changed), nil
}

// PlanPorts implements devicemodel.Planner: the ports ApplyPorts would write.
func (c *Collector) PlanPorts(desired []devicemodel.PortDesired) []int {
	pl, err := c.newPlan(desired)
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
// remembers the list for "Allow All". Never deletes. A list that contains
// native_vlan itself is refused whole: that id is the stand-in for VLAN 1.
func (c *Collector) EnsureVLANs(ctx context.Context, ids []int) (int, error) {
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
	c.siteVLANs = site
	c.mu.Unlock()
	if len(missing) == 0 {
		return 0, nil
	}
	add := "sudo config vlan add " + strconv.Itoa(missing[0])
	if len(missing) > 1 {
		add = "sudo config vlan add -m " + joinInts(missing)
	}
	cmds := []string{add, "sudo config save -y"}
	c.Log.Printf("sonic: creating VLANs %v", missing)
	if err := c.runBatch(ctx, cmds); err != nil {
		return 0, fmt.Errorf("sonic create vlans %v: %w", missing, err)
	}
	c.mu.Lock()
	if c.state == st {
		next := st.clone()
		for _, id := range missing {
			next.vlans[id] = true
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
	_, err := c.runner().Run(ctx, batchCommand, "set -e\n"+strings.Join(cmds, "\n")+"\n")
	return err
}
