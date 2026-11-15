package aristaeos

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// What FixtureTransport does with a config batch: each command the driver
// writes is applied to the replies the driver reads it back from, so the
// next read shows it. A transformation is written only where every form it
// produces is shown by a capture of the switch after such a command; each
// family below names its capture. Everything else is recorded in
// Unmodelled and leaves the replies as they are:
//
//   - a speed that splits or joins a cage (the lanes appear or go: the
//     tests switch Dir to a capture of the cage after it), a port with a
//     link joining an aggregate (LACP would bundle it; fixture_lag.go has
//     what is modelled), shutdown of a port with a link (its LLDP, MAC and
//     counters would change too);
//   - `ip address dhcp` (the driver declines DHCP), STP mode and priority
//     (no capture shows `spanning-tree priority` or a mode but rstp), SNMP
//     communities (no capture has one), SSH keys (every capture's key is
//     scrubbed, so how EOS shows a written one's comment is not shown), MTU
//     and any command not listed here. The driver writes no hostname.
//
// The aggregates are in fixture_lag.go and the ACL read-back (`show ip
// access-lists`, per list and summary) in fixture_acl.go.
// `vlan <list>` and `no vlan <list>` with the IGMP lines are held to
// vlan-prune/ (vlan_prune_test.go).
//
// Configuration sessions are modelled as EOS runs them: the commands of a
// `configure session NAME` are applied at its `commit timer` (EOS commits
// then and reverts at the timer unless confirmed), `configure session NAME
// commit` confirms it, and `configure session NAME abort` reverts a pending
// one at once. A session left without a commit changes nothing.

// fixtureSession is a configuration session the transport has seen.
type fixtureSession struct {
	cmds    []string
	pending bool                   // committed under a timer, not yet confirmed
	undo    map[string]*fixtureDoc // the written-to replies before the commit
	undoWas map[string]ifStatus
}

// ifStatus is the part of an interface's `show interfaces` entry a
// shutdown changes.
type ifStatus struct{ interfaceStatus, autoNegotiate any }

// configure runs one batch (f.mu held).
func (f *FixtureTransport) configure(cmds []string) {
	var sess *fixtureSession
	var sessName string
	var plain []string
	for _, c := range cmds {
		switch {
		case c == "enable", c == "configure", c == "end", c == "write memory":
			continue
		case strings.HasPrefix(c, "configure session "):
			name := strings.TrimPrefix(c, "configure session ")
			switch {
			case strings.HasSuffix(name, " commit"):
				f.confirmSession(strings.TrimSuffix(name, " commit"))
			case strings.HasSuffix(name, " abort"):
				f.abortSession(strings.TrimSuffix(name, " abort"))
			default:
				if f.sessions == nil {
					f.sessions = map[string]*fixtureSession{}
				}
				if f.sessions[name] == nil {
					f.sessions[name] = &fixtureSession{}
				}
				sess, sessName = f.sessions[name], name
			}
			continue
		}
		if sess == nil {
			plain = append(plain, c)
			continue
		}
		switch {
		case strings.HasPrefix(c, "commit timer "):
			f.commitSession(sess, true)
			sess = nil
		case c == "commit":
			f.commitSession(sess, false)
			sess = nil
		case c == "abort":
			f.abortSession(sessName)
			sess = nil
		default:
			sess.cmds = append(sess.cmds, c)
		}
	}
	f.apply(plain)
	f.syncACLSummary()
}

func (f *FixtureTransport) commitSession(s *fixtureSession, timer bool) {
	if timer {
		s.undo = map[string]*fixtureDoc{}
		for k, d := range f.docs {
			s.undo[k] = cloneDoc(d)
		}
		s.undoWas = map[string]ifStatus{}
		for k, v := range f.was {
			s.undoWas[k] = v
		}
		s.pending = true
	}
	f.apply(s.cmds)
	s.cmds = nil
}

func (f *FixtureTransport) confirmSession(name string) {
	s := f.sessions[name]
	switch {
	case s == nil:
	case s.pending:
		s.pending, s.undo, s.undoWas = false, nil, nil
	case len(s.cmds) > 0:
		f.commitSession(s, false)
	}
}

func (f *FixtureTransport) abortSession(name string) {
	s := f.sessions[name]
	if s == nil {
		return
	}
	if s.pending {
		f.docs, f.was = s.undo, s.undoWas
	}
	delete(f.sessions, name)
}

func cloneDoc(d *fixtureDoc) *fixtureDoc {
	b, _ := json.Marshal(d.v)
	var v map[string]any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	_ = dec.Decode(&v)
	return &fixtureDoc{path: d.path, v: v}
}

// apply runs config-mode commands in order, as EOS does: `interface X`,
// `ip access-list X` and `vlan X` enter a sub-mode, `exit` leaves it, and
// a command the sub-mode does not take is a config-mode command.
func (f *FixtureTransport) apply(cmds []string) {
	mode, arg := "", ""
	for _, c := range cmds {
		if c == "exit" {
			mode, arg = "", ""
			continue
		}
		switch mode {
		case "interface":
			if f.interfaceCmd(arg, c) {
				continue
			}
		case "acl":
			if f.aclCmd(arg, c) {
				continue
			}
		case "vlan":
			if !strings.HasPrefix(c, "interface ") && !strings.HasPrefix(c, "ip access-list ") && !strings.HasPrefix(c, "vlan ") {
				f.unmodelled("vlan "+arg, c)
				continue
			}
		}
		mode, arg = "", ""
		switch {
		case strings.HasPrefix(c, "interface "):
			mode, arg = "interface", strings.TrimPrefix(c, "interface ")
			if poNameRe.MatchString(arg) && f.rcTop() != nil && !f.portChannelExists(arg) {
				// Any `interface Port-Channel<n>` line creates one, bare
				// (docs/drivers/arista-eos.md §4; Po4 of empty-po-leftovers).
				f.createPortChannel(arg, "")
			}
		case strings.HasPrefix(c, "ip access-list "):
			mode, arg = "acl", strings.TrimPrefix(c, "ip access-list ")
			f.rcSection("ip access-list "+arg, true)
			f.aclList(arg, false)
		case strings.HasPrefix(c, "vlan "):
			mode, arg = "vlan", strings.TrimPrefix(c, "vlan ")
			f.createVLANs(arg)
		default:
			if !f.globalCmd(c) {
				f.unmodelled("config", c)
			}
		}
	}
}

func (f *FixtureTransport) unmodelled(mode, c string) {
	f.Unmodelled = append(f.Unmodelled, mode+": "+c)
}

// ---- the replies, as maps ----

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// child is m[k] as an object, created when create is set.
func child(m map[string]any, k string, create bool) map[string]any {
	if m == nil {
		return nil
	}
	c := obj(m[k])
	if c == nil && create {
		c = map[string]any{}
		m[k] = c
	}
	return c
}

// rcTop is the running config's top-level commands.
func (f *FixtureTransport) rcTop() map[string]any {
	d := f.doc("show running-config")
	if d == nil {
		return nil
	}
	return child(d.v, "cmds", true)
}

// rcSection is a section's commands (`interface X`, `ip access-list X`),
// created as EOS shows a new one ({"cmds": {}, "comments": []}, every
// section of the capture) when create is set.
func (f *FixtureTransport) rcSection(name string, create bool) map[string]any {
	top := f.rcTop()
	if top == nil {
		return nil
	}
	s := obj(top[name])
	if s == nil {
		if !create {
			return nil
		}
		s = map[string]any{"cmds": map[string]any{}, "comments": []any{}}
		top[name] = s
	}
	return child(s, "cmds", true)
}

// ---- config mode ----

var (
	mirrorSourceRe = regexp.MustCompile(`^monitor session (\d+) source (\S+) both$`)
	mirrorDestRe   = regexp.MustCompile(`^monitor session (\d+) destination (\S+)$`)
	igmpVLANRe     = regexp.MustCompile(`^(no )?ip igmp snooping vlan (\d+)$`)
)

// globalCmd applies a config-mode command; false when it is not modelled.
func (f *FixtureTransport) globalCmd(c string) bool {
	switch {
	case strings.HasPrefix(c, "no ip access-list "):
		// The list goes from the running config (the noacl round of
		// 2026-11-14 read back no list: docs/drivers/arista-eos.md §9.7).
		if top := f.rcTop(); top != nil {
			name := strings.TrimPrefix(c, "no ip access-list ")
			delete(top, "ip access-list "+name)
			f.aclList(name, true)
			return true
		}
	case strings.HasPrefix(c, "no interface Port-Channel"):
		return f.removePortChannel(strings.TrimPrefix(c, "no interface "))
	case strings.HasPrefix(c, "mac address-table static "), strings.HasPrefix(c, "no mac address-table static "):
		// As written, a one-liner (`mac address-table static 0200.0000.0001
		// vlan 1 drop`, mac-drop/show-running-config-mac-drop.json).
		return f.rcLine(c, "mac address-table static ", func(v string) string { return v })
	case strings.HasPrefix(c, "ntp server "), strings.HasPrefix(c, "no ntp server "):
		// One line per server, as written (`ntp server 192.0.2.2`, the
		// capture's running config).
		return f.rcLine(c, "ntp server ", func(v string) string { return v })
	case strings.HasPrefix(c, "ip route 0.0.0.0/0 "), strings.HasPrefix(c, "no ip route 0.0.0.0/0 "):
		// The default route as written (`ip route 0.0.0.0/0 192.0.2.2`, the
		// running config of every capture), the address move's gateway.
		return f.rcLine(c, "ip route 0.0.0.0/0 ", func(v string) string { return v })
	case strings.HasPrefix(c, "ip name-server vrf default "), strings.HasPrefix(c, "no ip name-server vrf default "):
		// As written (`ip name-server vrf default 192.0.2.2`, every capture).
		return f.rcLine(c, "ip name-server vrf default ", func(v string) string { return v })
	case strings.HasPrefix(c, "logging host "), strings.HasPrefix(c, "no logging host "):
		// EOS shows the default port: `logging host 192.0.2.5 514`
		// (the running config of the split-cage54 captures, written as
		// `logging host 192.0.2.5`).
		return f.rcLine(c, "logging host ", func(v string) string {
			if !strings.Contains(v, " ") {
				return v + " 514"
			}
			return v
		})
	case mirrorSourceRe.MatchString(c):
		// The running config drops `both`, the default (`monitor session
		// 1542 source Ethernet54/1`, split-cage54-lag; arista-eos.md §10).
		if top := f.rcTop(); top != nil {
			top[strings.TrimSuffix(c, " both")] = nil
			return true
		}
	case mirrorDestRe.MatchString(c):
		if top := f.rcTop(); top != nil {
			top[c] = nil
			return true
		}
	case strings.HasPrefix(c, "no monitor session "):
		if top := f.rcTop(); top != nil {
			p := "monitor session " + strings.TrimPrefix(c, "no monitor session ") + " "
			for k := range top {
				if strings.HasPrefix(k, p) {
					delete(top, k)
				}
			}
			return true
		}
	case igmpVLANRe.MatchString(c):
		// `igmpSnoopingState` per VLAN, enabled or disabled (both in the
		// split-cage54 capture, after the driver's own writes), and the
		// running config's `no ip igmp snooping vlan N` line, there while
		// it is disabled, for a VLAN that exists or not (vlan-prune/:
		// 1-created adds it, 2-deleted keeps it after `no vlan`,
		// 3-igmp-cleared drops it at `ip igmp snooping vlan N`).
		m := igmpVLANRe.FindStringSubmatch(c)
		d := f.doc("show ip igmp snooping")
		top := f.rcTop()
		if d == nil && top == nil {
			return false
		}
		if v := child(child(d.vOrNil(), "vlans", false), m[2], false); v != nil {
			// Three fields follow the state on every VLAN of the vlan-prune
			// captures: off floods (floodingTraffic, no pruning, report
			// flooding "default"), on prunes.
			off := m[1] != ""
			v["igmpSnoopingState"] = map[bool]string{true: "disabled", false: "enabled"}[off]
			v["floodingTraffic"], v["pruningActive"] = off, !off
			v["reportFlooding"] = map[bool]string{true: "default", false: "disabled"}[off]
		} else if top == nil {
			return false
		}
		if top != nil {
			if m[1] != "" {
				top["no ip igmp snooping vlan "+m[2]] = nil
			} else {
				delete(top, "no ip igmp snooping vlan "+m[2])
			}
		}
		return true
	case strings.HasPrefix(c, "no vlan "):
		return f.deleteVLANs(strings.TrimPrefix(c, "no vlan "))
	case c == "environment fan-speed override 30", c == "no environment fan-speed":
		// The two fields the driver reads, as captured after each:
		// show-system-environment-cooling-override30.json (manual, 30) and
		// show-system-environment-cooling-default-20261114-after-quiet.json
		// (automatic, 0).
		d := f.doc("show system environment cooling")
		if d == nil {
			return false
		}
		if c == "no environment fan-speed" {
			d.v["coolingMode"], d.v["overrideFanSpeed"] = "automatic", json.Number("0")
		} else {
			d.v["coolingMode"], d.v["overrideFanSpeed"] = "manual", json.Number("30")
		}
		return true
	}
	return false
}

// removePortChannel is `no interface Port-Channel<n>`: it leaves `show
// interfaces`, `show interfaces switchport`, `show interfaces status` and
// the running config (empty-po-leftovers/, then empty-po-none/ after the
// driver removed Port-Channel1-4), and takes any member out of the group
// as `no channel-group` does (channel-group-po9/3-no-interface-with-member;
// empty-po-lag5 -> empty-po-lag5-removed-fixed). One that does not exist
// is silent, as on the switch.
func (f *FixtureTransport) removePortChannel(name string) bool {
	top := f.rcTop()
	if top == nil {
		return false
	}
	m := poNameRe.FindStringSubmatch(name)
	if m == nil {
		return false
	}
	n, _ := strconv.Atoi(m[1])
	for _, l := range aclListsBoundIn(f.rcSection("interface "+name, false)) {
		f.aclList(l, false) // its binding goes with the interface
	}
	members := f.members(n)
	for _, iface := range members {
		if !f.canLeave(iface) {
			return false
		}
	}
	for _, iface := range members {
		f.leaveChannel(iface)
	}
	delete(child(f.doc("show port-channel summary").vOrNil(), "portChannels", false), name)
	delete(top, "interface "+name)
	for _, cmd := range []string{"show interfaces", "show interfaces status"} {
		delete(child(f.doc(cmd).vOrNil(), "interfaceStatuses", false), name)
		delete(child(f.doc(cmd).vOrNil(), "interfaces", false), name)
	}
	delete(child(f.doc("show interfaces switchport").vOrNil(), "switchports", false), name)
	return true
}

// rcLine adds or removes a one-line config-mode command (prefix + value);
// shown maps the written value to the running config's form.
func (f *FixtureTransport) rcLine(c, prefix string, shown func(string) string) bool {
	top := f.rcTop()
	if top == nil {
		return false
	}
	if v, ok := strings.CutPrefix(c, "no "+prefix); ok {
		key := prefix + shown(v)
		for k := range top {
			if k == key || strings.HasPrefix(k, prefix+v+" ") {
				delete(top, k)
			}
		}
		return true
	}
	top[prefix+shown(strings.TrimPrefix(c, prefix))] = nil
	return true
}

// createVLANs adds the VLANs of `vlan <list>` to `show vlan` and `show ip
// igmp snooping` as EOS lists a VLAN it creates without a name: VLAN0069,
// VLAN4000 and VLAN4010 in the split-cage54 capture (taken after the driver
// created them), active, snooping on as the switch is, and in the running
// config's list of unnamed VLANs (rcVLANSections). Only the ids and the
// snooping state are read from `show vlan`; `interfaces` (the forwarding
// members) is left empty.
func (f *FixtureTransport) createVLANs(list string) {
	ids, all := parseVLANList(list)
	if all || len(ids) == 0 {
		f.unmodelled("config", "vlan "+list)
		return
	}
	vd := f.doc("show vlan")
	if vd == nil {
		f.unmodelled("config", "vlan "+list)
		return
	}
	vlans := child(vd.v, "vlans", true)
	igmp := f.doc("show ip igmp snooping")
	f.rcVLANSections(ids, nil)
	for _, id := range ids {
		k := strconv.Itoa(id)
		if _, ok := vlans[k]; ok {
			continue
		}
		vlans[k] = map[string]any{"dynamic": false, "interfaces": map[string]any{}, "name": fmt.Sprintf("VLAN%04d", id), "status": "active"}
		if igmp == nil {
			continue
		}
		iv := child(igmp.v, "vlans", true)
		if _, ok := iv[k]; ok {
			continue
		}
		// The other fields as the lowest VLAN the capture lists has them.
		keys := make([]int, 0, len(iv))
		for kk := range iv {
			if n, err := strconv.Atoi(kk); err == nil {
				keys = append(keys, n)
			}
		}
		sort.Ints(keys)
		e := map[string]any{}
		if len(keys) > 0 {
			for kk, vv := range obj(iv[strconv.Itoa(keys[0])]) {
				e[kk] = vv
			}
		}
		e["igmpSnoopingState"] = igmp.v["igmpSnoopingState"]
		iv[k] = e
	}
}

// deleteVLANs is `no vlan <list>` (vlan-prune/2-deleted, after `no vlan
// 3977,3978`): the VLANs leave `show vlan`, `show ip igmp snooping` and
// their `vlan` sections of the running config, and nothing else does: an
// `interface VlanN` stays, as do the `no ip igmp snooping vlan N` and `no
// spanning-tree vlan-id` lines and any port's switchport lines naming them.
// EOS deletes silently, an SVI or not.
func (f *FixtureTransport) deleteVLANs(list string) bool {
	ids, all := parseVLANList(list)
	vd := f.doc("show vlan")
	if all || len(ids) == 0 || vd == nil {
		return false
	}
	gone := map[int]bool{}
	for _, id := range ids {
		gone[id] = true
		k := strconv.Itoa(id)
		delete(child(vd.v, "vlans", false), k)
		delete(child(f.doc("show ip igmp snooping").vOrNil(), "vlans", false), k)
	}
	f.rcVLANSections(nil, gone)
	return true
}

// rcVLANSections keeps the running config's `vlan` sections as EOS shows
// them: a named VLAN in a section of its own (`vlan 2` / `name kids`), the
// unnamed ones folded into one list in the compact form (`vlan
// 69,3000,3977-3978,4000,4010,4040`, vlan-prune/1-created after `vlan
// 3977,3978`; 2-deleted after `no vlan 3977,3978`). add joins the unnamed
// list unless a section has the VLAN already; gone leaves every section.
func (f *FixtureTransport) rcVLANSections(add []int, gone map[int]bool) {
	top := f.rcTop()
	if top == nil {
		return
	}
	have := map[int]bool{}
	var group []int
	groupKey := ""
	for k, v := range top {
		list, ok := strings.CutPrefix(k, "vlan ")
		if !ok {
			continue
		}
		ids, all := parseVLANList(list)
		if all {
			continue
		}
		sec := obj(v)
		if len(child(sec, "cmds", false)) == 0 && groupKey == "" {
			groupKey, group = k, ids
			for _, id := range ids {
				have[id] = true
			}
			continue
		}
		var kept []int
		for _, id := range ids {
			have[id] = true
			if !gone[id] {
				kept = append(kept, id)
			}
		}
		if len(kept) != len(ids) {
			delete(top, k)
			if len(kept) > 0 {
				top["vlan "+formatVLANList(kept)] = sec
			}
		}
	}
	var next []int
	for _, id := range group {
		if !gone[id] {
			next = append(next, id)
		}
	}
	for _, id := range add {
		if !have[id] {
			next = append(next, id)
			have[id] = true
		}
	}
	if groupKey != "" {
		delete(top, groupKey)
	}
	if len(next) > 0 {
		top["vlan "+formatVLANList(uniqueSorted(next))] = map[string]any{"cmds": map[string]any{}, "comments": []any{}}
	}
}

// ---- interface mode ----

var (
	ipAddressRe  = regexp.MustCompile(`^ip address (\d+\.\d+\.\d+\.\d+)/(\d+)$`)
	stormLevelRe = regexp.MustCompile(`^storm-control (broadcast|multicast|unknown-unicast) level ([0-9.]+)$`)
	speedRe      = regexp.MustCompile(`^speed forced (\S+)$`)
)

// interfaceCmd applies an interface-mode command to iface; false when the
// command is not an interface-mode one (the caller then takes it as a
// config-mode command). One it takes but does not model is recorded.
func (f *FixtureTransport) interfaceCmd(iface, c string) bool {
	switch {
	case c == "shutdown", c == "no shutdown":
		if !f.shutdown(iface, c == "shutdown") {
			f.unmodelled("interface "+iface, c)
		}
	case strings.HasPrefix(c, "description "), c == "no description":
		// `description` in `show interfaces`, the text as written
		// (write-forms/, captured after `description` on the copper test port).
		d := f.doc("show interfaces")
		e := child(child(d.vOrNil(), "interfaces", false), iface, false)
		if e == nil {
			f.unmodelled("interface "+iface, c)
			break
		}
		e["description"] = strings.TrimPrefix(strings.TrimPrefix(c, "no description"), "description ")
	case strings.HasPrefix(c, "switchport "), c == "no switchport trunk native vlan":
		if !f.switchport(iface, c) {
			f.unmodelled("interface "+iface, c)
		}
	case strings.HasPrefix(c, "storm-control "), strings.HasPrefix(c, "no storm-control "):
		if !f.storm(iface, c) {
			f.unmodelled("interface "+iface, c)
		}
	case strings.HasPrefix(c, "spanning-tree "), strings.HasPrefix(c, "no spanning-tree "),
		strings.HasPrefix(c, "error-correction encoding "), c == "no error-correction encoding",
		strings.HasPrefix(c, "ip access-group "), strings.HasPrefix(c, "no ip access-group "):
		if !f.ifLine(iface, c) {
			f.unmodelled("interface "+iface, c)
		}
	case strings.HasPrefix(c, "speed "):
		if !f.speed(iface, c) {
			f.unmodelled("interface "+iface, c)
		}
	case strings.HasPrefix(c, "channel-group "):
		if !f.joinChannel(iface, c) {
			f.unmodelled("interface "+iface, c)
		}
	case c == "no channel-group":
		if !f.leaveChannel(iface) {
			f.unmodelled("interface "+iface, c)
		}
	case ipAddressRe.MatchString(c):
		if !f.ipAddress(iface, c) {
			f.unmodelled("interface "+iface, c)
		}
	case strings.HasPrefix(c, "mtu "), strings.HasPrefix(c, "ip address "), strings.HasPrefix(c, "no ip address"):
		f.unmodelled("interface "+iface, c)
	default:
		return false
	}
	return true
}

// ipAddress is an interface's primary address, the address move's write:
// the running config's `ip address A/N` (one primary per interface) and
// `show interfaces` interfaceAddress[0].primaryIp (address, maskLen), as
// the captures show it on Management1 (the poll capture, 2026-09-19) and on
// Vlan1 (empty-po-*, 2026-11-15, after the address was moved there). An
// interface with no address yet takes the entry those captures print.
func (f *FixtureTransport) ipAddress(iface, c string) bool {
	m := ipAddressRe.FindStringSubmatch(c)
	e := f.ifEntry("show interfaces", "interfaces", iface)
	s := f.rcSection("interface "+iface, true)
	if e == nil || s == nil {
		return false
	}
	plen, _ := strconv.Atoi(m[2])
	addrs, _ := e["interfaceAddress"].([]any)
	if len(addrs) == 0 {
		addrs = []any{decodeTemplate(ifAddressJSON)}
		e["interfaceAddress"] = addrs
	}
	child(obj(addrs[0]), "primaryIp", true)["address"] = m[1]
	child(obj(addrs[0]), "primaryIp", true)["maskLen"] = json.Number(strconv.Itoa(plen))
	for k := range s {
		if strings.HasPrefix(k, "ip address ") && !strings.HasSuffix(k, " secondary") {
			delete(s, k)
		}
	}
	s[c] = nil
	return true
}

const ifAddressJSON = `{"broadcastAddress": "255.255.255.255", "dhcp": false, "primaryIp": {"address": "0.0.0.0", "maskLen": 0}, "secondaryIps": {}, "secondaryIpsOrderedList": [], "virtualIp": {"address": "0.0.0.0", "maskLen": 0}, "virtualSecondaryIps": {}, "virtualSecondaryIpsOrderedList": []}`

func (d *fixtureDoc) vOrNil() map[string]any {
	if d == nil {
		return nil
	}
	return d.v
}

// shutdown: a port without a link reads `disabled`, autonegotiation `off`
// (link-counters/show-interfaces-Ethernet2-shutdown.json, the copper test
// port shut, against the poll capture's notconnect/negotiating); no
// shutdown brings back what it read before. A port with a link is not
// modelled.
func (f *FixtureTransport) shutdown(iface string, shut bool) bool {
	e := child(child(f.doc("show interfaces").vOrNil(), "interfaces", false), iface, false)
	if e == nil {
		return false
	}
	if shut {
		switch e["interfaceStatus"] {
		case "disabled":
			return true
		case "notconnect":
		default:
			return false
		}
		if f.was == nil {
			f.was = map[string]ifStatus{}
		}
		f.was[iface] = ifStatus{e["interfaceStatus"], e["autoNegotiate"]}
		e["interfaceStatus"], e["autoNegotiate"] = "disabled", "off"
		return true
	}
	if e["interfaceStatus"] != "disabled" {
		return true
	}
	w, ok := f.was[iface]
	if !ok {
		return false // shut in the capture: what it reads when up is not captured
	}
	e["interfaceStatus"], e["autoNegotiate"] = w.interfaceStatus, w.autoNegotiate
	delete(f.was, iface)
	return true
}

// switchport sets `show interfaces switchport`'s mode and VLANs: mode
// trunk/access, accessVlanId, trunkingNativeVlanId and trunkAllowedVlans
// (ALL, or the list as EOS prints it: the ranges joined by commas,
// write-forms/, captured after each command on the copper test port).
func (f *FixtureTransport) switchport(iface, c string) bool {
	d := f.doc("show interfaces switchport")
	info := child(child(child(d.vOrNil(), "switchports", false), iface, false), "switchportInfo", false)
	if info == nil {
		return false
	}
	switch {
	case c == "switchport mode access":
		info["mode"] = "access"
	case c == "switchport mode trunk":
		info["mode"] = "trunk"
	case strings.HasPrefix(c, "switchport access vlan "):
		n, err := strconv.Atoi(strings.TrimPrefix(c, "switchport access vlan "))
		if err != nil {
			return false
		}
		info["accessVlanId"] = json.Number(strconv.Itoa(n))
		info["accessVlanName"] = f.vlanName(n)
	case c == "no switchport trunk native vlan":
		info["trunkingNativeVlanId"], info["trunkingNativeVlanName"] = json.Number("1"), f.vlanName(1)
	case strings.HasPrefix(c, "switchport trunk native vlan "):
		n, err := strconv.Atoi(strings.TrimPrefix(c, "switchport trunk native vlan "))
		if err != nil {
			return false
		}
		info["trunkingNativeVlanId"], info["trunkingNativeVlanName"] = json.Number(strconv.Itoa(n)), f.vlanName(n)
	case c == "switchport trunk allowed vlan all":
		info["trunkAllowedVlans"] = "ALL"
	case strings.HasPrefix(c, "switchport trunk allowed vlan "):
		ids, all := parseVLANList(strings.TrimPrefix(c, "switchport trunk allowed vlan "))
		if all || len(ids) == 0 {
			return false
		}
		info["trunkAllowedVlans"] = formatVLANList(uniqueSorted(ids))
	default:
		return false
	}
	return true
}

// vlanName is a VLAN's name as `show vlan` has it.
func (f *FixtureTransport) vlanName(id int) any {
	v := child(child(f.doc("show vlan").vOrNil(), "vlans", false), strconv.Itoa(id), false)
	if v == nil {
		return fmt.Sprintf("VLAN%04d", id)
	}
	return v["name"]
}

// storm sets or removes one type in `show storm-control`: a type as the
// storm-control-all-types capture shows a level set on a linkless port
// (level in hundredths, percentage, rate, drop and dormant 0/false); an
// interface with no type left is not listed (the split-cage54 capture,
// after the driver removed both of cage 54's).
func (f *FixtureTransport) storm(iface, c string) bool {
	d := f.doc("show storm-control")
	ifs := child(d.vOrNil(), "interfaces", true)
	if ifs == nil {
		return false
	}
	if typ, ok := strings.CutPrefix(c, "no storm-control "); ok {
		e := child(ifs, iface, false)
		if e == nil {
			return true
		}
		tt := child(e, "trafficTypes", false)
		delete(tt, typ)
		if len(tt) == 0 {
			delete(ifs, iface)
		}
		return true
	}
	m := stormLevelRe.FindStringSubmatch(c)
	if m == nil {
		return false
	}
	pct, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		return false
	}
	e := child(ifs, iface, false)
	if e == nil {
		e = map[string]any{"active": true, "reason": "", "trafficTypes": map[string]any{}}
		ifs[iface] = e
	}
	child(e, "trafficTypes", true)[m[1]] = map[string]any{
		"dormant": false, "drop": json.Number("0"), "level": json.Number(strconv.Itoa(int(pct*100 + 0.5))),
		"rate": json.Number("0"), "thresholdType": "percentage",
	}
	return true
}

// ifLine adds or removes an interface's line in the running config, which
// is where the driver reads STP port settings, FEC and the ACL binding:
//
//   - `spanning-tree bpduguard enable` (cage 54 in the capture) and `no
//     spanning-tree bpduguard`; `spanning-tree portfast`, `spanning-tree
//     bpdufilter enable` and their `no` (write-forms/);
//   - `error-correction encoding reed-solomon` (the capture's 100G cages),
//     `fire-code` and `no error-correction encoding`, one line replacing
//     the other (write-forms/);
//   - `ip access-group NAME in` (show-running-config-dui-acl.json, after
//     session-dui-acl-test-ports.txt) and its `no`.
func (f *FixtureTransport) ifLine(iface, c string) bool {
	s := f.rcSection("interface "+iface, true)
	if s == nil {
		return false
	}
	switch c {
	case "no spanning-tree bpduguard":
		delete(s, "spanning-tree bpduguard enable")
	case "no spanning-tree portfast":
		delete(s, "spanning-tree portfast")
	case "no spanning-tree bpdufilter":
		delete(s, "spanning-tree bpdufilter enable")
	case "spanning-tree bpduguard enable", "spanning-tree portfast", "spanning-tree bpdufilter enable":
		s[c] = nil
	case "error-correction encoding reed-solomon", "error-correction encoding fire-code", "no error-correction encoding":
		for k := range s {
			if strings.HasPrefix(k, "error-correction encoding ") || k == "no error-correction encoding" {
				delete(s, k)
			}
		}
		s[c] = nil
	default:
		if name, ok := strings.CutPrefix(c, "no ip access-group "); ok {
			delete(s, "ip access-group "+name)
			f.aclList(strings.TrimSuffix(name, " in"), false)
			return true
		}
		if strings.HasPrefix(c, "ip access-group ") && strings.HasSuffix(c, " in") {
			s[c] = nil
			f.aclList(strings.TrimSuffix(strings.TrimPrefix(c, "ip access-group "), " in"), false)
			return true
		}
		return false
	}
	return true
}

// speed sets the admin speed `show interfaces transceiver properties`
// reports ("auto"/"auto", or "<N>Gbps"/"full": 1, 10, 25 and 100 captured;
// write-forms/ for 1G) and the running config's `speed` line. A speed on lane 1
// of a cage that splits or joins it is not modelled: its lanes appear or
// go.
func (f *FixtureTransport) speed(iface, c string) bool {
	var speed, duplex string
	mbps := 0
	if c == "speed auto" {
		speed, duplex = "auto", "auto"
	} else {
		m := speedRe.FindStringSubmatch(c)
		if m == nil {
			return false
		}
		for _, n := range []int{1000, 10000, 25000, 40000, 50000, 100000} {
			if speedKeyword(n) == m[1] {
				mbps = n
			}
		}
		if mbps == 0 {
			return false
		}
		speed, duplex = strconv.Itoa(mbps/1000)+"Gbps", "full"
	}
	if slot, lane, ok := parseEthName(iface); ok && lane == 1 {
		lane2 := "Ethernet" + strconv.Itoa(slot) + "/2"
		split := child(child(f.doc("show interfaces").vOrNil(), "interfaces", false), lane2, false) != nil
		if (mbps >= 40000 && split) || (mbps > 0 && mbps < 40000 && !split) {
			return false // a join or a split
		}
	}
	d := f.doc("show interfaces transceiver properties")
	e := child(child(d.vOrNil(), "interfaces", false), iface, false)
	if e == nil {
		return false
	}
	as := child(e, "adminSpeed", true)
	as["speed"], as["duplex"] = speed, duplex
	if s := f.rcSection("interface "+iface, true); s != nil {
		for k := range s {
			if strings.HasPrefix(k, "speed ") {
				delete(s, k)
			}
		}
		s[c] = nil
	}
	return true
}

// ---- ACL mode ----

// aclPortNames is how EOS shows a port it has a name for, by protocol:
// 67 and 68 are bootps and bootpc (session-dui-acl-test-ports.txt, written
// `eq 67 any eq 68`, read back as `eq bootps any eq bootpc` in
// show-running-config-dui-acl.json); 9, 22 and 443 from write-forms/
// (written by number, read back as discard, ssh and https), which also shows
// 1001 kept as a number (5353 too, the capture's mDNS-block). Any other
// port is shown as written and recorded in Unmodelled: EOS may name it.
var aclNumbersKept = map[string]map[string]bool{"udp": {"5353": true}, "tcp": {"1001": true}}

var aclPortNames = map[string]map[string]string{
	"udp": {"9": "discard", "67": "bootps", "68": "bootpc"},
	"tcp": {"9": "discard", "22": "ssh", "443": "https"},
}

// aclCmd applies an ACL-mode command to the list; false when it is not one.
func (f *FixtureTransport) aclCmd(name, c string) bool {
	s := f.rcSection("ip access-list "+name, true)
	if s == nil {
		return false
	}
	switch {
	case c == aclCounters:
		s[c] = nil
		f.aclList(name, false)["countersEnabled"] = true
	case c == "no "+aclCounters:
		delete(s, aclCounters)
		f.aclList(name, false)["countersEnabled"] = false
	case strings.HasPrefix(c, "no "):
		seq := strings.TrimPrefix(c, "no ")
		if _, err := strconv.Atoi(seq); err != nil {
			return false
		}
		for k := range s {
			if strings.HasPrefix(k, seq+" ") {
				delete(s, k)
			}
		}
		n, _ := strconv.Atoi(seq)
		f.aclDropEntry(name, n)
	default:
		seq, rule, ok := strings.Cut(c, " ")
		if _, err := strconv.Atoi(seq); !ok || err != nil {
			return false
		}
		shown, unknown := aclShown(rule)
		if unknown != "" {
			f.unmodelled("ip access-list "+name, c+" (how EOS shows port "+unknown+" is not captured)")
		}
		s[seq+" "+shown] = nil
		n, _ := strconv.Atoi(seq)
		f.aclSetEntry(name, n, shown)
	}
	return true
}

// aclShown is a rule as the running config shows it: a port EOS names is
// shown by its name. unknown is a port no capture shows either way.
func aclShown(rule string) (shown, unknown string) {
	f := strings.Fields(rule)
	if len(f) < 2 || strings.HasPrefix(rule, "remark ") {
		return rule, ""
	}
	names := aclPortNames[f[1]]
	for i := 0; i+1 < len(f); i++ {
		if f[i] != "eq" {
			continue
		}
		if n, ok := names[f[i+1]]; ok {
			f[i+1] = n
		} else if !aclNumbersKept[f[1]][f[i+1]] {
			unknown = f[i+1]
		}
	}
	return strings.Join(f, " "), unknown
}
