package apcups

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/powerevent"
)

// eventRig polls the captured unit through the collector into a tracker,
// one poll per inform interval, with the fixture's registers changed the
// way the unit's would be. Every reading starts from the live capture.
type eventRig struct {
	t     *testing.T
	c     *Collector
	fr    *FixtureRunner
	tr    *powerevent.Tracker
	now   time.Time
	lines []string
}

func newEventRig(t *testing.T, s devicemodel.PowerEventSettings) *eventRig {
	t.Helper()
	c, fr := startFixture(t)
	c.PowerEventSettings = s
	r := &eventRig{t: t, c: c, fr: fr, now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	r.tr = &powerevent.Tracker{
		Device:   "ups",
		Name:     func() string { return "example-ups" },
		Settings: c.PowerEvents(),
		Logf:     func(f string, a ...any) { r.lines = append(r.lines, fmt.Sprintf(f, a...)) },
	}
	return r
}

// poll takes one reading 70 s after the last (the controller's interval
// here) and returns the lines it produced.
func (r *eventRig) poll() []string {
	r.t.Helper()
	r.now = r.now.Add(70 * time.Second)
	n := len(r.lines)
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		r.tr.Failed(r.now, err)
	} else {
		r.tr.Observe(r.now, snap)
	}
	return r.lines[n:]
}

// status sets or clears UPSStatus_BF bits (the low word is register 1).
func (r *eventRig) status(set, clear uint32) {
	st := uint32(r.fr.Regs[regStatus])<<16 | uint32(r.fr.Regs[regStatus+1])
	st = st&^clear | set
	r.fr.Regs[regStatus], r.fr.Regs[regStatus+1] = uint16(st>>16), uint16(st)
}

func (r *eventRig) onBattery() {
	r.status(statusOnBattery, statusOnline)
	r.fr.Regs[regInputVoltage] = 0xffff
}

func (r *eventRig) onMains(inputRaw uint16) {
	r.status(statusOnline, statusOnBattery)
	r.fr.Regs[regInputVoltage] = inputRaw
}

func (r *eventRig) runtime(sec uint32) {
	r.fr.Regs[regRuntime], r.fr.Regs[regRuntime+1] = uint16(sec>>16), uint16(sec)
}

// expect asserts the lines are exactly these events, in order.
func expect(t *testing.T, step string, got []string, events ...string) {
	t.Helper()
	if len(got) != len(events) {
		t.Fatalf("%s: want events %v, got %d lines:\n%s", step, events, len(got), strings.Join(got, "\n"))
	}
	for i, e := range events {
		if !strings.Contains(got[i], " event="+e+" ") && !strings.HasSuffix(got[i], " event="+e) {
			t.Fatalf("%s: line %d is not event=%s:\n%s", step, i, e, got[i])
		}
	}
}

func has(t *testing.T, line string, fields ...string) {
	t.Helper()
	for _, f := range fields {
		if !strings.Contains(" "+line+" ", " "+f+" ") {
			t.Errorf("line lacks %q:\n%s", f, line)
		}
	}
}

// The first reading after a start is the baseline: one initial_state line,
// no transitions, and nothing more while nothing changes.
func TestPowerEventsSeedSilently(t *testing.T) {
	r := newEventRig(t, devicemodel.PowerEventSettings{})
	got := r.poll()
	expect(t, "first poll", got, "initial_state")
	// The captured unit: on mains, 100 % charge, 376 s at 79 % of 1350 W, input 115.8 V.
	has(t, got[0], "level=info", "device=ups", "name=example-ups", "model=SMTL1500RM3UC",
		"power=mains", "output=on", "charge=100", "runtime_s=376", "load_w=1067", "load_pct=79", "input_v=116",
		"cause=AcceptableInput", "active=none")
	if !strings.HasPrefix(got[0], "power-event level=info ") {
		t.Errorf("level must lead the line: %s", got[0])
	}
	// The site's Vector rule reads level= within the first 80 characters of
	// the journal line, which the bridge's logger prefixes with the device
	// and a timestamp.
	journal := "[ups] 2026/10/31 00:00:26.095724 " + got[0]
	if i := strings.Index(journal, "level="); i < 0 || i > 80 {
		t.Errorf("level= at %d in the journal line, past Vector's 80 characters", i)
	}
	for i := 0; i < 5; i++ {
		expect(t, "unchanged poll", r.poll())
	}
}

// A start in the middle of an abnormal state is still one line, a warning
// naming what is active, never a burst of alarms; the recovery is logged
// when it comes.
func TestPowerEventsSeedAbnormalState(t *testing.T) {
	r := newEventRig(t, devicemodel.PowerEventSettings{LoadAlarmW: 1000})
	r.onBattery()
	got := r.poll()
	expect(t, "first poll", got, "initial_state")
	has(t, got[0], "level=warning", "power=battery", "input_v=0", "active=on_battery,overload")
	expect(t, "unchanged", r.poll())
	r.onMains(7409)
	expect(t, "mains once", r.poll())
	got = r.poll()
	expect(t, "mains twice", got, "ac_restored")
	has(t, got[0], "level=info", "on_battery_s=140")
}

// On battery is logged on the first poll that shows it; AC restored only
// after two polls on mains, so a flapping input is one outage with its
// whole length, not several.
func TestPowerEventsOutageWithFlapping(t *testing.T) {
	r := newEventRig(t, devicemodel.PowerEventSettings{})
	r.poll()
	r.onBattery()
	got := r.poll() // t0
	expect(t, "outage", got, "on_battery")
	has(t, got[0], "level=error", "power=battery", "input_v=0", "charge=100", "runtime_s=376")
	expect(t, "still on battery", r.poll())
	r.onMains(7409)
	expect(t, "mains for one poll", r.poll())
	r.onBattery()
	expect(t, "battery again: same outage", r.poll())
	r.onMains(7409)
	expect(t, "mains, first of two", r.poll()) // t0 + 4 polls
	got = r.poll()
	expect(t, "mains, second of two", got, "ac_restored")
	has(t, got[0], "level=info", "power=mains", "on_battery_s=280")
	for i := 0; i < 3; i++ {
		expect(t, "after", r.poll())
	}
}

// Low battery: the unit's shutdown-imminent signal, its own low-runtime
// bit, or runtime under the default floor; it clears only with margin.
func TestPowerEventsLowBattery(t *testing.T) {
	r := newEventRig(t, devicemodel.PowerEventSettings{})
	r.poll()
	r.onBattery()
	r.poll()
	r.runtime(100)
	got := r.poll()
	expect(t, "runtime under 2m", got, "low_battery")
	has(t, got[0], "level=error", "runtime_s=100", "reason=runtime_below_120s")
	r.fr.Regs[regSimpleSignal] |= simpleShutdownImminent
	expect(t, "shutdown imminent while already low", r.poll())
	r.fr.Regs[regSimpleSignal] &^= simpleShutdownImminent
	r.onMains(7409)
	r.runtime(140) // above the threshold, below its 144 s clear level
	expect(t, "mains, runtime in the hysteresis band", r.poll())
	got = r.poll()
	expect(t, "second mains poll", got, "ac_restored")
	r.runtime(300)
	expect(t, "recharged, first poll", r.poll())
	got = r.poll()
	expect(t, "recharged, second poll", got, "battery_ok")
	has(t, got[0], "level=info", "low_s=280")

	// The unit's own low-runtime bit (the Main group's OutletStatus_BF bit
	// 14) counts with any runtime.
	r.fr.Regs[regOutletGroup0+1] |= outletGroupLowRuntime
	got = r.poll()
	expect(t, "unit's low-runtime bit", got, "low_battery")
	has(t, got[0], "reason=ups_low_runtime", "runtime_s=300")
}

func TestPowerEventsOverloadAndOutput(t *testing.T) {
	r := newEventRig(t, devicemodel.PowerEventSettings{})
	r.poll()
	r.status(statusOverload, 0)
	got := r.poll()
	expect(t, "overload bit", got, "overload")
	has(t, got[0], "level=error", "reason=ups_overload")
	r.status(0, statusOverload)
	r.poll()
	expect(t, "cleared", r.poll(), "overload_cleared")

	r.status(statusOff, 0)
	got = r.poll()
	expect(t, "output off", got, "output_off")
	has(t, got[0], "level=error", "output=off")
	r.status(0, statusOff)
	r.poll()
	got = r.poll()
	expect(t, "output on", got, "output_on")
	has(t, got[0], "level=info", "off_s=70")

	// load_alarm_w above the live 1067 W: a load that grows past it raises it.
	r2 := newEventRig(t, devicemodel.PowerEventSettings{LoadAlarmW: 1200})
	r2.poll()
	r2.fr.Regs[regLoadPct] = 23000 // 89.8 % of 1350 W = 1213 W
	got = r2.poll()
	expect(t, "over the budget", got, "overload")
	has(t, got[0], "reason=load_at_or_above_1200w", "load_w=1213")
	r2.fr.Regs[regLoadPct] = 22400 // 1181 W: under the alarm, inside its 5 % band
	expect(t, "in the band", r2.poll())
	expect(t, "in the band", r2.poll())
}

// ReplaceBatteryTestStatus_BF (register 23): a new final outcome is one
// line; a test in progress is not; the captured unit records none.
func TestPowerEventsSelfTest(t *testing.T) {
	r := newEventRig(t, devicemodel.PowerEventSettings{})
	if got := r.poll(); strings.Contains(got[0], "test=") {
		t.Errorf("the captured unit records no test result: %s", got[0])
	}
	r.fr.Regs[regBatteryTest] = 1<<1 | 1<<6 // in progress, asked over the protocol
	expect(t, "in progress", r.poll())
	r.fr.Regs[regBatteryTest] = 1<<2 | 1<<6
	got := r.poll()
	expect(t, "passed", got, "self_test")
	has(t, got[0], "level=info", "result=passed")
	expect(t, "unchanged", r.poll())
	r.fr.Regs[regBatteryTest] = 1 << 3
	got = r.poll()
	expect(t, "failed", got, "self_test")
	has(t, got[0], "level=error", "result=failed")
	r.fr.Regs[regBatteryTest] = 1<<4 | 1<<11
	got = r.poll()
	expect(t, "refused", got, "self_test")
	has(t, got[0], "level=warning", "result=refused")
}

// Contact is lost after three failed polls in a row, logged once with the
// last good readings, and regained on the first success; two failures and
// a success log nothing. A change that happened in the gap is logged when
// contact returns.
func TestPowerEventsCommLoss(t *testing.T) {
	r := newEventRig(t, devicemodel.PowerEventSettings{})
	r.poll()
	gone := errors.New("dial tcp 192.0.2.30:502: connect: connection refused")
	r.fr.FailReads = gone
	expect(t, "fail 1", r.poll())
	expect(t, "fail 2", r.poll())
	r.fr.FailReads = nil
	expect(t, "back after 2", r.poll())

	r.fr.FailReads = gone
	r.poll()
	r.poll()
	got := r.poll()
	expect(t, "fail 3", got, "comm_lost")
	has(t, got[0], "level=error", "failures=3", "since_s=140", "power=mains", "charge=100")
	if !strings.Contains(got[0], `error="dial tcp 192.0.2.30:502: connect: connection refused"`) {
		t.Errorf("error not quoted whole: %s", got[0])
	}
	expect(t, "fail 4", r.poll())
	r.fr.FailReads = nil
	r.onBattery()
	got = r.poll()
	expect(t, "contact back, on battery", got, "comm_restored", "on_battery")
	has(t, got[0], "level=info", "failures=4", "lost_s=280")
}

// A device that never answers after start never seeds: a comm_lost line
// without readings, and the initial state when it first answers.
func TestPowerEventsCommLossBeforeFirstReading(t *testing.T) {
	r := newEventRig(t, devicemodel.PowerEventSettings{})
	r.fr.FailReads = errors.New("i/o timeout")
	r.poll()
	r.poll()
	got := r.poll()
	expect(t, "lost", got, "comm_lost")
	if strings.Contains(got[0], "charge=") || strings.Contains(got[0], "model=") {
		t.Errorf("no readings exist yet: %s", got[0])
	}
	r.fr.FailReads = nil
	expect(t, "first answer", r.poll(), "comm_restored", "initial_state")
}
