// Package stub is an instrument for controller and UI experiments, not a
// device: Collect reports a snapshot described in a file (options.snapshot),
// re-read every cycle, so a port's state, speed or media can be changed by
// editing the file while the bridge runs. It writes nothing anywhere -- no
// Controller, no DeviceController -- so whatever the controller pushes is
// only logged and recorded in the reply log, as on every device.
// See docs/drivers/stub.md.
package stub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/reach"
)

// Driver is the stub driver.
//
// Config: url = the address the device is reported at (nothing is dialled;
// a literal or a name, resolved each cycle as apc-ups does); auth: none.
// Options:
//
//	snapshot     the file describing the device (YAML or JSON; the format
//	             is File below, the reference is docs/fixtures/stub/)
//	mac          the device's MAC (required: the stub has none of its own)
//	netmask      the network mask reported with the address, dotted
//	gateway_mac  the gateway's L2 address as the segment sees it
//
// mac, netmask and gateway_mac are taken exactly as apc-ups takes them
// (internal/drivers/reach).
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "stub" }

func (Driver) Describe() string {
	return "a device described by a file (options.snapshot), for controller and UI experiments; writes nothing"
}

// File is the snapshot file's format.
type File struct {
	Hostname string `yaml:"hostname"`
	Serial   string `yaml:"serial"`
	Version  string `yaml:"version"`
	Vendor   string `yaml:"vendor"` // default "disunified"
	Model    string `yaml:"model"`  // the vendor model string; default "stub"
	// Capabilities are what the controller is told the device honours. A
	// real driver's claims must be true; the stub's are the experiment's
	// (the UI offers what is claimed, and the push it sends is logged).
	// Read once, at start: the claims go out with the device's identity.
	Capabilities Caps       `yaml:"capabilities"`
	Ports        []FilePort `yaml:"ports"`
}

// Caps is the subset of devicemodel.Capabilities a file can claim.
type Caps struct {
	STP               bool `yaml:"stp"`
	BPDUGuard         bool `yaml:"bpdu_guard"`
	LoopProtection    bool `yaml:"loop_protection"`
	Jumbo             bool `yaml:"jumbo"`
	FEC               bool `yaml:"fec"`
	LACP              bool `yaml:"lacp"`
	StormControl      bool `yaml:"storm_control"`
	IGMPSnooping      bool `yaml:"igmp_snooping"`
	MirrorSessions    int  `yaml:"mirror_sessions"`
	AggregateSessions int  `yaml:"aggregate_sessions"`
}

// FilePort is one port. Speeds are in Mb/s; media is one of the
// devicemodel.Media names ("SFP28", "QSFP28", "10G-T", ...); fec one of
// "rs-fec", "fc-fec", "disabled".
type FilePort struct {
	Index        int       `yaml:"index"`
	IfName       string    `yaml:"ifname"` // default "Port N"
	Up           bool      `yaml:"up"`
	Enabled      bool      `yaml:"enabled"`
	Speed        int       `yaml:"speed"`
	FullDuplex   bool      `yaml:"full_duplex"`
	Media        string    `yaml:"media"`
	SpeedCaps    []int     `yaml:"speed_caps"`
	FEC          string    `yaml:"fec"`
	OpticPresent bool      `yaml:"optic_present"`
	IsUplink     bool      `yaml:"is_uplink"`
	MTU          int       `yaml:"mtu"` // default 1500
	Neighbor     *FileLLDP `yaml:"neighbor"`
}

// FileLLDP is a port's LLDP neighbour, when the experiment wants one.
type FileLLDP struct {
	ChassisID  string `yaml:"chassis_id"`
	SystemName string `yaml:"system_name"`
	PortID     string `yaml:"port_id"`
	Bridge     bool   `yaml:"bridge"`
	Router     bool   `yaml:"router"`
}

var media = map[string]devicemodel.Media{}

func init() {
	for _, m := range []devicemodel.Media{
		devicemodel.MediaCopper1G, devicemodel.MediaCopper2G5, devicemodel.MediaCopper10G,
		devicemodel.MediaSFP, devicemodel.MediaSFPPlus, devicemodel.MediaSFP28,
		devicemodel.MediaQSFPPlus, devicemodel.MediaQSFP28,
	} {
		media[string(m)] = m
	}
}

// Open checks the options and reads the file once; Start reads it again.
func (Driver) Open(_ context.Context, cfg devicemodel.DriverConfig) (devicemodel.Device, error) {
	path := strings.TrimSpace(cfg.Options["snapshot"])
	if path == "" {
		return nil, errors.New("stub: set options.snapshot: the file describing the device")
	}
	d := &Device{Path: path}
	d.Driver = "stub"
	d.SetOptions(strings.TrimSpace(cfg.URL), cfg.Options)
	if hw, err := net.ParseMAC(d.MAC); err != nil || len(hw) != 6 {
		return nil, fmt.Errorf("stub: options.mac %q is not a 6-byte MAC (the stub has no MAC of its own)", d.MAC)
	}
	f, err := Read(path)
	if err != nil {
		return nil, err
	}
	d.caps = f.Capabilities
	return d, nil
}

// Device is an open stub.
type Device struct {
	Path string
	reach.Identity

	caps    Caps
	started time.Time
	mu      sync.Mutex
	sig     string // the file's last port layout, to log a change once
}

// Read parses a snapshot file and checks it.
func Read(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("stub: %w", err)
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("stub: %s: %w", path, err)
	}
	if len(f.Ports) == 0 {
		return nil, fmt.Errorf("stub: %s: no ports", path)
	}
	seen := map[int]bool{}
	uplinks := 0
	for _, p := range f.Ports {
		if p.Index <= 0 || seen[p.Index] {
			return nil, fmt.Errorf("stub: %s: port index %d missing, zero or repeated", path, p.Index)
		}
		seen[p.Index] = true
		if _, ok := media[p.Media]; !ok && p.Media != "" {
			return nil, fmt.Errorf("stub: %s: port %d: unknown media %q", path, p.Index, p.Media)
		}
		switch devicemodel.FEC(p.FEC) {
		case devicemodel.FECUnknown, devicemodel.FECDisabled, devicemodel.FECRS, devicemodel.FECFC:
		default:
			return nil, fmt.Errorf("stub: %s: port %d: unknown fec %q", path, p.Index, p.FEC)
		}
		if p.IsUplink {
			uplinks++
		}
	}
	if uplinks > 1 {
		return nil, fmt.Errorf("stub: %s: more than one port is_uplink", path)
	}
	return &f, nil
}

// Start reads the file and reports it.
func (d *Device) Start(ctx context.Context) (*devicemodel.Snapshot, error) {
	d.started = time.Now()
	snap, err := d.Collect(ctx)
	if err != nil {
		return nil, err
	}
	log.Printf("stub: %s %s at %s, %d ports from %s (writes nothing)", snap.System.Hostname, snap.System.MAC, d.Addr, len(snap.Ports), d.Path)
	return snap, nil
}

// Collect re-reads the file, so an edit shows on the next inform.
func (d *Device) Collect(ctx context.Context) (*devicemodel.Snapshot, error) {
	f, err := Read(d.Path)
	if err != nil {
		return nil, err
	}
	snap := &devicemodel.Snapshot{TakenAt: time.Now()}
	snap.System = devicemodel.System{
		Vendor: f.Vendor, Model: f.Model, Serial: f.Serial, Version: f.Version, Hostname: f.Hostname,
	}
	if snap.System.Vendor == "" {
		snap.System.Vendor = "disunified"
	}
	if snap.System.Model == "" {
		snap.System.Model = "stub"
	}
	if !d.started.IsZero() {
		snap.System.Uptime = time.Since(d.started).Round(time.Second)
	}
	d.Fill(ctx, &snap.System)
	var layout []string
	for _, fp := range f.Ports {
		p := devicemodel.Port{
			Index: fp.Index, IfName: fp.IfName, Media: media[fp.Media], Lanes: 1,
			Present: fp.OpticPresent, Enabled: fp.Enabled, Up: fp.Up, SpeedMbps: fp.Speed,
			FullDuplex: fp.FullDuplex, MTU: fp.MTU, SpeedCaps: fp.SpeedCaps,
			FEC: devicemodel.FEC(fp.FEC), FECConfig: devicemodel.FEC(fp.FEC), FECCapable: fp.FEC != "",
			VLAN: devicemodel.PortVLAN{Mode: "access", NativeVLAN: 1},
		}
		if p.IfName == "" {
			p.IfName = fmt.Sprintf("Port %d", fp.Index)
		}
		p.Name = p.IfName
		if p.MTU == 0 {
			p.MTU = 1500
		}
		if !p.Up {
			p.SpeedMbps = 0
		}
		if n := fp.Neighbor; n != nil {
			p.Neighbor = &devicemodel.Neighbor{ChassisID: strings.ToLower(n.ChassisID), SystemName: n.SystemName, PortID: n.PortID, IsBridge: n.Bridge, IsRouter: n.Router}
		}
		if fp.IsUplink {
			snap.UplinkHint = fp.Index
		}
		snap.Ports = append(snap.Ports, p)
		layout = append(layout, fmt.Sprintf("%d:%s/%v/%d", p.Index, p.Media, p.Up, fp.Speed))
	}
	sort.Slice(snap.Ports, func(i, j int) bool { return snap.Ports[i].Index < snap.Ports[j].Index })
	sig := strings.Join(layout, " ")
	d.mu.Lock()
	if d.sig != "" && d.sig != sig {
		log.Printf("stub: %s changed: %s", d.Path, sig)
	}
	d.sig = sig
	d.mu.Unlock()
	return snap, nil
}

// Close has nothing to release.
func (d *Device) Close() error { return nil }

// Capabilities are the file's claims, as read at start.
func (d *Device) Capabilities() devicemodel.Capabilities {
	c := d.caps
	return devicemodel.Capabilities{
		STP: c.STP, BPDUGuard: c.BPDUGuard, LoopProtection: c.LoopProtection, Jumbo: c.Jumbo,
		FEC: c.FEC, LACP: c.LACP, StormControl: c.StormControl, IGMPSnooping: c.IGMPSnooping,
		MirrorSessions: c.MirrorSessions, AggregateSessions: c.AggregateSessions,
	}
}

// Compile-time checks: a collect-only device. Deliberately nothing else.
var (
	_ devicemodel.Driver  = Driver{}
	_ devicemodel.Device  = (*Device)(nil)
	_ devicemodel.Capable = (*Device)(nil)
)
