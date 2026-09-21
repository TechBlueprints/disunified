package aristaeos

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/switchmodel"
)

// ApplyPorts implements switchmodel.Controller: it compares each desired
// port with the last collected snapshot and writes only the differences,
// as one eAPI/SSH batch:
//
//	enable / configure / interface EthernetN / [no] shutdown / [no] description X / ... / end / write memory
//
// eAPI sessions start at privilege 1 whatever the user's level, so `enable`
// leads. A breakout slot writes to every lane. `write memory` persists the
// running config so a switch reboot keeps the controller's intent.
func (c *Collector) ApplyPorts(ctx context.Context, desired []switchmodel.PortDesired) (int, error) {
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	if snap == nil {
		return 0, fmt.Errorf("eos apply: no snapshot yet")
	}
	live := map[int]switchmodel.Port{}
	for _, p := range snap.Ports {
		live[p.Index] = p
	}

	cmds := []string{"enable", "configure"}
	changed := 0
	mirror := mirrorCommands(snap.Ports, desired)
	if len(mirror) > 0 {
		cmds = append(cmds, mirror...)
		changed++
	}
	lagVLAN := map[int]switchmodel.PortDesired{} // LAG id -> the VLAN intent its Port-Channel carries
	for _, d := range desired {
		p, ok := live[d.Index]
		if !ok {
			continue // claimed model has more ports than the switch
		}
		var portCmds []string
		if d.Enabled != p.Enabled {
			if d.Enabled {
				portCmds = append(portCmds, "no shutdown")
			} else {
				portCmds = append(portCmds, "shutdown")
			}
		}
		if d.Description != p.Description {
			if d.Description == "" {
				portCmds = append(portCmds, "no description")
			} else {
				portCmds = append(portCmds, "description "+sanitizeDescription(d.Description))
			}
		}
		if d.LAG > 0 {
			// VLAN config belongs on the Port-Channel; members inherit it.
			if _, seen := lagVLAN[d.LAG]; !seen {
				lagVLAN[d.LAG] = d
			}
		} else {
			portCmds = append(portCmds, vlanCommands(p, d)...)
		}
		portCmds = append(portCmds, lagCommands(p, d)...)
		portCmds = append(portCmds, fecCommands(p, d)...)
		portCmds = append(portCmds, stormCommands(p, d)...)
		portCmds = append(portCmds, bpduGuardCommands(p, d)...)
		portCmds = append(portCmds, stpEdgeCommands(p, d)...)
		// Speed on a QSFP cage (verified on the 7160): `speed forced
		// 25gfull`/`10gfull` on lane 1 splits the cage into 4 lanes and
		// `100gfull`/`40gfull` on lane 1 joins them back — but changing the
		// lane speed of an already split cage must reach every lane, or the
		// others go errdisabled "speed-misconfigured". So a join speed goes
		// to lane 1 only; a lane speed goes to every lane that exists.
		// Everything else goes to every lane, and a freshly split cage
		// picks up its VLAN/FEC/storm config on the next reconcile.
		speedCmds := speedCommands(p, d)
		if len(portCmds) == 0 && len(speedCmds) == 0 {
			continue
		}
		changed++
		lanes := p.Interfaces
		if len(lanes) == 0 {
			lanes = []string{p.IfName}
		}
		speedEveryLane := d.SpeedMbps > 0 && d.SpeedMbps < 40000
		for i, ifname := range lanes {
			laneCmds := portCmds
			if i == 0 || speedEveryLane {
				laneCmds = append(append([]string(nil), speedCmds...), portCmds...)
			}
			if len(laneCmds) == 0 {
				continue
			}
			cmds = append(cmds, "interface "+ifname)
			cmds = append(cmds, laneCmds...)
		}
	}
	for lag, d := range lagVLAN {
		pc := switchmodel.Port{IfName: "Port-Channel" + strconv.Itoa(lag)}
		if pv, ok := c.portChannelVLAN(snap, lag); ok {
			pc.VLAN = pv
		}
		if v := vlanCommands(pc, d); len(v) > 0 {
			cmds = append(cmds, "interface "+pc.IfName)
			cmds = append(cmds, v...)
			changed++
		}
	}
	if changed == 0 {
		return 0, nil
	}
	cmds = append(cmds, "end", "write memory")
	if c.Log != nil {
		c.Log.Printf("eos apply: %s", strings.Join(cmds[2:len(cmds)-2], " | "))
	}
	if err := c.t.Configure(ctx, cmds); err != nil {
		return 0, fmt.Errorf("eos apply (%d ports): %w", changed, err)
	}
	// The snapshot is now stale for the ports just written; the next Collect
	// refreshes it. Mark it so a second ApplyPorts before that does not
	// re-issue the same commands.
	c.mu.Lock()
	if c.last == snap {
		updated := *snap
		updated.Ports = append([]switchmodel.Port(nil), snap.Ports...)
		for i := range updated.Ports {
			for _, d := range desired {
				if updated.Ports[i].Index == d.Index {
					updated.Ports[i].Enabled = d.Enabled
					updated.Ports[i].Description = d.Description
					if d.SpeedMbps > 0 || isCopperMedia(updated.Ports[i].Media) {
						updated.Ports[i].AdminSpeed = d.SpeedMbps
					}
					if d.VLANSet {
						updated.Ports[i].VLAN = desiredVLAN(d)
						updated.Ports[i].LanesDiverge = false
					}
					if d.FEC != nil {
						updated.Ports[i].FECConfig = *d.FEC
					}
					updated.Ports[i].StormCtrl = d.StormCtrl
					updated.Ports[i].BPDUGuard = d.BPDUGuard
					updated.Ports[i].STPEdge = d.STPEdge
					updated.Ports[i].LAGID = d.LAG
					if d.LAG > 0 {
						updated.Ports[i].LAG = "Port-Channel" + strconv.Itoa(d.LAG)
					} else {
						updated.Ports[i].LAG = ""
					}
					updated.Ports[i].MirrorFrom = d.MirrorSource
				}
			}
		}
		c.last = &updated
	}
	c.mu.Unlock()
	return changed, nil
}

// speedCommands: an explicit UniFi speed is forced; "auto" is written only
// on copper ports, because optical ports on this platform run forced
// (Ethernet49/1 is `speed forced 100gfull` by hand) and a blanket `speed
// auto` would drop the 100G links. An optical port reverts to auto only via
// the switch CLI.
func speedCommands(p switchmodel.Port, d switchmodel.PortDesired) []string {
	switch {
	case d.SpeedMbps > 0:
		if p.AdminSpeed == d.SpeedMbps && !p.LanesDiverge {
			return nil
		}
		kw := speedKeyword(d.SpeedMbps)
		if kw == "" {
			return nil
		}
		return []string{"speed forced " + kw}
	case isCopperMedia(p.Media) && p.AdminSpeed != 0:
		return []string{"speed auto"}
	}
	return nil
}

// desiredVLAN is the switch-side state d asks for.
func desiredVLAN(d switchmodel.PortDesired) switchmodel.PortVLAN {
	if !d.TaggedAll && len(d.TaggedVLANs) == 0 {
		return switchmodel.PortVLAN{Mode: "access", NativeVLAN: d.NativeVLAN}
	}
	v := switchmodel.PortVLAN{Mode: "trunk", NativeVLAN: d.NativeVLAN, AllowAll: d.TaggedAll}
	if !d.TaggedAll {
		v.Allowed = append([]int(nil), d.TaggedVLANs...)
		if d.NativeVLAN > 0 {
			v.Allowed = append(v.Allowed, d.NativeVLAN)
		}
		v.Allowed = uniqueSorted(v.Allowed)
	}
	return v
}

// vlanCommands diffs the port's 802.1Q state against d. UniFi's "Allow All"
// is an EOS trunk with every VLAN allowed; a custom tagged set is a trunk
// with an explicit allowed list (native included); "Block All" (native
// only) is an access port.
func vlanCommands(p switchmodel.Port, d switchmodel.PortDesired) []string {
	if !d.VLANSet || d.NativeVLAN == 0 {
		return nil
	}
	want := desiredVLAN(d)
	have := p.VLAN
	if !p.LanesDiverge && have.Mode == want.Mode && have.NativeVLAN == want.NativeVLAN && have.AllowAll == want.AllowAll &&
		(want.AllowAll || equalInts(have.Allowed, want.Allowed)) {
		return nil
	}
	var cmds []string
	if want.Mode == "access" {
		cmds = append(cmds, "switchport mode access", "switchport access vlan "+strconv.Itoa(want.NativeVLAN))
		return cmds
	}
	cmds = append(cmds, "switchport mode trunk")
	if want.NativeVLAN == 1 {
		cmds = append(cmds, "no switchport trunk native vlan")
	} else {
		cmds = append(cmds, "switchport trunk native vlan "+strconv.Itoa(want.NativeVLAN))
	}
	if want.AllowAll {
		cmds = append(cmds, "switchport trunk allowed vlan all")
	} else {
		cmds = append(cmds, "switchport trunk allowed vlan "+formatVLANList(want.Allowed))
	}
	return cmds
}

func uniqueSorted(ids []int) []int {
	sort.Ints(ids)
	out := ids[:0]
	for i, v := range ids {
		if i == 0 || v != ids[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// EnsureVLANs creates any of ids missing on the switch. UniFi's site VLAN
// list is the source of truth for existence; VLANs the switch has and UniFi
// does not are left alone.
func (c *Collector) EnsureVLANs(ctx context.Context, ids []int) (int, error) {
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	if snap == nil {
		return 0, fmt.Errorf("eos ensure vlans: no snapshot yet")
	}
	have := map[int]bool{}
	for _, v := range snap.VLANs {
		have[v] = true
	}
	var missing []int
	for _, id := range ids {
		if id > 0 && id < 4095 && !have[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return 0, nil
	}
	cmds := []string{"enable", "configure", "vlan " + formatVLANList(missing), "end", "write memory"}
	if err := c.t.Configure(ctx, cmds); err != nil {
		return 0, fmt.Errorf("eos create vlans %v: %w", missing, err)
	}
	c.mu.Lock()
	if c.last == snap {
		updated := *snap
		updated.VLANs = uniqueSorted(append(append([]int(nil), snap.VLANs...), missing...))
		c.last = &updated
	}
	c.mu.Unlock()
	return len(missing), nil
}

// sanitizeDescription keeps an EOS description to one printable line. EOS
// accepts up to 240 characters; the CLI treats a newline as the end of the
// command, so strip control characters rather than risk command injection
// through a port name typed in the UniFi UI.
func sanitizeDescription(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 240 {
		out = out[:240]
	}
	return out
}

// portChannelVLAN reads a Port-Channel's 802.1Q state from the snapshot's
// switchport table (kept per collector for the diff).
func (c *Collector) portChannelVLAN(snap *switchmodel.Snapshot, lag int) (switchmodel.PortVLAN, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.portChannels["Port-Channel"+strconv.Itoa(lag)]
	return v, ok
}
