package aristaeos

import (
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
