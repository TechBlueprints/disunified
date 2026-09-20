package proxmox

import (
	"fmt"
	"strings"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// DeviceName names the switch "pve-<node>" ("pve-proxmox-2 vmbr1" for a
// bridge other than vmbr0): distinct from the node's own DNS name, which
// the controller stops publishing once the node's MAC is an adopted device
// (docs/proxmox.md §6), so a static record for the host keeps its plain name.
func (c *Collector) DeviceName(sys switchmodel.System) string {
	name := "pve-" + sys.Hostname
	if c.Bridge != "vmbr0" {
		name += " " + c.Bridge
	}
	return name
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
	if p.IfName == "host" {
		out = append(out, "host")
	}
	if strings.HasPrefix(p.IfName, "vm") || strings.HasPrefix(p.IfName, "ct") {
		// "vm100-net0" -> "100", "vm100"
		id := strings.TrimLeft(strings.SplitN(p.IfName, "-", 2)[0], "vmct")
		out = append(out, id, "vm"+id, "ct"+id)
	}
	c.mu.Lock()
	out = append(out, c.knownNames[p.Index]...)
	if key, ok := c.keyOf[p.Index]; ok {
		if n, ok := c.nics[key]; ok {
			out = append(out, legacyLabels(n, true)...)
			out = append(out, legacyLabels(n, false)...)
		}
	}
	c.mu.Unlock()
	return out
}

// guestLabel is the port name for a guest NIC: "VM-100" ("CT-200" for a
// container), plus the NIC when the guest has more than one on the bridge
// ("VM-119 net1"). The guest's name is not part of it: the controller shows
// the guest itself as the client behind the port, and a VMID is what a
// Proxmox user reaches for (Clint, 2026-09-20).
func guestLabel(n guestNIC, multi bool) string {
	prefix := "VM"
	if n.Kind == "lxc" {
		prefix = "CT"
	}
	label := fmt.Sprintf("%s-%d", prefix, n.VMID)
	if multi {
		label += fmt.Sprintf(" net%d", n.Index)
	}
	return label
}

// legacyLabels are the forms an earlier build named a guest port with
// ("100 proxy", "119 FusionHub net1"), so those still count as defaults.
func legacyLabels(n guestNIC, multi bool) []string {
	base := fmt.Sprintf("%d %s", n.VMID, n.Name)
	out := []string{base, fmt.Sprintf("%d", n.VMID)}
	if multi {
		out = append(out, fmt.Sprintf("%s net%d", base, n.Index))
	}
	return out
}
