package switchmodel

import (
	"fmt"
	"strings"
)

// Namer decides what a switch and its ports are called in the controller
// when nobody has named them. The bridge provisions these names through the
// controller API on first adoption and again whenever the port layout
// changes (a cage splits or joins), but only over names it recognises as
// defaults — an operator's own name is never replaced.
//
// DefaultNamer covers the common case; a driver may implement Namer to
// match its platform's conventions.
type Namer interface {
	// DeviceName is the device's display name, e.g. "Arista DCS-7160-48TC6-F".
	DeviceName(sys System) string
	// PortName is the port's name: its interface name, or a lane range for a
	// split cage ("Ethernet54/1-4"), or its description when one is set.
	PortName(p Port) string
	// DefaultPortNames lists every name the bridge treats as "unnamed" for
	// the port: all forms PortName could have produced for it in any lane
	// layout, plus each lane's own name. The controller's and profile's
	// generic names ("Port 5", "SFP28 5") are added by the caller.
	DefaultPortNames(p Port) []string
}

// DefaultNamer names by "<Vendor> <Model>" and "<ifname>" / "<base>/1-<lanes>".
type DefaultNamer struct{}

func (DefaultNamer) DeviceName(sys System) string {
	return strings.TrimSpace(sys.Vendor + " " + sys.Model)
}

func (DefaultNamer) PortName(p Port) string {
	if p.Description != "" {
		return p.Description
	}
	if p.Lanes > 1 {
		return fmt.Sprintf("%s/1-%d", p.IfName, p.Lanes)
	}
	return p.IfName
}

func (DefaultNamer) DefaultPortNames(p Port) []string {
	base := strings.SplitN(p.IfName, "/", 2)[0]
	out := []string{p.IfName, base, base + "/1", base + "/1-2", base + "/1-4", base + "/1-8"}
	if p.Lanes > 1 {
		out = append(out, fmt.Sprintf("%s/1-%d", p.IfName, p.Lanes))
	}
	out = append(out, p.Interfaces...)
	return out
}

// NamerFor returns the driver's Namer when the open Switch implements one,
// else DefaultNamer.
func NamerFor(sw Switch) Namer {
	if n, ok := sw.(Namer); ok {
		return n
	}
	return DefaultNamer{}
}
