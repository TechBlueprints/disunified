// Package powerevent turns a power device's polled state into one log line
// per transition -- on battery, AC restored, low battery, overload, output
// off, a self-test result, losing and regaining contact -- for a device that
// has no event log of its own. An APC Smart-UPS read over its SmartConnect
// port is the case it was written for: no management card, so no syslog,
// and the bridge's poll is the only place an outage is seen at all.
//
// Lines are fixed-shape and greppable:
//
//	power-event level=error device=ups name=example-ups model=SMTL1500RM3UC event=on_battery power=battery output=on charge=97 runtime_s=2460 load_w=310 load_pct=23 input_v=0 cause=...
//
// level comes first so a log shipper that reads `level=X` near the start of
// a line (the site's Vector rule looks in the first 80 characters) stores
// it: error for the alarms, info for the recoveries, warning for an initial
// state that is already abnormal and for a self-test that did not run.
//
// Only changes are logged. The first reading after a start is the baseline,
// logged once as event=initial_state, never as transitions; an alarm is
// logged on the first poll that shows it, its recovery only after
// RecoverPolls polls in a row agree, so a flapping input is one outage, not
// several.
package powerevent

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Defaults for a zero Tracker field or setting.
const (
	DefaultLowRuntime    = 2 * time.Minute
	DefaultCommLostAfter = 3
	DefaultRecoverPolls  = 2
)

// Tracker follows one device. It is not safe for concurrent use; the loop
// calls it from its one goroutine.
type Tracker struct {
	Device   string        // the bridge's name for the device (the config entry), device=
	Name     func() string // the controller's name for it, name=; nil or "" omits the field
	Settings devicemodel.PowerEventSettings
	// Logf receives each finished line.
	Logf func(format string, args ...any)
	// CommLostAfter is how many failed polls in a row mean contact is lost.
	CommLostAfter int
	// RecoverPolls is how many polls in a row must agree before a recovery
	// (AC restored, battery OK, overload cleared, output on) is logged.
	RecoverPolls int

	seeded bool
	model  string
	last   *devicemodel.Battery
	conds  []*cond
	test   string

	failures    int
	firstFailed time.Time
	commLost    bool
}

// cond is one alarm with a recovery: raised on the first poll that shows
// it, cleared after RecoverPolls polls that show it gone.
type cond struct {
	alarm, recovery string
	durationKey     string // the recovery line's field for how long the alarm held
	raised          func(*devicemodel.Battery) bool
	cleared         func(*devicemodel.Battery) bool // stricter than !raised where there is hysteresis
	reason          func(*devicemodel.Battery) string

	active  bool
	since   time.Time // first poll that showed the alarm
	okRun   int       // polls in a row that showed it gone
	okSince time.Time // first of those
}

func (t *Tracker) init() {
	if t.conds != nil {
		return
	}
	if t.CommLostAfter <= 0 {
		t.CommLostAfter = DefaultCommLostAfter
	}
	if t.RecoverPolls <= 0 {
		t.RecoverPolls = DefaultRecoverPolls
	}
	low := t.Settings.LowRuntime
	if low <= 0 {
		low = DefaultLowRuntime
	}
	// Hysteresis: a runtime estimate wobbles with the load, so a low battery
	// clears only a fifth above the threshold, and an overload only 5 %
	// below the alarm level.
	lowClear := low + low/5
	alarmW := func(b *devicemodel.Battery) float64 {
		if t.Settings.LoadAlarmW > 0 {
			return t.Settings.LoadAlarmW
		}
		return b.RealPowerRatingW
	}
	overW := func(b *devicemodel.Battery, frac float64) bool {
		w := alarmW(b)
		return b.HasOutput && w > 0 && b.RealPowerW >= w*frac
	}
	t.conds = []*cond{
		{
			alarm: "on_battery", recovery: "ac_restored", durationKey: "on_battery_s",
			raised:  func(b *devicemodel.Battery) bool { return b.OnBattery },
			cleared: func(b *devicemodel.Battery) bool { return !b.OnBattery },
		},
		{
			alarm: "low_battery", recovery: "battery_ok", durationKey: "low_s",
			raised: func(b *devicemodel.Battery) bool {
				return b.LowBattery || b.LowRuntime || b.Runtime < low
			},
			cleared: func(b *devicemodel.Battery) bool {
				return !b.LowBattery && !b.LowRuntime && b.Runtime >= lowClear
			},
			reason: func(b *devicemodel.Battery) string {
				switch {
				case b.LowBattery:
					return "shutdown_imminent"
				case b.LowRuntime:
					return "ups_low_runtime"
				}
				return fmt.Sprintf("runtime_below_%ds", int(low/time.Second))
			},
		},
		{
			alarm: "overload", recovery: "overload_cleared", durationKey: "overload_s",
			raised:  func(b *devicemodel.Battery) bool { return b.Overload || overW(b, 1) },
			cleared: func(b *devicemodel.Battery) bool { return !b.Overload && !overW(b, 0.95) },
			reason: func(b *devicemodel.Battery) string {
				if b.Overload {
					return "ups_overload"
				}
				return fmt.Sprintf("load_at_or_above_%.0fw", alarmW(b))
			},
		},
		{
			alarm: "output_off", recovery: "output_on", durationKey: "off_s",
			raised:  func(b *devicemodel.Battery) bool { return b.OutputOff },
			cleared: func(b *devicemodel.Battery) bool { return !b.OutputOff },
		},
	}
}

// Observe takes one successful poll.
func (t *Tracker) Observe(now time.Time, snap *devicemodel.Snapshot) {
	if snap == nil || snap.System.Battery == nil {
		return
	}
	t.init()
	b := snap.System.Battery
	t.model = snap.System.SKU
	if t.model == "" {
		t.model = snap.System.Model
	}
	if t.commLost {
		t.emit("info", "comm_restored", b, fmt.Sprintf("failures=%d lost_s=%d", t.failures, secs(now.Sub(t.firstFailed))))
	}
	t.failures, t.commLost = 0, false
	t.last = b

	if !t.seeded {
		t.seeded = true
		t.test = b.TestResult
		level := "info"
		var active []string
		for _, c := range t.conds {
			if c.raised(b) {
				c.active, c.since = true, now
				active = append(active, c.alarm)
			}
		}
		if len(active) > 0 {
			level = "warning"
		}
		extra := "active=" + strings.Join(active, ",")
		if len(active) == 0 {
			extra = "active=none"
		}
		if b.TestResult != "" {
			extra += " test=" + b.TestResult
		}
		t.emit(level, "initial_state", b, extra)
		return
	}

	for _, c := range t.conds {
		switch {
		case !c.active && c.raised(b):
			c.active, c.since, c.okRun = true, now, 0
			extra := ""
			if c.reason != nil {
				extra = "reason=" + c.reason(b)
			}
			t.emit("error", c.alarm, b, extra)
		case c.active && c.cleared(b):
			if c.okRun == 0 {
				c.okSince = now
			}
			c.okRun++
			if c.okRun >= t.RecoverPolls {
				c.active, c.okRun = false, 0
				t.emit("info", c.recovery, b, fmt.Sprintf("%s=%d", c.durationKey, secs(c.okSince.Sub(c.since))))
			}
		case c.active:
			c.okRun = 0
		}
	}

	// A self-test is logged when its outcome changes to a final one. The
	// unit holds only the last outcome, so a test that ends the way the
	// previous one did, between two polls, is not seen.
	if b.TestResult != t.test {
		t.test = b.TestResult
		switch b.TestResult {
		case "passed":
			t.emit("info", "self_test", b, "result=passed")
		case "failed":
			t.emit("error", "self_test", b, "result=failed")
		case "refused", "aborted":
			t.emit("warning", "self_test", b, "result="+b.TestResult)
		}
	}
}

// Failed takes one failed poll. Contact is declared lost after
// CommLostAfter in a row; the readings on that line are the last good ones.
func (t *Tracker) Failed(now time.Time, err error) {
	t.init()
	t.failures++
	if t.failures == 1 {
		t.firstFailed = now
	}
	if t.commLost || t.failures < t.CommLostAfter {
		return
	}
	t.commLost = true
	msg := "unknown"
	if err != nil {
		msg = err.Error()
	}
	t.emit("error", "comm_lost", t.last, fmt.Sprintf("failures=%d since_s=%d error=%q", t.failures, secs(now.Sub(t.firstFailed)), msg))
}

// emit writes one line: the fixed head, the readings (when there are any),
// then the event's own fields.
func (t *Tracker) emit(level, event string, b *devicemodel.Battery, extra string) {
	if t.Logf == nil {
		return
	}
	var s strings.Builder
	fmt.Fprintf(&s, "power-event level=%s device=%s", level, field(t.Device))
	if t.Name != nil {
		if n := t.Name(); n != "" {
			fmt.Fprintf(&s, " name=%s", field(n))
		}
	}
	if t.model != "" {
		fmt.Fprintf(&s, " model=%s", field(t.model))
	}
	fmt.Fprintf(&s, " event=%s", event)
	if b != nil {
		power, output := "mains", "on"
		if b.OnBattery {
			power = "battery"
		}
		if b.OutputOff {
			output = "off"
		}
		inV := 0.0 // 0 = the device gives no input reading (an APC unit on battery)
		if b.HasInput {
			inV = b.InputVoltageV
		}
		fmt.Fprintf(&s, " power=%s output=%s charge=%d runtime_s=%d load_w=%.0f load_pct=%.0f input_v=%.0f",
			power, output, b.ChargePct, secs(b.Runtime), b.RealPowerW, b.LoadPct, inV)
		if b.TransferCause != "" && (event == "on_battery" || event == "ac_restored" || event == "initial_state") {
			fmt.Fprintf(&s, " cause=%s", field(b.TransferCause))
		}
	}
	if extra != "" {
		s.WriteString(" " + extra)
	}
	t.Logf("%s", s.String())
}

// field keeps a value one token: spaces become underscores.
func field(v string) string { return strings.ReplaceAll(strings.TrimSpace(v), " ", "_") }

func secs(d time.Duration) int64 { return int64(math.Round(d.Seconds())) }
