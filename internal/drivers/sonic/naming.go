package sonic

import (
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// DeviceName names the switch after the OS and the hardware SKU
// ("SONiC Seastone-DX010"): the SKU names the box and its port layout.
func (c *Collector) DeviceName(sys devicemodel.System) string {
	return strings.TrimSpace("SONiC " + sys.Model)
}

// DefaultDeviceNames: the generic "<Vendor> <Model>" form, so a device that
// was named before this driver had a Namer is still recognised as unnamed.
func (c *Collector) DefaultDeviceNames(sys devicemodel.System) []string {
	return []string{devicemodel.DefaultNamer{}.DeviceName(sys)}
}

// PortName is the port's description when set, else SONiC's alias for the
// cage (Eth1..Eth32, the faceplate labels), else the interface name.
func (c *Collector) PortName(p devicemodel.Port) string {
	if p.Description != "" {
		return p.Description
	}
	c.mu.Lock()
	alias := c.aliases[p.Index]
	c.mu.Unlock()
	if alias != "" {
		return alias
	}
	return p.IfName
}

// DefaultPortNames: the alias and every interface name the cage has carried
// (a split adds the child ports).
func (c *Collector) DefaultPortNames(p devicemodel.Port) []string {
	c.mu.Lock()
	alias := c.aliases[p.Index]
	ifs := c.ifnames[p.Index]
	c.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(alias)
	add(p.IfName)
	for _, n := range p.Interfaces {
		add(n)
	}
	for _, n := range ifs {
		add(n)
	}
	return out
}
