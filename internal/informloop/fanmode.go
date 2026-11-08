package informloop

import (
	"context"
	"fmt"
	"slices"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// applyFanMode brings the device's fan mode to the operator's (Config.FanMode)
// from a fresh collect. The snapshot's System.FanMode is what the device runs
// now, read back by the driver, so this is a diff on every cycle: a device
// already in the mode is not written, and one changed by hand is put back on
// the next collect. A mode the driver cannot set is logged once, not tried.
func (l *Loop) applyFanMode(ctx context.Context, snap *devicemodel.Snapshot) {
	want := l.cfg.FanMode
	if want == "" || snap == nil {
		return
	}
	fc, ok := deviceAs[devicemodel.FanModeController](l)
	switch {
	case !ok:
		l.noteFan(fmt.Sprintf("fan_mode %s: this driver cannot set the fan mode; the fans are left as they are", want))
		return
	case !slices.Contains(snap.System.FanModes, want):
		l.noteFan(fmt.Sprintf("fan_mode %s: not settable on this device (it offers %v); the fans are left as they are", want, snap.System.FanModes))
		return
	case snap.System.FanMode == want:
		l.noteFan("")
		return
	}
	actx, cancel := context.WithTimeout(ctx, l.cfg.ApplyTimeout)
	defer cancel()
	if err := fc.ApplyFanMode(actx, want); err != nil {
		l.noteFan(fmt.Sprintf("fan_mode %s: %v", want, err))
		return
	}
	l.fanNoted = ""
	l.cfg.Logger.Printf("[%s] fan mode %s -> %s", l.desc.MAC, orUnknown(snap.System.FanMode), want)
}

// noteFan logs a fan-mode refusal or failure when it differs from the last one.
func (l *Loop) noteFan(msg string) {
	if msg == l.fanNoted {
		return
	}
	l.fanNoted = msg
	if msg != "" {
		l.cfg.Logger.Printf("[%s] %s", l.desc.MAC, msg)
	}
}

func orUnknown(m devicemodel.FanMode) string {
	if m == "" {
		return "unknown"
	}
	return string(m)
}
