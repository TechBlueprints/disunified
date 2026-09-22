package proxmox

import (
	"fmt"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// DeviceName names the switch after the node ("proxmox-2"; "proxmox-2
// vmbr1" for a bridge other than vmbr0): the node is the switch, same MAC,
// address and hostname. An earlier build used "pve-<node>" while the host
// was modelled as a separate client; that form still counts as a default.
func (c *Collector) DeviceName(sys devicemodel.System) string {
	name := sys.Hostname
	if c.Bridge != "vmbr0" {
		name += " " + c.Bridge
	}
	return name
}

// DefaultDeviceNames: the "pve-<node>" form of an earlier build.
func (c *Collector) DefaultDeviceNames(sys devicemodel.System) []string {
	name := "pve-" + sys.Hostname
	if c.Bridge != "vmbr0" {
		name += " " + c.Bridge
	}
	return []string{name}
}

// PortName names a guest port "VM-<vmid>" ("VM-119 net1" when the guest
// has several NICs), a physical port after its NIC, and an unused slot
// "Open-<port>" (Clint, 2026-09-20: a free slot should read as one, not
// as the profile's "SFP28 26", and nothing is reserved for NICs).
func (c *Collector) PortName(p devicemodel.Port) string {
	if p.Description != "" {
		return p.Description
	}
	if p.IfName != "" {
		return p.IfName
	}
	return openLabel(p.Index)
}

// openLabel is the name of an unused slot.
func openLabel(idx int) string { return fmt.Sprintf("Open-%d", idx) }

// DefaultPortNames lists every name this driver could have given the port,
// so a rename after a guest is renamed or moved still works while an
// operator's own name is kept.
func (c *Collector) DefaultPortNames(p devicemodel.Port) []string {
	// The slot was free before this port took it (two earlier builds split
	// free slots into VM-Open/NIC-Open).
	out := []string{openLabel(p.Index), fmt.Sprintf("VM-Open-%d", p.Index), fmt.Sprintf("NIC-Open-%d", p.Index)}
	if p.IfName != "" {
		out = append(out, p.IfName)
	}
	if p.Description != "" {
		out = append(out, p.Description)
	}
	out = append(out, p.Interfaces...)
	if i := strings.LastIndex(p.IfName, "-"); i > 0 {
		out = append(out, p.IfName[:i]) // "bond0-1" was "bond0" before the slaves were separate ports
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
