package main

import (
	"context"
	"fmt"
	"log"
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

	mu      sync.Mutex
	running map[int]bool
	noted   map[string]bool
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
	running := a.running[cage]
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
		if attach("the controller adopted it") {
			a.handOver(cage)
		}
	}
	c.resume = func() { attach("it is adopted (its state file)") }
	c.detach = func() {
		a.lp.DetachLaneSwitch(cage)
		a.log.Printf("lane switch %s (%s) detached from port %d: the controller forgot it, so the lanes are this device's again (the cage's own settings go back on them)", name, mac, cage)
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
		a.running = map[int]bool{}
	}
	a.running[c.Cage] = true
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

// tick starts a lane switch for every split cage that has none: pending,
// unattached, the cage still the parent's fold.
func (a *autoLanes) tick() {
	for _, cage := range a.lp.SplitCages() {
		c, ok := a.child(cage)
		if !ok {
			continue
		}
		if a.adopted(c.Name) {
			c.resume() // adopted since the start (its state file came back): as startAdopted
		} else {
			a.log.Printf("lane switch %s (%s) started for split port %d: pending in the controller; nothing changes on this device until it is adopted there (lane_switches: off stops this)", c.Name, c.MAC, cage)
		}
		a.run(c)
	}
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
			log:        lg, parent: parent.Name, parentMAC: parentMAC, lp: lp, declared: declared, taken: taken,
			adopted: func(name string) bool {
				st, err := (&device.Store{Path: filepath.Join(f.StateDir, "state", name, "device.json")}).Load()
				return err == nil && st.Adopted
			},
			start: func(c autoLane) { spawn(autoLaneOptions(f, parent, c)) },
		}
	}
}
