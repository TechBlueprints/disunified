package main

import (
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// provisionSpacing is the least time between two provisions of a device:
// the adoption handshake, the first collect and a held push all ask for one
// within seconds of each other, and one read-modify-write of the
// controller's record serves them all.
const provisionSpacing = 10 * time.Second

// stopper is the part of *time.Timer the spacer uses.
type stopper interface{ Stop() bool }

// provisionSpacer decides when runOne's provisioning action runs: never for
// a session that is not adopted or a nil snapshot, at most once per
// provisionSpacing, again on a timer while the controller's API cannot be
// reached, and -- for a layout change that falls inside the spacing -- once
// the spacing ends instead of not at all. The action runs under the
// spacer's lock: the loop's callbacks and the timer never run two
// read-modify-writes of the controller's record at once.
type provisionSpacer struct {
	now       func() time.Time
	afterFunc func(time.Duration, func()) stopper
	adopted   func() bool
	// current is the session's snapshot when a timer fires.
	current func() *devicemodel.Snapshot
	// action provisions from snap and returns how long to wait before
	// asking again (the API could not be reached), or 0 for no retry.
	action func(snap *devicemodel.Snapshot) (retry time.Duration)

	mu    sync.Mutex
	last  time.Time
	timer stopper // a retry, or a layout change waiting for the spacing
}

// newProvisionSpacer is the spacer on the real clock.
func newProvisionSpacer(adopted func() bool, current func() *devicemodel.Snapshot, action func(*devicemodel.Snapshot) time.Duration) *provisionSpacer {
	return &provisionSpacer{
		now:       time.Now,
		afterFunc: func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) },
		adopted:   adopted,
		current:   current,
		action:    action,
	}
}

// provision runs the action from snap unless the session is not adopted,
// snap is nil or the last provision was less than provisionSpacing ago
// (then the call is dropped). A pending timer is stopped once the action
// has run; a retry the action asks for arms a new one.
func (p *provisionSpacer) provision(snap *devicemodel.Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.adopted() || snap == nil || p.now().Sub(p.last) < provisionSpacing {
		return
	}
	p.last = p.now()
	retry := p.action(snap)
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if retry > 0 {
		p.timer = p.afterFunc(retry, p.fire)
	}
}

// layoutChanged provisions after a layout change. One that falls inside the
// spacing (the first collect of a run differing from the startup snapshot)
// waits for the spacing to end instead of being dropped: its ports are
// withheld until the controller is seeded. A timer already pending (a
// retry, or an earlier layout change) covers it.
func (p *provisionSpacer) layoutChanged(snap *devicemodel.Snapshot) {
	p.mu.Lock()
	if wait := provisionSpacing - p.now().Sub(p.last); wait > 0 && p.timer == nil && p.adopted() {
		p.timer = p.afterFunc(wait, p.fire)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.provision(snap)
}

// fire is a timer's provision, from the session's snapshot at that moment.
func (p *provisionSpacer) fire() { p.provision(p.current()) }
