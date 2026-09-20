package informloop

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/device"
	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
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
