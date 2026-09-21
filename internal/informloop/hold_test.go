package informloop

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/switchmodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

// planningController changes ports 18 and 19 whenever asked and records applies.
type planningController struct{ applied int }

func (p *planningController) ApplyPorts(_ context.Context, d []switchmodel.PortDesired) (int, error) {
	p.applied++
	return 2, nil
}
func (p *planningController) PlanPorts(d []switchmodel.PortDesired) []int { return []int{18, 19} }

func newPendingLoop(t *testing.T, ctl switchmodel.Controller, allow bool, everApplied bool) (*Loop, *device.Session, *strings.Builder) {
	t.Helper()
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: "switch.port.18.status=enabled\nswitch.port.19.status=enabled\n"}
	if everApplied {
		st.CfgVersion, st.SystemCfg = "v0", "switch.port.18.status=enabled\n"
	}
	sess := device.NewSession(inform.Descriptor{MAC: "02:00:00:00:00:01"}, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	var buf strings.Builder
	l, err := New(inform.Descriptor{MAC: "02:00:00:00:00:01"}, sess, Config{Logger: log.New(&buf, "", 0), Controller: ctl, AllowInitialChanges: allow})
	if err != nil {
		t.Fatal(err)
	}
	return l, sess, &buf
}

func TestFirstPushThatWouldChangePortsIsHeld(t *testing.T) {
	ctl := &planningController{}
	l, sess, buf := newPendingLoop(t, ctl, false, false)
	if !l.applyPending(context.Background()) {
		t.Fatal("a pending push must be consumed by applyPending")
	}
	if ctl.applied != 0 {
		t.Errorf("the first push was applied (%d ApplyPorts calls)", ctl.applied)
	}
	if _, _, ok := sess.Pending(); !ok {
		t.Errorf("the held push must stay pending so the controller keeps re-sending")
	}
	if !strings.Contains(buf.String(), "HOLDING the first push") || !strings.Contains(buf.String(), "[18 19]") {
		t.Errorf("log = %q", buf.String())
	}
	// Once something has been applied to this device, pushes go through.
	l2, sess2, _ := newPendingLoop(t, &planningController{}, false, true)
	l2.applyPending(context.Background())
	if _, _, ok := sess2.Pending(); ok {
		t.Errorf("a later push must be applied")
	}
	// And the operator can override the hold.
	ctl3 := &planningController{}
	l3, _, _ := newPendingLoop(t, ctl3, true, false)
	l3.applyPending(context.Background())
	if ctl3.applied != 1 {
		t.Errorf("allow_initial_changes did not apply the push")
	}
}

// freshController changes only port 30 (a guest that just appeared).
type freshController struct{ applied []switchmodel.PortDesired }

func (f *freshController) ApplyPorts(_ context.Context, d []switchmodel.PortDesired) (int, error) {
	f.applied = append(f.applied, d...)
	return len(d), nil
}
func (f *freshController) PlanPorts(d []switchmodel.PortDesired) []int {
	for _, p := range d {
		if p.Index == 30 {
			return []int{30}
		}
	}
	return nil
}

func TestFreshPortIsWithheldUntilTheControllerAgrees(t *testing.T) {
	ctl := &freshController{}
	l, _, buf := newPendingLoop(t, ctl, false, true) // an established device: later pushes apply
	l.freshPorts = map[int]string{30: "vm998-net0"}
	desired := []switchmodel.PortDesired{{Index: 1, Enabled: true}, {Index: 30, Enabled: true}}
	got := l.withholdFreshPorts(desired)
	if len(got) != 1 || got[0].Index != 1 {
		t.Errorf("fresh port not withheld: %+v", got)
	}
	if !strings.Contains(buf.String(), "port 30 (vm998-net0) is new") {
		t.Errorf("log = %q", buf.String())
	}
	// A push that matches the port's live state releases it.
	ctl2 := &freshController{}
	l2, _, _ := newPendingLoop(t, ctl2, false, true)
	l2.freshPorts = map[int]string{30: "vm998-net0"}
	agree := &planningNothing{}
	l2.cfg.Controller = agree
	got = l2.withholdFreshPorts(desired)
	if len(got) != 2 || len(l2.freshPorts) != 0 {
		t.Errorf("agreeing port still withheld: %+v fresh=%v", got, l2.freshPorts)
	}
}

type planningNothing struct{}

func (planningNothing) ApplyPorts(context.Context, []switchmodel.PortDesired) (int, error) {
	return 0, nil
}
func (planningNothing) PlanPorts([]switchmodel.PortDesired) []int { return nil }
