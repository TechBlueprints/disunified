package informloop

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

// fanDevice is a device whose fan mode lives in one variable: Collect reads
// it back, ApplyFanMode sets it.
type fanDevice struct {
	modes   []devicemodel.FanMode
	mode    devicemodel.FanMode
	applied []devicemodel.FanMode
}

func (f *fanDevice) Collect(context.Context) (*devicemodel.Snapshot, error) {
	return &devicemodel.Snapshot{System: devicemodel.System{FanModes: f.modes, FanMode: f.mode}}, nil
}

func (f *fanDevice) ApplyFanMode(_ context.Context, m devicemodel.FanMode) error {
	f.applied = append(f.applied, m)
	f.mode = m
	return nil
}

func fanLoop(t *testing.T, coll devicemodel.Collector, want devicemodel.FanMode) (*Loop, *device.Session, *strings.Builder) {
	t.Helper()
	desc := inform.Descriptor{MAC: "02:00:00:00:00:03", Model: "USWF07D"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef"}, nil, time.Now())
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), Collector: coll, CollectTimeout: time.Second, FanMode: want})
	if err != nil {
		t.Fatal(err)
	}
	return l, sess, &buf
}

func sysFanModeOf(t *testing.T, sess *device.Session) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	fm, _ := m["sys_fan_mode"].(map[string]any)
	return fm
}

// The configured mode is written once, when the device is not in it; the
// collects after it read the mode back and write nothing; a hand change
// is put back on the next collect. The inform reports the gateway's shape:
// caps (DEFAULT 1 | QUIET 2) and the current mode.
func TestFanModeIsADiffAgainstTheReadBack(t *testing.T) {
	dev := &fanDevice{modes: []devicemodel.FanMode{devicemodel.FanModeDefault, devicemodel.FanModeQuiet}, mode: devicemodel.FanModeDefault}
	l, sess, buf := fanLoop(t, dev, devicemodel.FanModeQuiet)
	ctx := context.Background()
	l.collect(ctx)
	if len(dev.applied) != 1 || dev.applied[0] != devicemodel.FanModeQuiet {
		t.Fatalf("applied %v, want [quiet]", dev.applied)
	}
	if !strings.Contains(buf.String(), "fan mode default -> quiet") {
		t.Errorf("log: %s", buf)
	}
	if fm := sysFanModeOf(t, sess); fm["caps"] != float64(3) || fm["mode"] != "default" {
		t.Errorf("sys_fan_mode %v: the collect that wrote reports what it read", fm)
	}
	l.collect(ctx)
	l.collect(ctx)
	if len(dev.applied) != 1 {
		t.Fatalf("a converged device was written again: %v", dev.applied)
	}
	if fm := sysFanModeOf(t, sess); fm["caps"] != float64(3) || fm["mode"] != "quiet" {
		t.Errorf("sys_fan_mode %v, want caps 3 mode quiet", fm)
	}
	dev.mode = devicemodel.FanModeDefault // changed by hand on the device
	l.collect(ctx)
	if len(dev.applied) != 2 || dev.mode != devicemodel.FanModeQuiet {
		t.Errorf("a hand change was not put back: applied %v, mode %s", dev.applied, dev.mode)
	}
}

// Unset fan_mode leaves the fans alone; the inform still says what the
// device can do and runs.
func TestFanModeUnsetWritesNothing(t *testing.T) {
	dev := &fanDevice{modes: []devicemodel.FanMode{devicemodel.FanModeDefault, devicemodel.FanModeQuiet}, mode: devicemodel.FanModeQuiet}
	l, sess, _ := fanLoop(t, dev, "")
	l.collect(context.Background())
	if len(dev.applied) != 0 {
		t.Fatalf("applied %v with no fan_mode", dev.applied)
	}
	if fm := sysFanModeOf(t, sess); fm["caps"] != float64(3) || fm["mode"] != "quiet" {
		t.Errorf("sys_fan_mode %v", fm)
	}
}

// A device that cannot be set (no fan files on this platform) claims
// nothing, and the configured mode is refused once in the log, not tried.
func TestFanModeRefusedWhereTheDeviceCannotSetIt(t *testing.T) {
	dev := &fanDevice{}
	l, sess, buf := fanLoop(t, dev, devicemodel.FanModeQuiet)
	for i := 0; i < 3; i++ {
		l.collect(context.Background())
	}
	if len(dev.applied) != 0 {
		t.Fatalf("applied %v on a device that offers no modes", dev.applied)
	}
	if n := strings.Count(buf.String(), "fan_mode quiet: not settable on this device"); n != 1 {
		t.Errorf("refusal logged %d times, want once: %s", n, buf)
	}
	if fm := sysFanModeOf(t, sess); fm != nil {
		t.Errorf("sys_fan_mode %v claimed for a device that offers no modes", fm)
	}
	// A driver without the interface at all.
	l2, _, buf2 := fanLoop(t, &failingCollector{}, devicemodel.FanModeQuiet)
	l2.collect(context.Background())
	l2.collect(context.Background())
	if n := strings.Count(buf2.String(), "this driver cannot set the fan mode"); n != 1 {
		t.Errorf("logged %d times: %s", n, buf2)
	}
}
