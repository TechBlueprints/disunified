package main

import (
	"context"
	"time"
)

// A bridged device on a UniFi product's firmware line is offered that
// line's updates, and taking one changes nothing but the version it
// reports (device.Session, the `upgrade` reply). The controller sends an
// update only when asked, so without this the UI shows an update badge
// until an operator clicks it. upgradeAsker asks for the device: it reads
// the controller's offer and requests it, as the Update button does.
const (
	// upgradeSettle is how long after a start or a (re)connect the first
	// look waits: the controller works out its offer from the device's
	// informs.
	upgradeSettle = time.Minute
	// upgradeCheckEvery is the spacing of the looks after that. An offer
	// appears when the controller learns of a new release.
	upgradeCheckEvery = time.Hour
	// upgradeAskAgain: one version is asked for once, and again only after
	// this long if the controller still offers it.
	upgradeAskAgain = 6 * time.Hour
)

type upgradeAsker struct {
	now     func() time.Time
	adopted func() bool
	// offered is the version the controller offers, "" for none.
	offered func(context.Context) (string, error)
	request func(context.Context) error
	logf    func(string, ...any)

	kick chan struct{}

	// Touched only by check, which only run calls.
	target    string // the version last asked for
	asked     time.Time
	failNoted bool
}

func newUpgradeAsker(adopted func() bool, offered func(context.Context) (string, error), request func(context.Context) error, logf func(string, ...any)) *upgradeAsker {
	return &upgradeAsker{now: time.Now, adopted: adopted, offered: offered, request: request, logf: logf, kick: make(chan struct{}, 1)}
}

// connected says the device has just connected (an adoption, a reconnect):
// the next look comes upgradeSettle from now. Never blocks.
func (u *upgradeAsker) connected() {
	select {
	case u.kick <- struct{}{}:
	default:
	}
}

// run looks until ctx ends.
func (u *upgradeAsker) run(ctx context.Context) {
	t := time.NewTimer(upgradeSettle)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.kick:
			t.Reset(upgradeSettle)
		case <-t.C:
			u.check(ctx)
			t.Reset(upgradeCheckEvery)
		}
	}
}

// check asks for the offered version unless the device is not adopted,
// nothing is offered, or that version was asked for less than
// upgradeAskAgain ago.
func (u *upgradeAsker) check(ctx context.Context) {
	if !u.adopted() {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	target, err := u.offered(cctx)
	if err != nil {
		if ctx.Err() == nil && !u.failNoted {
			u.failNoted = true
			u.logf("firmware: the controller's offer cannot be read: %v (looked for again every %s)", err, upgradeCheckEvery)
		}
		return
	}
	u.failNoted = false
	if target == "" {
		return
	}
	now := u.now()
	if target == u.target && now.Sub(u.asked) < upgradeAskAgain {
		return
	}
	if err := u.request(cctx); err != nil {
		u.logf("firmware: the controller offers %s, but the request for it failed: %v (asked again in %s)", target, err, upgradeCheckEvery)
		return
	}
	u.target, u.asked = target, now
	u.logf("firmware: the controller offers %s; asked it to send the update (emulated: only the reported version changes)", target)
}
