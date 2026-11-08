package aristaeos

// UniFi's Fan Mode on EOS 4.26 (docs/drivers/arista-eos.md §8).
//
// EOS has two fan settings and no others: `environment fan-speed auto`
// (the default, the switch's own thermal control) and `environment
// fan-speed override <30-100>`, a fixed percentage held "regardless of the
// switch temperature conditions" (Arista's manual). There is no minimum or
// ceiling form. Default is auto; Quiet is the lowest override, 30 %, by
// Clint's decision (2026-11-07: "fixed override unless the arista can do
// stepping on top of the 30%" -- EOS documents no stepping on top of an
// override). The switch's overheat shutdown (`environment overheat action`,
// shutdown by default) is left alone.
//
// The mode is read every poll from `show system environment cooling`:
// `overrideFanSpeed` is 0 under auto (captured, 4.26.14M); a non-zero value
// is an override. Which `coolingMode` string an override reports is not
// captured yet, so it is not read.

import "fmt"

// quietFanPct is the override Quiet writes: the bottom of EOS's range.
const quietFanPct = 30

// Fan modes as the driver names them (the controller's Default / Quiet).
const (
	fanModeDefault = "default"
	fanModeQuiet   = "quiet"
)

// fanModeOf maps the cooling state onto a UniFi mode. An override at any
// speed other than Quiet's is not a UniFi mode; it is reported as neither,
// and the next push of either mode replaces it.
func fanModeOf(c showSystemEnvCooling) string {
	switch c.OverrideFanSpeed {
	case 0:
		return fanModeDefault
	case quietFanPct:
		return fanModeQuiet
	}
	return ""
}

// fanModeCommands is the config diff from the switch's override (0 = auto)
// to the wanted mode; nil when the switch is already there.
func fanModeCommands(override int, want string) ([]string, error) {
	switch want {
	case fanModeDefault:
		if override == 0 {
			return nil, nil
		}
		return []string{"no environment fan-speed"}, nil
	case fanModeQuiet:
		if override == quietFanPct {
			return nil, nil
		}
		return []string{fmt.Sprintf("environment fan-speed override %d", quietFanPct)}, nil
	}
	return nil, fmt.Errorf("eos fan mode %q: the switch offers auto and a fixed override only", want)
}
