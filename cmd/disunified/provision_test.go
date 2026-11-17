package main

import (
	"sync"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// fakeTimer is one afterFunc timer; the test fires it by hand.
type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
	fired   bool
}

func (t *fakeTimer) Stop() bool {
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}

// spacerRig drives a provisionSpacer on a fake clock with fake timers and a
// recording action: no sleeping, no controller.
type spacerRig struct {
	t      *testing.T
	mu     sync.Mutex
	clock  time.Time
	timers []*fakeTimer
	// adopted and snap are the session's state.
	adopted bool
	snap    *devicemodel.Snapshot
	// calls are the snapshots the action ran with; retry is what it returns.
	calls []*devicemodel.Snapshot
	retry time.Duration
	p     *provisionSpacer
}

func newSpacerRig(t *testing.T) *spacerRig {
	r := &spacerRig{t: t, clock: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), adopted: true, snap: snapNamed("session")}
	r.p = &provisionSpacer{
		now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.clock },
		afterFunc: func(d time.Duration, f func()) stopper {
			r.mu.Lock()
			defer r.mu.Unlock()
			ft := &fakeTimer{d: d, f: f}
			r.timers = append(r.timers, ft)
			return ft
		},
		adopted: func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.adopted },
		current: func() *devicemodel.Snapshot { r.mu.Lock(); defer r.mu.Unlock(); return r.snap },
		action: func(snap *devicemodel.Snapshot) time.Duration {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, snap)
			return r.retry
		},
	}
	return r
}

func (r *spacerRig) advance(d time.Duration) {
	r.mu.Lock()
	r.clock = r.clock.Add(d)
	r.mu.Unlock()
}

// live is the timers neither stopped nor fired.
func (r *spacerRig) live() []*fakeTimer {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*fakeTimer
	for _, t := range r.timers {
		if !t.stopped && !t.fired {
			out = append(out, t)
		}
	}
	return out
}

// fire runs a timer, as time.AfterFunc would once its wait is over (the test
// advances the clock to that moment first).
func (r *spacerRig) fire(ft *fakeTimer) {
	r.mu.Lock()
	ft.fired = true
	r.mu.Unlock()
	ft.f()
}

func (r *spacerRig) ncalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func snapNamed(id string) *devicemodel.Snapshot {
	return &devicemodel.Snapshot{Ports: []devicemodel.Port{{Index: 1, IfName: id}}}
}

// The ordinary path (the adoption handshake, a held push) inside the spacing
// is dropped, as it always was, and arms nothing.
func TestProvisionSpacerDropsInsideSpacing(t *testing.T) {
	r := newSpacerRig(t)
	r.p.provision(snapNamed("a"))
	if r.ncalls() != 1 {
		t.Fatalf("first provision: %d calls, want 1", r.ncalls())
	}
	r.advance(provisionSpacing - time.Millisecond)
	r.p.provision(snapNamed("b"))
	if r.ncalls() != 1 {
		t.Errorf("inside the spacing: %d calls, want the call dropped", r.ncalls())
	}
	if n := len(r.live()); n != 0 {
		t.Errorf("inside the spacing: %d timers armed, want none", n)
	}
	r.advance(time.Millisecond)
	r.p.provision(snapNamed("c"))
	if r.ncalls() != 2 {
		t.Errorf("at the end of the spacing: %d calls, want 2", r.ncalls())
	}
	// A nil snapshot never provisions.
	r.advance(provisionSpacing)
	r.p.provision(nil)
	if r.ncalls() != 2 {
		t.Errorf("nil snapshot: %d calls, want 2", r.ncalls())
	}
}

// A layout change inside the spacing provisions once, when the spacing
// ends, from the session's snapshot at that moment, not the one it was
// called with.
func TestProvisionSpacerLayoutChangeWaitsForSpacing(t *testing.T) {
	r := newSpacerRig(t)
	r.p.provision(snapNamed("startup"))
	r.advance(3 * time.Second)
	r.p.layoutChanged(snapNamed("first-collect"))
	if r.ncalls() != 1 {
		t.Fatalf("inside the spacing: %d calls, want the change to wait", r.ncalls())
	}
	live := r.live()
	if len(live) != 1 {
		t.Fatalf("%d timers armed, want 1", len(live))
	}
	if live[0].d != provisionSpacing-3*time.Second {
		t.Errorf("timer waits %s, want the rest of the spacing (%s)", live[0].d, provisionSpacing-3*time.Second)
	}
	later := snapNamed("at-the-timer")
	r.mu.Lock()
	r.snap = later
	r.mu.Unlock()
	r.advance(live[0].d)
	r.fire(live[0])
	if r.ncalls() != 2 {
		t.Fatalf("after the timer: %d calls, want 2", r.ncalls())
	}
	if r.calls[1] != later {
		t.Errorf("the timer provisioned from %v, want the session's snapshot at that moment", r.calls[1].Ports)
	}
	if n := len(r.live()); n != 0 {
		t.Errorf("after the timer: %d timers live, want none", n)
	}
	// An ordinary provision racing the timer at the end of the spacing: it
	// runs first and stops the timer; the timer's own call, already on its
	// way, is then inside the new spacing and dropped. Exactly once.
	r.advance(time.Second)
	r.p.layoutChanged(snapNamed("again"))
	live = r.live()
	if len(live) != 1 {
		t.Fatalf("second change: %d timers, want 1", len(live))
	}
	r.advance(live[0].d)
	r.p.provision(snapNamed("held-push"))
	if !live[0].stopped {
		t.Error("the provision that ran did not stop the waiting timer")
	}
	live[0].f() // a timer whose Stop came too late still runs
	if r.ncalls() != 3 {
		t.Errorf("provision and a late timer at the end of the spacing: %d calls, want 3", r.ncalls())
	}
}

// A second layout change while one waits adds no timer and no call.
func TestProvisionSpacerOneTimerForTwoLayoutChanges(t *testing.T) {
	r := newSpacerRig(t)
	r.p.provision(snapNamed("startup"))
	r.advance(2 * time.Second)
	r.p.layoutChanged(snapNamed("one"))
	r.advance(2 * time.Second)
	r.p.layoutChanged(snapNamed("two"))
	if n := len(r.live()); n != 1 {
		t.Fatalf("%d timers armed, want 1", n)
	}
	r.mu.Lock()
	ntimers := len(r.timers)
	r.mu.Unlock()
	if ntimers != 1 {
		t.Errorf("%d timers ever armed, want 1", ntimers)
	}
	if r.ncalls() != 1 {
		t.Errorf("%d calls before the timer, want 1", r.ncalls())
	}
	ft := r.live()[0]
	r.advance(6 * time.Second)
	r.fire(ft)
	if r.ncalls() != 2 {
		t.Errorf("%d calls after the timer, want 2", r.ncalls())
	}
}

// A layout change outside the spacing provisions at once with its own
// snapshot; so does the first one of a run, before any provision.
func TestProvisionSpacerLayoutChangeOutsideSpacing(t *testing.T) {
	r := newSpacerRig(t)
	first := snapNamed("first")
	r.p.layoutChanged(first)
	if r.ncalls() != 1 || r.calls[0] != first {
		t.Fatalf("no provision yet: %d calls, want 1 from the change's snapshot", r.ncalls())
	}
	r.advance(provisionSpacing)
	next := snapNamed("next")
	r.p.layoutChanged(next)
	if r.ncalls() != 2 || r.calls[1] != next {
		t.Errorf("outside the spacing: %d calls, want 2, the second from the change's snapshot", r.ncalls())
	}
	if n := len(r.live()); n != 0 {
		t.Errorf("%d timers armed, want none", n)
	}
}

// An unadopted session is neither provisioned nor scheduled.
func TestProvisionSpacerUnadopted(t *testing.T) {
	r := newSpacerRig(t)
	r.p.provision(snapNamed("startup"))
	r.mu.Lock()
	r.adopted = false
	r.mu.Unlock()
	r.advance(time.Second)
	r.p.layoutChanged(snapNamed("inside"))
	r.advance(provisionSpacing)
	r.p.layoutChanged(snapNamed("outside"))
	r.p.provision(snapNamed("ordinary"))
	if r.ncalls() != 1 {
		t.Errorf("unadopted: %d calls, want only the adopted one", r.ncalls())
	}
	if n := len(r.live()); n != 0 {
		t.Errorf("unadopted: %d timers armed, want none", n)
	}
}

// An action that asks for a retry arms one timer for that wait; the retry
// provisions from the session's snapshot, and a success arms nothing more.
// A layout change inside the spacing while the retry waits is covered by it.
func TestProvisionSpacerRetry(t *testing.T) {
	r := newSpacerRig(t)
	r.retry = 15 * time.Second
	r.p.provision(snapNamed("startup"))
	live := r.live()
	if len(live) != 1 || live[0].d != 15*time.Second {
		t.Fatalf("after a failure: timers %v, want one of 15s", live)
	}
	r.advance(time.Second)
	r.p.layoutChanged(snapNamed("change"))
	if n := len(r.live()); n != 1 || r.ncalls() != 1 {
		t.Errorf("change while the retry waits: %d timers, %d calls; want 1 and 1", n, r.ncalls())
	}
	r.mu.Lock()
	r.retry = 0
	r.snap = snapNamed("now")
	r.mu.Unlock()
	r.advance(14 * time.Second)
	r.fire(live[0])
	if r.ncalls() != 2 || r.calls[1].Ports[0].IfName != "now" {
		t.Errorf("the retry: %d calls; want 2, from the session's snapshot", r.ncalls())
	}
	if n := len(r.live()); n != 0 {
		t.Errorf("after a success: %d timers, want none", n)
	}
}
