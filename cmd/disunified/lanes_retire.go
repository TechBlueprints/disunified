package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/TechBlueprints/disunified/internal/config"
	"github.com/TechBlueprints/disunified/internal/unifiapi"
)

// Retire on join (docs/drivers/breakout.md 16.13; Clint, 2026-11-18: "build
// it"). A lane switch whose cage is no longer split has nothing to present:
// its collect fails "not split" every cycle and the controller shows it
// disconnected for good. So when the parent positively reads the cage as one
// port, several collects in a row, the bridge
//
//   - stops the lane switch's loop (it informs no more),
//   - for an automatic one, hands the lanes back (DetachLaneSwitch, the
//     path a Forget takes), forgets its record in the controller (by its
//     exact MAC, only a lane switch's record), and once the controller holds
//     no record for it removes its state directory, so that a later split is
//     a fresh lane switch, pending and then adopted with a new key;
//   - for an automatic one of a `lane_switches: manual` parent, or with no
//     controller API, stops it and hands the lanes back only: its state and
//     its record are the operator's, as its adoption was;
//   - for a declared one (`driver: breakout`), stops it only: it stays
//     attached, and its state and its record are left alone.
//
// A stopped lane switch whose cage is split again is started again (a
// declared or kept one resumes from its state file).

// The count: retireReads joined reads in a row, each from a collect of its
// own (a later snapshot time than the last), spanning at least retireSpan,
// no two more than retireMaxGap apart. Any other read with a new snapshot
// time -- split, or one the parent cannot give (still starting, no such
// port, a fold that disagrees with its lanes) -- starts the count again.
//
// Why these: a parent's own split or join runs inside one batch under its
// write lock, so no collect ever sees it half done, and the read right
// after it disagrees with itself until the next collect (CageLanes answers
// 0). What a single read cannot rule out is a breakout made by hand, whose
// CLI can take a minute (sonic.md 11.38: a refused breakout bails after
// 60 s); two minutes of whole reads is past that, and is the span 16.11's
// rule for a refused key takes for the same reason (one site's controller,
// a wrong guess costs a re-adoption). Four reads keep a parent that
// collects quickly (10 s at start) from reaching the count on one long
// read's worth of time. A gap of more than three minutes (the lane
// switch's own max_age: the parent was not answering) is not "in a row".
const (
	retireReads  = 4
	retireSpan   = 2 * time.Minute
	retireMaxGap = 3 * time.Minute
)

// The Forget's pace: retried after forgetRetry, each pause doubling up to
// forgetMaxWait. api.err.DeviceBusy lasts about two minutes after an
// adoption (16.12).
const (
	forgetRetry   = 30 * time.Second
	forgetMaxWait = 10 * time.Minute
)

// joinCount counts one cage's joined reads.
type joinCount struct {
	n           int
	first, last time.Time // when the first and the last counted read were taken in (the watcher's clock)
	seen        time.Time // the last snapshot time looked at
}

// read takes the parent's current read of the cage at now: lanes and at are
// what CageLanes answered; a read whose snapshot time is not newer than the
// last one looked at is no read (fresh false). It reports whether the count
// is reached.
func (j *joinCount) read(lanes int, at, now time.Time) (reached, fresh bool) {
	if at.IsZero() || !at.After(j.seen) {
		return j.reached(), false
	}
	j.seen = at
	if lanes != 1 {
		*j = joinCount{seen: at}
		return false, true
	}
	if j.n > 0 && now.Sub(j.last) > retireMaxGap {
		j.n = 0
	}
	if j.n == 0 {
		j.first = now
	}
	j.n++
	j.last = now
	return j.reached(), true
}

func (j *joinCount) reached() bool {
	return j.n >= retireReads && j.last.Sub(j.first) >= retireSpan
}

// stopChild ends a lane switch's loop and waits for it to return (up to
// wait): it informs no more. It reports whether the loop has returned.
func stopChild(halt, exited chan struct{}, wait time.Duration) bool {
	if halt != nil {
		select {
		case <-halt:
		default:
			close(halt)
		}
	}
	if exited == nil {
		return true
	}
	select {
	case <-exited:
		return true
	default:
	}
	if wait <= 0 {
		return false
	}
	select {
	case <-exited:
		return true
	case <-time.After(wait):
		return false
	}
}

// childExitWait is how long a retire waits for the loop to return within one
// tick; a loop that takes longer is looked at again on the next tick.
const childExitWait = 10 * time.Second

// retire runs the retire for every automatic lane switch this watcher
// started and every declared one on this parent's cages. It runs on the
// watcher's tick, before the split cages are looked at (a stopped lane
// switch whose cage is split again is started again there).
func (a *autoLanes) retire() {
	a.mu.Lock()
	runs := make(map[int]*laneRun, len(a.running))
	for k, v := range a.running {
		runs[k] = v
	}
	a.mu.Unlock()
	for cage, r := range runs {
		a.retireAuto(cage, r)
	}
	for cage, d := range a.declaredBy {
		a.retireDeclared(cage, d)
	}
}

// retireAuto is one automatic lane switch's step.
func (a *autoLanes) retireAuto(cage int, r *laneRun) {
	lanes, at := a.lp.CageLanes(cage)
	reached, fresh := r.joined.read(lanes, at, a.clock())
	name, mac := r.c.Name, r.c.MAC
	if r.stopped {
		if fresh && lanes > 1 {
			// Split again before the Forget went through: the lane switch is
			// still adopted in the controller, and the tick starts it again
			// from its state file.
			a.forgetRun(cage)
			a.log.Printf("lane switch %s (%s): port %d is split again before the lane switch was retired; it starts again from its state file", name, mac, cage)
			return
		}
		if r.awaitCount {
			// Not started at the bridge's start (its cage read whole): the
			// retire waits for the count as for a running one.
			if !reached {
				return
			}
			r.awaitCount = false
			a.log.Printf("lane switch %s (%s): port %d has read as one port on %d collects in a row over %s (the cage is joined), so the lane switch is retired", name, mac, cage, r.joined.n, r.joined.last.Sub(r.joined.first).Round(time.Second))
		}
		if !r.exitedSeen {
			if !stopChild(r.c.halt, r.c.exited, 0) {
				return
			}
			r.exitedSeen = true
			a.handBack(cage, r)
		}
		a.forgetStep(cage, r)
		return
	}
	if !reached {
		return
	}
	r.stopped = true
	r.since, r.next, r.wait, r.lastErr = time.Time{}, time.Time{}, 0, ""
	a.log.Printf("lane switch %s (%s): port %d has read as one port on %d collects in a row over %s (the cage is joined), so the lane switch is stopped", name, mac, cage, r.joined.n, r.joined.last.Sub(r.joined.first).Round(time.Second))
	if !stopChild(r.c.halt, r.c.exited, childExitWait) {
		a.log.Printf("lane switch %s (%s): its loop has not returned yet; the retire goes on once it has", name, mac)
		return
	}
	r.exitedSeen = true
	a.handBack(cage, r)
	a.forgetStep(cage, r)
}

// handBack detaches the cage from the lane switch (the hand-back a Forget
// makes, OnUnadopted) and says what is next.
func (a *autoLanes) handBack(cage int, r *laneRun) {
	a.lp.DetachLaneSwitch(cage)
	switch {
	case a.manual:
		a.log.Printf("lane switch %s (%s) detached from port %d: the lanes are this device's again; its state and its record in UniFi are left as they are (lane_switches: manual: forget it there by hand); it starts again if the cage is split again", r.c.Name, r.c.MAC, cage)
	case a.forget == nil:
		a.log.Printf("lane switch %s (%s) detached from port %d: the lanes are this device's again; no controller API is configured, so its record stays in UniFi, disconnected (forget it there by hand) and its state is kept; it starts again if the cage is split again", r.c.Name, r.c.MAC, cage)
	default:
		a.log.Printf("lane switch %s (%s) detached from port %d: the lanes are this device's again; forgetting its record in UniFi next (its names, VLANs and profiles there go with it; a breakout put back later is a new lane switch, adopted with defaults)", r.c.Name, r.c.MAC, cage)
	}
}

// forgetStep forgets the record and removes the state (automatic, auto
// parent, API configured); otherwise the lane switch is done with, kept.
func (a *autoLanes) forgetStep(cage int, r *laneRun) {
	name, mac := r.c.Name, r.c.MAC
	if a.manual || a.forget == nil {
		a.forgetRun(cage)
		return
	}
	now := a.clock()
	if now.Before(r.forgetNext) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	res, err := a.forget(ctx, mac)
	if err != nil {
		if msg := err.Error(); msg != r.forgetErr {
			r.forgetErr = msg
			a.log.Printf("lane switch %s (%s): its record in UniFi was not forgotten: %v; asked again later (this line is not repeated while the reason stays)", name, mac, err)
		}
		switch {
		case r.forgetWait < forgetRetry:
			r.forgetWait = forgetRetry
		case r.forgetWait*2 > forgetMaxWait:
			r.forgetWait = forgetMaxWait
		default:
			r.forgetWait *= 2
		}
		r.forgetNext = now.Add(r.forgetWait)
		if errors.Is(err, unifiapi.ErrNotLaneSwitch) {
			r.forgetNext = now.Add(forgetMaxWait)
		}
		return
	}
	switch res {
	case unifiapi.Forgotten:
		a.log.Printf("lane switch %s (%s): forgotten in UniFi by its exact MAC (port %d is joined); a breakout put back in it later is a new lane switch", name, mac, cage)
	default:
		a.log.Printf("lane switch %s (%s): UniFi holds no record for it (port %d is joined)", name, mac, cage)
	}
	dir := a.stateDirOf(name)
	if err := os.RemoveAll(dir); err != nil {
		a.log.Printf("WARNING: lane switch %s: its state directory %s could not be removed (%v): remove it by hand, or a split of port %d resumes from it", name, dir, err, cage)
	} else {
		a.log.Printf("lane switch %s: state directory removed (%s); retired", name, dir)
	}
	a.forgetRun(cage)
}

// forgetRun drops the cage's lane switch from the running set, with its
// once-only notes: the next split starts one afresh.
func (a *autoLanes) forgetRun(cage int) {
	name := config.AutoLaneSwitchName(a.parent, cage)
	a.mu.Lock()
	delete(a.running, cage)
	for _, k := range []string{name, "noapi:" + name, "parent:" + name} {
		delete(a.noted, k)
	}
	a.mu.Unlock()
}

// stateDirOf is a lane switch's state directory (state/<name>).
func (a *autoLanes) stateDirOf(name string) string {
	if a.stateDir != nil {
		return a.stateDir(name)
	}
	return filepath.Join("state", name)
}

// retireDeclared is one declared lane switch's step: stopped while its cage
// stays joined, started again when it is split; never detached, forgotten or
// cleared (the file declares it).
func (a *autoLanes) retireDeclared(cage int, d *declaredLane) {
	lanes, at := a.lp.CageLanes(cage)
	d.mu.Lock()
	reached, fresh := d.joined.read(lanes, at, a.clock())
	halt, exited, respawn, stopped := d.halt, d.exited, d.respawn, d.stopped
	d.mu.Unlock()
	switch {
	case stopped && fresh && lanes > 1 && respawn != nil:
		d.mu.Lock()
		d.stopped = false
		d.joined = joinCount{seen: at}
		d.mu.Unlock()
		a.log.Printf("lane switch %s: port %d is split again; started again (declared in the config file)", d.name, cage)
		respawn()
	case !stopped && reached && halt != nil:
		d.mu.Lock()
		d.stopped = true
		d.mu.Unlock()
		stopChild(halt, exited, childExitWait)
		a.log.Printf("lane switch %s: port %d has read as one port on %d collects in a row (the cage is joined), so the lane switch is stopped; the config file declares it, so it stays attached and its state and its record in UniFi are left as they are; it starts again when the cage is split", d.name, cage, retireReads)
	}
}
