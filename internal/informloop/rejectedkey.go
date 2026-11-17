package informloop

import (
	"net/http"
	"time"
)

// A Forget the bridge missed (docs/drivers/breakout.md 16.11).
//
// The controller sends a forgotten device's `setdefault` once, to its first
// inform after the Forget; every later inform on the old key is answered
// with HTTP 400 and an empty body (Network 11.0.99, 2026-11-16:
// controller-11.0.99/replies-lane-switch-dx010-cage3.ndjson). A bridge that
// was down at that moment comes back adopted and is refused from then on.
//
// What else a 400 has been, from every reply log there is (nine deployed
// bridges, 2026-09-20 to 2026-11-16, about 500 000 informs on adopted keys,
// across controller restarts, the 10.6.109 and 11.0.99 upgrades and the
// four failed 11.0.97 migrations with their rollbacks): never an answer to
// an adopted key that was not forgotten. A controller that is down or
// starting refuses the connection, resets it or answers 503, and the
// adopted key is answered 200 again as soon as it is up. The only other
// 400s are on the default key, for a minute or two after a Forget, and a
// 400 with a body (`invalid inform_ip`), which is about the inform, not the
// key.
//
// So a run of empty-body 400s on an adopted key, rejectedKeyInforms of them
// over at least rejectedKeyAfter, is read as that Forget. The count is the
// recorded run (three informs over 2 min 28 s, until the state file was
// removed by hand); the time keeps a restarted bridge's 10 s start-up
// cadence from reaching the count in 20 s. Only a device whose loop is told
// to (Config.ForgetOnRejectedKey: the lane switches) acts on it;
// every other device logs it once and keeps its key, because a wrong
// "forgotten" would drop a real switch's adoption on one site's evidence.
const (
	rejectedKeyInforms = 3
	rejectedKeyAfter   = 2 * time.Minute
)

// missedForget is the reply applied in place of the setdefault the bridge
// missed: the same path as the controller's own (internal/device).
var missedForget = []byte(`{"_type":"setdefault"}`)

// noteRejectedKey follows the run of empty-body HTTP 400s answered to
// informs sent on an adopted key. Any other answer ends the run; an inform
// that got no answer at all (the controller down) neither counts nor ends
// it. It reports true once the run reads as a missed Forget and the loop
// may act on it.
func (l *Loop) noteRejectedKey(now time.Time, status, bodyLen int, sentAdopted bool) bool {
	if status != http.StatusBadRequest || bodyLen != 0 || !sentAdopted {
		l.rejectRun, l.rejectWarned = 0, false
		return false
	}
	if l.rejectRun == 0 {
		l.rejectSince = now
	}
	l.rejectRun++
	if l.rejectRun < rejectedKeyInforms || now.Sub(l.rejectSince) < rejectedKeyAfter {
		return false
	}
	run := now.Sub(l.rejectSince).Round(time.Second)
	if l.cfg.ForgetOnRejectedKey {
		l.cfg.Logger.Printf("[%s] the controller has refused this device's key %d times over %s (HTTP 400, empty body): that is a device it has forgotten, whose one setdefault this bridge missed; treated as that setdefault", l.desc.MAC, l.rejectRun, run)
		l.rejectRun = 0
		return true
	}
	if !l.rejectWarned {
		l.rejectWarned = true
		l.cfg.Logger.Printf("[%s] WARNING: the controller has refused this device's key %d times over %s (HTTP 400, empty body). That is how it answers a device it has forgotten, once its one setdefault has gone: if the device is gone from UniFi, stop the bridge, remove this device's device.json from its state directory and start it again to adopt it anew. The bridge keeps the key (only a lane switch is released on this alone)", l.desc.MAC, l.rejectRun, run)
	}
	return false
}
