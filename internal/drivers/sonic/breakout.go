package sonic

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// Dynamic port breakout (docs/drivers/sonic.md §7.1, §7.5). UniFi asks for
// a split or a join as a speed on the cage (lane 1); SONiC changes the
// cage's mode with `config interface breakout <parent> <mode>`, the modes
// coming from the platform's platform.json (the DX010: 1x100G[40G], 2x50G,
// 4x25G[10G]). The parent port is replaced by child ports that keep its
// `index`, so the read side folds them into one cage as before.
//
// Built from the SONiC source (sonic-utilities config/main.py `breakout`,
// sonic-config-engine portconfig.py `BreakoutCfg.get_config`) and tested
// against the DX010's platform.json; hardware-untested (the VS has no
// platform.json and refuses breakout).

// platformPort is one parent port of platform.json's interfaces block, as
// collect.py prints it.
type platformPort struct {
	Index         string              `json:"index"` // "2,2,2,2": one per serdes lane
	Lanes         string              `json:"lanes"` // "69,70,71,72"
	BreakoutModes map[string][]string `json:"breakout_modes"`
}

// parsePlatform reads the platform_json section: "no" (absent), the old
// "yes" form (present, nothing read), or the reduced interfaces block.
func parsePlatform(body string) (present bool, ports map[string]platformPort) {
	body = strings.TrimSpace(body)
	switch {
	case body == "yes":
		return true, nil
	case strings.HasPrefix(body, "{"):
		var m map[string]platformPort
		if err := json.Unmarshal([]byte(body), &m); err != nil || len(m) == 0 {
			return true, nil
		}
		return true, m
	}
	return false, nil
}

// cageIndex is the front-panel index a parent port's lanes share.
func (pp platformPort) cageIndex() int {
	first, _, _ := strings.Cut(pp.Index, ",")
	return atoi(first)
}

func (pp platformPort) lanes() []string {
	var out []string
	for _, l := range strings.Split(pp.Lanes, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (pp platformPort) indexes() []string {
	var out []string
	for _, l := range strings.Split(pp.Index, ",") {
		out = append(out, strings.TrimSpace(l))
	}
	return out
}

// brkMode is one breakout mode: "4x25G[10G]" is 4 ports at 25G by default,
// 10G allowed too; "(n)" would assign n of the cage's lanes.
type brkMode struct {
	name     string
	ports    int
	def      int   // Mb/s
	speeds   []int // def first, then the bracketed alternatives
	assigned int   // lanes used; 0 = all of them
}

var brkModeRe = regexp.MustCompile(`^(\d{1,6})x(\d{1,6}G?)(\[([0-9G,]*)\])?(\((\d{1,6})\))?$`)

// parseMode reads a single-entry mode ("1x100G[40G]", "2x50G"). Combined
// modes ("2x25G(2)+1x50G(2)") are not offered: no platform this driver
// knows has them, and UniFi's one speed per cage cannot name one.
func parseMode(name string) (brkMode, bool) {
	m := brkModeRe.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil {
		return brkMode{}, false
	}
	speed := func(s string) int {
		s = strings.TrimSpace(s)
		if strings.HasSuffix(s, "G") {
			return atoi(strings.TrimSuffix(s, "G")) * 1000
		}
		return atoi(s) // portconfig.py: a bare number is Mb/s
	}
	md := brkMode{name: name, ports: atoi(m[1]), def: speed(m[2]), assigned: atoi(m[6])}
	if md.ports <= 0 || md.def <= 0 {
		return brkMode{}, false
	}
	md.speeds = []int{md.def}
	for _, s := range strings.Split(m[4], ",") {
		if v := speed(s); v > 0 && !containsInt(md.speeds, v) {
			md.speeds = append(md.speeds, v)
		}
	}
	return md, true
}

// modes lists the parent's breakout modes, fewest ports first (a stable
// order: platform.json's own order is lost in JSON decoding).
func (pp platformPort) modes() []brkMode {
	var out []brkMode
	for name := range pp.BreakoutModes {
		if m, ok := parseMode(name); ok {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ports != out[j].ports {
			return out[i].ports < out[j].ports
		}
		return out[i].name < out[j].name
	})
	return out
}

// speeds is every speed any of the cage's modes runs: what the cage is
// claimed to support, so the controller lets the operator split and join.
func (pp platformPort) speeds() []int {
	var out []int
	for _, m := range pp.modes() {
		out = mergeSpeeds(out, m.speeds)
	}
	return out
}

// childPort is one PORT entry a breakout creates.
type childPort struct {
	name   string
	fields map[string]string
}

// children is SONiC's BreakoutCfg.get_config: the ports a mode makes of a
// parent. Names count up from the parent's number by lane (Ethernet4 in
// 4x25G is Ethernet4..7, in 2x50G Ethernet4 and Ethernet6), each takes its
// share of the lanes, the alias from platform.json's list, the mode's
// default speed and the parent's index; RS-FEC where a lane runs 50G+.
func (pp platformPort) children(parent string, m brkMode) ([]childPort, error) {
	lanes, idx := pp.lanes(), pp.indexes()
	assigned := m.assigned
	if assigned == 0 {
		assigned = len(lanes)
	}
	if assigned > len(lanes) || m.ports == 0 || assigned%m.ports != 0 {
		return nil, fmt.Errorf("mode %s does not fit %d lanes", m.name, len(lanes))
	}
	aliases := pp.BreakoutModes[m.name]
	if len(aliases) < m.ports {
		return nil, fmt.Errorf("mode %s lists %d aliases for %d ports", m.name, len(aliases), m.ports)
	}
	base := ifNum(parent)
	per := assigned / m.ports
	var out []childPort
	for i := 0; i < m.ports; i++ {
		laneID := i * per
		f := map[string]string{
			"alias": aliases[i],
			"lanes": strings.Join(lanes[laneID:laneID+per], ","),
			"speed": strconv.Itoa(m.def),
			"index": idx[min(laneID, len(idx)-1)],
		}
		if m.ports == 1 {
			f["subport"] = "0"
		} else {
			f["subport"] = strconv.Itoa(i + 1)
		}
		if m.def/per >= 50000 {
			f["fec"] = "rs"
		}
		out = append(out, childPort{name: "Ethernet" + strconv.Itoa(base+laneID), fields: f})
	}
	return out, nil
}

// cageAlias is the faceplate label of a cage: the parent's alias, which a
// split cage's children carry with "/<n>" (Eth2/1 -> Eth2), so the port's
// name does not change on a split or a join.
func cageAlias(alias string, split bool) string {
	if !split {
		return alias
	}
	if i := strings.LastIndex(alias, "/"); i > 0 && atoi(alias[i+1:]) > 0 {
		return alias[:i]
	}
	return alias
}

// platformCage is the platform.json parent of cage idx.
func (s *swState) platformCage(idx int) (string, platformPort, bool) {
	for name, pp := range s.platform {
		if pp.cageIndex() == idx {
			return name, pp, true
		}
	}
	return "", platformPort{}, false
}

// cagePorts lists the PORT entries of cage idx, in lane order.
func (s *swState) cagePorts(idx int) []string {
	var out []string
	for name, f := range s.port {
		if atoi(f["index"]) == idx {
			out = append(out, name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return ifNum(out[i]) < ifNum(out[j]) })
	return out
}

// applyBreakout is a breakout's effect on CONFIG_DB with -f: the cage's
// ports and every dependency on them (VLAN memberships, storm control, STP
// port settings, LAG memberships) go, the mode's children come, and
// BREAKOUT_CFG records the mode. The children carry no admin_status (down)
// and no MTU (SONiC's 9100): the driver writes the cage's intent on them.
func (s *swState) applyBreakout(parent string, pp platformPort, idx int, children []childPort, mode string) {
	for _, name := range s.cagePorts(idx) {
		delete(s.port, name)
		delete(s.members, name)
		delete(s.storm, name)
		delete(s.stp, name)
		for _, m := range s.lags {
			delete(m, name)
		}
	}
	for _, ch := range children {
		s.port[ch.name] = copyFields(ch.fields)
	}
	if s.brkout == nil {
		s.brkout = map[string]string{}
	}
	s.brkout[parent] = mode
}

// breakoutCommand is the write: -y answers the confirmation; -f removes
// the dependencies (the cage's VLAN memberships, which every port on this
// driver has) instead of refusing; no -l, because it loads
// /etc/sonic/port_breakout_config_db.json, which no SONiC build generates
// (absent on the VS) and whose absence fails the breakout. stdin is
// /dev/null: the breakout may still prompt ("Below Config can not be
// verified ... Do you wish to Continue?") for tables without a YANG model,
// and inside the `bash -s` batch that prompt would read the rest of the
// batch as its answer; with /dev/null it aborts and the batch stops there.
func breakoutCommand(parent, mode string) string {
	return fmt.Sprintf("config interface breakout -f -y %s %s </dev/null", parent, sshrun.ShellQuote(mode))
}

// breakoutSpeed handles a speed on a cage the platform can break out: a
// speed the current mode runs is a lane-speed change on every lane; any
// other is a new mode (the cage's intent is then written on the children by
// the rest of the plan), then the lane speed when the mode's default is not
// the one asked for (4x25G[10G] at 10G).
func (pl *plan) breakoutSpeed(p devicemodel.Port, d devicemodel.PortDesired, want int, parent string, pp platformPort) {
	cur, ok := parseMode(pl.sim.brkout[parent])
	if !ok {
		pl.note("port %d: BREAKOUT_CFG has no usable mode for %s (%q); speed not written", d.Index, parent, pl.sim.brkout[parent])
		return
	}
	if !containsInt(cur.speeds, want) {
		var target *brkMode
		for _, m := range pp.modes() {
			if !containsInt(m.speeds, want) {
				continue
			}
			if target == nil || (m.def == want && target.def != want) {
				mm := m
				target = &mm
			}
		}
		if target == nil {
			pl.note("port %d: %d Mb/s is not among the cage's breakout speeds %v; speed not written", d.Index, want, pp.speeds())
			return
		}
		if why := pl.breakoutBlocked(d, p.Index); why != "" {
			pl.note("port %d: %d Mb/s needs breakout mode %s, but the cage %s; take it out first; speed not written", d.Index, want, target.name, why)
			return
		}
		children, err := pp.children(parent, *target)
		if err != nil {
			pl.note("port %d: breakout %s of %s: %v; speed not written", d.Index, target.name, parent, err)
			return
		}
		idx, mode, par := p.Index, target.name, parent
		pl.add(d.Index, breakoutCommand(parent, mode), func(s *swState) { s.applyBreakout(par, pp, idx, children, mode) })
		port := pl.ports[d.Index]
		port.Interfaces = pl.sim.cagePorts(idx)
		port.IfName = port.Interfaces[0]
		pl.ports[d.Index] = port
	}
	// The lane speed, on every lane (a lane left at another speed would not
	// link with its neighbours' optic).
	for _, ln := range lanesOf(pl.ports[d.Index]) {
		lane := ln
		if isTrue(pl.sim.field(lane, "autoneg")) {
			pl.add(d.Index, fmt.Sprintf("config interface autoneg %s disabled", lane), func(s *swState) { s.setField(lane, "autoneg", "off") })
		}
		if atoi(pl.sim.field(lane, "speed")) != want {
			v := strconv.Itoa(want)
			pl.add(d.Index, fmt.Sprintf("config interface speed %s %s", lane, v), func(s *swState) { s.setField(lane, "speed", v) })
		}
	}
}

// breakoutBlocked says why a cage cannot be broken out now: SONiC's -f
// would silently drop its aggregate membership or mirror session, which
// the controller still wants.
func (pl *plan) breakoutBlocked(d devicemodel.PortDesired, idx int) string {
	if d.LAG > 0 {
		return "is in an aggregate"
	}
	if d.MirrorSource > 0 {
		return "is a mirror destination"
	}
	for _, ln := range pl.sim.cagePorts(idx) {
		if pl.sim.lagOf(ln) != "" {
			return "is in an aggregate"
		}
		if pl.sim.mirrorTo(ln) != "" {
			return "is a mirror destination"
		}
		for _, m := range pl.sim.mirrors {
			for _, src := range strings.Split(m["src_port"], ",") {
				if strings.TrimSpace(src) == ln {
					return "is a mirror source"
				}
			}
		}
	}
	return ""
}
