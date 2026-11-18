package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/config"
	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unifiapi"
)

// Automatic lane switches (docs/drivers/breakout.md 16): every split cage
// of a device that can present lanes gets a lane switch, started here and
// not declared in the file. It informs as a pending device and writes
// nothing; the lanes become its own when the controller adopts it (the
// attach) and the cage's again when the controller forgets it (the detach).
//
// Since 2026-11-17 the bridge asks for that adoption itself (16.12): a lane
// switch whose parent is adopted needs no Adopt click. It asks for the one
// MAC it made, never for anything else on the controller's pending list. A
// lane switch the operator forgot stays pending until it is adopted by
// hand (the forgotten mark), and `lane_switches: manual` leaves every one
// of a parent's pending.

// laneWatchInterval is how often a parent's split cages are looked at.
const laneWatchInterval = 15 * time.Second

// maxLaneCage is the highest cage that gets a lane switch: a lane switch's
// aggregates take their ids from the cage number, and both parents stop at
// 99 (PortChannel<100*cage+lag> on SONiC, Port-Channel<1000+10*cage+lag>
// on EOS).
const maxLaneCage = 99

// autoLanes starts and tracks one parent's automatic lane switches.
type autoLanes struct {
	log       *log.Logger
	parent    string // the parent's config name
	parentMAC string // the parent's own MAC: each child's is derived from it
	lp        devicemodel.LaneProvider
	// declared are the cages with a `driver: breakout` entry in the file
	// (those win); taken are the file's entry names.
	declared map[int]bool
	taken    map[string]bool
	// adopted reports whether the lane switch of that name holds an adopted
	// state file; start runs it (its own goroutine).
	adopted func(name string) bool
	start   func(child autoLane)
	// cageToAuto sets the cage's port on the parent to Auto in the
	// controller when it holds a forced lane speed (the hand-over,
	// docs/drivers/breakout.md 16.7); nil without a controller API.
	cageToAuto func(ctx context.Context, cage int) (before map[string]any, err error)
	// manual is the parent's `lane_switches: manual`: lane switches are
	// started and stay pending until the operator adopts them.
	manual bool
	// adopt asks the controller to adopt the pending device with exactly
	// this MAC (unifiapi.AdoptPending); nil without a controller API.
	adopt func(ctx context.Context, mac string) error
	// markPath is where a lane switch's Forget is remembered (a file in its
	// state directory); now is the clock (nil: time.Now).
	markPath func(name string) string
	now      func() time.Time

	mu      sync.Mutex
	running map[int]*laneRun
	noted   map[string]bool
}

// laneRun is one started lane switch and where its adoption stands.
type laneRun struct {
	c autoLane
	// since is when it was first seen unadopted (zero while adopted); next
	// is the earliest time of the next adopt request, wait the pause it
	// came from, lastErr the last failure logged.
	since, next time.Time
	wait        time.Duration
	lastErr     string
}

// Adopt requests: the first no sooner than one watch interval after the
// lane switch was seen unadopted (it has to inform before the controller
// lists it, and a Forget's mark is written in that time); a request the
// controller took is repeated after adoptAskAgain if the lane switch is
// still unadopted, a failed one after adoptRetry, each pause doubling up
// to adoptMaxWait.
const (
	adoptRetry    = 30 * time.Second
	adoptAskAgain = 2 * time.Minute
	adoptMaxWait  = 10 * time.Minute
)

func (a *autoLanes) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// forgotten reports whether the lane switch's Forget is remembered.
func (a *autoLanes) forgotten(name string) bool {
	if a.markPath == nil {
		return false
	}
	_, err := os.Stat(a.markPath(name))
	return err == nil
}

// markForgotten writes or removes the lane switch's forgotten mark.
func (a *autoLanes) markForgotten(name string, on bool) {
	if a.markPath == nil {
		return
	}
	p := a.markPath(name)
	if !on {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.log.Printf("lane switch %s: its forgotten mark %s could not be removed: %v", name, p, err)
		}
		return
	}
	err := os.MkdirAll(filepath.Dir(p), 0o700)
	if err == nil {
		err = os.WriteFile(p, []byte("forgotten in the controller "+a.clock().UTC().Format(time.RFC3339)+": this lane switch stays pending until it is adopted there by hand (docs/drivers/breakout.md 16.12)\n"), 0o600)
	}
	if err != nil {
		a.log.Printf("WARNING: lane switch %s: its Forget could not be recorded in %s (%v): this device will ask the controller to adopt it again", name, p, err)
	}
}

// autoLane is one automatic lane switch as it is started.
type autoLane struct {
	Name string
	Cage int
	MAC  string
	// attach and detach hand the lanes over and back: the child's loop
	// calls them when the adoption handshake completes and when the
	// controller's forget arrives.
	attach func()
	detach func()
	// resume is attach for a lane switch that was adopted before this run
	// (its state file says so): the hand-over's controller step was done at
	// its adoption, and a start must not wait on the controller's API.
	resume func()
}

func (a *autoLanes) noteOnce(key, format string, args ...any) {
	a.mu.Lock()
	seen := a.noted[key]
	if a.noted == nil {
		a.noted = map[string]bool{}
	}
	a.noted[key] = true
	a.mu.Unlock()
	if !seen {
		a.log.Printf(format, args...)
	}
}

// child is the lane switch for a cage, or false with the reason logged
// once: a declared entry has the cage, the name is another entry's, the
// cage is out of range, or one is running already (no log for that).
func (a *autoLanes) child(cage int) (autoLane, bool) {
	name := config.AutoLaneSwitchName(a.parent, cage)
	a.mu.Lock()
	running := a.running[cage] != nil
	a.mu.Unlock()
	switch {
	case running || a.declared[cage]:
		return autoLane{}, false
	case cage < 1 || cage > maxLaneCage:
		a.noteOnce(name, "lane switches: port %d is split, but a lane switch's aggregate ids come from the cage number and stop at cage %d; none started", cage, maxLaneCage)
		return autoLane{}, false
	case a.taken[name]:
		a.noteOnce(name, "lane switches: port %d is split, but %q is another entry of the config file; no lane switch started for it (rename that entry, or declare it as this cage's lane switch)", cage, name)
		return autoLane{}, false
	}
	mac, err := devicemodel.LaneSwitchMAC(a.parentMAC, cage)
	if err != nil {
		a.noteOnce(name, "lane switches: port %d: %v; none started", cage, err)
		return autoLane{}, false
	}
	c := autoLane{Name: name, Cage: cage, MAC: mac}
	attach := func(why string) bool {
		if err := a.lp.AttachLaneSwitch(cage, devicemodel.Attachment{MAC: mac, Name: name}); err != nil {
			a.log.Printf("lane switch %s (%s): adopted, but not attached to port %d: %v", name, mac, cage, err)
			return false
		}
		a.log.Printf("lane switch %s (%s) attached to port %d: %s, so the lanes are its own from here (this device keeps the cage's mode and admin state)", name, mac, cage, why)
		return true
	}
	c.attach = func() {
		a.markForgotten(name, false) // adopted again, by this device's request or by hand
		if attach("the controller adopted it") {
			a.handOver(cage)
		}
	}
	c.resume = func() {
		a.markForgotten(name, false)
		attach("it is adopted (its state file)")
	}
	c.detach = func() {
		// The mark first: the watcher must not ask for the adoption the
		// operator has just ended.
		a.markForgotten(name, true)
		a.lp.DetachLaneSwitch(cage)
		a.log.Printf("lane switch %s (%s) detached from port %d: the controller forgot it, so the lanes are this device's again (the cage's own settings go back on them). It stays pending until it is adopted in UniFi by hand: this device does not ask for a forgotten lane switch's adoption", name, mac, cage)
	}
	return c, true
}

// handOver clears a forced lane speed from the cage's port on the parent in
// the controller (handOverCage).
func (a *autoLanes) handOver(cage int) { handOverCage(a.log, a.cageToAuto, cage) }

// handOverCage clears a forced lane speed from the cage's port on the
// parent in the controller, logged on the parent's logger. As a fold the
// cage reported its lanes' form and took a forced 25G with a FEC; owned,
// it reports QSFP28, for which the controller refuses that pair -- on every
// save of any port of the parent, since each save carries every override
// (seen live, docs/drivers/breakout.md 16.7). Auto keeps an owned cage as
// it is, so nothing changes on the switch. toAuto nil: no controller API.
func handOverCage(lg *log.Logger, toAuto func(context.Context, int) (map[string]any, error), cage int) {
	if toAuto == nil {
		lg.Printf("port %d: a lane switch has the lanes now. If this port holds a forced speed below 40G in UniFi, set it to Auto there: the controller refuses every port save on this device until it is (no controller API is configured, so the bridge cannot do it)", cage)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before, err := toAuto(ctx, cage)
	switch {
	case err != nil:
		lg.Printf("WARNING: port %d: a lane switch has the lanes now, and this port's settings in UniFi could not be checked (%v). If it holds a forced speed below 40G, set it to Auto there: the controller refuses every port save on this device until it is", cage, err)
	case before != nil:
		lg.Printf("port %d: set to Auto in UniFi (was %s): a lane switch has the lanes now, and the controller refuses a forced lane speed on the cage's port, with it every port save on this device. Auto keeps the cage split as it is", cage, overrideText(before))
	}
}

// adoptFor is the adoption request (unifiapi.AdoptPending), nil without a
// controller API URL and key.
func adoptFor(f *config.File) func(context.Context, string) error {
	key := f.Controller.APIKey()
	if f.Controller.APIURL == "" || key == "" {
		return nil
	}
	return unifiapi.New(f.Controller.APIURL, key, f.Controller.Site, true).AdoptPending
}

// cageToAutoFor is the hand-over's controller step for one parent, nil
// without a controller API URL and key.
func cageToAutoFor(f *config.File, parentMAC string) func(context.Context, int) (map[string]any, error) {
	key := f.Controller.APIKey()
	if f.Controller.APIURL == "" || key == "" {
		return nil
	}
	api := unifiapi.New(f.Controller.APIURL, key, f.Controller.Site, true)
	return func(ctx context.Context, cage int) (map[string]any, error) {
		return api.CageToAuto(ctx, parentMAC, cage)
	}
}

// Declared lane switches (a `driver: breakout` entry; docs/drivers/breakout.md
// 0) are attached by the parent at start, adopted or not: the file is the
// operator's say that the lanes are the lane switch's. So they own the cage
// from the start, and the hand-over's controller step runs then (off the
// start's path), and again when the controller adopts the lane switch. A
// Forget does not detach one: the file still declares it, and the log says
// how to hand the lanes back (remove the entry, or adopt it again).

// declaredLane is one declared lane switch's hand-over hooks, shared by its
// parent's start (bind) and its own loop (adopted, forgotten).
type declaredLane struct {
	name string
	cage int
	// toAutoFor builds the controller step from the parent's MAC
	// (cageToAutoFor); goFn runs the start's step off the start's path.
	toAutoFor func(parentMAC string) func(context.Context, int) (map[string]any, error)
	goFn      func(func())

	mu     sync.Mutex
	bound  bool
	log    *log.Logger // the parent's
	mac    string
	toAuto func(context.Context, int) (map[string]any, error)
}

// declaredLanes are the file's declared lane switches by name.
func declaredLanes(f *config.File) map[string]*declaredLane {
	out := map[string]*declaredLane{}
	for _, d := range f.Devices {
		if d.Driver != config.LaneDriver {
			continue
		}
		for _, l := range f.LaneSwitchesOf(strings.TrimSpace(d.Options["parent"])) {
			if l.Name == d.Name {
				out[d.Name] = &declaredLane{
					name: l.Name, cage: l.Cage,
					toAutoFor: func(mac string) func(context.Context, int) (map[string]any, error) { return cageToAutoFor(f, mac) },
					goFn:      func(fn func()) { go fn() },
				}
			}
		}
	}
	return out
}

// attached runs on the parent once the lane switch is attached at start:
// the cage is owned from here, adopted or not, so its port goes to Auto in
// the controller if it holds a forced lane speed -- off the start's path (a
// start must not wait on the controller's API).
func (d *declaredLane) attached(lg *log.Logger, parentMAC, mac string) {
	d.mu.Lock()
	d.bound, d.log, d.mac, d.toAuto = true, lg, mac, d.toAutoFor(parentMAC)
	lg2, toAuto := d.log, d.toAuto
	d.mu.Unlock()
	d.goFn(func() { handOverCage(lg2, toAuto, d.cage) })
}

func (d *declaredLane) state() (bool, *log.Logger, string, func(context.Context, int) (map[string]any, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bound, d.log, d.mac, d.toAuto
}

// adopted is the lane switch's OnConnected: the hand-over's controller step
// again (an override saved since the start, or a start whose step could not
// reach the controller). Nothing when the parent did not attach it.
func (d *declaredLane) adopted() {
	if bound, lg, _, toAuto := d.state(); bound {
		handOverCage(lg, toAuto, d.cage)
	}
}

// forgotten is the lane switch's OnUnadopted: it stays attached, and says so.
func (d *declaredLane) forgotten() {
	if bound, lg, mac, _ := d.state(); bound {
		lg.Printf("lane switch %s (%s) was forgotten in the controller and stays attached to port %d: the config file declares it, so the lanes stay its own and nothing applies their settings until it is adopted again (remove its entry from the config file to give the lanes back to this device)", d.name, mac, d.cage)
	}
}

// overrideText prints an override's speed keys in a fixed order.
func overrideText(o map[string]any) string {
	var parts []string
	for _, k := range []string{"autoneg", "speed", "full_duplex", "fec_mode"} {
		if v, ok := o[k]; ok {
			parts = append(parts, fmt.Sprintf("%s %v", k, v))
		}
	}
	return strings.Join(parts, ", ")
}

func (a *autoLanes) run(c autoLane) {
	a.mu.Lock()
	if a.running == nil {
		a.running = map[int]*laneRun{}
	}
	a.running[c.Cage] = &laneRun{c: c}
	a.mu.Unlock()
	a.start(c)
}

// startAdopted attaches and starts every automatic lane switch whose state
// file is adopted, split or not (a whole cage's comes back when the cage is
// split again). It runs before the parent's loop starts: the parent's first
// apply would otherwise write the cage's intent over the lanes on every
// restart.
func (a *autoLanes) startAdopted() {
	for cage := 1; cage <= maxLaneCage; cage++ {
		name := config.AutoLaneSwitchName(a.parent, cage)
		if a.declared[cage] || a.taken[name] || !a.adopted(name) {
			continue
		}
		c, ok := a.child(cage)
		if !ok {
			continue
		}
		c.resume()
		a.run(c)
	}
}

// tick starts a lane switch for every split cage that has none (pending,
// unattached, the cage still the parent's fold) and asks the controller to
// adopt the pending ones that need no operator (adoptPending).
func (a *autoLanes) tick() {
	for _, cage := range a.lp.SplitCages() {
		if c, ok := a.child(cage); ok {
			switch {
			case a.adopted(c.Name):
				c.resume() // adopted since the start (its state file came back): as startAdopted
			case a.manual:
				a.log.Printf("lane switch %s (%s) started for split port %d: pending in the controller; nothing changes on this device until it is adopted there (lane_switches: manual; auto has this device ask for the adoption, off stops lane switches)", c.Name, c.MAC, cage)
			default:
				a.log.Printf("lane switch %s (%s) started for split port %d: pending in the controller; nothing changes on this device until it is adopted there, which this device asks for by itself once it is adopted (lane_switches: manual leaves it to the operator, off stops lane switches)", c.Name, c.MAC, cage)
			}
			a.run(c)
		}
		a.adoptPending(cage)
	}
}

// adoptPending asks the controller to adopt the cage's lane switch when it
// is pending and nothing stands against it: the parent is adopted (an
// adopted switch's split cage is all the consent a lane switch needs,
// Clint 2026-11-17), the operator has not forgotten it, the parent's entry
// does not say manual, and a controller API is configured. Each reason it
// does not ask is said once.
func (a *autoLanes) adoptPending(cage int) {
	a.mu.Lock()
	r := a.running[cage]
	a.mu.Unlock()
	if r == nil {
		return
	}
	name, mac := r.c.Name, r.c.MAC
	if a.adopted(name) {
		r.since, r.next, r.wait, r.lastErr = time.Time{}, time.Time{}, 0, ""
		return
	}
	now := a.clock()
	if r.since.IsZero() {
		r.since = now
		return
	}
	switch {
	case a.manual:
		return
	case a.forgotten(name):
		a.noteOnce("forgotten:"+name, "lane switch %s (%s) stays pending: it was forgotten in the controller (%s). Adopt it in UniFi to hand port %d's lanes over again; that removes the mark", name, mac, a.markPath(name), cage)
		return
	case a.adopt == nil:
		a.noteOnce("noapi:"+name, "lane switch %s (%s) stays pending until it is adopted in UniFi by hand: no controller API is configured, so this device cannot ask for it (set controller.api_url and the API key, DUI_UNIFI_API_KEY)", name, mac)
		return
	case !a.adopted(a.parent):
		a.noteOnce("parent:"+name, "lane switch %s (%s) stays pending while this device is not adopted itself; its adoption is asked for once this device is adopted", name, mac)
		return
	case now.Before(r.next):
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	err := a.adopt(ctx, mac)
	floor := adoptAskAgain
	if err != nil {
		floor = adoptRetry
		if msg := err.Error(); msg != r.lastErr {
			r.lastErr = msg
			a.log.Printf("lane switch %s (%s): the controller was not asked to adopt it: %v; asked again later (this line is not repeated while the reason stays)", name, mac, err)
		}
	} else {
		r.lastErr = ""
		a.log.Printf("lane switch %s (%s) of split port %d: asked the controller to adopt it, by its exact MAC: this device is adopted, so its lane switch needs no Adopt click (lane_switches: manual leaves it to the operator)", name, mac, cage)
	}
	switch {
	case r.wait < floor:
		r.wait = floor
	case r.wait*2 > adoptMaxWait:
		r.wait = adoptMaxWait
	default:
		r.wait *= 2
	}
	r.next = now.Add(r.wait)
}

// watch ticks until ctx ends.
func (a *autoLanes) watch(ctx context.Context) {
	t := time.NewTicker(laneWatchInterval)
	defer t.Stop()
	for {
		a.tick()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// autoLaneOptions is an automatic lane switch as runOne takes it: the
// entry a `driver: breakout` declaration of the cage would make, under the
// parent's controller settings, with control.ports following the parent's.
func autoLaneOptions(f *config.File, parent config.Device, c autoLane) options {
	o := configOptions(f, config.Device{
		Name: c.Name, Driver: config.LaneDriver, Model: config.LaneModel, Hostname: c.Name,
		UDAPIVersion: parent.UDAPIVersion,
		Options:      map[string]string{"parent": parent.Name, "cage": itoa(c.Cage), "mac": c.MAC, "name": c.Name},
		Control:      config.Control{Ports: parent.Control.Ports},
		ReplyLog:     parent.ReplyLog,
	})
	o.onAdopted, o.onUnadopted = c.attach, c.detach
	return o
}

// newAutoLanes is the parent's watcher, nil when the file turns it off for
// this device or the device is a lane switch itself. spawn runs a child.
func newAutoLanes(f *config.File, parent config.Device, spawn func(options)) func(*log.Logger, devicemodel.LaneProvider, string) *autoLanes {
	if parent.LaneSwitches == config.LaneSwitchesOff || parent.Driver == config.LaneDriver {
		return nil
	}
	declared, taken := map[int]bool{}, map[string]bool{}
	for _, l := range f.LaneSwitchesOf(parent.Name) {
		declared[l.Cage] = true
	}
	for _, d := range f.Devices {
		taken[d.Name] = true
	}
	return func(lg *log.Logger, lp devicemodel.LaneProvider, parentMAC string) *autoLanes {
		return &autoLanes{
			cageToAuto: cageToAutoFor(f, parentMAC),
			manual:     parent.LaneSwitches == config.LaneSwitchesManual,
			adopt:      adoptFor(f),
			markPath:   func(name string) string { return filepath.Join(f.StateDir, "state", name, "forgotten") },
			log:        lg, parent: parent.Name, parentMAC: parentMAC, lp: lp, declared: declared, taken: taken,
			adopted: func(name string) bool {
				st, err := (&device.Store{Path: filepath.Join(f.StateDir, "state", name, "device.json")}).Load()
				return err == nil && st.Adopted
			},
			start: func(c autoLane) { spawn(autoLaneOptions(f, parent, c)) },
		}
	}
}
