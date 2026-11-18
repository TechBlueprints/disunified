package sonic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// IGMP snooping through the image's l2mcd (the Micas series, docs/drivers/
// sonic.md §8.10-8.11, §8.14): the controller's per-VLAN settings become
// whole rows in CONFIG_DB's L2MC tables, as the ECS keeps them in its own
// CFG_L2MC_* tables, written by one swsscommon script per apply. Measured
// on the DX010 (Tomahawk-1, libsaibcm 13.2.1.100, 2026-11-03): snooping and
// (*,G) entries program; optimised flood -- unknown multicast to the
// router ports only -- is refused by the SAI and stays unwritten.

// The tables the driver owns, in CONFIG_DB key form.
const (
	l2mcRow    = "L2MC|"
	l2mcSupp   = "L2MC_SUPPRESS|"
	l2mcGroup  = "L2MC_STATIC_GROUP|"
	l2mcMember = "L2MC_STATIC_MEMBER|"
	l2mcRouter = "L2MC_MROUTER|"
)

// reservedGroups are the groups UniFi's flood_known_protocols floods to
// every port of a snooping VLAN, by IANA's addresses (the ECS names them
// symbolically; §8.10): SSDP, NTP, PTP, H.323 gatekeeper discovery and the
// three SLP groups.
var reservedGroups = []string{"239.255.255.250", "224.0.1.1", "224.0.1.129", "224.0.1.41", "224.0.1.35", "224.0.1.22", "239.255.255.253"}

// UniFi's timer defaults (the ECS's push) where a VLAN's key is absent.
const (
	defaultMaxResponse = 10
	defaultMembership  = 260
	// l2mcd's last-member query interval (ms) is not pushed; the CLI's
	// default.
	lastMemberQueryMs = "1000"
)

// igmpWant is the controller's snooping intent from the last ApplyDevice,
// in UniFi VLAN ids; set false until a push names snooping at all.
type igmpWant struct {
	set           bool
	vlans         map[int]bool
	igmp          map[int]devicemodel.IGMPVLAN
	floodReserved bool
	unknownRouter *bool
}

// igmpOn: the driver writes snooping -- options.igmp on and an image with
// the series (FEATURE|l2mcd) whose L2MC tables the collector reads.
func (c *Collector) igmpOn(st *swState) bool {
	return c.IGMP && st != nil && st.l2mcKnown
}

func (c *Collector) igmpSet(d devicemodel.DeviceDesired) {
	w := igmpWant{set: d.IGMPSnooping != nil, vlans: d.IGMPSnooping, igmp: d.IGMP, unknownRouter: d.IGMPUnknownToRouters}
	w.floodReserved = d.IGMPFloodReserved != nil && *d.IGMPFloodReserved
	c.mu.Lock()
	c.igmp = w
	c.mu.Unlock()
}

// igmpReport is the snapshot's view: the VLANs with an enabled L2MC row,
// in UniFi ids (Vlan<native_vlan> is 1).
func igmpReport(t table, native int) map[int]bool {
	var out map[int]bool
	for k, f := range t {
		n, ok := strings.CutPrefix(k, l2mcRow)
		if !ok {
			continue
		}
		id := atoi(strings.TrimPrefix(n, "Vlan"))
		if id <= 0 {
			continue
		}
		if id == native {
			id = 1
		}
		if out == nil {
			out = map[int]bool{}
		}
		out[id] = f["enabled"] == "true"
	}
	return out
}

// l2mcPlan is the rows the intent asks for, in CONFIG_DB key form, and
// what it cannot carry.
func (c *Collector) l2mcPlan(w igmpWant, sim *swState) (map[string]map[string]string, []string) {
	rows := map[string]map[string]string{}
	var notes []string
	sonic := func(id int) int {
		if id == 1 {
			return c.NativeVLAN
		}
		return id
	}
	var ids []int
	for id, on := range w.vlans {
		if on {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	var routerExpiry, suppression, flooded []string
	for _, id := range ids {
		sid := sonic(id)
		vlan := "Vlan" + strconv.Itoa(sid)
		if !sim.vlans[sid] {
			notes = append(notes, fmt.Sprintf("IGMP snooping on VLAN %d: %s does not exist on the switch; skipped", id, vlan))
			continue
		}
		g := w.igmp[id]
		mr := g.MaxResponse
		if mr == 0 {
			mr = defaultMaxResponse
		}
		if mr < 1 || mr > 25 {
			// l2mcd and its YANG take 1-25 s.
			clamped := min(max(mr, 1), 25)
			notes = append(notes, fmt.Sprintf("IGMP VLAN %d: max response %d s is outside l2mcd's 1-25 s; %d s written", id, mr, clamped))
			mr = clamped
		}
		// l2mcd derives the membership time as 2 x query interval + max
		// response, so the interval follows from UniFi's membership time
		// (260/10 -> 125 s, RFC 3376's default).
		mem := g.Membership
		if mem == 0 {
			mem = defaultMembership
		}
		qi := (mem - mr) / 2
		if qi <= mr || qi > 18000 {
			fixed := min(max(qi, mr+1), 18000)
			notes = append(notes, fmt.Sprintf("IGMP VLAN %d: membership %d s gives a query interval of %d s, outside l2mcd's range above the max response; %d s written", id, mem, qi, fixed))
			qi = fixed
		}
		rows[l2mcRow+vlan] = map[string]string{
			"enabled":                    "true",
			"querier":                    strconv.FormatBool(g.Querier),
			"fast-leave":                 strconv.FormatBool(g.FastLeave),
			"version":                    "3", // l2mcd at 2 ignores the IGMPv3 reports hosts send (§8.10)
			"query-interval":             strconv.Itoa(qi),
			"query-max-response-time":    strconv.Itoa(mr),
			"last-member-query-interval": lastMemberQueryMs,
		}
		if g.Querier {
			own := sim.vlanAddress(vlan)
			switch {
			case g.QuerierAddress != "" && own == "":
				notes = append(notes, fmt.Sprintf("IGMP VLAN %d: querier address %s has no l2mcd field and %s has no address; queries leave from 0.0.0.0 (§8.11)", id, g.QuerierAddress, vlan))
			case g.QuerierAddress != "":
				notes = append(notes, fmt.Sprintf("IGMP VLAN %d: querier address %s has no l2mcd field; queries leave from %s's own %s (§8.11)", id, g.QuerierAddress, vlan, own))
			case own == "":
				notes = append(notes, fmt.Sprintf("IGMP VLAN %d: querier on and %s has no address; queries leave from 0.0.0.0 (§8.11)", id, vlan))
			}
		}
		if g.RouterExpiry > 0 {
			routerExpiry = append(routerExpiry, strconv.Itoa(id))
		}
		// Static multicast-router ports, as the CLI's `igmp-snooping
		// mrouter add` writes them (l2mcmgrd reads L2MC_MROUTER): a
		// PortChannel member is its PortChannel, a split cage every lane.
		for _, idx := range g.RouterPorts {
			ifs := c.ifnamesOf(idx)
			if len(ifs) == 0 {
				notes = append(notes, fmt.Sprintf("IGMP VLAN %d: router port %d is not a port of this switch; skipped", id, idx))
				continue
			}
			if pc := sim.lagOf(ifs[0]); pc != "" {
				ifs = []string{pc}
			}
			for _, iface := range ifs {
				rows[l2mcRouter+vlan+"|"+iface] = map[string]string{"mrouter_port": iface}
			}
		}
		if g.Suppression {
			suppression = append(suppression, strconv.Itoa(id))
		}
		if g.FloodUnknown != nil && !*g.FloodUnknown {
			flooded = append(flooded, strconv.Itoa(id))
		}
		if !w.floodReserved {
			continue
		}
		var members []string
		for iface, m := range sim.members {
			if _, in := m[sid]; in {
				members = append(members, iface)
			}
		}
		members = sortIfnames(setOf(members))
		if len(members) == 0 {
			continue
		}
		for _, grp := range reservedGroups {
			rows[l2mcGroup+vlan+"|"+grp] = map[string]string{"static-members@": strings.Join(members, ",")}
			for _, m := range members {
				rows[l2mcMember+vlan+"|"+grp+"|"+m] = map[string]string{"port": m}
			}
		}
	}
	if len(routerExpiry) > 0 {
		notes = append(notes, fmt.Sprintf("IGMP: the multicast-router expiry (VLANs %s) has no l2mcd setting; not written", strings.Join(routerExpiry, ", ")))
	}
	if len(suppression) > 0 {
		notes = append(notes, fmt.Sprintf("IGMP: report suppression (VLANs %s) has no l2mcd setting; not written", strings.Join(suppression, ", ")))
	}
	if len(flooded) > 0 {
		// optimised flood: the SAI refuses its wildcard entry (INVALID_PARAMETER)
		// and the VLAN's unknown-multicast group (NOT_IMPLEMENTED), and
		// orchagent retries it twice a second (§8.14).
		notes = append(notes, fmt.Sprintf("IGMP: unknown multicast to the router ports only (VLANs %s) is refused by this switch's SAI; those VLANs keep flooding it", strings.Join(flooded, ", ")))
	}
	if w.unknownRouter != nil && !*w.unknownRouter {
		notes = append(notes, "IGMP: unknown multicast kept from the router ports is not something l2mcd does; it reaches them")
	}
	return rows, notes
}

// l2mcOp is one CONFIG_DB write of the IGMP script: a whole row set, or a
// row removed (fields nil).
type l2mcOp struct {
	Key    string            `json:"key"`
	Fields map[string]string `json:"fields,omitempty"`
}

// l2mcOps diffs the wanted rows against the switch's, in the CLI's order:
// static members, their groups, the suppression rows and a disabled VLAN's
// router ports go before its L2MC row; rows are set before their statics.
func l2mcOps(want map[string]map[string]string, have map[string]map[string]string) []l2mcOp {
	going := map[string]bool{} // VLANs whose L2MC row goes
	for k := range have {
		if v, ok := strings.CutPrefix(k, l2mcRow); ok && want[k] == nil {
			going[v] = true
		}
	}
	var dels [5][]string
	for k := range have {
		if want[k] != nil {
			continue
		}
		switch {
		case strings.HasPrefix(k, l2mcMember):
			dels[0] = append(dels[0], k)
		case strings.HasPrefix(k, l2mcGroup):
			dels[1] = append(dels[1], k)
		case strings.HasPrefix(k, l2mcSupp):
			// Every suppression row goes: optimised flood is refused here,
			// and a row with it on makes orchagent retry for good.
			dels[2] = append(dels[2], k)
		case strings.HasPrefix(k, l2mcRouter):
			// UniFi's Multicast Router Port: the controller's list is the
			// switch's, a hand-made row goes too.
			dels[3] = append(dels[3], k)
		case strings.HasPrefix(k, l2mcRow):
			dels[4] = append(dels[4], k)
		}
	}
	var ops []l2mcOp
	for _, d := range dels {
		sort.Strings(d)
		for _, k := range d {
			ops = append(ops, l2mcOp{Key: k})
		}
	}
	var sets [3][]string
	for k, f := range want {
		if equalFields(have[k], f) {
			continue
		}
		switch {
		case strings.HasPrefix(k, l2mcRow):
			sets[0] = append(sets[0], k)
		case strings.HasPrefix(k, l2mcGroup):
			sets[1] = append(sets[1], k)
		default:
			sets[2] = append(sets[2], k)
		}
	}
	for _, s := range sets {
		sort.Strings(s)
		for _, k := range s {
			ops = append(ops, l2mcOp{Key: k, Fields: copyFields(want[k])})
		}
	}
	return ops
}

func equalFields(a, b map[string]string) bool {
	if a == nil || len(a) != len(b) {
		return false
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

func setOf(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// l2mcCommand runs l2mcScript: one swsscommon session for every row, where
// a sonic-db-cli call per row (~100 ms) would put the reserved groups of a
// 32-port VLAN (224 rows) past the apply budget.
const l2mcCommand = "sudo python3 - disunified-l2mc"

// l2mcScriptHead applies OPS (the last line of the script, a JSON string)
// through ConfigDBConnector: set_entry replaces a whole row, None removes
// it. A field named "x@" is written as is (the CLI's static-members@).
const l2mcScriptHead = `import json
from swsscommon.swsscommon import ConfigDBConnector

db = ConfigDBConnector()
db.connect(wait_for_init=False)


def run(ops):
    for op in ops:
        table, _, key = op["key"].partition("|")
        parts = tuple(key.split("|"))
        db.set_entry(table, parts if len(parts) > 1 else parts[0], op.get("fields"))

`

func l2mcScript(ops []l2mcOp) string {
	b, _ := json.Marshal(ops)
	lit, _ := json.Marshal(string(b)) // a JSON string is a Python string literal
	return l2mcScriptHead + "run(json.loads(" + string(lit) + "))\n"
}

// applyIGMP brings the L2MC tables in line with the last intent: ApplyDevice
// runs it for a push, ApplyPorts again once VLAN memberships may have
// changed (the reserved groups follow them). The feature goes on with the
// first snooping VLAN and stays on, as on the ECS.
func (c *Collector) applyIGMP(ctx context.Context) (int, error) {
	c.mu.Lock()
	st, w := c.state, c.igmp
	c.mu.Unlock()
	if !w.set || st == nil {
		return 0, nil
	}
	if !c.igmpOn(st) {
		for _, on := range w.vlans {
			if on {
				why := "the image has no l2mcd"
				if st.l2mcKnown {
					why = "options.igmp is off"
				}
				c.warnOnce("igmp off", "sonic: IGMP snooping requested; %s; not written", why)
				break
			}
		}
		return 0, nil
	}
	sim := st.clone()
	want, notes := c.l2mcPlan(w, sim)
	for _, n := range notes {
		c.warnOnce("igmp: "+n, "sonic: %s", n)
	}
	ops := l2mcOps(want, sim.l2mc)
	feature := false
	for k := range want {
		if strings.HasPrefix(k, l2mcRow) && sim.l2mcFeature != "enabled" {
			feature = true
			break
		}
	}
	if len(ops) == 0 && !feature {
		return 0, nil
	}
	if feature {
		cmd := "sudo config feature state l2mcd enabled"
		c.Log.Printf("sonic apply igmp: %s", cmd)
		if err := c.runBatch(ctx, []string{cmd}); err != nil {
			return 0, fmt.Errorf("sonic apply igmp: %w", err)
		}
		sim.l2mcFeature = "enabled"
	}
	if len(ops) > 0 {
		c.Log.Printf("sonic apply igmp: %s; %s", l2mcSummary(ops), "sudo config save -y")
		if _, err := c.run(ctx, l2mcCommand, l2mcScript(ops)); err != nil {
			return 0, fmt.Errorf("sonic apply igmp: %w", err)
		}
		for _, o := range ops {
			sim.applyL2MC(o)
		}
	}
	if err := c.runBatch(ctx, []string{"sudo config save -y"}); err != nil {
		return 0, fmt.Errorf("sonic apply igmp: %w", err)
	}
	c.mu.Lock()
	if c.state == st {
		c.state = sim
	}
	c.mu.Unlock()
	return 1, nil
}

// l2mcSummary is the log line for a run of ops: each VLAN row in full,
// the statics counted.
func l2mcSummary(ops []l2mcOp) string {
	var parts []string
	setMembers, delMembers, groups := 0, 0, 0
	for _, o := range ops {
		switch {
		case strings.HasPrefix(o.Key, l2mcMember):
			if o.Fields == nil {
				delMembers++
			} else {
				setMembers++
			}
		case strings.HasPrefix(o.Key, l2mcGroup):
			groups++
		case o.Fields == nil:
			parts = append(parts, "del "+o.Key)
		default:
			parts = append(parts, "set "+o.Key+" "+pyDict(o.Fields))
		}
	}
	if setMembers+delMembers+groups > 0 {
		parts = append(parts, fmt.Sprintf("static groups: %d rows, %d members set, %d removed", groups, setMembers, delMembers))
	}
	return strings.Join(parts, " ; ")
}

// applyL2MC is an op's effect on the plan's copy of CONFIG_DB.
func (s *swState) applyL2MC(o l2mcOp) {
	if o.Fields == nil {
		delete(s.l2mc, o.Key)
		return
	}
	s.l2mc[o.Key] = copyFields(o.Fields)
}

// l2mcArgScript is l2mcScript taking its ops as sys.argv[2], so that the
// rows can be written from inside a port batch (one line per command).
const l2mcArgScript = l2mcScriptHead + "import sys\nrun(json.loads(sys.argv[2]))\n"

// l2mcArgCommand runs l2mcArgScript (base64, as applCommand); the batch's
// sudo comes from plan.add. Its first argument names it in the process list.
var l2mcArgCommand = "python3 -c " + sshrun.ShellQuote("import base64; exec(base64.b64decode('"+base64.StdEncoding.EncodeToString([]byte(l2mcArgScript))+"'))") + " disunified-l2mc"

// l2mcArgLogged stands for l2mcArgCommand in the apply log line, its ops
// summarised as applyIGMP's are.
const l2mcArgLogged = "python3 <ConfigDBConnector> disunified-l2mc"

// L2MCArgOps decodes a batch line of l2mcArgCommand (as sent, with sudo)
// into its ops: "del <key>" or "set <key>"; ok is false for any other line.
func L2MCArgOps(cmd string) (ops []string, ok bool) {
	raw, ok := l2mcArgJSON(cmd)
	if !ok {
		return nil, false
	}
	var os []l2mcOp
	if json.Unmarshal([]byte(raw), &os) != nil {
		return nil, false
	}
	for _, o := range os {
		if o.Fields == nil {
			ops = append(ops, "del "+o.Key)
		} else {
			ops = append(ops, "set "+o.Key)
		}
	}
	return ops, true
}

// l2mcArgJSON is the ops argument of an l2mcArgCommand line.
func l2mcArgJSON(cmd string) (string, bool) {
	rest, ok := strings.CutPrefix(cmd, "sudo "+l2mcArgCommand+" ")
	if !ok || len(rest) < 2 || rest[0] != '\'' || rest[len(rest)-1] != '\'' {
		return "", false
	}
	return strings.ReplaceAll(rest[1:len(rest)-1], `'\''`, "'"), true
}

// loggedCmds is cmds as the apply log prints them: an l2mcArgCommand line
// as l2mcArgLogged with its ops summarised.
func loggedCmds(cmds []string) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = c
		if w := strings.Fields(c); strings.Contains(c, "SAI_OBJECT_TYPE_ISOLATION_GROUP_MEMBER") && len(w) >= 4 {
			out[i] = "python3 <isolation wait> " + w[len(w)-3]
		}
		if raw, ok := l2mcArgJSON(c); ok {
			var ops []l2mcOp
			if json.Unmarshal([]byte(raw), &ops) == nil {
				out[i] = "sudo " + l2mcArgLogged + " (" + l2mcSummary(ops) + ")"
			}
		}
	}
	return out
}

// l2mcUnbind takes every L2MC row naming one of ifaces out of CONFIG_DB
// before a breakout replaces them: the static members and router ports
// keyed by them, and their place in the static groups' member lists.
// `config interface breakout -f` removes a port's dependencies the YANG
// models declare, and the image kit's sonic-buildimage 0010 made every
// interface in sonic-igmp-snooping.yang a plain string, so these rows
// outlived the port -- the old port's rows became the new port's of the
// same name, a vanished lane's named nothing (docs/drivers/sonic.md
// §11.25.3). With them gone first, l2mcd drops the members before the
// breakout takes the VLAN membership (§8.15's order), and applyIGMP writes
// the new ports' rows after the batch.
func (pl *plan) l2mcUnbind(idx int, ifaces []string, write func(int, string, func(*swState))) {
	gone := setOf(ifaces)
	var members, groups []string
	for k := range pl.sim.l2mc {
		parts := strings.Split(k, "|")
		switch {
		case parts[0] == strings.TrimSuffix(l2mcMember, "|") && len(parts) == 4 && gone[parts[3]],
			parts[0] == strings.TrimSuffix(l2mcRouter, "|") && len(parts) == 3 && gone[parts[2]]:
			members = append(members, k)
		case parts[0] == strings.TrimSuffix(l2mcGroup, "|"):
			for _, m := range strings.Split(pl.sim.l2mc[k]["static-members@"], ",") {
				if gone[m] {
					groups = append(groups, k)
					break
				}
			}
		}
	}
	sort.Strings(members)
	sort.Strings(groups)
	var ops []l2mcOp
	for _, k := range members {
		ops = append(ops, l2mcOp{Key: k})
	}
	// A group keeps its other members (its row is CLI-only: l2mcmgrd reads
	// the member rows); one left with none goes, applyIGMP sets it again.
	for _, k := range groups {
		var keep []string
		for _, m := range strings.Split(pl.sim.l2mc[k]["static-members@"], ",") {
			if m != "" && !gone[m] {
				keep = append(keep, m)
			}
		}
		if len(keep) == 0 {
			ops = append(ops, l2mcOp{Key: k})
			continue
		}
		f := copyFields(pl.sim.l2mc[k])
		f["static-members@"] = strings.Join(keep, ",")
		ops = append(ops, l2mcOp{Key: k, Fields: f})
	}
	if len(ops) == 0 {
		return
	}
	b, _ := json.Marshal(ops)
	write(idx, l2mcArgCommand+" "+sshrun.ShellQuote(string(b)), func(s *swState) {
		for _, o := range ops {
			s.applyL2MC(o)
		}
	})
}

// staticsOf are the static-member rows naming iface in VLAN id.
func (s *swState) staticsOf(iface string, id int) []string {
	pre := l2mcMember + "Vlan" + strconv.Itoa(id) + "|"
	var out []string
	for k := range s.l2mc {
		if rest, ok := strings.CutPrefix(k, pre); ok && strings.HasSuffix(rest, "|"+iface) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// vlanAddress is an IPv4 address CONFIG_DB gives the VLAN interface, ""
// when none: l2mcd's query source (§8.11).
func (s *swState) vlanAddress(vlan string) string {
	var out []string
	for k := range s.ifaddrs {
		if rest, ok := strings.CutPrefix(k, vlan+"|"); ok && !strings.Contains(rest, ":") {
			ip, _, _ := strings.Cut(rest, "/")
			out = append(out, ip)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return ""
	}
	return out[0]
}

// ifnamesOf is the switch interfaces of a front-panel port from the last
// collect (every lane of a split cage).
func (c *Collector) ifnamesOf(idx int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ifnames[idx]...)
}

// l2mcFixCheckCommand finds the image kit's sonic-swss 0004 in the running
// orchagent: the notice its L2mcOrch logs when it drops a port's group
// members with the port's VLAN membership or bridge port, which no other
// orchagent carries. Read-only, as macACLCheckCommand.
const l2mcFixCheckCommand = "docker exec swss grep -c -a -F 'l2mc group member %s removed with its port' /usr/bin/orchagent || true"

// checkL2MCFix runs l2mcFixCheckCommand once, on an image with l2mcd.
func (c *Collector) checkL2MCFix(ctx context.Context) {
	c.mu.Lock()
	known := c.state != nil && c.state.l2mcKnown
	c.mu.Unlock()
	if !known {
		return
	}
	out, err := c.run(ctx, l2mcFixCheckCommand, "")
	fixed := err == nil && atoi(strings.TrimSpace(out)) > 0
	c.mu.Lock()
	c.l2mcFixed = fixed
	c.mu.Unlock()
	if !fixed {
		c.Log.Printf("sonic: orchagent lacks the image kit's sonic-swss 0004: a port leaving its last VLAN waits for its bridge port to go (docs/drivers/sonic.md §8.15)")
	}
}

// bridgePortWaitLimit bounds bridgePortWait: l2mcd's member deletes reach
// orchagent within a second when nothing is wrong.
const bridgePortWaitLimit = 10

// bridgePortWaitScript waits until the chip has no bridge port for
// sys.argv[1] (ASIC_DB, mapped by COUNTERS_DB's name maps), exiting 1 with
// the reason after sys.argv[2] seconds. Read-only; stdin is not read.
const bridgePortWaitScript = `import sys, time
from swsscommon.swsscommon import SonicV2Connector
port, limit = sys.argv[1], float(sys.argv[2])
db = SonicV2Connector(use_unix_socket_path=True)
db.connect(db.COUNTERS_DB)
db.connect(db.ASIC_DB)
oid = db.get(db.COUNTERS_DB, "COUNTERS_PORT_NAME_MAP", port) or db.get(db.COUNTERS_DB, "COUNTERS_LAG_NAME_MAP", port)
if not oid:
    sys.exit(0)
end = time.time() + limit
while True:
    left = [k for k in (db.keys(db.ASIC_DB, "ASIC_STATE:SAI_OBJECT_TYPE_BRIDGE_PORT:*") or [])
            if db.get(db.ASIC_DB, k, "SAI_BRIDGE_PORT_ATTR_PORT_ID") == oid]
    if not left:
        sys.exit(0)
    if time.time() > end:
        print("%s: orchagent has not removed its bridge port in %d s (it refuses while L2MC members still name the port, docs/drivers/sonic.md 8.15); a swss restart or a reboot clears it" % (port, limit))
        sys.exit(1)
    time.sleep(0.2)
`

// bridgePortWait is the batch line that runs bridgePortWaitScript for iface:
// one line (a batch is read line by line), the script as a JSON string
// literal -- valid Python for ASCII -- handed to exec.
func bridgePortWait(iface string) string {
	lit, _ := json.Marshal(bridgePortWaitScript)
	return fmt.Sprintf("python3 -c %s %s %d </dev/null", sshrun.ShellQuote("exec("+string(lit)+")"), iface, bridgePortWaitLimit)
}
