// Package breakout presents a split cage of another bridged switch as a
// five-port switch of its own (a "lane switch"): ports 1..4 are the lanes,
// port 5 is the breakout cable, marked as the uplink. It has no device of
// its own -- it reads the lanes from its parent driver's last snapshot and
// writes them through the parent's own plan (devicemodel.LaneProvider) --
// and runs in the same process as its parent, as another entry of the same
// config file. See docs/drivers/breakout.md.
package breakout

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Ports is the lane switch's port count: four lanes and the uplink, the
// layout of the Flex XG profile it is claimed as (docs/breakout-lag.md 6.2).
const (
	Ports      = 5
	UplinkPort = 5
	maxLanes   = Ports - 1
)

// DefaultMaxAge is how old the parent's snapshot may be before the lane
// switch stops answering: the parent's collects have failed, and reporting
// its last one as fresh would be minutes of frozen numbers.
const DefaultMaxAge = 3 * time.Minute

// Driver is the lane-switch driver.
//
// Options:
//
//	parent   the config name of the device whose cage this is (required;
//	         an entry of the same config file, opened before this one)
//	cage     the parent's port index of the cage (required)
//	mac      the lane switch's MAC; default derived from the parent's
//	         (devicemodel.LaneSwitchMAC), as main derives it when it attaches
//	name     the name it reports (its hostname); main sets the entry's name;
//	         default "<parent>-cage<N>"
//	max_age  how old the parent's snapshot may be (a Go duration; default 3m)
//
// A lane switch answers whether or not it is attached to its parent
// (AttachLaneSwitch): unattached it reports the lanes and writes nothing,
// which is how an automatic one waits for its adoption
// (docs/drivers/breakout.md 16). main attaches a declared one before the
// parent's loop starts and an automatic one when the controller adopts it;
// this driver only refuses a cage that is attached to another MAC.
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "breakout" }

func (Driver) Describe() string {
	return "a split cage of another bridged switch as a five-port switch of its own (options.parent, options.cage); reads and writes through the parent"
}

// Options are the parsed driver options.
type Options struct {
	Parent string
	Cage   int
	MAC    string
	Name   string
	MaxAge time.Duration
}

// ParseOptions reads and checks the options (config.Load runs it too).
func ParseOptions(opts map[string]string) (Options, error) {
	o := Options{Parent: strings.TrimSpace(opts["parent"]), Name: strings.TrimSpace(opts["name"]), MaxAge: DefaultMaxAge}
	if o.Parent == "" {
		return o, errors.New("breakout: options.parent is required: the config name of the device whose cage this is")
	}
	n, err := strconv.Atoi(strings.TrimSpace(opts["cage"]))
	if err != nil || n < 1 {
		return o, fmt.Errorf("breakout: options.cage %q must be the parent's port index of the cage (1 or more)", opts["cage"])
	}
	o.Cage = n
	if m := strings.TrimSpace(opts["mac"]); m != "" {
		if o.MAC, err = normMAC(m); err != nil {
			return o, fmt.Errorf("breakout: options.mac: %w", err)
		}
	}
	if v := strings.TrimSpace(opts["max_age"]); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return o, fmt.Errorf("breakout: options.max_age %q is not a positive duration", v)
		}
		o.MaxAge = d
	}
	return o, nil
}

// Open finds the parent among this process's opened devices.
func (Driver) Open(_ context.Context, cfg devicemodel.DriverConfig) (devicemodel.Device, error) {
	o, err := ParseOptions(cfg.Options)
	if err != nil {
		return nil, err
	}
	pd, ok := devicemodel.OpenedDevice(o.Parent)
	if !ok {
		return nil, fmt.Errorf("breakout: parent %q is not open (yet): it must be an entry of the same config file, and it starts first", o.Parent)
	}
	lp, ok := pd.(devicemodel.LaneProvider)
	if !ok {
		return nil, fmt.Errorf("breakout: parent %q (%T) cannot present lane switches", o.Parent, pd)
	}
	return New(o, pd, lp), nil
}

// New is the lane switch over a parent (tests pass their own).
func New(o Options, parent devicemodel.Device, lp devicemodel.LaneProvider) *Device {
	return &Device{opt: o, parent: parent, lp: lp, Log: log.Default(), noted: map[string]bool{}}
}

// Device is an open lane switch.
type Device struct {
	opt    Options
	parent devicemodel.Device
	lp     devicemodel.LaneProvider
	Log    *log.Logger
	// Now is the clock (tests set it); nil = time.Now.
	Now func() time.Time

	mu    sync.Mutex
	view  devicemodel.LaneView // the last one Collect saw
	noted map[string]bool
}

func (d *Device) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// noteOnce logs a message the first time it is seen.
func (d *Device) noteOnce(msg string) {
	d.mu.Lock()
	seen := d.noted[msg]
	d.noted[msg] = true
	d.mu.Unlock()
	if !seen {
		d.Log.Printf("breakout %s cage %d: %s", d.opt.Parent, d.opt.Cage, msg)
	}
}

// Start takes the first snapshot: an error while the cage is whole, which
// main's start retry keeps retrying, so a lane switch declared before its
// cage is split comes up when it is.
func (d *Device) Start(ctx context.Context) (*devicemodel.Snapshot, error) {
	snap, err := d.Collect(ctx)
	if err != nil {
		return nil, err
	}
	d.Log.Printf("breakout: %s cage %d (%s) as %s, %s, %d lanes", d.opt.Parent, d.opt.Cage, d.view.Mode, snap.System.MAC, snap.System.Hostname, len(d.view.Lanes))
	return snap, nil
}

// Collect is the parent's last snapshot of the cage, never a device read.
func (d *Device) Collect(context.Context) (*devicemodel.Snapshot, error) {
	v, err := d.lp.Lanes(d.opt.Cage)
	if err != nil {
		return nil, fmt.Errorf("breakout: %s: %w", d.opt.Parent, err)
	}
	self, err := d.identity(v)
	if err != nil {
		return nil, err
	}
	if v.Child.MAC != "" && v.Child.MAC != self.MAC {
		return nil, fmt.Errorf("breakout: cage %d of %s is attached to another lane switch (%s); this one is %s", d.opt.Cage, d.opt.Parent, v.Child.MAC, self.MAC)
	}
	if age := d.now().Sub(v.TakenAt); age > d.opt.MaxAge {
		return nil, fmt.Errorf("breakout: %s's last snapshot is %s old (max_age %s): the parent is not answering", d.opt.Parent, age.Round(time.Second), d.opt.MaxAge)
	}
	if len(v.Lanes) > maxLanes {
		return nil, fmt.Errorf("breakout: cage %d of %s has %d lanes; a lane switch has %d", d.opt.Cage, d.opt.Parent, len(v.Lanes), maxLanes)
	}
	// Unattached, the snapshot still carries this lane switch's own identity
	// (Snapshot reads it from the view's Child); d.view keeps the parent's
	// answer, which is how attached() tells the two apart.
	d.mu.Lock()
	d.view = v
	d.mu.Unlock()
	v.Child = self
	return Snapshot(v), nil
}

// identity is this lane switch's MAC and name: options.mac, or the MAC
// derived from the parent's as main derives it; options.name, or
// "<parent>-cage<N>".
func (d *Device) identity(v devicemodel.LaneView) (devicemodel.Attachment, error) {
	a := devicemodel.Attachment{MAC: d.opt.MAC, Name: d.opt.Name}
	if a.MAC == "" {
		m, err := devicemodel.LaneSwitchMAC(v.System.MAC, d.opt.Cage)
		if err != nil {
			return a, fmt.Errorf("breakout: cage %d of %s: %w", d.opt.Cage, d.opt.Parent, err)
		}
		a.MAC = m
	}
	if a.Name == "" {
		a.Name = fmt.Sprintf("%s-cage%d", d.opt.Parent, d.opt.Cage)
	}
	return a, nil
}

// attached reports whether the parent has this lane switch on the cage now
// (asked of the parent, not of the last collect: an attach or a detach
// takes effect at once). Unattached, the lanes are the parent's cage's, and
// nothing a push says may be written to them.
func (d *Device) attached() error {
	v, err := d.lp.Lanes(d.opt.Cage)
	if err != nil {
		return fmt.Errorf("breakout: %s: %w", d.opt.Parent, err)
	}
	self, err := d.identity(v)
	if err != nil {
		return err
	}
	if v.Child.MAC != self.MAC {
		return fmt.Errorf("breakout: cage %d of %s is not attached to this lane switch (%s): the lanes are still the cage's on %s, and nothing is written to them until the controller has adopted the lane switch", d.opt.Cage, d.opt.Parent, self.MAC, d.opt.Parent)
	}
	return nil
}

// Close has nothing of its own to release; the parent is the parent's.
func (d *Device) Close() error { return nil }

// lanes is the lane count of the last view.
func (d *Device) lanes() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.view.Lanes)
}

// split separates the lanes' intent from the rest: an empty slot's is
// dropped, the uplink's is logged when it asks for anything (a real
// switch's uplink is mostly untouchable in the UI; the Flex XG's uplink
// drawer offers 100 GbE and FEC only, docs/breakout-lag.md 6.6), and a
// mirror whose source is not a lane of this cage is refused.
func (d *Device) split(desired []devicemodel.PortDesired) []devicemodel.PortDesired {
	n := d.lanes()
	var out []devicemodel.PortDesired
	for _, p := range desired {
		switch {
		case p.Index >= 1 && p.Index <= n:
			if p.MirrorSource > 0 && (p.MirrorSource > n || p.MirrorSource == p.Index) {
				d.noteOnce(fmt.Sprintf("lane %d: mirror source port %d is not another lane of this cage; not mirrored", p.Index, p.MirrorSource))
				p.MirrorSource = 0
			}
			out = append(out, p)
		case p.Index == UplinkPort:
			if why := uplinkAsks(p); why != "" {
				d.noteOnce(fmt.Sprintf("port %d is the breakout cable; its settings (%s) are not applied -- the cage's own are the parent's", UplinkPort, why))
			}
		}
	}
	return out
}

// uplinkAsks lists what a push sets on the uplink beyond the defaults.
func uplinkAsks(p devicemodel.PortDesired) string {
	var w []string
	if !p.Enabled {
		w = append(w, "disabled")
	}
	if p.Description != "" {
		w = append(w, "name "+strconv.Quote(p.Description))
	}
	if p.LAG > 0 {
		w = append(w, "aggregate")
	}
	if p.MirrorSource > 0 {
		w = append(w, "mirror")
	}
	if p.StormCtrl != nil {
		w = append(w, "storm control")
	}
	return strings.Join(w, ", ")
}

// ApplyPorts implements devicemodel.Controller through the parent.
func (d *Device) ApplyPorts(ctx context.Context, desired []devicemodel.PortDesired) (int, error) {
	if err := d.attached(); err != nil {
		return 0, err
	}
	return d.lp.ApplyLanes(ctx, d.opt.Cage, d.split(desired))
}

// PlanPorts implements devicemodel.Planner through the parent.
// Unattached it plans nothing and says why once: ApplyPorts then refuses.
func (d *Device) PlanPorts(desired []devicemodel.PortDesired) []int {
	if err := d.attached(); err != nil {
		d.noteOnce(fmt.Sprintf("a push arrived while unattached; not planned (%v)", err))
		return nil
	}
	return d.lp.PlanLanes(d.opt.Cage, d.split(desired))
}

// EnsureVLANs implements devicemodel.VLANController: the VLANs are the
// parent's to create. The lane switch's push names only what its lanes
// carry, which can differ from the parent's own push, so a parent that
// keeps the lists apart (devicemodel.LaneVLANController) gets it per cage.
func (d *Device) EnsureVLANs(ctx context.Context, ids []int) (int, error) {
	if err := d.attached(); err != nil {
		return 0, err
	}
	if lc, ok := d.parent.(devicemodel.LaneVLANController); ok {
		return lc.EnsureLaneVLANs(ctx, d.opt.Cage, ids)
	}
	if vc, ok := d.parent.(devicemodel.VLANController); ok {
		return vc.EnsureVLANs(ctx, ids)
	}
	return 0, nil
}

// SwitchesJumbo: the lanes take the lane switch's MTU where the parent can
// set one.
func (d *Device) SwitchesJumbo() bool {
	js, ok := d.parent.(devicemodel.JumboSwitcher)
	return ok && js.SwitchesJumbo()
}

// ApplyTimeout is the parent's write budget: the writes are the parent's.
func (d *Device) ApplyTimeout() time.Duration {
	if b, ok := d.parent.(devicemodel.ApplyBudget); ok {
		return b.ApplyTimeout()
	}
	return 0
}

// CurrentAddress implements devicemodel.AddressReporter: the parent's, so
// an address move on the parent is followed (the lane switch has no
// address of its own; a shared one was fine, docs/breakout-lag.md 6.6).
func (d *Device) CurrentAddress() string {
	if ar, ok := d.parent.(devicemodel.AddressReporter); ok {
		if a := ar.CurrentAddress(); a != "" {
			return a
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.view.System.Addresses) > 0 {
		return d.view.System.Addresses[0].IP
	}
	return ""
}

// Capabilities are the parent's per-port claims; the switch-wide ones (STP
// mode and priority, IGMP, DHCP snooping, SNMP, ACLs, routing) are the
// parent's business, and so are the STP port settings and the other
// switch-wide computed port features, which the parent applies to the
// lanes from its own cage (docs/drivers/breakout.md).
func (d *Device) Capabilities() devicemodel.Capabilities {
	var pc devicemodel.Capabilities
	if c, ok := d.parent.(devicemodel.Capable); ok {
		pc = c.Capabilities()
	}
	return devicemodel.Capabilities{
		LACP: pc.LACP, FEC: pc.FEC, Jumbo: pc.Jumbo, StormControl: pc.StormControl,
		AggregateSessions: min(pc.AggregateSessions, maxLanes/2),
		MirrorSessions:    min(pc.MirrorSessions, 1),
	}
}

// DeviceName is "<parent hostname> <cage label>" ("core-1 Eth2").
func (d *Device) DeviceName(devicemodel.System) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	cage := d.view.CageAlias
	if cage == "" {
		cage = d.view.Cage.IfName
	}
	return strings.TrimSpace(d.view.System.Hostname + " " + cage)
}

// DefaultDeviceNames: the generic "<Vendor> <Model>" form.
func (d *Device) DefaultDeviceNames(sys devicemodel.System) []string {
	return []string{devicemodel.DefaultNamer{}.DeviceName(sys)}
}

// PortName is the lane's description, else its faceplate label ("Eth2/1"),
// else its interface name; the uplink is the cage's label.
func (d *Device) PortName(p devicemodel.Port) string {
	if p.Description != "" {
		return p.Description
	}
	if a := d.alias(p.Index); a != "" {
		return a
	}
	return p.IfName
}

// DefaultPortNames: the label and the interface name.
func (d *Device) DefaultPortNames(p devicemodel.Port) []string {
	var out []string
	for _, s := range []string{d.alias(p.Index), p.IfName} {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func (d *Device) alias(idx int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case idx == UplinkPort:
		return d.view.CageAlias
	case idx >= 1 && idx <= len(d.view.LaneAliases):
		return d.view.LaneAliases[idx-1]
	}
	return ""
}

// Compile-time checks.
var (
	_ devicemodel.Driver          = Driver{}
	_ devicemodel.Device          = (*Device)(nil)
	_ devicemodel.Controller      = (*Device)(nil)
	_ devicemodel.Planner         = (*Device)(nil)
	_ devicemodel.VLANController  = (*Device)(nil)
	_ devicemodel.JumboSwitcher   = (*Device)(nil)
	_ devicemodel.AddressReporter = (*Device)(nil)
	_ devicemodel.Capable         = (*Device)(nil)
	_ devicemodel.Namer           = (*Device)(nil)
	_ devicemodel.ApplyBudget     = (*Device)(nil)
)
