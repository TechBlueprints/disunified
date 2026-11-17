package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type askerRig struct {
	u        *upgradeAsker
	clock    time.Time
	adopted  bool
	offer    string
	offerErr error
	reqErr   error
	requests int
	lines    []string
}

func newAskerRig() *askerRig {
	r := &askerRig{clock: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), adopted: true}
	r.u = newUpgradeAsker(
		func() bool { return r.adopted },
		func(context.Context) (string, error) { return r.offer, r.offerErr },
		func(context.Context) error { r.requests++; return r.reqErr },
		func(f string, a ...any) { r.lines = append(r.lines, fmt.Sprintf(f, a...)) },
	)
	r.u.now = func() time.Time { return r.clock }
	return r
}

// An offered version is asked for once; while the controller still shows
// it (the device is mid-update) it is not asked for again, until
// upgradeAskAgain has passed. A newer offer is asked for at once.
func TestUpgradeAskerAsksOncePerOffer(t *testing.T) {
	r := newAskerRig()
	ctx := context.Background()

	r.u.check(ctx)
	if r.requests != 0 || len(r.lines) != 0 {
		t.Fatalf("nothing offered: %d requests, lines %v", r.requests, r.lines)
	}

	r.offer = "7.6.11.17272"
	r.u.check(ctx)
	r.clock = r.clock.Add(time.Hour)
	r.u.check(ctx)
	if r.requests != 1 || len(r.lines) != 1 {
		t.Fatalf("one offer: %d requests, lines %v; want 1 and 1", r.requests, r.lines)
	}

	r.clock = r.clock.Add(upgradeAskAgain)
	r.u.check(ctx)
	if r.requests != 2 {
		t.Fatalf("still offered after %s: %d requests, want 2", upgradeAskAgain, r.requests)
	}

	r.offer = "7.6.12.1"
	r.u.check(ctx)
	if r.requests != 3 {
		t.Fatalf("a newer offer: %d requests, want 3", r.requests)
	}
}

// A device that is not adopted is not looked up; a read that fails is
// logged once and asked again; a request that fails is not remembered as
// asked.
func TestUpgradeAskerNotAdoptedAndFailures(t *testing.T) {
	r := newAskerRig()
	ctx := context.Background()
	r.offer = "7.6.11.17272"

	r.adopted = false
	r.u.check(ctx)
	if r.requests != 0 {
		t.Fatalf("not adopted: %d requests", r.requests)
	}
	r.adopted = true

	r.offerErr = errors.New("no route")
	r.u.check(ctx)
	r.u.check(ctx)
	if r.requests != 0 || len(r.lines) != 1 {
		t.Fatalf("unreadable offer: %d requests, lines %v; want 0 and 1", r.requests, r.lines)
	}
	r.offerErr = nil

	r.reqErr = errors.New("HTTP 503")
	r.u.check(ctx)
	r.reqErr = nil
	r.u.check(ctx)
	if r.requests != 2 {
		t.Fatalf("a failed request is asked again: %d requests, want 2", r.requests)
	}
}
