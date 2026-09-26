package informloop

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

// planningOutlets: outlet 2 is on; a push turning it off would change it.
type planningOutlets struct{ applied [][]devicemodel.OutletDesired }

func (p *planningOutlets) ApplyOutlets(_ context.Context, d []devicemodel.OutletDesired) (int, error) {
	p.applied = append(p.applied, d)
	return len(p.PlanOutlets(d)), nil
}
func (p *planningOutlets) PlanOutlets(d []devicemodel.OutletDesired) []int {
	var out []int
	for _, o := range d {
		if o.Index == 2 && !o.On {
			out = append(out, 2)
		}
	}
	return out
}

func newPendingOutletLoop(t *testing.T, oc devicemodel.OutletController, allow bool, cfg string) (*Loop, *device.Session, *strings.Builder) {
	t.Helper()
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: cfg}
	desc := inform.Descriptor{MAC: "02:00:00:00:00:02", Model: "USWDA25"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), OutletController: oc, AllowInitialChanges: allow})
	if err != nil {
		t.Fatal(err)
	}
	return l, sess, &buf
}

// The first outlet push of a run that would switch a group is held: an
// outlet group is a rack of load, and a controller that disagrees with the
// device on first contact is more likely stale than right.
func TestFirstOutletPushThatWouldSwitchIsHeld(t *testing.T) {
	oc := &planningOutlets{}
	l, sess, buf := newPendingOutletLoop(t, oc, false, "outlet.2.relay_state=disabled\n")
	if !l.applyPending(context.Background()) {
		t.Fatal("a pending push must be consumed by applyPending")
	}
	if len(oc.applied) != 0 {
		t.Errorf("the first push was applied: %v", oc.applied)
	}
	if _, _, ok := sess.Pending(); !ok {
		t.Error("the held push must stay pending so the controller keeps re-sending")
	}
	if !strings.Contains(buf.String(), "HOLDING the first outlet push") || !strings.Contains(buf.String(), "[2]") {
		t.Errorf("log = %q", buf.String())
	}
}

// The operator can override the hold.
func TestAllowInitialChangesAppliesTheFirstOutletPush(t *testing.T) {
	oc := &planningOutlets{}
	l, sess, _ := newPendingOutletLoop(t, oc, true, "outlet.2.relay_state=disabled\n")
	l.applyPending(context.Background())
	if len(oc.applied) != 1 {
		t.Errorf("allow_initial_changes did not apply the push: %v", oc.applied)
	}
	if _, _, ok := sess.Pending(); ok {
		t.Error("an applied push must not stay pending")
	}
}

// A push that changes nothing goes through and ends the hold; the next
// push that does change something is then applied.
func TestOutletHoldEndsWithAPushThatChangesNothing(t *testing.T) {
	oc := &planningOutlets{}
	l, sess, _ := newPendingOutletLoop(t, oc, false, "outlet.2.relay_state=enabled\n")
	l.applyPending(context.Background())
	if len(oc.applied) != 1 {
		t.Fatalf("a push that changes nothing must be applied, got %v", oc.applied)
	}
	if !l.everOutletApplied {
		t.Fatal("everOutletApplied not set after a successful apply")
	}
	// A later push that switches the group is applied, not held.
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v2", PendingSystemCfg: "outlet.2.relay_state=disabled\n"}
	l.session = device.NewSession(inform.Descriptor{MAC: "02:00:00:00:00:02", Model: "USWDA25"}, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess = l.session
	l.applyPending(context.Background())
	if len(oc.applied) != 2 || len(oc.applied[1]) != 1 || oc.applied[1][0].On {
		t.Errorf("the changing push after the hold ended was not applied: %v", oc.applied)
	}
	if _, _, ok := sess.Pending(); ok {
		t.Error("the applied push stayed pending")
	}
}

// The main group is index 1 on a UPS 2U (index base 1); the controller's
// push for it reaches the driver, which is where it is refused -- the loop
// only translates indices.
func TestOutletPushIndicesAreTranslatedFromTheModelBase(t *testing.T) {
	oc := &planningOutlets{}
	l, _, _ := newPendingOutletLoop(t, oc, true, "outlet.1.relay_state=disabled\noutlet.2.relay_state=enabled\n")
	l.applyPending(context.Background())
	if len(oc.applied) != 1 || len(oc.applied[0]) != 2 || oc.applied[0][0].Index != 1 || oc.applied[0][1].Index != 2 {
		t.Errorf("desired = %v, want indices 1 and 2 as pushed (base 1)", oc.applied)
	}
}
