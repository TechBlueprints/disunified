package podman

import (
	"fmt"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// DeviceName names the switch after the host: the host is the switch,
// same MAC, address and hostname.
func (c *Collector) DeviceName(sys devicemodel.System) string { return sys.Hostname }

// DefaultDeviceNames: no earlier convention.
func (c *Collector) DefaultDeviceNames(sys devicemodel.System) []string { return nil }

// PortName names a container port after the container ("lobehub", or
// "lobehub lan" when the container has several endpoints), a NIC after
// itself, and an unused slot "Open-<port>" (the proxmox convention).
func (c *Collector) PortName(p devicemodel.Port) string {
	if p.Description != "" {
		return p.Description
	}
	if p.IfName != "" {
		return p.IfName
	}
	return openLabel(p.Index)
}

func openLabel(idx int) string { return fmt.Sprintf("Open-%d", idx) }

// DefaultPortNames lists every name this driver could have given the
// slot, so a rename after a container is renamed or replaced still works
// while an operator's own name is kept.
func (c *Collector) DefaultPortNames(p devicemodel.Port) []string {
	out := []string{openLabel(p.Index)}
	if p.IfName != "" {
		out = append(out, p.IfName)
	}
	if p.Description != "" {
		out = append(out, p.Description)
	}
	c.mu.Lock()
	out = append(out, c.knownNames[p.Index]...)
	c.mu.Unlock()
	return out
}
