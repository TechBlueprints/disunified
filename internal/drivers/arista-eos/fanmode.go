package aristaeos

// Fan Mode on EOS 4.26 (docs/drivers/arista-eos.md §8).
//
// EOS has two fan settings and no others: `environment fan-speed auto`
// (the default, the switch's own thermal control) and `environment
// fan-speed override <30-100>`, a fixed percentage held "regardless of the
// switch temperature conditions" (Arista's manual). There is no minimum
// that the thermal control can still raise, and no ceiling. Default is
// auto. Quiet is the lowest override, 30 %, by Clint's decision
// (2026-11-07). It is a fixed speed: under Quiet EOS does not raise the
// fans as the switch heats, and only the alert threshold and the overheat
// shutdown (`environment overheat action`, left alone) act.
//
// The mode is read every poll from `show system environment cooling`:
// `overrideFanSpeed` is 0 under auto (captured, 4.26.14M), and a non-zero
// value is an override. Which `coolingMode` string an override reports is
// not captured yet, so it is not read.

import (
	"context"
	"fmt"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// quietFanPct is the override Quiet writes: the bottom of EOS's range.
const quietFanPct = 30

// fanModes are the modes the driver can set on the 7160.
var fanModes = []devicemodel.FanMode{devicemodel.FanModeDefault, devicemodel.FanModeQuiet}

// fanModeOf maps the cooling state onto a fan mode. An override at any
// speed other than Quiet's is neither mode ("", unknown), so the loop puts
// the configured mode back.
func fanModeOf(c showSystemEnvCooling) devicemodel.FanMode {
	switch c.OverrideFanSpeed {
	case 0:
		return devicemodel.FanModeDefault
	case quietFanPct:
		return devicemodel.FanModeQuiet
	}
	return ""
}

// fanModeCommands is the config diff from the switch's mode to the wanted
// one; nil when the switch is already there.
func fanModeCommands(have, want devicemodel.FanMode) ([]string, error) {
	if have == want {
		switch want {
		case devicemodel.FanModeDefault, devicemodel.FanModeQuiet:
			return nil, nil
		}
	}
	switch want {
	case devicemodel.FanModeDefault:
		return []string{"no environment fan-speed"}, nil
	case devicemodel.FanModeQuiet:
		return []string{fmt.Sprintf("environment fan-speed override %d", quietFanPct)}, nil
	}
	return nil, fmt.Errorf("eos fan mode %q: the switch offers auto (default) and a fixed override (quiet) only", want)
}

// ApplyFanMode implements devicemodel.FanModeController: one config batch
// when the switch is not in the mode (read from the last poll), nothing
// when it is. The next poll reads the mode back.
func (c *Collector) ApplyFanMode(ctx context.Context, mode devicemodel.FanMode) error {
	c.mu.Lock()
	snap := c.last
	c.mu.Unlock()
	if snap == nil {
		return fmt.Errorf("eos fan mode: no snapshot yet")
	}
	cmds, err := fanModeCommands(snap.System.FanMode, mode)
	if err != nil || len(cmds) == 0 {
		return err
	}
	batch := append([]string{"enable", "configure"}, cmds...)
	batch = append(batch, "end", "write memory")
	if err := c.t.Configure(ctx, batch); err != nil {
		return fmt.Errorf("eos fan mode %s: %w", mode, err)
	}
	c.mu.Lock()
	if c.last == snap {
		updated := *snap
		updated.System.FanMode = mode
		c.last = &updated
	}
	c.mu.Unlock()
	return nil
}
