package aristaeos

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// A VLAN the controller stops pushing (docs/drivers/arista-eos.md §12; the
// rule is sonic's, docs/drivers/sonic.md §11.29).
//
// The controller's push names a VLAN only while something on the device
// uses it: a port carrying it, which on this switch is every port, all of
// them Allow All, so every site network. A network deleted from the site,
// the Inter-VLAN routing transit going (VLAN 4040: in the Arista's own
// push from 17:43:34 to 17:46:34 UTC on 2026-11-15, created by EnsureVLANs
// and left behind), a tagged set that drops it: in each the VLAN leaves the
// push. UniFi's list is the switch's, as for NTP and syslog, so once the
// port batch has taken every member the driver writes out of it, a VLAN no
// push names is deleted (`no vlan <list>`, then `write memory`).
//
// EOS deletes a VLAN whatever still names it (captured 2026-11-15,
// vlan-prune/): an SVI stays with no VLAN under it (lowerlayerdown), a
// port keeps naming it, `no ip igmp snooping vlan N` and `no
// spanning-tree vlan-id N` stay in the running config. So the driver checks
// first and keeps, whatever the push says: VLAN 1, the VLAN of the
// management interface, a dynamic VLAN (EOS's own), every VLAN the device's
// push or an attached lane switch's push names, and a VLAN that still holds
// what the driver did not take out of it: an `interface VlanN` (an SVI, an
// address), a line in its `vlan` section other than its name (a trunk
// group, a state), an interface that names it explicitly (an access or
// native VLAN, or an allowed list, on a port outside control.ports, a lane
// a lane switch owns, a Port-Channel made by hand). An Allow All trunk
// names no VLAN: it carries what the switch has. The driver's own IGMP line
// for the VLAN (UniFi's per-VLAN snooping) goes with it; a hand-made `no
// spanning-tree vlan-id` stays as written, inert until the VLAN comes back.
//
// Nothing is deleted from a list this run has not been given: the device's
// push (EnsureVLANs) and the push of every attached lane switch
// (EnsureLaneVLANs). A held first push never reaches EnsureVLANs.

// vlanConfig is what the running config says about the switch's VLANs,
// beside `show vlan`'s ids: kept from each collect for the prune.
type vlanConfig struct {
	extra   map[int][]string // a `vlan` section's lines other than `name`
	svis    map[int]string   // `interface VlanN` -> its first line ("" = bare)
	igmpOff map[int]bool     // `no ip igmp snooping vlan N`
	dynamic map[int]bool     // `show vlan`'s dynamic VLANs
}

// vlanConfigFrom reads vlanConfig. EOS keeps a named VLAN's section on its
// own (`vlan 2` / `name kids`) and folds the unnamed ones into one list
// (`vlan 69,3000,3977-3978,4000,4010,4040`, vlan-prune/1-created).
func vlanConfigFrom(rc showRunningConfig, vl showVLAN) vlanConfig {
	vc := vlanConfig{extra: map[int][]string{}, svis: map[int]string{}, igmpOff: map[int]bool{}, dynamic: map[int]bool{}}
	for k, sec := range rc.Cmds {
		switch {
		case strings.HasPrefix(k, "vlan "):
			ids, all := parseVLANList(strings.TrimPrefix(k, "vlan "))
			if all || sec == nil {
				continue
			}
			var lines []string
			for l := range sec.Cmds {
				if !strings.HasPrefix(l, "name ") {
					lines = append(lines, l)
				}
			}
			sort.Strings(lines)
			if len(lines) == 0 {
				continue
			}
			for _, id := range ids {
				vc.extra[id] = append(vc.extra[id], lines...)
			}
		case strings.HasPrefix(k, "interface Vlan"):
			id, err := strconv.Atoi(strings.TrimPrefix(k, "interface Vlan"))
			if err != nil {
				continue
			}
			first := ""
			if sec != nil {
				lines := slices.Sorted(maps.Keys(sec.Cmds))
				if len(lines) > 0 {
					first = lines[0]
				}
			}
			vc.svis[id] = first
		default:
			if m := igmpVLANRe.FindStringSubmatch(k); m != nil && m[1] != "" {
				if id, err := strconv.Atoi(m[2]); err == nil {
					vc.igmpOff[id] = true
				}
			}
		}
	}
	for k, v := range vl.VLANs {
		if id, err := strconv.Atoi(k); err == nil && v.Dynamic {
			vc.dynamic[id] = true
		}
	}
	return vc
}

// EnsureLaneVLANs implements devicemodel.LaneVLANController: a lane
// switch's own list, kept apart from the device's so that the prune keeps
// what the lanes carry; the VLANs are created as EnsureVLANs does.
func (c *Collector) EnsureLaneVLANs(ctx context.Context, cage int, ids []int) (int, error) {
	return c.ensureVLANs(ctx, cage, ids)
}

var _ devicemodel.LaneVLANController = (*Collector)(nil)

// pruneVLANs deletes the VLANs no push names any more, as one batch, after
// the port batch (ApplyPorts; lockApply held).
func (c *Collector) pruneVLANs(ctx context.Context) (int, error) {
	del, igmp, notes := c.planPrune()
	c.logNotes(notes)
	if len(del) == 0 {
		return 0, nil
	}
	var body []string
	for _, id := range igmp {
		body = append(body, "ip igmp snooping vlan "+strconv.Itoa(id))
	}
	body = append(body, "no vlan "+formatVLANList(del))
	c.logf("eos: deleting VLANs %v, which the controller no longer pushes: %s", del, strings.Join(body, " | "))
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	cmds := append(append([]string{"enable", "configure"}, body...), "end", "write memory")
	if err := c.t.Configure(ctx, cmds); err != nil {
		return 0, fmt.Errorf("eos delete vlans %v: %w", del, err)
	}
	gone := map[int]bool{}
	for _, id := range del {
		gone[id] = true
	}
	c.mu.Lock()
	if c.last == snap && snap != nil {
		updated := *snap
		updated.VLANs = slices.DeleteFunc(slices.Clone(snap.VLANs), func(id int) bool { return gone[id] })
		updated.System.IGMPSnooping = maps.Clone(snap.System.IGMPSnooping)
		for id := range gone {
			delete(updated.System.IGMPSnooping, id)
		}
		c.last = &updated
	}
	vc := c.vlanCfg
	vc.igmpOff = maps.Clone(vc.igmpOff)
	for _, id := range igmp {
		delete(vc.igmpOff, id)
	}
	c.vlanCfg = vc
	c.mu.Unlock()
	return len(del), nil
}

// planPrune is the VLANs pruneVLANs would delete (ascending), the IGMP
// lines that go with them, and why each other candidate stays.
func (c *Collector) planPrune() (del, igmp []int, notes []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.last
	if snap == nil || !c.siteKnown {
		return nil, nil, nil
	}
	keep := map[int]bool{1: true}
	for _, id := range c.siteVLANs {
		keep[id] = true
	}
	for _, cage := range slices.Sorted(maps.Keys(c.children)) {
		l, ok := c.laneVLANs[cage]
		if !ok {
			return nil, nil, []string{fmt.Sprintf("the lane switch on cage %d has not applied a push yet; no VLAN is deleted until it has (its lanes may carry one the device's push does not name)", cage)}
		}
		for _, id := range l {
			keep[id] = true
		}
	}
	if iface, _ := managementInterface(snap, c.host); strings.HasPrefix(iface, "Vlan") {
		if id, err := strconv.Atoi(strings.TrimPrefix(iface, "Vlan")); err == nil {
			keep[id] = true
		}
	}
	named := c.namedVLANs(snap)
	for _, id := range snap.VLANs {
		if keep[id] || c.vlanCfg.dynamic[id] || id < 1 || id > 4094 {
			continue
		}
		if why := c.vlanHeld(id, named); why != "" {
			notes = append(notes, fmt.Sprintf("VLAN %d is no longer in the controller's push, but %s; left on the switch", id, why))
			continue
		}
		del = append(del, id)
		if c.vlanCfg.igmpOff[id] {
			igmp = append(igmp, id)
		}
	}
	return del, igmp, notes
}

// vlanHeld says what keeps a VLAN on the switch whatever the push says
// (c.mu held). "" = nothing.
func (c *Collector) vlanHeld(id int, named map[int][]string) string {
	if ifs := named[id]; len(ifs) > 0 {
		return fmt.Sprintf("%s still names it (an interface the bridge does not write)", strings.Join(ifs, ","))
	}
	if first, ok := c.vlanCfg.svis[id]; ok {
		if first != "" {
			return fmt.Sprintf("interface Vlan%d exists (%s)", id, first)
		}
		return fmt.Sprintf("interface Vlan%d exists", id)
	}
	if lines := c.vlanCfg.extra[id]; len(lines) > 0 {
		return fmt.Sprintf("its vlan section carries %q, which this driver never writes", strings.Join(lines, "; "))
	}
	return ""
}

// namedVLANs maps each VLAN to the interfaces that name it explicitly, as
// the switch is once the port batch has run (c.mu held): the ports, the
// split cages' lanes and the Port-Channels. A port or Port-Channel the
// batch wrote names only VLANs the push lists, so what remains is the
// unwritten ones. Allow All names nothing.
func (c *Collector) namedVLANs(snap *devicemodel.Snapshot) map[int][]string {
	out := map[int][]string{}
	add := func(name string, v devicemodel.PortVLAN) {
		ids := []int{}
		if v.NativeVLAN > 0 {
			ids = append(ids, v.NativeVLAN)
		}
		if v.Mode != "access" && !v.AllowAll {
			ids = append(ids, v.Allowed...)
		}
		for _, id := range uniqueSorted(ids) {
			out[id] = append(out[id], name)
		}
	}
	for _, p := range snap.Ports {
		if p.LAG != "" {
			continue // a member: the Port-Channel's VLANs are the ones it carries
		}
		if l := c.lanes[p.Index]; p.Lanes > 1 && len(l) > 0 {
			for _, lane := range l {
				if lane.LAG == "" {
					add(lane.IfName, lane.VLAN)
				}
			}
			continue
		}
		add(p.IfName, p.VLAN)
	}
	for _, name := range slices.Sorted(maps.Keys(c.portChannels)) {
		add(name, c.portChannels[name])
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out
}
