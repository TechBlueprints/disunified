package sonic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// Port isolation, port security, egress rate limits and LLDP-MED
// (docs/drivers/sonic.md §11.12). Each follows what Ubiquiti's own SONiC
// switch does where community SONiC 202511 has the same mechanism:
//
//   - Isolation is one SAI isolation group of type port, DUI_ISOLATION,
//     written to APPL_DB ISOLATION_GROUP_TABLE (orchagent's
//     isolationgrouporch; no CONFIG_DB table, no CLI): the isolated ports are
//     both its members and its bind ports, so traffic entering an isolated
//     port cannot leave through another isolated one.
//   - Port security is the ECS's learn mode: the port stops learning
//     (PORT|<p> or PORTCHANNEL|<pc> learn_mode drop: unknown source MACs are
//     dropped, not learnt, not forwarded), and each allowed MAC gets a static
//     FDB entry on the port in every VLAN the port carries (APPL_DB
//     FDB_TABLE, fdborch; the ECS learns them in software instead).
//   - The egress rate limit is a port shaper: a SCHEDULER profile
//     DUI_EGRESS_<kbps> (meter_type bytes, pir in bytes/s) named by
//     PORT_QOS_MAP|<p> scheduler (qosorch, SAI_PORT_ATTR_QOS_SCHEDULER_PROFILE_ID).
//   - LLDP-MED is lldpd's custom TLV for the TIA OUI's capabilities TLV
//     (00-12-BB subtype 1: capabilities "LLDP-MED capabilities", device
//     class 4 "network connectivity"), set per port with lldpcli, as the
//     ECS's lldp manager does.
//
// The APPL_DB rows and lldpd's settings do not survive a swss or lldp
// restart; the loop reconciles every cycle, so the next cycle restores
// them. learn_mode and the shaper are CONFIG_DB and kept by config save.
const (
	isoGroup      = "DUI_ISOLATION"
	isoKey        = "ISOLATION_GROUP_TABLE:" + isoGroup
	egressPrefix  = "DUI_EGRESS_"
	learnDrop     = "drop"
	learnHardware = "hardware"
)

// The LLDP-MED TLVs (TIA OUI 00-12-BB): subtype 1, capabilities -- "00,01"
// LLDP-MED capabilities, "00,03" with network policy too -- and device class
// 4 (network connectivity); subtype 2, the voice network policy: application
// 1 (voice), then tagged, the VLAN, L2 priority 5 and DSCP 0, the values the
// ECS's own lldp manager sends. lldpcli takes and reports them as comma-
// separated hex bytes.
const (
	medCaps      = "00,01,04"
	medCapsVoice = "00,03,04"
)

func medPolicy(vlan int) string {
	v := 1<<22 | (vlan&0xfff)<<9 | 5<<6
	return fmt.Sprintf("01,%02x,%02x,%02x", v>>16&0xff, v>>8&0xff, v&0xff)
}

func medSet(ln string, subtype int, info string) string {
	return fmt.Sprintf("docker exec lldp lldpcli configure ports %s lldp custom-tlv replace oui 00,12,bb subtype %d oui-info %s", ln, subtype, info)
}

func medUnset(ln string, subtype int) string {
	return fmt.Sprintf("docker exec lldp lldpcli unconfigure ports %s lldp custom-tlv oui 00,12,bb subtype %d", ln, subtype)
}

// normHex lower-cases and zero-pads lldpcli's comma-separated hex bytes so
// what the driver writes compares with what lldpcli reports.
func normHex(s string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), ",")
	for i, p := range parts {
		if n, err := strconv.ParseUint(p, 16, 8); err == nil {
			parts[i] = fmt.Sprintf("%02x", n)
		}
	}
	return strings.Join(parts, ",")
}

// portFeat is the switch's side of the four features, read by collect.py.
type portFeat struct {
	// known: the collector printed the sections (captures before
	// 2026-11-07 lack them, and nothing is written then).
	known bool
	iso   map[string]bool   // DUI_ISOLATION's members ("" key absent = no group)
	fdb   map[string]string // APPL_DB FDB_TABLE static entries: "Vlan<id>:<mac>" -> port
	sched map[string]string // DUI_EGRESS_* scheduler -> its pir
	qos   map[string]string // PORT_QOS_MAP: interface -> scheduler
	// medKnown: the lldp_med section was read; med is each interface's
	// LLDP-MED capabilities TLV and medPol its network policy TLV, as
	// normalised hex ("" = not sent).
	medKnown bool
	med      map[string]string
	medPol   map[string]string
	// dot1xAuth is each 802.1X-controlled interface's hosts disunified-l2d
	// admitted (STATE_DB DUI_DOT1X_TABLE): l2d writes their FDB entries,
	// and the plan keeps them (§11.26).
	dot1xAuth map[string][]string
}

func (f portFeat) clone() portFeat {
	c := portFeat{known: f.known, medKnown: f.medKnown, iso: map[string]bool{}, fdb: map[string]string{}, sched: map[string]string{}, qos: map[string]string{}, med: map[string]string{}, medPol: map[string]string{}, dot1xAuth: f.dot1xAuth}
	for k := range f.iso {
		c.iso[k] = true
	}
	for k, v := range f.fdb {
		c.fdb[k] = v
	}
	for k, v := range f.sched {
		c.sched[k] = v
	}
	for k, v := range f.qos {
		c.qos[k] = v
	}
	for k, v := range f.med {
		c.med[k] = v
	}
	for k, v := range f.medPol {
		c.medPol[k] = v
	}
	return c
}

// readPortFeat fills portFeat from collect.py's appl_iso, appl_fdb,
// cfg_qos and lldp_med sections.
func readPortFeat(db map[string]table, sec map[string]string) portFeat {
	f := portFeat{iso: map[string]bool{}, fdb: map[string]string{}, sched: map[string]string{}, qos: map[string]string{}, med: map[string]string{}, medPol: map[string]string{}}
	_, a := sec["appl_iso"]
	_, b := sec["appl_fdb"]
	_, q := sec["cfg_qos"]
	f.known = a && b && q
	for k, v := range db["l2_dot1x"] {
		if n, ok := strings.CutPrefix(k, "DUI_DOT1X_TABLE|"); ok && v["authorized"] != "" {
			if f.dot1xAuth == nil {
				f.dot1xAuth = map[string][]string{}
			}
			f.dot1xAuth[n] = splitList(strings.ToLower(v["authorized"]))
		}
	}
	if g, ok := db["appl_iso"][isoKey]; ok {
		for _, p := range splitList(g["MEMBERS"]) {
			f.iso[p] = true
		}
	}
	for k, v := range db["appl_fdb"] {
		if v["type"] != "static" {
			continue
		}
		if key, ok := strings.CutPrefix(k, "FDB_TABLE:"); ok {
			f.fdb[strings.ToLower(key)] = v["port"]
		}
	}
	for k, v := range db["cfg_qos"] {
		if n, ok := strings.CutPrefix(k, "SCHEDULER|"); ok {
			f.sched[n] = v["pir"]
		} else if n, ok := strings.CutPrefix(k, "PORT_QOS_MAP|"); ok && v["scheduler"] != "" {
			f.qos[n] = v["scheduler"]
		}
	}
	// An empty section is lldpcli unread (the lldp container down): known
	// only once it lists a port.
	// Each line: "<port> <caps>|- [<policy>|-]" (before 2026-11-07's voice
	// VLAN, "<port> on|off": on was the plain capabilities TLV).
	for _, line := range strings.Split(sec["lldp_med"], "\n") {
		fs := strings.Fields(line)
		if len(fs) < 2 {
			continue
		}
		f.medKnown = true
		switch c := fs[1]; c {
		case "on":
			f.med[fs[0]] = medCaps
		case "off", "-":
		default:
			f.med[fs[0]] = normHex(c)
		}
		if len(fs) > 2 && fs[2] != "-" {
			f.medPol[fs[0]] = normHex(fs[2])
		}
	}
	return f
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// egressSched names the shaper profile for a rate; pir is bytes per second.
func egressSched(kbps int) string { return egressPrefix + strconv.Itoa(kbps) }
func egressPIR(kbps int) string   { return strconv.Itoa(kbps * 125) }

// egressPBS is the shaper's burst in bytes: 10 ms at the rate, at least
// two jumbo frames so a 9216-byte frame always fits.
func egressPBS(kbps int) string {
	return strconv.Itoa(max(kbps*125/100, 2*9216))
}

// learnTarget is where a port's learn mode lives: its PortChannel when it
// is a member (teammgrd hands PORTCHANNEL learn_mode to the LAG), else
// each lane.
func (pl *plan) secureTargets(d devicemodel.PortDesired) []string {
	if d.LAG > 0 {
		return []string{pcName(d.LAG)}
	}
	if pc := pl.sim.lagOf(lanesOf(pl.ports[d.Index])[0]); pc != "" {
		return []string{pc}
	}
	return lanesOf(pl.ports[d.Index])
}

// learnMode reads an interface's or PortChannel's CONFIG_DB learn_mode.
func (pl *plan) learnMode(iface string) string {
	if strings.HasPrefix(iface, "PortChannel") {
		return pl.sim.lagLearn[iface]
	}
	return pl.sim.field(iface, "learn_mode")
}

func (pl *plan) setLearnMode(idx int, iface, mode string) {
	tbl := "PORT|"
	if strings.HasPrefix(iface, "PortChannel") {
		tbl = "PORTCHANNEL|"
	}
	key := sshrun.ShellQuote(tbl + iface)
	set := func(s *swState, v string) {
		if strings.HasPrefix(iface, "PortChannel") {
			if v == "" {
				delete(s.lagLearn, iface)
			} else {
				s.lagLearn[iface] = v
			}
			return
		}
		s.setField(iface, "learn_mode", v)
	}
	if mode == "" {
		// Removing the field does not reach the ASIC (portmgrd passes only
		// the fields that remain), so the port goes back to hardware
		// learning first.
		pl.add(idx, "sonic-db-cli CONFIG_DB hset "+key+" learn_mode "+learnHardware, nil)
		pl.add(idx, "sonic-db-cli CONFIG_DB hdel "+key+" learn_mode", func(s *swState) { set(s, "") })
		return
	}
	pl.add(idx, "sonic-db-cli CONFIG_DB hset "+key+" learn_mode "+mode, func(s *swState) { set(s, mode) })
}

// portFeatures plans the four features for the pushed ports, after their
// VLANs and aggregates (the static FDB entries follow the VLANs).
func (pl *plan) portFeatures(ds []devicemodel.PortDesired, caps devicemodel.Capabilities) {
	if !pl.sim.pf.known {
		return
	}
	if caps.PortIsolation {
		pl.isolation(ds)
	}
	if caps.PortSecurity {
		pl.portSecurity(ds, caps.Dot1X)
	}
	if caps.EgressRateLimit {
		pl.egress(ds)
	}
	if caps.LLDPMED && pl.sim.pf.medKnown {
		pl.lldpMED(ds)
	}
	pl.applFlush()
}

func (pl *plan) isolation(ds []devicemodel.PortDesired) {
	want := map[string]bool{}
	first := 0
	for _, d := range ds {
		if !d.Isolation {
			continue
		}
		if first == 0 {
			first = d.Index
		}
		for _, t := range pl.secureTargets(d) {
			want[t] = true
		}
	}
	if sameSet(want, pl.sim.pf.iso) {
		return
	}
	idx := first
	if idx == 0 {
		for _, d := range ds {
			for _, t := range pl.secureTargets(d) {
				if pl.sim.pf.iso[t] {
					idx = d.Index
				}
			}
		}
	}
	if len(want) == 0 {
		pl.applDel(idx, "ISOLATION_GROUP_TABLE", isoGroup, func(s *swState) { s.pf.iso = map[string]bool{} })
		return
	}
	list := strings.Join(sortedSet(want), ",")
	pl.applSet(idx, "ISOLATION_GROUP_TABLE", isoGroup, []string{"TYPE", "port", "PORTS", list, "MEMBERS", list}, func(s *swState) {
		s.pf.iso = map[string]bool{}
		for k := range want {
			s.pf.iso[k] = true
		}
	})
}

// dot1xClosed: 802.1X holds the port closed -- a mode that admits hosts
// one by one, or none -- and the port is no aggregate's member (802.1X
// authenticates one netdev: an aggregate's members stay open, l2.go).
func (pl *plan) dot1xClosed(d devicemodel.PortDesired) bool {
	return d.Dot1X.Closed() && d.LAG == 0 && pl.sim.lagOf(lanesOf(pl.ports[d.Index])[0]) == ""
}

// portSecurity plans the MAC filter's learn mode and static FDB entries,
// and 802.1X's closed ports (the same learn mode; their admitted hosts'
// entries are l2d's, kept here).
func (pl *plan) portSecurity(ds []devicemodel.PortDesired, dot1x bool) {
	wantFDB := map[string]string{}
	owner := map[string]int{}
	for _, d := range ds {
		closed := dot1x && pl.dot1xClosed(d)
		for _, t := range pl.secureTargets(d) {
			owner[t] = d.Index
			mode := ""
			if d.PortSecurity || closed {
				mode = learnDrop
			}
			if closed {
				for _, mac := range pl.sim.pf.dot1xAuth[t] {
					for v := range pl.sim.members[t] {
						wantFDB[fmt.Sprintf("vlan%d:%s", v, mac)] = t
					}
				}
			}
			if have := pl.learnMode(t); have != mode && !(mode == "" && have == learnHardware) {
				pl.setLearnMode(d.Index, t, mode)
			}
			if !d.PortSecurity {
				continue
			}
			for _, mac := range d.AllowedMACs {
				for v := range pl.sim.members[t] {
					wantFDB[fmt.Sprintf("vlan%d:%s", v, strings.ToLower(mac))] = t
				}
			}
		}
	}
	keys := make([]string, 0, len(pl.sim.pf.fdb))
	for k := range pl.sim.pf.fdb {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if wantFDB[k] == pl.sim.pf.fdb[k] {
			continue
		}
		key := k
		pl.applDel(owner[pl.sim.pf.fdb[k]], "FDB_TABLE", fdbKey(key), func(s *swState) { delete(s.pf.fdb, key) })
	}
	keys = keys[:0]
	for k := range wantFDB {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if pl.sim.pf.fdb[k] == wantFDB[k] {
			continue
		}
		key, port := k, wantFDB[k]
		pl.applSet(owner[port], "FDB_TABLE", fdbKey(key), []string{"port", port, "type", "static"}, func(s *swState) { s.pf.fdb[key] = port })
	}
}

// fdbKey turns the lower-case map key back into the FDB_TABLE key
// "Vlan<id>:<mac>".
func fdbKey(k string) string { return "V" + strings.TrimPrefix(k, "v") }

// APPL_DB rows reach orchagent only through ProducerStateTable (its
// consumers wait on the table's key set and channel; a plain HSET is never
// seen -- probed on the DX010, §11.12), so these writes are collected and
// run as one python3 call at the end of the batch: the script, base64 so
// the batch stays one line per command, then the marker and the operations
// as a JSON argument.
const applScript = `import json, sys
import swsscommon.swsscommon as sw
db = sw.DBConnector("APPL_DB", 0, True)
tables = {}
for op in json.loads(sys.argv[2]):
    t = tables.setdefault(op["table"], sw.ProducerStateTable(db, op["table"]))
    if op["op"] == "set":
        fv = op["fv"]
        t.set(op["key"], sw.FieldValuePairs([(fv[i], fv[i + 1]) for i in range(0, len(fv), 2)]))
    else:
        t._del(op["key"])
`

// applCommand runs applScript; its first argument names it in the process
// list and the journal.
var applCommand = "sudo python3 -c " + sshrun.ShellQuote("import base64; exec(base64.b64decode('"+base64.StdEncoding.EncodeToString([]byte(applScript))+"'))") + " disunified-appl"

// applLogged stands for applCommand in the apply log line.
const applLogged = "sudo python3 <ProducerStateTable> disunified-appl"

type applOp struct {
	Op    string   `json:"op"`
	Table string   `json:"table"`
	Key   string   `json:"key"`
	FV    []string `json:"fv,omitempty"`
}

func (pl *plan) applSet(idx int, table, key string, fv []string, effect func(*swState)) {
	pl.appl = append(pl.appl, applOp{Op: "set", Table: table, Key: key, FV: fv})
	pl.applEffect(idx, effect)
}

func (pl *plan) applDel(idx int, table, key string, effect func(*swState)) {
	pl.appl = append(pl.appl, applOp{Op: "del", Table: table, Key: key})
	pl.applEffect(idx, effect)
}

func (pl *plan) applEffect(idx int, effect func(*swState)) {
	if effect != nil {
		effect(pl.sim)
	}
	if idx > 0 {
		pl.changed[idx] = true
	}
}

// applFlush appends the collected APPL_DB writes to the batch as one call.
func (pl *plan) applFlush() {
	if len(pl.appl) == 0 {
		return
	}
	b, _ := json.Marshal(pl.appl)
	pl.ops = append(pl.ops, op{cmd: applCommand + " " + sshrun.ShellQuote(string(b))})
	pl.appl = nil
}

func (pl *plan) egress(ds []devicemodel.PortDesired) {
	used := map[string]bool{}
	for _, d := range ds {
		for _, ln := range lanesOf(pl.ports[d.Index]) {
			want := ""
			if d.EgressKbps > 0 {
				want = egressSched(d.EgressKbps)
				used[want] = true
				if pl.sim.pf.sched[want] != egressPIR(d.EgressKbps) {
					name, pir := want, egressPIR(d.EgressKbps)
					pl.add(d.Index, fmt.Sprintf("sonic-db-cli CONFIG_DB hset %s meter_type bytes pir %s pbs %s", sshrun.ShellQuote("SCHEDULER|"+name), pir, egressPBS(d.EgressKbps)), func(s *swState) { s.pf.sched[name] = pir })
				}
			}
			have := pl.sim.pf.qos[ln]
			if have == want || (want == "" && !strings.HasPrefix(have, egressPrefix)) {
				continue
			}
			lane, key := ln, sshrun.ShellQuote("PORT_QOS_MAP|"+ln)
			if want == "" {
				pl.add(d.Index, "sonic-db-cli CONFIG_DB hdel "+key+" scheduler", func(s *swState) { delete(s.pf.qos, lane) })
				continue
			}
			w := want
			pl.add(d.Index, "sonic-db-cli CONFIG_DB hset "+key+" scheduler "+w, func(s *swState) { s.pf.qos[lane] = w })
		}
	}
	// Profiles nothing names any more go after the ports moved off them.
	for _, ln := range keysOf(pl.sim.pf.qos) {
		used[pl.sim.pf.qos[ln]] = true
	}
	for _, name := range keysOf(pl.sim.pf.sched) {
		if used[name] {
			continue
		}
		n := name
		pl.add(0, "sonic-db-cli CONFIG_DB del "+sshrun.ShellQuote("SCHEDULER|"+n), func(s *swState) { delete(s.pf.sched, n) })
	}
}

func (pl *plan) lldpMED(ds []devicemodel.PortDesired) {
	for _, d := range ds {
		on := d.LLDPMED == nil || *d.LLDPMED
		wantCaps, wantPol := "", ""
		if on {
			wantCaps = medCaps
			if d.VoiceVLAN > 0 {
				wantCaps, wantPol = medCapsVoice, medPolicy(d.VoiceVLAN)
			}
		}
		for _, ln := range lanesOf(pl.ports[d.Index]) {
			lane := ln
			// The policy goes first when it goes, last when it comes: a
			// network policy is never sent without the capabilities that
			// announce it.
			if wantPol == "" && pl.sim.pf.medPol[lane] != "" {
				pl.add(d.Index, medUnset(lane, 2), func(s *swState) { delete(s.pf.medPol, lane) })
			}
			if pl.sim.pf.med[lane] != wantCaps {
				if wantCaps == "" {
					pl.add(d.Index, medUnset(lane, 1), func(s *swState) { delete(s.pf.med, lane) })
				} else {
					w := wantCaps
					pl.add(d.Index, medSet(lane, 1, w), func(s *swState) { s.pf.med[lane] = w })
				}
			}
			if wantPol != "" && pl.sim.pf.medPol[lane] != wantPol {
				w := wantPol
				pl.add(d.Index, medSet(lane, 2, w), func(s *swState) { s.pf.medPol[lane] = w })
			}
		}
	}
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readBackPortFeat fills the ports' read-side settings from portFeat.
func readBackPortFeat(st *swState, p *devicemodel.Port, macsByIface map[string][]string) {
	if st == nil || !st.pf.known {
		return
	}
	ifs := lanesOf(*p)
	target := ifs[0]
	if pc := st.lagOf(target); pc != "" {
		target = pc
	}
	p.Isolated = st.pf.iso[target]
	learn := st.field(target, "learn_mode")
	if strings.HasPrefix(target, "PortChannel") {
		learn = st.lagLearn[target]
	}
	if learn == learnDrop {
		macs := macsByIface[target]
		dot1x := p.Dot1X.Closed() && !strings.HasPrefix(target, "PortChannel")
		if dot1x {
			// 802.1X closes the port with the same learn mode, and its
			// admitted hosts' static entries are l2d's: a MAC filter on
			// it shows only by entries of its own. Read as a MAC filter
			// with nothing allowed, the controller's UI showed the port
			// Disabled (its own rule for an empty list; live 2026-11-15,
			// docs/drivers/sonic.md §11.26.11).
			macs = withoutMACs(macs, st.pf.dot1xAuth[target])
		}
		if !dot1x || len(macs) > 0 {
			p.PortSecurity = true
			p.AllowedMACs = macs
		}
	}
	if n, ok := strings.CutPrefix(st.pf.qos[ifs[0]], egressPrefix); ok {
		p.EgressKbps = atoi(n)
	}
	if st.pf.medKnown {
		p.LLDPMEDOff = st.pf.med[ifs[0]] == ""
		if pol := st.pf.medPol[ifs[0]]; pol != "" {
			p.VoiceVLAN = policyVLAN(pol)
		}
	}
}

// withoutMACs is macs less those in drop (both lower case).
func withoutMACs(macs, drop []string) []string {
	var out []string
	for _, m := range macs {
		if !slices.Contains(drop, m) {
			out = append(out, m)
		}
	}
	return out
}

// staticMACs groups portFeat's static entries by port, each MAC once.
func staticMACs(f portFeat) map[string][]string {
	seen := map[string]map[string]bool{}
	for k, port := range f.fdb {
		_, mac, ok := strings.Cut(k, ":")
		if !ok {
			continue
		}
		if seen[port] == nil {
			seen[port] = map[string]bool{}
		}
		seen[port][mac] = true
	}
	out := map[string][]string{}
	for port, m := range seen {
		out[port] = keysOf(m)
	}
	return out
}

// heldLearn is a learn_mode row a breakout takes out of CONFIG_DB and puts
// back (breakout.go); lane is "" for a PortChannel's.
type heldLearn struct{ key, lane, mode string }

func (pl *plan) learnModesHeld() []heldLearn {
	var out []heldLearn
	for _, ln := range keysOf(pl.sim.port) {
		if m := pl.sim.port[ln]["learn_mode"]; m != "" {
			out = append(out, heldLearn{key: "PORT|" + ln, lane: ln, mode: m})
		}
	}
	for _, pc := range keysOf(pl.sim.lagLearn) {
		out = append(out, heldLearn{key: "PORTCHANNEL|" + pc, mode: pl.sim.lagLearn[pc]})
	}
	return out
}

// SwitchesLLDPMED implements devicemodel.LLDPMEDSwitcher.
func (c *Collector) SwitchesLLDPMED() bool { return true }

// policyVLAN reads the VLAN back out of a network policy TLV's bytes.
func policyVLAN(info string) int {
	b := strings.Split(info, ",")
	if len(b) != 4 {
		return 0
	}
	var v uint64
	for _, x := range b[1:] {
		n, err := strconv.ParseUint(x, 16, 8)
		if err != nil {
			return 0
		}
		v = v<<8 | n
	}
	return int(v>>9) & 0xfff
}
