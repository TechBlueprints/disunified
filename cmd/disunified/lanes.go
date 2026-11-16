package main

import (
	"context"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/config"
	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
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
	c.attach = func() {
		if err := a.lp.AttachLaneSwitch(cage, devicemodel.Attachment{MAC: mac, Name: name}); err != nil {
			a.log.Printf("lane switch %s (%s): adopted, but not attached to port %d: %v", name, mac, cage, err)
			return
		}
		a.log.Printf("lane switch %s (%s) attached to port %d: the controller adopted it, so the lanes are its own from here (this device keeps the cage's mode and admin state)", name, mac, cage)
	}
	c.detach = func() {
		a.lp.DetachLaneSwitch(cage)
		a.log.Printf("lane switch %s (%s) detached from port %d: the controller forgot it, so the lanes are this device's again (the cage's own settings go back on them)", name, mac, cage)
	}
	return c, true
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
		c.attach()
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
			c.attach() // adopted since the start (its state file came back): as startAdopted
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
			log: lg, parent: parent.Name, parentMAC: parentMAC, lp: lp, declared: declared, taken: taken,
			adopted: func(name string) bool {
				st, err := (&device.Store{Path: filepath.Join(f.StateDir, "state", name, "device.json")}).Load()
				return err == nil && st.Adopted
			},
			start: func(c autoLane) { spawn(autoLaneOptions(f, parent, c)) },
		}
	}
}
