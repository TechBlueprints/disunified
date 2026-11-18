// Command disunified presents a non-UniFi device to a UniFi Network
// controller as if it were UniFi hardware. Today every driver presents a
// switch, but the bridge itself is device-type neutral.
//
// A driver (-driver) reads the device and, when writes are enabled, applies
// the controller's port and device-wide settings to it. The bridge claims a
// shipping UniFi model (-model, or "auto" to pick by port layout), informs
// the controller at the interval it asks for, persists the adoption state
// to -state, and records every controller reply to -record-dir as NDJSON.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TechBlueprints/disunified/internal/config"
	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/informloop"
	"github.com/TechBlueprints/disunified/internal/logredact"
	"github.com/TechBlueprints/disunified/internal/nutd"
	"github.com/TechBlueprints/disunified/internal/unifiapi"
	"github.com/TechBlueprints/disunified/internal/unificfg"
	"github.com/TechBlueprints/disunified/internal/unifimodel"
	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/inform"

	// Drivers register themselves; add a blank import per driver.
	_ "github.com/TechBlueprints/disunified/internal/drivers/apc-backups"
	_ "github.com/TechBlueprints/disunified/internal/drivers/apc-pdu"
	_ "github.com/TechBlueprints/disunified/internal/drivers/apc-ups"
	_ "github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
	_ "github.com/TechBlueprints/disunified/internal/drivers/breakout"
	_ "github.com/TechBlueprints/disunified/internal/drivers/podman"
	_ "github.com/TechBlueprints/disunified/internal/drivers/proxmox"
	_ "github.com/TechBlueprints/disunified/internal/drivers/sonic"
	_ "github.com/TechBlueprints/disunified/internal/drivers/stub"
)

// buildVersion is this bridge's own version, stamped by the release build
// (-ldflags "-X main.buildVersion=v1.2.3"); a plain `go build` leaves "dev".
// It is not the firmware version reported to the controller (-version).
var buildVersion = "dev"

func main() {
	var (
		configPath = flag.String("config", envChain("", "DUI_CONFIG", "STU_CONFIG"), "YAML config file (deploy/config.example.yaml): one controller, any number of devices; the flags below then apply only as one-shots")

		// Controller side
		controller = flag.String("controller", envChain("", "DUI_CONTROLLER", "STU_CONTROLLER"), "controller host or IP; the inform URL becomes http://<ip>:8080/inform")
		informURL  = flag.String("inform", envChain("", "DUI_INFORM_URL", "STU_INFORM_URL"), "full inform URL (overrides -controller); must contain an IP literal")
		model      = flag.String("model", envChain("auto", "DUI_MODEL", "STU_MODEL"), "UniFi model string to claim, or \"auto\" to pick the best catalogue match for the device's port layout")
		udapiVer   = flag.String("udapi-version", envChain("1.0.0", "DUI_UDAPI_VERSION", "STU_UDAPI_VERSION"), "UDAPI schema version to report; the controller only stores capability claims when this is set (\"\" = omit)")
		interval   = flag.Duration("interval", envDuration(10*time.Second, "DUI_INTERVAL", "STU_INTERVAL"), "initial inform interval; the controller then sets its own")
		recordDir  = flag.String("record-dir", envChain("inform-log", "DUI_RECORD_DIR", "STU_RECORD_DIR"), "directory for the NDJSON reply log (\"\" disables)")
		stateFile  = flag.String("state", envChain("state/device.json", "DUI_STATE", "STU_STATE"), "file holding the adopted key and provisioned config across restarts")

		// Identity (defaults come from the device when a driver is configured)
		mac      = flag.String("mac", envChain("", "DUI_MAC", "STU_MAC"), "device MAC to present (default: the device's system MAC)")
		serial   = flag.String("serial", envChain("", "DUI_SERIAL", "STU_SERIAL"), "device serial (default: the device's serial)")
		ip       = flag.String("ip", envChain("", "DUI_IP", "STU_IP"), "device IP to report (default: the device address from -device-url/-device-ssh when it is an IP literal)")
		hostname = flag.String("hostname", envChain("", "DUI_HOSTNAME", "STU_HOSTNAME"), "device hostname to report (default: the device's hostname)")
		version  = flag.String("version", envChain("", "DUI_VERSION", "STU_VERSION"), "firmware version to report, digits and dots such as 7.2.123.16565 (default: the model's UniFi firmware version; for a device claimed as the USW Leaf, the device's own reduced to digits and dots: EOS 4.26.14M goes out as 4.26.14)")
		uplink   = flag.Int("uplink-port", envInt(0, "DUI_UPLINK_PORT", "STU_UPLINK_PORT"), "port_idx to flag as the uplink (0 = the port whose LLDP neighbour is the upstream switch)")

		// Device side. -device-url/-device-ssh are the current names;
		// -switch-url/-switch-ssh are the pre-rename aliases, reconciled after
		// Parse (see below). The env default feeds the -device-* flags.
		driverName     = flag.String("driver", envChain(os.Getenv("STU_DRIVER"), "DUI_DRIVER"), "device driver, required with -device-url/-device-ssh (see -list-drivers)")
		deviceURLFlag  = flag.String("device-url", envChain("", "DUI_DEVICE_URL", "STU_SWITCH_URL", "STU_EOS_URL"), "device API endpoint (driver-specific), with DUI_DEVICE_USER/DUI_DEVICE_PASS")
		deviceSSHFlag  = flag.String("device-ssh", envChain("", "DUI_DEVICE_SSH", "STU_SWITCH_SSH", "STU_EOS_SSH"), "device SSH target user@host[:port] (key auth), used when -device-url is empty")
		deviceSSHKey   = flag.String("device-ssh-key", envChain("", "DUI_DEVICE_SSH_KEY"), "-device-ssh's key, as the config file's options.ssh_key: a private key file, or a .pub that selects that one ssh-agent identity (default: every agent key, one at a time)")
		switchURLFlag  = flag.String("switch-url", "", "pre-rename alias for -device-url")
		switchSSHFlag  = flag.String("switch-ssh", "", "pre-rename alias for -device-ssh")
		controlPorts   = flag.String("control-ports", envChain("", "DUI_CONTROL_PORTS", "STU_CONTROL_PORTS"), "write controller port config to the device: \"all\" (the default when the driver can), \"off\", or a list like \"2,5-8\"")
		controlOutlets = flag.String("control-outlets", envChain("", "DUI_CONTROL_OUTLETS", "STU_CONTROL_OUTLETS"), "switch and name a power device's outlets from the controller: \"all\" (the default when the driver can), \"off\", or a list like \"2,5-8\"")
		controlNUT     = flag.Bool("control-nut", envChain("", "DUI_CONTROL_NUT") == "1", "power devices: claim the NUT-server capability so the controller's NUT Server switch runs a NUT server in the bridge (off by default)")

		// Controller REST API (names)
		unifiURL  = flag.String("unifi-url", envChain("", "DUI_UNIFI_URL", "STU_UNIFI_URL"), "controller URL for the REST API, e.g. https://unifi.example.net (needs STU_UNIFI_API_KEY)")
		unifiSite = flag.String("unifi-site", envChain("default", "DUI_UNIFI_SITE", "STU_UNIFI_SITE"), "controller site")
		provision = flag.Bool("provision-names", envChain("1", "DUI_PROVISION_NAMES", "STU_PROVISION_NAMES") != "0", "name the device \"<vendor> <model>\" and ports after the device's interfaces via the REST API, touching only controller-default names (needs -unifi-url)")

		// One-shots
		listModels  = flag.Bool("list-models", false, "print the switch models the bundled catalogue knows and exit")
		listDrivers = flag.Bool("list-drivers", false, "print the registered device drivers and exit")
		showBuild   = flag.Bool("build-version", false, "print this bridge's own build version and exit (-version is the firmware version reported to the controller)")
		collectOnce = flag.Bool("collect-once", false, "collect one snapshot from the device (or, with -config, from each device in the file), print it as JSON with the suggested model, and exit; nothing is informed")
	)
	// Retired: UniFi owns the device's address (IP Settings), IGMP
	// snooping, NTP, the hostname, the syslog host, Restart, SSH keys and
	// SNMP, as it does on its own switches; a driver applies whatever of it
	// the device can take. The flags are accepted and ignored so existing
	// command lines keep working.
	for _, name := range []string{"control-address", "control-igmp", "control-ntp", "control-hostname", "control-syslog", "control-reboot", "control-ssh-keys", "control-snmp"} {
		flag.Bool(name, false, "ignored: the controller's setting applies whenever the driver can apply it")
	}
	flag.Parse()

	// Reconcile the pre-rename -switch-url/-switch-ssh aliases with the
	// current -device-url/-device-ssh. The -device-* form (and its env
	// default) wins; the alias is used only when -device-* is empty.
	deviceURL, deviceSSH := *deviceURLFlag, *deviceSSHFlag
	if deviceURL == "" {
		deviceURL = *switchURLFlag
	} else if *switchURLFlag != "" && *switchURLFlag != deviceURL {
		log.Printf("both -device-url and -switch-url set; using -device-url %q", deviceURL)
	}
	if deviceSSH == "" {
		deviceSSH = *switchSSHFlag
	} else if *switchSSHFlag != "" && *switchSSHFlag != deviceSSH {
		log.Printf("both -device-ssh and -switch-ssh set; using -device-ssh %q", deviceSSH)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	// Every log line passes the controller's pushed secrets through a mask
	// (internal/logredact; the loop hands it each push's values).
	log.SetOutput(logredact.Stderr)

	if *showBuild {
		fmt.Println(buildVersion)
		return
	}
	if *listDrivers {
		for _, d := range devicemodel.Drivers() {
			fmt.Printf("%-12s %s\n", d.Name(), d.Describe())
		}
		return
	}
	if *listModels {
		for _, m := range emu.Models() {
			if p, ok := emu.Profile(m); ok && (p.Type == "usw" || p.Type == "usp") {
				fmt.Printf("%-10s %-40s %d ports\n", m, p.ModelDisplay, len(p.Ports))
			}
		}
		return
	}

	log.Printf("disunified %s starting", buildVersion)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *configPath != "" {
		f, err := config.Load(*configPath)
		if err != nil {
			log.Fatal(err)
		}
		if *collectOnce {
			if err := collectConfig(ctx, f); err != nil {
				log.Fatal(err)
			}
			return
		}
		runConfig(ctx, f)
		return
	}

	o := options{
		controller: *controller, informURL: *informURL, model: *model, udapiVersion: *udapiVer,
		interval: *interval, recordDir: *recordDir, stateFile: *stateFile,
		mac: *mac, serial: *serial, ip: *ip, hostname: *hostname, version: *version, uplink: *uplink,
		driver: *driverName, deviceURL: deviceURL, deviceSSH: deviceSSH, driverOptions: flagDriverOptions(*deviceSSHKey),
		username:     envChain("", "DUI_DEVICE_USER", "STU_SWITCH_USER", "STU_EOS_USER"),
		password:     envChain("", "DUI_DEVICE_PASS", "STU_SWITCH_PASS", "STU_EOS_PASS"),
		controlPorts: *controlPorts, controlOutlets: *controlOutlets, controlNUT: *controlNUT,
		unifiURL: *unifiURL, unifiSite: *unifiSite, unifiKey: envChain("", "DUI_UNIFI_API_KEY", "STU_UNIFI_API_KEY"), provision: *provision,
		collectOnce: *collectOnce, logger: log.Default(),
	}
	if err := runOne(ctx, o); err != nil {
		log.Fatal(err)
	}
}

// flagDriverOptions is the driver options a device run from the flags
// carries: ssh_key from -device-ssh-key, read by every SSH driver as the
// config file's options.ssh_key is. nil when none is set.
func flagDriverOptions(sshKey string) map[string]string {
	if sshKey = strings.TrimSpace(sshKey); sshKey == "" {
		return nil
	}
	return map[string]string{"ssh_key": sshKey}
}

// options is everything one bridged device needs, from flags or a config entry.
type options struct {
	controller, informURL, model, udapiVersion string
	interval                                   time.Duration
	recordDir, stateFile                       string
	mac, serial, ip, hostname, version         string
	uplink                                     int
	driver, deviceURL, deviceSSH               string
	username, password                         string
	driverOptions                              map[string]string
	controlPorts                               string
	controlOutlets                             string
	controlNUT                                 bool
	allowInitialChanges, noSeed                bool
	applyTimeout                               time.Duration       // 0 = the loop's default
	fanMode                                    devicemodel.FanMode // config fan_mode; "" = leave the fans alone
	recordMaxSize                              int64               // 0 = the loop's default (8 MB), -1 = no cap
	recordKeep                                 int                 // 0 = the loop's default (3)
	unifiURL, unifiSite, unifiKey              string
	provision                                  bool
	collectOnce                                bool
	logger                                     *log.Logger
	stateDir                                   string
	name                                       string // the config entry's name; "" from flags
	children                                   []informloop.Child
	laneSwitches                               []config.LaneSwitch // lane switches on this device's cages (attached at start)
	laneDriver                                 bool                // a lane switch: no transport, opened through its parent
	// autoLanes builds this device's automatic lane switches' watcher once
	// the device is open (nil: none, lanes.go); onAdopted and onUnadopted
	// are an automatic lane switch's own hand-over hooks.
	autoLanes              func(*log.Logger, devicemodel.LaneProvider, string) *autoLanes
	onAdopted, onUnadopted func()
	// halt ends a lane switch's run (a retire, lanes_retire.go); exited is
	// closed when runOne has returned.
	halt, exited chan struct{}
	// declared are the file's declared lane switches' hand-over hooks by
	// name (lanes.go): a parent binds those it attaches, and a declared
	// lane switch's onAdopted and onUnadopted are its own entry's.
	declared map[string]*declaredLane
}

// runConfig runs every switch in the file concurrently; a switch that fails
// to start is logged and the others keep running.
func runConfig(ctx context.Context, f *config.File) {
	var wg sync.WaitGroup
	spawn := func(o options) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rctx := ctx
			if o.halt != nil {
				var cancel context.CancelFunc
				rctx, cancel = context.WithCancel(ctx)
				defer cancel()
				go func() {
					select {
					case <-o.halt:
						cancel()
					case <-rctx.Done():
					}
				}()
			}
			err := runOne(rctx, o)
			halted := o.halt != nil && rctx.Err() != nil && ctx.Err() == nil
			switch {
			case halted:
				o.logger.Printf("stopped: retired (its cage is joined)")
			case err != nil:
				o.logger.Printf("FAILED: %v", err)
			}
			if o.exited != nil {
				close(o.exited)
			}
		}()
	}
	declared := declaredLanes(f)
	for _, sw := range f.Devices {
		o := configOptions(f, sw)
		// A split cage of this device gets a lane switch by itself
		// (lane_switches: auto, the default): started from its watcher,
		// through the same runOne a declared entry uses.
		o.autoLanes = newAutoLanes(f, sw, spawn)
		o.declared = declared
		if d := declared[sw.Name]; d != nil {
			o.onAdopted, o.onUnadopted = d.adopted, d.forgotten
			o.halt, o.exited = d.arm()
			base := o
			d.mu.Lock()
			d.respawn = func() {
				next := base
				next.halt, next.exited = d.arm()
				spawn(next)
			}
			d.mu.Unlock()
		}
		spawn(o)
	}
	wg.Wait()
}

// collectConfig is -collect-once with -config: each device in the file is
// opened and started as the service would (its options, its state_dir) and
// its snapshot printed, one after the other; nothing is informed and the
// adopted key is not read. The first device that fails stops the run.
func collectConfig(ctx context.Context, f *config.File) error {
	for _, sw := range f.Devices {
		o := configOptions(f, sw)
		o.collectOnce = true
		if err := runOne(ctx, o); err != nil {
			return fmt.Errorf("%s: %w", sw.Name, err)
		}
	}
	return nil
}

// configOptions is one config-file device as runOne takes it.
func configOptions(f *config.File, sw config.Device) options {
	logger := log.New(logredact.Stderr, "["+sw.Name+"] ", log.LstdFlags|log.Lmicroseconds)
	return options{
		controller: f.Controller.Host, informURL: f.Controller.InformURL,
		model: sw.Model, udapiVersion: sw.UDAPIVersion, interval: 10 * time.Second,
		recordDir: filepath.Join(f.LogRoot(), "inform-log", sw.Name),
		stateFile: filepath.Join(f.StateDir, "state", sw.Name, "device.json"),
		ip:        sw.IP, hostname: sw.Hostname, uplink: sw.UplinkPort,
		version: sw.Firmware, // the config file's -version; config.Load has checked its form
		driver:  sw.Driver, deviceURL: sw.URL, deviceSSH: sw.SSH, username: sw.Username, password: sw.Password,
		driverOptions: sw.Options,
		controlPorts:  sw.Control.Ports, controlOutlets: sw.Control.Outlets, controlNUT: sw.Control.NUT,
		allowInitialChanges: sw.Control.AllowInitialChanges, noSeed: sw.Control.NoSeed,
		applyTimeout:  parseApplyTimeout(sw.Control.ApplyTimeout),
		fanMode:       fanModeOf(sw.FanMode),
		recordMaxSize: sw.ReplyLog.Bytes(), recordKeep: sw.ReplyLog.Keep,
		unifiURL: f.Controller.APIURL, unifiSite: f.Controller.Site, unifiKey: f.Controller.APIKey(), provision: true,
		logger: logger, stateDir: filepath.Join(f.StateDir, "state", sw.Name), name: sw.Name,
		children:     childrenOf(sw.Children),
		laneSwitches: f.LaneSwitchesOf(sw.Name),
		laneDriver:   sw.Driver == config.LaneDriver,
	}
}

// stateDirOf is the device's state directory, where drivers and the loop
// keep their own files next to device.json: the config entry's, or the
// -state file's directory; "" for a -collect-once run from flags, which
// leaves nothing behind.
func stateDirOf(o options) string {
	if o.stateDir != "" {
		return o.stateDir
	}
	if o.stateFile != "" && !o.collectOnce {
		return filepath.Dir(o.stateFile)
	}
	return ""
}

// childrenOf is a config entry's children as the loop takes them.
func childrenOf(cs []config.Child) []informloop.Child {
	var out []informloop.Child
	for _, c := range cs {
		out = append(out, informloop.Child{Port: c.Port, MAC: c.MAC, Name: c.Name, Report: c.Report})
	}
	return out
}

// attachLaneSwitches declares the config's lane switches on their parent's
// cages (docs/drivers/breakout.md): each one's MAC is its options.mac or
// derived from the parent's, and the lane switch reads it back from the
// parent, so the two never disagree. hooks are the declared lane switches'
// hand-over hooks (nil for -collect-once): each one attached is bound to
// this device, and its cage's port goes to Auto in the controller if it
// holds a forced lane speed (declaredLane.attached).
func attachLaneSwitches(log *log.Logger, sw devicemodel.Device, snap *devicemodel.Snapshot, ls []config.LaneSwitch, hooks map[string]*declaredLane) {
	if len(ls) == 0 {
		return
	}
	lp, ok := sw.(devicemodel.LaneProvider)
	if !ok {
		log.Printf("lane switches %v: this driver cannot present a split cage as a lane switch; not attached", ls)
		return
	}
	for _, l := range ls {
		mac := l.MAC
		if mac == "" {
			m, err := devicemodel.LaneSwitchMAC(snap.System.MAC, l.Cage)
			if err != nil {
				log.Printf("lane switch %s on port %d: %v; not attached", l.Name, l.Cage, err)
				continue
			}
			mac = m
		}
		if err := lp.AttachLaneSwitch(l.Cage, devicemodel.Attachment{MAC: mac, Name: l.Name}); err != nil {
			log.Printf("lane switch %s on port %d: %v; not attached", l.Name, l.Cage, err)
			continue
		}
		log.Printf("lane switch %s attached to port %d as %s (reported in its MAC table and as its LLDP neighbour while the cage is split)", l.Name, l.Cage, mac)
		if d := hooks[l.Name]; d != nil {
			d.attached(log, snap.System.MAC, mac)
		}
	}
}

// openAndStart opens a driver and takes its first snapshot, closing the
// device again if Start fails so a retry begins from a clean connection.
// prepare, when set, runs between the two (a site-network lookup handed to
// a device that takes one).
func openAndStart(ctx context.Context, drv devicemodel.Driver, cfg devicemodel.DriverConfig, prepare func(devicemodel.Device)) (devicemodel.Device, *devicemodel.Snapshot, error) {
	sw, err := drv.Open(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	if prepare != nil {
		prepare(sw)
	}
	started := time.Now()
	snap, err := sw.Start(ctx)
	if err != nil {
		_ = sw.Close()
		return nil, nil, err
	}
	_ = started
	return sw, snap, nil
}

// controlScope resolves control.ports / control.outlets: unset means "all"
// when the driver can write them, "off" means none, anything else is kept
// (a list such as "2,5-8" for trying a few ports first).
func controlScope(v string, writable bool) string {
	switch {
	case strings.EqualFold(v, "off"):
		return ""
	case v == "" && writable:
		return "all"
	}
	return v
}

// isA reports whether the device implements T (false for no device).
func isA[T any](sw devicemodel.Device) bool {
	if sw == nil {
		return false
	}
	_, ok := sw.(T)
	return ok
}

// runOne bridges one switch until ctx is cancelled.
// reportedIP is the address the informs report the device at from start:
// the operator's (`ip:`/-ip) when set; else the driver's own
// (devicemodel.AddressReporter: an address move the bridge applied earlier,
// or the address the device carries when it is dialled elsewhere -- the
// Podman host over a podman bridge network); else the dial target, or, for a
// target that is a name, the address it resolved to (the driver puts it
// first in Addresses), so a lease-backed DNS name is followed without an
// operator naming the address.
func reportedIP(explicit, deviceURL, deviceSSH string, sw devicemodel.Device, snap *devicemodel.Snapshot) string {
	if explicit != "" {
		return explicit
	}
	ip := hostOf(deviceURL, deviceSSH)
	if ar, ok := sw.(devicemodel.AddressReporter); ok {
		if a := ar.CurrentAddress(); net.ParseIP(a) != nil {
			ip = a
		}
	}
	if net.ParseIP(ip) == nil && len(snap.System.Addresses) > 0 {
		ip = snap.System.Addresses[0].IP
	}
	return ip
}

func runOne(ctx context.Context, o options) error {
	log := o.logger

	// --- Device ---
	var (
		sw   devicemodel.Device
		snap *devicemodel.Snapshot
	)
	if o.deviceURL != "" || o.deviceSSH != "" || o.laneDriver || config.LocalTransport(o.driverOptions) {
		if o.driver == "" {
			return errors.New("no driver named: set -driver / DUI_DRIVER, or driver: in the config file (see -list-drivers)")
		}
		drv, err := devicemodel.LookupDriver(o.driver)
		if err != nil {
			return err
		}
		opts := map[string]string{}
		for k, v := range o.driverOptions {
			opts[k] = v
		}
		// options.probe: an address this bridge must keep reaching, which a
		// driver with nothing to dial (sonic on the switch itself) proves
		// reachability against. The controller's inform host:port, unless
		// the device names its own.
		if opts["probe"] == "" {
			if u, err := informURLFor(o); err == nil {
				if p := probeAddr(u); p != "" {
					opts["probe"] = p
				}
			}
		}
		if o.laneDriver && opts["name"] == "" {
			opts["name"] = o.name // a lane switch reports its entry's name whether or not it is attached
		}
		if d := stateDirOf(o); d != "" {
			opts["state_dir"] = d // drivers with their own persistent state keep it next to device.json
		}
		cfg := devicemodel.DriverConfig{URL: o.deviceURL, SSH: o.deviceSSH, Username: o.username, Password: o.password, Options: opts}
		// A device that cannot report its own netmask and gateway (a UPS)
		// takes them from the controller's network list; its options
		// override (docs/drivers/apc-ups.md §14).
		var prepare func(devicemodel.Device)
		if o.unifiURL != "" && o.unifiKey != "" {
			lookup := cachedSiteNetwork(unifiapi.New(o.unifiURL, o.unifiKey, o.unifiSite, true), 5*time.Minute)
			prepare = func(d devicemodel.Device) {
				if u, ok := d.(devicemodel.SiteNetworkUser); ok {
					u.SetSiteNetworkLookup(lookup)
				}
			}
		}
		// A device that does not answer at startup is retried, not dropped.
		// Start is meant to fail loudly on a wrong command or credential, and
		// it still does -- every attempt is logged -- but the same failure
		// also comes from a card that is rebooting, a switch mid-upgrade or
		// a lease that moved, and dropping the device for those meant it
		// stayed gone until someone restarted the container (the PDU,
		// 2026-09-22: one SNMP timeout at start, then nothing for hours).
		// -collect-once keeps failing fast: it is a probe, not a service.
		for attempt, wait := 1, 15*time.Second; ; attempt++ {
			sw, snap, err = openAndStart(ctx, drv, cfg, prepare)
			if err == nil {
				break
			}
			if o.collectOnce {
				return err
			}
			log.Printf("start attempt %d failed, retrying in %s: %v", attempt, wait, err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			if wait < 5*time.Minute {
				wait *= 2
				if wait > 5*time.Minute {
					wait = 5 * time.Minute
				}
			}
		}
		defer sw.Close()
		if _, ok := sw.(devicemodel.SiteNetworkUser); ok && prepare == nil {
			log.Printf("reach: no controller API URL or key (DUI_UNIFI_API_KEY): the device's netmask and gateway come from its options only")
		}
		log.Printf("switch: %s %s serial %s, %s, %d ports", snap.System.Vendor, snap.System.Model, snap.System.Serial, snap.System.Version, len(snap.Ports))
		// Lane switches on this device's cages are attached before its loop
		// starts, so its first apply already leaves their lanes to them; a
		// lane switch finds this device by its name.
		hooks := o.declared
		if o.collectOnce {
			hooks = nil // a probe changes nothing in the controller
		}
		attachLaneSwitches(log, sw, snap, o.laneSwitches, hooks)
		if o.name != "" && !o.collectOnce {
			devicemodel.RegisterOpened(o.name, sw)
			defer devicemodel.UnregisterOpened(o.name, sw)
		}
		// The automatic ones (docs/drivers/breakout.md 16): an adopted one
		// is attached here, before this device's loop starts, for the same
		// reason; a split cage without one gets a pending one from the
		// watcher. Both find this device by its name, registered above.
		if lp, ok := sw.(devicemodel.LaneProvider); ok && o.autoLanes != nil && o.name != "" && !o.collectOnce {
			al := o.autoLanes(log, lp, snap.System.MAC)
			al.bindDeclared(o.laneSwitches, o.declared)
			al.startAdopted()
			go al.watch(ctx)
		}
	}

	if o.collectOnce {
		if snap == nil {
			return errors.New("-collect-once needs -device-url or -device-ssh (or, in a config file, options.transport: local)")
		}
		out := map[string]any{"snapshot": snap, "layout": unifimodel.LayoutOf(snap)}
		if o.name != "" {
			out["device"] = o.name
		}
		if c, err := unifimodel.SuggestFor(snap); err == nil {
			out["suggested_model"] = c
		} else {
			out["suggested_model_error"] = err.Error()
			out["closest"] = c
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	// Unset, the controller writes every port or outlet the driver can
	// write, as it does on its own devices; "off" writes none. A fresh
	// adoption is still safe: the controller is seeded from the device and
	// a first push that would change ports or outlets is held until it
	// matches.
	o.controlPorts = controlScope(o.controlPorts, isA[devicemodel.Controller](sw))
	o.controlOutlets = controlScope(o.controlOutlets, isA[devicemodel.OutletController](sw))

	// --- Identity ---
	// The serial is the MAC's hex digits, as on every UniFi device, unless
	// the operator names one, which must fit the controller's
	// (device.SerialFor). The device's own is only logged above.
	if len(o.serial) > device.MaxSerialLen {
		return fmt.Errorf("-serial %q is %d characters; the controller keeps at most %d", o.serial, len(o.serial), device.MaxSerialLen)
	}
	// The version goes on the wire as a real device's: digits and dots, at
	// most four groups and device.MaxVersionLen characters. A device's own
	// string is reduced to that; an operator's must already be it.
	if o.version != "" && !device.VersionFits(o.version) {
		return fmt.Errorf("-version %q is not a version the controller keeps: digits and dots, at most 4 groups and %d characters, such as 7.2.123.16565 (it would go out as %q)", o.version, device.MaxVersionLen, device.WireVersion(o.version))
	}
	if snap != nil {
		if o.mac == "" {
			o.mac = snap.System.MAC
		}
		if o.hostname == "" {
			o.hostname = snap.System.Hostname
		}
		o.ip = reportedIP(o.ip, o.deviceURL, o.deviceSSH, sw, snap)
		if o.uplink == 0 {
			o.uplink = snap.UplinkPort()
		}
	}
	if o.hostname == "" {
		o.hostname = "UBNT"
	}
	hw, err := net.ParseMAC(o.mac)
	if err != nil || len(hw) != 6 {
		return fmt.Errorf("-mac is required (or comes from the switch) and must be a 6-byte MAC (got %q)", o.mac)
	}
	macStr := hw.String()
	if net.ParseIP(o.ip) == nil {
		return fmt.Errorf("-ip is required and must be an IP literal (got %q)", o.ip)
	}
	// --- Model ---
	if o.model == "auto" {
		if snap == nil {
			return errors.New("-model auto needs a switch connection; pass -model explicitly otherwise")
		}
		c, err := unifimodel.SuggestFor(snap)
		if err != nil {
			return err
		}
		o.model = c.Model
		if len(snap.Outlets) > 0 {
			log.Printf("model: auto-selected %s (%s) for a power device with %d outlets", c.Model, c.Display, len(snap.Outlets))
		} else {
			log.Printf("model: auto-selected %s (%s) for switch layout %+v", c.Model, c.Display, unifimodel.LayoutOf(snap))
		}
		if c.Note != "" {
			log.Printf("model: %s", c.Note)
		}
	}
	profile, ok := emu.Profile(o.model)
	if !ok {
		return fmt.Errorf("unknown model %q (see -list-models)", o.model)
	}
	if profile.Type != "usw" && profile.Type != "usp" {
		return fmt.Errorf("model %q is a %q, not a switch or power device", o.model, profile.Type)
	}
	if snap != nil && len(profile.Ports) != len(snap.Ports) {
		log.Printf("WARNING: model %s has %d ports, the switch has %d; ports beyond the profile are not drawn", o.model, len(profile.Ports), len(snap.Ports))
	}

	// --- Controller URL ---
	url, err := informURLFor(o)
	if err != nil {
		return err
	}

	desc, err := device.DescriptorFor(profile.Model, snap, device.Identity{
		MAC: macStr, Serial: o.serial, IP: o.ip, Hostname: o.hostname, Version: o.version,
		UDAPIVersion: o.udapiVersion, UplinkPort: o.uplink,
	})
	if err != nil {
		return err
	}
	log.Printf("serial: reporting %s", desc.Serial)
	versionPinned := o.version != "" // the operator named one; else the switch's own
	o.version = desc.Version
	// Capability claims come from the driver; they gate the UI, so a
	// driver that declares none claims nothing.
	caps := device.DefaultCapabilities
	if c, ok := sw.(devicemodel.Capable); ok && sw != nil {
		caps = c.Capabilities()
	}
	desc.FWCaps = device.FWCapsFor(caps)

	// --- Session ---
	store := &device.Store{Path: o.stateFile}
	st, err := store.Load()
	if err != nil {
		return err
	}
	// The state file keeps the applied push unredacted (a restart applies it
	// again): its secrets are masked in the log before anything runs.
	logredact.Remember(unificfg.Secrets(st.SystemCfg)...)
	logredact.Remember(unificfg.Secrets(st.PendingSystemCfg)...)
	if st.Key != inform.DefaultKey {
		logredact.Remember(st.Key)
	}
	if st.Adopted {
		log.Printf("resuming adopted state from %s (cfgversion %s, gcm=%v)", o.stateFile, st.CfgVersion, st.UseAESGCM)
	}
	sess := device.NewSession(desc, url, st, store, time.Now())
	sess.SetCapabilities(caps)
	// Every UPS claims the NUT-server capability, whether or not
	// control.nut was set: the controller's UI bundles a nut_server block
	// into every save of a UPS's settings panel and rejects the save
	// (api.err.InvalidPayload / NutInformationAccessNotSupported) for a UPS
	// that has not claimed the bit -- even a rename fails (2026-09-27). The
	// claim only makes the UI's "NUT Server" switch real; the server itself
	// starts when that switch is on and stops when it is off.
	isUPS := snap != nil && snap.System.Battery != nil
	if o.controlNUT && !isUPS {
		return fmt.Errorf("control.nut: the device is not a UPS (no battery reported); the NUT server serves battery status")
	}
	if isUPS {
		sess.SetSmartPowerCaps(device.SmartPowerCapNUTInfo)
		log.Printf("control: NUT server capability claimed (every UPS claims it, or the UI cannot save the device's settings); the controller's NUT Server switch is honoured")
	}
	// The NUT server serves the session's latest snapshot; the controller's
	// nutserver block (via the loop) starts and stops it, and its logged-in
	// clients are reported back as nut_client_ips.
	var nut *nutd.Server
	if isUPS {
		nut = nutd.New(sess.Snapshot, log)
		sess.SetNUTClients(nut.Clients)
		defer nut.Close()
	}
	if versionPinned {
		sess.PinVersion()
	}
	if snap != nil {
		sess.SetSnapshot(snap)
	} else {
		log.Printf("no -device-url/-device-ssh: reporting the model's synthetic port table")
	}

	// --- Names: defaults, and provisioning through the REST API ---
	namer := devicemodel.Namer(devicemodel.DefaultNamer{})
	if sw != nil {
		namer = devicemodel.NamerFor(sw)
	}
	defaults := defaultPortNames(desc.Ports, snap, namer)
	// isDefaultPortNameIn answers against the snapshot in hand: the names a
	// port could have been given depend on what is on it now (a guest that
	// arrived after startup has its own labels), so the provisioner must
	// not use the startup-time table (live, 2026-09-20: a guest taking a
	// free slot was not re-seeded because its new name looked custom).
	isDefaultPortNameIn := func(defs map[int][]string) func(idx int, name string) bool {
		return func(idx int, name string) bool {
			for _, d := range defs[idx] {
				if name == d {
					return true
				}
			}
			return false
		}
	}
	// provision names the device and ports and, with control on, seeds the
	// controller's port config from the switch — one read-modify-write —
	// on the adoption handshake, on a layout change (a new guest) and while
	// a first push is held (a retry). Only controller-default names and
	// ports the controller has no config for are touched.
	// The spacing between provisions, the retry timer and a layout change
	// that waits for the spacing are provisionSpacer's (provision.go).
	var provision func(snap *devicemodel.Snapshot)
	// layoutChanged provisions after a layout change. One that falls inside
	// provision's 10 s spacing (the first collect of a run differing from
	// the startup snapshot) waits for the spacing to end instead of being
	// dropped: its ports are withheld until the controller is seeded.
	var layoutChanged func(snap *devicemodel.Snapshot)
	// upgradeConnected tells the upgrade asker (upgrade.go) of a connect.
	var upgradeConnected func()
	if o.provision && o.unifiURL != "" {
		key := o.unifiKey
		if key == "" {
			log.Printf("provision-names: no UNIFI API key set (DUI_UNIFI_API_KEY), skipping")
		} else {
			api := unifiapi.New(o.unifiURL, key, o.unifiSite, true)
			api.PowerPath = desc.Type == "usp"
			seed := !o.noSeed && o.controlPorts != ""
			var (
				// pendingNoted: the "listed as pending" line was logged; cleared
				// by the next provision that succeeds. It and retry are
				// touched only inside the action, under the spacer's lock.
				pendingNoted bool
				retry        apiRetry
			)
			spacer := newProvisionSpacer(sess.Adopted, sess.Snapshot, func(snap *devicemodel.Snapshot) time.Duration {
				pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
				r, err := api.Provision(pctx, macStr, snap, namer, append([]string{profile.ModelDisplay, "USW Leaf", profile.Model}, unifimodel.ControllerDisplayNames(profile.Model)...), isDefaultPortNameIn(defaultPortNames(desc.Ports, snap, namer)), seed)
				pcancel()
				for _, note := range r.Notes {
					log.Printf("provision: seed: %s", note)
				}
				if err != nil {
					// Not only on the next layout change or held push: a
					// controller that cannot be reached when the bridge
					// starts (no DNS yet early in a boot, the network
					// late) is asked again on a timer until it answers.
					if ctx.Err() != nil {
						return 0
					}
					// A device the controller lists as pending has no
					// record to name: not an API outage, and asking on a
					// timer changes nothing. The next adoption provisions.
					if errors.Is(err, unifiapi.ErrPending) {
						if !pendingNoted {
							pendingNoted = true
							log.Printf("provision: %v; the names and the seed follow its adoption", err)
						}
						return 0
					}
					wait, first := retry.failed()
					if first {
						log.Printf("provision: the controller's API cannot be reached yet: %v (asking again in %s, then at growing intervals)", err, wait)
					}
					return wait
				}
				pendingNoted = false
				if retry.succeeded() {
					log.Printf("provision: the controller's API answers again")
				}
				switch {
				case r.RenamedDevice || r.RenamedPorts > 0 || r.Seeded > 0 || r.Cleared > 0:
					log.Printf("provision: device renamed=%v, %d ports named after the switch, %d ports seeded from the switch's own config, %d released slots cleared", r.RenamedDevice, r.RenamedPorts, r.Seeded, r.Cleared)
				}
				// Outlet slots the model draws but the device lacks: the
				// controller's default override shows them powered; set it
				// off so the picture matches the rows the device reports.
				if ph := device.PlaceholderOutlets(profile.Model, snap); len(ph) > 0 {
					pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
					n, err := api.SeedOutletOverrides(pctx, macStr, ph)
					pcancel()
					switch {
					case err != nil:
						log.Printf("provision: %v (retried on the next provision)", err)
					case n > 0:
						log.Printf("provision: %d outlet slots the device does not have set off in the controller (of %v)", n, ph)
					}
				}
				return 0
			})
			// The run ends with runOne, the context not always: a retry
			// timer must not outlive it.
			defer spacer.stop()
			provision, layoutChanged = spacer.provision, spacer.layoutChanged
			if snap != nil {
				provision(snap)
			}
			// A model on a UniFi product's firmware line takes the updates
			// the controller offers it by itself: they change the reported
			// version and nothing else, and an update badge that waits for
			// a click says nothing true. Not the Leaf, which reports the
			// device's own version, and not under an operator's -version,
			// which an update would not replace.
			if !device.ReportsOwnVersion(profile.Model) && !versionPinned {
				asker := newUpgradeAsker(sess.Adopted,
					func(c context.Context) (string, error) { return api.OfferedUpgrade(c, macStr) },
					func(c context.Context) error { return api.RequestUpgrade(c, macStr) },
					log.Printf)
				uctx, ucancel := context.WithCancel(ctx)
				defer ucancel() // the asker ends with the run
				go asker.run(uctx)
				upgradeConnected = asker.connected
			}
		}
	}

	// --- Loop ---
	loopCfg := informloop.Config{
		Logger:        log,
		DeviceName:    o.name,
		Children:      o.children,
		NUT:           nutApplier(nut),
		Interval:      o.interval,
		ApplyTimeout:  o.applyTimeout,
		FanMode:       o.fanMode,
		RecordDir:     o.recordDir,
		StateDir:      stateDirOf(o),
		RecordMaxSize: o.recordMaxSize,
		RecordKeep:    o.recordKeep,
		DeviceHost:    hostOf(o.deviceURL, o.deviceSSH),
		GatewayIP:     o.controller,
		OnLayoutChange: func(snap *devicemodel.Snapshot) {
			if layoutChanged != nil {
				layoutChanged(snap)
			}
		},
		OnConnected: func(snap *devicemodel.Snapshot) {
			// An automatic lane switch takes its lanes over before it is
			// seeded and before its first push is planned.
			if o.onAdopted != nil {
				o.onAdopted()
			}
			if provision != nil {
				provision(snap)
			}
			if upgradeConnected != nil {
				upgradeConnected()
			}
		},
		OnUnadopted: o.onUnadopted,
		// A lane switch reads a sustained run of HTTP 400s on its adopted
		// key as the Forget it missed (docs/drivers/breakout.md 16.11).
		ForgetOnRejectedKey: o.laneDriver,
		OnHeld: func(snap *devicemodel.Snapshot) {
			if provision != nil {
				provision(snap)
			}
		},
	}
	if sw != nil {
		loopCfg.Collector = sw
	}
	if o.controlOutlets != "" {
		oc, ok := sw.(devicemodel.OutletController)
		if sw == nil || !ok {
			return errors.New("-control-outlets needs a device connection whose driver controls outlets")
		}
		allow, err := parsePortList(o.controlOutlets)
		if err != nil {
			return fmt.Errorf("-control-outlets: %v", err)
		}
		loopCfg.OutletController = oc
		loopCfg.ControlOutlets = allow
		if allow == nil {
			log.Printf("control: switching and naming ALL outlets from the controller")
		} else {
			log.Printf("control: switching and naming outlets %s from the controller", o.controlOutlets)
		}
	}
	// What UniFi owns on the device follows the controller whenever the
	// driver can apply it: the device's own address (static IP Settings;
	// the DHCP default is never applied), a power device's syslog host, and
	// -- through the device's DeviceController, whether or not its ports are
	// written -- STP, IGMP, NTP, syslog, SNMP, the hostname, SSH keys and
	// Restart.
	if ac, ok := sw.(devicemodel.AddressController); sw != nil && ok {
		loopCfg.AddressController = ac
		log.Printf("control: the controller's IP Settings set the device's own address")
	}
	if sc, ok := sw.(devicemodel.SyslogController); sw != nil && ok {
		loopCfg.SyslogController = sc
		log.Printf("control: the controller's remote syslog host is written to the device")
	}
	if cc, ok := sw.(devicemodel.ClockController); sw != nil && ok {
		loopCfg.ClockController = cc
		log.Printf("control: the controller's time zone, DST rule and NTP servers are written to the device")
	}
	// IPv6 on the management interface follows the management network's
	// IPv6 setting, which the push does not carry: a REST read
	// (docs/drivers/sonic.md §11.22).
	if _, ok := sw.(devicemodel.ManagementIPv6Controller); sw != nil && ok {
		if o.unifiURL == "" || o.unifiKey == "" {
			log.Printf("control: no controller API URL or key (DUI_UNIFI_API_KEY): the management network's IPv6 setting cannot be read, and the device's IPv6 is left as it is")
		} else {
			loopCfg.ManagementIPv6 = cachedManagementIPv6(unifiapi.New(o.unifiURL, o.unifiKey, o.unifiSite, true), time.Minute)
			log.Printf("control: the device takes the gateway's IPv6 router advertisements on its management interface while the controller runs IPv6 on the management network")
		}
	}
	loopCfg.ControlIGMP, loopCfg.ControlNTP, loopCfg.ControlSyslog, loopCfg.ControlHostname = true, true, true, true
	loopCfg.ControlReboot, loopCfg.ControlSSHKeys, loopCfg.ControlSNMP = true, true, true
	if o.controlPorts != "" {
		ctl, ok := sw.(devicemodel.Controller)
		if sw == nil || !ok {
			return errors.New("-control-ports needs a switch connection whose driver supports writes")
		}
		allow, err := parsePortList(o.controlPorts)
		if err != nil {
			return fmt.Errorf("-control-ports: %v", err)
		}
		loopCfg.Controller = ctl
		loopCfg.ControlPorts = allow
		loopCfg.AllowInitialChanges = o.allowInitialChanges
		// EOS 7160 and Linux bridges forward jumbo at L2 unconditionally; a
		// driver that can turn it off (SONiC, by port MTU) says so.
		if js, ok := ctl.(devicemodel.JumboSwitcher); !ok || !js.SwitchesJumbo() {
			loopCfg.JumboAlwaysOn = true
		}
		loopCfg.DefaultPortNames = defaults
		if allow == nil {
			log.Printf("control: writing controller config to ALL ports, and the device-wide settings the driver takes")
		} else {
			log.Printf("control: writing controller config to ports %s, and the device-wide settings the driver takes", o.controlPorts)
		}
	} else if o.controlOutlets == "" {
		why := "control.ports is off"
		if !isA[devicemodel.Controller](sw) {
			why = "the driver does not write ports"
		}
		log.Printf("control: no port writes (%s); the device-wide settings the driver takes still follow the controller", why)
	}
	loop, err := informloop.New(desc, sess, loopCfg)
	if err != nil {
		return err
	}
	loop.Run(ctx)
	log.Printf("stopped in state %s", loop.State())
	return nil
}

// defaultPortNames returns, per port_idx, the names that mean "nobody named
// this port": the profile's "Port N", the controller's own "<media> k"
// convention ("SFP28 1".."SFP28 48", "QSFP28 1".."QSFP28 6"), and — when a
// switch is connected — every form the switch's interface name can take
// ("Ethernet54", "Ethernet54/1", "Ethernet54/1-4", each lane). A UniFi
// port name in this set is never written to the switch as a description,
// and the naming provisioner may replace it.
func defaultPortNames(ports []inform.Port, snap *devicemodel.Snapshot, namer devicemodel.Namer) map[int][]string {
	out := map[int][]string{}
	perMedia := map[string]int{}
	for _, p := range ports {
		perMedia[p.Media]++
		out[p.PortIdx] = []string{p.Name, fmt.Sprintf("%s %d", p.Media, perMedia[p.Media])}
	}
	if snap != nil {
		for _, p := range snap.Ports {
			out[p.Index] = append(out[p.Index], namer.DefaultPortNames(p)...)
		}
	}
	return out
}

// informURLFor is the inform URL: -inform (controller.inform_url) as
// given, else http://<the controller's IPv4>:8080/inform.
func informURLFor(o options) (string, error) {
	if o.informURL != "" {
		return o.informURL, nil
	}
	if o.controller == "" {
		return "", errors.New("one of -controller or -inform is required")
	}
	ipLit, err := resolveIPv4(o.controller)
	if err != nil {
		return "", fmt.Errorf("resolve controller %q: %v", o.controller, err)
	}
	return "http://" + ipLit + ":8080/inform", nil
}

// probeAddr is an inform URL's host:port (the scheme's port when the URL
// names none); "" when it has no host.
func probeAddr(informURL string) string {
	u, err := url.Parse(informURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// hostOf extracts an IP literal from a switch URL or user@host target; ""
// when the host is a name (the operator then passes -ip).
// fanModeOf reads a config fan_mode (validated by config.Load); "" = unset.
func fanModeOf(s string) devicemodel.FanMode {
	m, _ := devicemodel.ParseFanMode(s)
	return m
}

// parseApplyTimeout reads control.apply_timeout (validated by config.Load);
// "" leaves the loop's default.
func parseApplyTimeout(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

func hostOf(deviceURL, deviceSSH string) string {
	host := ""
	if deviceURL != "" {
		if u, err := url.Parse(deviceURL); err == nil && u.Hostname() != "" {
			host = u.Hostname()
		} else {
			// A driver whose endpoint is a bare address rather than a URL (a
			// PDU is reached at an address, not an API path) parses as a
			// path with no host, so the address has to be read directly.
			h := deviceURL
			if i := strings.IndexAny(h, "/"); i >= 0 {
				h = h[:i]
			}
			if hh, _, ok := strings.Cut(h, ":"); ok {
				h = hh
			}
			host = h
		}
	} else if deviceSSH != "" {
		_, h, _ := strings.Cut(deviceSSH, "@")
		host, _, _ = strings.Cut(h, ":")
	}
	if net.ParseIP(host) != nil {
		return host
	}
	return ""
}

// parsePortList parses "all" (nil = no restriction) or "2,5-8,49".
func parsePortList(s string) (map[int]bool, error) {
	if strings.EqualFold(strings.TrimSpace(s), "all") {
		return nil, nil
	}
	out := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 1 {
			return nil, fmt.Errorf("bad port %q", part)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, fmt.Errorf("bad range %q", part)
			}
		}
		for i := a; i <= b; i++ {
			out[i] = true
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no ports listed")
	}
	return out, nil
}

// resolveIPv4 returns s if it is already an IPv4 literal, otherwise the first
// IPv4 address it resolves to. The controller rejects a hostname in
// inform_url with HTTP 400 "invalid inform_ip", so the URL must carry a literal.
func resolveIPv4(s string) (string, error) {
	if ip := net.ParseIP(s); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4.String(), nil
		}
		return "", fmt.Errorf("%s is not IPv4", s)
	}
	addrs, err := net.LookupIP(s)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ip4 := a.To4(); ip4 != nil {
			return ip4.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 address for %s", s)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envChain returns the first non-empty environment variable among keys, in
// order, else def. Used to prefer the current DUI_ names while still reading
// the pre-rename STU_ (and older STU_EOS_) names, so existing env files keep
// working.
// flagGiven reports whether the named flag was set on the command line.
func flagGiven(name string) bool {
	given := false
	flag.Visit(func(f *flag.Flag) { given = given || f.Name == name })
	return given
}

func envChain(def string, keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

func envInt(def int, keys ...string) int {
	if v := envChain("", keys...); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(def time.Duration, keys ...string) time.Duration {
	if v := envChain("", keys...); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// nutApplier keeps a nil *nutd.Server from becoming a non-nil interface.
func nutApplier(s *nutd.Server) informloop.NUTApplier {
	if s == nil {
		return nil
	}
	return s
}

// cachedManagementIPv6 is the controller's answer to "IPv6 on the
// management network?", read again after ttl: the loop asks every cycle.
// An error is not cached.
func cachedManagementIPv6(api *unifiapi.Client, ttl time.Duration) func(context.Context, int) (bool, error) {
	var (
		mu     sync.Mutex
		at     time.Time
		vlanAt int
		on     bool
	)
	return func(ctx context.Context, vlan int) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if !at.IsZero() && vlan == vlanAt && time.Since(at) < ttl {
			return on, nil
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		v, err := api.ManagementIPv6(cctx, vlan)
		if err != nil {
			return false, err
		}
		at, vlanAt, on = time.Now(), vlan, v
		return v, nil
	}
}

// cachedSiteNetwork is the controller's network for an address, read
// again after ttl: a device asks every collect. An error is not cached (the
// device keeps its own last good answer).
func cachedSiteNetwork(api *unifiapi.Client, ttl time.Duration) func(context.Context, string) (devicemodel.SiteNetwork, error) {
	type entry struct {
		at time.Time
		n  devicemodel.SiteNetwork
	}
	var (
		mu    sync.Mutex
		cache = map[string]entry{}
	)
	return func(ctx context.Context, ip string) (devicemodel.SiteNetwork, error) {
		mu.Lock()
		defer mu.Unlock()
		if e, ok := cache[ip]; ok && time.Since(e.at) < ttl {
			return e.n, nil
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		n, err := api.NetworkFor(cctx, ip)
		if err != nil {
			return n, err
		}
		cache[ip] = entry{time.Now(), n}
		return n, nil
	}
}

// apiRetry spaces the retries of a controller API step that failed: 15 s
// after the first failure, doubling to five minutes, until a call succeeds.
type apiRetry struct {
	wait    time.Duration
	failing bool
}

// failed records a failed call and returns how long to wait before the next
// one, and whether this is the first failure since a success (the one worth
// a log line).
func (r *apiRetry) failed() (wait time.Duration, first bool) {
	first = !r.failing
	r.failing = true
	switch {
	case r.wait == 0:
		r.wait = 15 * time.Second
	case r.wait < 5*time.Minute:
		r.wait *= 2
		if r.wait > 5*time.Minute {
			r.wait = 5 * time.Minute
		}
	}
	return r.wait, first
}

// succeeded records a successful call and reports whether it ends a run of
// failures.
func (r *apiRetry) succeeded() (recovered bool) {
	recovered = r.failing
	r.failing, r.wait = false, 0
	return recovered
}

func itoa(n int) string { return strconv.Itoa(n) }
