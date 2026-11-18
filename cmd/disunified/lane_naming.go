package main

import (
	"context"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unifiapi"
)

// A lane switch's UniFi name (docs/drivers/breakout.md 16.14; Clint,
// 2026-11-18: "when these auto adopt, they really need a better name than
// Flex XG. Can we immediately rename it to {switch name}-{cage}", then
// "shouldn't that be <parent name>-cage3?"): "<the parent's name in
// UniFi>-cage<N>", given by the provision that every adoption runs (and
// every start of an adopted one), over a controller-default name only. The
// parent's name is read from its controller record in that same provision,
// so it is the name UniFi shows at that moment; once given, the lane
// switch's name is no default, and a later rename of the parent does not
// follow it. The bridge's own name for the lane switch (its config entry,
// state directory and log prefix) is unchanged.

// laneParentOf is a lane switch device that knows its parent (breakout).
type laneParentOf interface {
	ParentMAC() string
	Cage() int
}

// laneSwitchUniFiName is "<parent>-cage<N>", "" without a parent name.
func laneSwitchUniFiName(parent string, cage int) string {
	if parent == "" || cage < 1 {
		return ""
	}
	return parent + "-cage" + itoa(cage)
}

// laneNamer is the lane switch's Namer with the device name the parent's
// UniFi name gives. Without one (the parent's record could not be read) it
// names no device this time; the ports are named as before. The driver's
// own device name ("<parent hostname> <cage label>", what a build before
// 2026-11-18 would have given) counts as a default, so it is replaced too.
type laneNamer struct {
	devicemodel.Namer
	name string
}

func (l laneNamer) DeviceName(devicemodel.System) string { return l.name }

func (l laneNamer) DefaultDeviceNames(sys devicemodel.System) []string {
	return append(append([]string(nil), l.Namer.DefaultDeviceNames(sys)...), l.Namer.DeviceName(sys))
}

// namerForProvision is namer, or for a lane switch the laneNamer for this
// provision: the parent's record read by its MAC.
func namerForProvision(ctx context.Context, sw devicemodel.Device, namer devicemodel.Namer, lookup func(context.Context, string) (*unifiapi.Device, error)) devicemodel.Namer {
	lp, ok := sw.(laneParentOf)
	if !ok {
		return namer
	}
	l := laneNamer{Namer: namer}
	if mac := lp.ParentMAC(); mac != "" {
		if dev, err := lookup(ctx, mac); err == nil {
			l.name = laneSwitchUniFiName(dev.Name, lp.Cage())
		}
	}
	return l
}
