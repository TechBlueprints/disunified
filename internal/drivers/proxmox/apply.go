package proxmox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/switchmodel"
)

// ApplyPorts implements switchmodel.Controller for guest ports. The only
// per-port settings a bridge port has are the guest NIC's `link_down`,
// `tag` and `trunks` options, written with `qm set` / `pct set` so Proxmox
// persists them and hot-applies them to a running guest (network hotplug
// re-plugs the tap with the new VLAN membership). A guest that lives on
// another node is left to that node's bridge; a port with nothing assigned
// has nothing to write. Everything else the controller sends for a port
// (speed, FEC, storm control, STP, LAG, mirror) has no bridge equivalent
// and is ignored, and the capability claims keep those controls hidden.
//
// Writes are diffs against the config as last read: a converged node costs
// no command, and the loop's reconcile after every inform is free.
func (c *Collector) ApplyPorts(ctx context.Context, desired []switchmodel.PortDesired) (int, error) {
	c.mu.Lock()
	nics, node, keyOf := c.nics, c.node, c.keyOf
	c.mu.Unlock()
	if nics == nil {
		return 0, fmt.Errorf("proxmox apply: no snapshot yet")
	}
	changed := 0
	for _, d := range desired {
		n, want, ok := c.planPort(d)
		if !ok {
			continue
		}
		key := n.Key()
		raw := renderNICOptions(n.Raw, want)
		if err := c.setNIC(ctx, n, raw); err != nil {
			return changed, err
		}
		want.Raw = raw
		c.mu.Lock()
		c.nics[key] = want
		c.mu.Unlock()
		changed++
	}
	// Link aggregation on the node's physical ports (lag.go).
	n, err := c.applyLAG(ctx, desired)
	changed += n
	if err != nil {
		return changed, err
	}
	// BPDU guard on guest ports, when the bridge runs under mstpd.
	c.mu.Lock()
	stpManaged, stpPorts := c.stpManaged, c.stpPorts
	c.mu.Unlock()
	if stpManaged {
		var cmds []string
		for _, d := range desired {
			key, ok := keyOf[d.Index]
			if !ok {
				continue
			}
			n, ok := nics[key]
			if !ok || n.Node != node {
				continue
			}
			member := n.BridgeMember()
			sp, ok := stpPorts[member]
			if !ok {
				continue // the guest is not running here
			}
			if (sp.BPDUGuardPort == "yes") != d.BPDUGuard {
				v := "no"
				if d.BPDUGuard {
					v = "yes"
				}
				cmds = append(cmds, fmt.Sprintf("setportbpduguard %s %s %s", c.Bridge, member, v))
				sp.BPDUGuardPort = v
				stpPorts[member] = sp
			}
		}
		if len(cmds) > 0 {
			c.Log.Printf("proxmox %s: mstpd: %s", node, strings.Join(cmds, "; "))
			if _, err := c.r.Run(ctx, "mstpctl -s", strings.Join(cmds, "\n")+"\n"); err != nil {
				return changed, err
			}
			changed += len(cmds)
		}
	}
	return changed, nil
}

// planPort is the diff for one port: the guest NIC on this node the port
// maps to and what it should become; ok is false when nothing changes (or
// the port has no guest here).
func (c *Collector) planPort(d switchmodel.PortDesired) (n, want guestNIC, ok bool) {
	c.mu.Lock()
	node, nics, keyOf := c.node, c.nics, c.keyOf
	c.mu.Unlock()
	key, found := keyOf[d.Index]
	if !found {
		return n, want, false
	}
	n, found = nics[key]
	if !found || n.Node != node {
		return n, want, false
	}
	want = n
	want.LinkDown = !d.Enabled
	if d.VLANSet {
		want.Tag, want.Trunks = vlanToConfig(d)
	}
	if want.LinkDown == n.LinkDown && want.Tag == n.Tag && equalInts(want.Trunks, n.Trunks) {
		return n, want, false
	}
	return n, want, true
}

// PlanPorts implements switchmodel.Planner: the ports ApplyPorts would write.
func (c *Collector) PlanPorts(desired []switchmodel.PortDesired) []int {
	var out []int
	for _, d := range desired {
		if _, _, ok := c.planPort(d); ok {
			out = append(out, d.Index)
		}
	}
	return out
}

// vlanToConfig turns the controller's intent into Proxmox tag/trunks.
//
//	native 1, all tagged   -> no tag, no trunks (the bridge's default: every VLAN)
//	native N, no tagged    -> tag=N (access port; tag=1 for native 1)
//	native N, tagged list  -> tag=N (absent for 1), trunks=list
//	native N, all tagged   -> tag=N, trunks=2-4094 (every VLAN, N untagged)
func vlanToConfig(d switchmodel.PortDesired) (tag int, trunks []int) {
	native := d.NativeVLAN
	if native <= 0 {
		native = 1
	}
	if native != 1 {
		tag = native
	}
	switch {
	case d.TaggedAll && native == 1:
		return 0, nil
	case d.TaggedAll:
		return tag, parseVLANRanges("2-4094")
	}
	for _, v := range d.TaggedVLANs {
		if v != native && v > 0 {
			trunks = append(trunks, v)
		}
	}
	trunks = uniqueSorted(trunks)
	if native == 1 && len(trunks) == 0 {
		// "Block all" on native 1: an access port on VLAN 1 (verified: tag=1
		// leaves the tap with pvid 1 untagged and nothing else).
		tag = 1
	}
	return tag, trunks
}

// renderNICOptions rewrites the option string keeping every option the
// bridge does not manage (model=MAC, bridge, firewall, queues, mtu, rate...)
// in its original order, and sets link_down / tag / trunks from want.
func renderNICOptions(raw string, want guestNIC) string {
	var parts []string
	for _, opt := range strings.Split(raw, ",") {
		k, _, _ := strings.Cut(strings.TrimSpace(opt), "=")
		switch k {
		case "link_down", "tag", "trunks", "":
			continue
		}
		parts = append(parts, strings.TrimSpace(opt))
	}
	if want.Tag > 0 {
		parts = append(parts, fmt.Sprintf("tag=%d", want.Tag))
	}
	if len(want.Trunks) > 0 {
		parts = append(parts, "trunks="+formatVLANRanges(want.Trunks))
	}
	if want.LinkDown {
		parts = append(parts, "link_down=1")
	}
	return strings.Join(parts, ",")
}

var safeOptions = regexp.MustCompile(`^[A-Za-z0-9=:,;._-]+$`)

// setNIC writes a guest NIC's option string with the node's own tool.
func (c *Collector) setNIC(ctx context.Context, n guestNIC, raw string) error {
	if !safeOptions.MatchString(raw) {
		return fmt.Errorf("proxmox: refusing to write unsafe option string %q", raw)
	}
	tool := "qm"
	if n.Kind == "lxc" {
		tool = "pct"
	}
	cmd := fmt.Sprintf("%s set %d --net%d %s", tool, n.VMID, n.Index, shellQuote(raw))
	c.Log.Printf("proxmox %s: %s", c.node, cmd)
	out, err := c.r.Run(ctx, cmd, "")
	if err != nil {
		return fmt.Errorf("proxmox: %w", err)
	}
	if s := strings.TrimSpace(out); s != "" {
		c.Log.Printf("proxmox %s: %s", c.node, truncate(s, 200))
	}
	return nil
}

// CyclePort bounces a guest port: link_down=1, wait, back to what it was.
func (c *Collector) CyclePort(ctx context.Context, idx int) error {
	c.mu.Lock()
	node := c.node
	key, ok := c.keyOf[idx]
	n := c.nics[key]
	c.mu.Unlock()
	if !ok || n.Node != node {
		return fmt.Errorf("proxmox: port %d has no guest on this node", idx)
	}
	down := n
	down.LinkDown = true
	if err := c.setNIC(ctx, n, renderNICOptions(n.Raw, down)); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-time.After(c.cycleDelay):
	}
	return c.setNIC(ctx, n, renderNICOptions(n.Raw, n))
}

// ApplySwitch honours IGMP snooping (bridge-wide: on when any managed VLAN
// wants it) and, when the operator lets UniFi own NTP, chrony's server
// list. STP and syslog requests are logged once and left alone: a Proxmox
// bridge runs with bridge-stp off by design, and journald has no remote
// target to set.
func (c *Collector) ApplySwitch(ctx context.Context, d switchmodel.SwitchDesired) (int, error) {
	c.mu.Lock()
	snoop, ntpManaged := c.snooping, c.ntpManaged
	stpManaged, stpVersion := c.stpManaged, c.stpVersion
	c.mu.Unlock()
	changed := 0
	switch {
	case d.STPSet && !stpManaged && d.STPEnabled:
		c.warnOnce("stp", "the controller wants STP %s (priority %d); this node's bridge is not under mstpd (docs/drivers/proxmox.md §4b) so STP is neither claimed nor changed", d.STPMode, d.STPPriority)
	case d.STPSet && stpManaged:
		// Version follows the controller; priority never does (enforceSTP
		// pins the maximum so the node cannot become root).
		if d.STPEnabled && d.STPMode != "" && d.STPMode != stpVersion {
			cmd := fmt.Sprintf("mstpctl setforcevers %s %s", c.Bridge, d.STPMode)
			c.Log.Printf("proxmox %s: %s", c.node, cmd)
			if _, err := c.r.Run(ctx, cmd, ""); err != nil {
				return changed, err
			}
			c.mu.Lock()
			c.stpVersion = d.STPMode
			c.mu.Unlock()
			changed++
		}
		if !d.STPEnabled {
			c.warnOnce("stp-off", "the controller wants STP off; a node under mstpd keeps RSTP running (it is what protects the node's own uplinks) and reports it")
		}
		if d.STPPriority != 0 && d.STPPriority != nodeSTPPriority {
			c.warnOnce("stp-prio", "the controller wants STP priority %d; a node keeps the maximum (%d) so it is never elected root", d.STPPriority, nodeSTPPriority)
		}
	}
	if d.IGMPSnooping != nil {
		want, ok := d.IGMPSnooping[1]
		if !ok {
			for _, v := range d.IGMPSnooping {
				want = want || v
			}
		}
		if want != snoop {
			v := "0"
			if want {
				v = "1"
			}
			cmd := fmt.Sprintf("echo %s > /sys/class/net/%s/bridge/multicast_snooping", v, c.Bridge)
			c.Log.Printf("proxmox %s: %s", c.node, cmd)
			if _, err := c.r.Run(ctx, cmd, ""); err != nil {
				return changed, err
			}
			c.mu.Lock()
			c.snooping = want
			c.mu.Unlock()
			changed++
		}
	}
	if d.ManageNTP && d.NTPServers != nil {
		var b strings.Builder
		b.WriteString("# managed by disunified: the UniFi controller's NTP servers\n")
		for _, s := range d.NTPServers {
			if safeOptions.MatchString(s) {
				fmt.Fprintf(&b, "server %s iburst\n", s)
			}
		}
		want := strings.TrimSpace(b.String())
		if len(d.NTPServers) == 0 {
			want = ""
		}
		if want != ntpManaged {
			cmd := "install -m 644 /dev/stdin /etc/chrony/sources.d/disunified.sources && chronyc reload sources"
			if want == "" {
				cmd = "rm -f /etc/chrony/sources.d/disunified.sources && chronyc reload sources"
			}
			c.Log.Printf("proxmox %s: NTP servers -> %v", c.node, d.NTPServers)
			if _, err := c.r.Run(ctx, cmd, want+"\n"); err != nil {
				return changed, err
			}
			c.mu.Lock()
			c.ntpManaged = want
			c.mu.Unlock()
			changed++
		}
	}
	if d.ManageSyslog && len(d.SyslogHosts) > 0 {
		c.warnOnce("syslog", "the controller's remote syslog host %v is not applied: Proxmox logs through journald", d.SyslogHosts)
	}
	return changed, nil
}

// EnsureVLANs checks the site's VLANs against the bridge's bridge-vids; a
// VLAN-aware bridge with the default 2-4094 carries them all. Nothing is
// created: the bridge's VLAN range is the node's network config.
func (c *Collector) EnsureVLANs(ctx context.Context, ids []int) (int, error) {
	c.mu.Lock()
	vids := c.bridgeVIDs
	c.mu.Unlock()
	if len(vids) == 0 {
		return 0, nil // not VLAN-aware or unknown: nothing to check
	}
	have := map[int]bool{1: true}
	for _, v := range vids {
		have[v] = true
	}
	var missing []int
	for _, id := range ids {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		c.warnOnce("vids", "site VLANs %v are outside %s's bridge-vids; add them to the bridge in /etc/network/interfaces", missing, c.Bridge)
	}
	return 0, nil
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
