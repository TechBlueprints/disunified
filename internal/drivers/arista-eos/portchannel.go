package aristaeos

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Empty Port-Channels (docs/drivers/arista-eos.md §4, "Aggregates").
//
// EOS keeps `interface Port-Channel<n>` when its last member leaves with
// `no channel-group`, with whatever was configured on it (live on 4.26.14M,
// 2026-11-15: the deployed bridge's reconcile took Ethernet2 and
// Ethernet54/1 out of Port-Channel5 and left the interface with `switchport
// mode trunk`). `no interface Port-Channel<n>` removes it, its ACL binding
// with it (the list itself stays), and takes any members still in it out of
// the group; for a Port-Channel that does not exist it is silent. Any
// `interface Port-Channel<n>` line re-creates one, empty (access, VLAN 1):
// an ACL unbind aimed at a removed one would do that, so the removal is
// forgotten in the ACL state too (forgetPortChannels).
//
// So a plan removes a Port-Channel when it is in the plan's own range, has
// no member once the batch has run, and carries nothing but what the
// driver writes on a Port-Channel (its VLANs and the driver's own ACL).
// Anything else on it -- a description, an MTU, an address, another ACL --
// says someone made it by hand, and it stays.

// portChannelSections is every `interface Port-Channel<n>` section of the
// running config, by number, with its lines.
func portChannelSections(rc showRunningConfig) map[int][]string {
	out := map[int][]string{}
	for section, body := range rc.Cmds {
		num, ok := strings.CutPrefix(section, "interface Port-Channel")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil || n <= 0 {
			continue // a subinterface (Port-Channel5.10) is not an aggregate
		}
		var lines []string
		if body != nil {
			for l := range body.Cmds {
				lines = append(lines, l)
			}
		}
		sort.Strings(lines)
		out[n] = lines
	}
	return out
}

// driverChannelLine is a line the driver itself writes on a Port-Channel:
// vlanCommands' switchport lines and the binding of its own ACL.
func driverChannelLine(l string) bool {
	switch {
	case l == "switchport mode trunk", l == "switchport mode access",
		strings.HasPrefix(l, "switchport access vlan "),
		strings.HasPrefix(l, "switchport trunk native vlan "),
		strings.HasPrefix(l, "switchport trunk allowed vlan "):
		return true
	}
	if name, ok := strings.CutPrefix(l, "ip access-group "); ok {
		name, ok = strings.CutSuffix(name, " in")
		return ok && ownACL(name)
	}
	return false
}

// staleChannels is the Port-Channels in lo..hi that the switch has, that no
// port is in once the batch has run (members: the names they will be in),
// and that carry only the driver's own lines; kept is the rest of the
// empty ones in range, made by hand, with the line that says so.
func staleChannels(have map[int][]string, members map[string]bool, lo, hi int) (remove []int, kept map[int]string) {
	for n, lines := range have {
		if n < lo || n > hi || members["Port-Channel"+strconv.Itoa(n)] {
			continue
		}
		hand := ""
		for _, l := range lines {
			if !driverChannelLine(l) {
				hand = l
				break
			}
		}
		if hand != "" {
			if kept == nil {
				kept = map[int]string{}
			}
			kept[n] = hand
			continue
		}
		remove = append(remove, n)
	}
	sort.Ints(remove)
	return remove, kept
}

// forgetPortChannels records that a batch removed names (c.mu held): the
// next diff sees them gone, and the ACL plan does not unbind a list from
// one (which would re-create it) before the next collect.
func (c *Collector) forgetPortChannels(names []string) {
	if len(names) == 0 {
		return
	}
	pcs := make(map[string]devicemodel.PortVLAN, len(c.portChannels))
	for k, v := range c.portChannels {
		pcs[k] = v
	}
	cfg := make(map[int][]string, len(c.channelCfg))
	for k, v := range c.channelCfg {
		cfg[k] = v
	}
	var bound map[string]string
	if c.acl.bound != nil {
		bound = make(map[string]string, len(c.acl.bound))
		for k, v := range c.acl.bound {
			bound[k] = v
		}
	}
	for _, name := range names {
		delete(pcs, name)
		if n, err := strconv.Atoi(strings.TrimPrefix(name, "Port-Channel")); err == nil {
			delete(cfg, n)
		}
		delete(bound, name)
	}
	c.portChannels, c.channelCfg = pcs, cfg
	if c.acl.bound != nil {
		c.acl.bound = bound
	}
}

// A detached lane switch's Port-Channels (docs/drivers/breakout.md §7c).
//
// A cage's lane range, laneBase(cage)+1..+9, is its lane switch's while one
// is attached: the lane plan removes what it empties, and a join's dissolve
// the rest. Once none is attached (its config entry removed and the bridge
// restarted), no plan reached the range, and an empty Port-Channel the lane
// switch left there stayed for good. So the device's own plan takes the
// range of every cage with no lane switch attached, for removal only: an
// empty Port-Channel there that carries only the driver's own lines goes,
// as in its own range. One with a member stays, noted: it still bundles
// lanes, maybe toward an LACP partner, the controller's push for the cage
// says nothing about lane aggregates, and a lane switch attached again
// takes it back as it was. One with a hand-made line stays as anywhere.

// detachedLaneChannels is staleChannels over the lane range of every port
// of ports with no lane switch attached. Membership is the switch's own:
// the plan's members (the batch's result) and every lane's channel group
// (the fold reads one per cage; a split cage's lanes can be in two).
func detachedLaneChannels(ports []devicemodel.Port, lanes map[int][]devicemodel.Port, attached map[int]bool, have map[int][]string, members map[string]bool, note func(string, ...any)) (remove []int, kept map[int]string) {
	in := map[string][]string{} // Port-Channel -> its members' names
	for _, p := range ports {
		if p.LAG != "" && len(lanes[p.Index]) == 0 {
			in[p.LAG] = append(in[p.LAG], laneNames(p)...)
		}
	}
	for _, ls := range lanes {
		for _, l := range ls {
			if l.LAG != "" {
				in[l.LAG] = append(in[l.LAG], laneNames(l)...)
			}
		}
	}
	busy := map[string]bool{}
	for k := range members {
		busy[k] = true
	}
	for k := range in {
		busy[k] = true
	}
	for _, p := range ports {
		base := laneBase(p.Index)
		if attached[p.Index] || p.Index < 1 || base+laneGroupSpan-1 >= laneGroupCeil {
			continue // a lane switch's range, or none: EOS stops at 2000
		}
		r, k := staleChannels(have, busy, base+1, base+laneGroupSpan-1)
		remove = append(remove, r...)
		for n, l := range k {
			if kept == nil {
				kept = map[int]string{}
			}
			kept[n] = l
		}
		for n := base + 1; n < base+laneGroupSpan; n++ {
			name := "Port-Channel" + strconv.Itoa(n)
			if _, ok := have[n]; ok && len(in[name]) > 0 {
				m := slices.Clone(in[name])
				sort.Strings(m)
				note("%s is port %d's lane switch's aggregate and no lane switch is attached to port %d: left as it is while it has members (%s); attach the lane switch again, or take the members out and the plan removes it", name, p.Index, p.Index, strings.Join(m, ", "))
			}
		}
	}
	sort.Ints(remove)
	return remove, kept
}
