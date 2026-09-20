package proxmox

import (
	"fmt"
	"strings"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// DeviceName names the switch after the node: the controller shows
// "proxmox-2", or "proxmox-2 vmbr1" for a bridge other than vmbr0.
func (c *Collector) DeviceName(sys switchmodel.System) string {
	if c.Bridge != "vmbr0" {
		return sys.Hostname + " " + c.Bridge
	}
	return sys.Hostname
}

// PortName names a guest port "<vmid> <name>" ("119 FusionHub net1" when
// the guest has several NICs), a physical port after its NIC, and an empty
// slot "" (leave the controller's default).
func (c *Collector) PortName(p switchmodel.Port) string {
	if p.Description != "" {
		return p.Description
	}
	return p.IfName
}

// DefaultPortNames lists every name this driver could have given the port,
// so a rename after a guest is renamed or moved still works while an
// operator's own name is kept.
func (c *Collector) DefaultPortNames(p switchmodel.Port) []string {
	var out []string
	if p.IfName != "" {
		out = append(out, p.IfName)
	}
	if p.Description != "" {
		out = append(out, p.Description)
	}
	out = append(out, p.Interfaces...)
	if strings.HasPrefix(p.IfName, "vm") || strings.HasPrefix(p.IfName, "ct") {
		// "vm100-net0" -> "100", "vm100"
		id := strings.TrimLeft(strings.SplitN(p.IfName, "-", 2)[0], "vmct")
		out = append(out, id, "vm"+id, "ct"+id)
	}
	c.mu.Lock()
	for _, n := range c.knownNames[p.Index] {
		out = append(out, n)
	}
	c.mu.Unlock()
	return out
}

// guestLabel is the port name for a guest NIC: "<vmid> <name>", plus the
// NIC when the guest has more than one on the bridge.
func guestLabel(n guestNIC, multi bool) string {
	label := fmt.Sprintf("%d %s", n.VMID, n.Name)
	if n.Name == "" {
		label = fmt.Sprintf("%d", n.VMID)
	}
	if multi {
		label += fmt.Sprintf(" net%d", n.Index)
	}
	return label
}
