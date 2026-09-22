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
	"github.com/TechBlueprints/disunified/internal/unifiapi"
	"github.com/TechBlueprints/disunified/internal/unifimodel"
	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/inform"

	// Drivers register themselves; add a blank import per driver.
	_ "github.com/TechBlueprints/disunified/internal/drivers/apc-pdu"
	_ "github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
	_ "github.com/TechBlueprints/disunified/internal/drivers/proxmox"
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
		version  = flag.String("version", envChain("", "DUI_VERSION", "STU_VERSION"), "firmware version to report (default: the device's own, e.g. EOS 4.26.14M or PVE 9.1.6)")
		uplink   = flag.Int("uplink-port", envInt(0, "DUI_UPLINK_PORT", "STU_UPLINK_PORT"), "port_idx to flag as the uplink (0 = the port whose LLDP neighbour is the upstream switch)")

		// Device side. -device-url/-device-ssh are the current names;
		// -switch-url/-switch-ssh are the pre-rename aliases, reconciled after
		// Parse (see below). The env default feeds the -device-* flags.
		driverName     = flag.String("driver", envChain(os.Getenv("STU_DRIVER"), "DUI_DRIVER"), "device driver, required with -device-url/-device-ssh (see -list-drivers)")
		deviceURLFlag  = flag.String("device-url", envChain("", "DUI_DEVICE_URL", "STU_SWITCH_URL", "STU_EOS_URL"), "device API endpoint (driver-specific), with DUI_DEVICE_USER/DUI_DEVICE_PASS")
		deviceSSHFlag  = flag.String("device-ssh", envChain("", "DUI_DEVICE_SSH", "STU_SWITCH_SSH", "STU_EOS_SSH"), "device SSH target user@host[:port] (key auth), used when -device-url is empty")
		switchURLFlag  = flag.String("switch-url", "", "pre-rename alias for -device-url")
		switchSSHFlag  = flag.String("switch-ssh", "", "pre-rename alias for -device-ssh")
		controlPorts   = flag.String("control-ports", envChain("", "DUI_CONTROL_PORTS", "STU_CONTROL_PORTS"), "write controller port config to the device: \"all\", or a list like \"2,5-8\"; empty = read-only")
		controlOutlets = flag.String("control-outlets", envChain("", "DUI_CONTROL_OUTLETS", "STU_CONTROL_OUTLETS"), "switch and name a power device's outlets from the controller: \"all\", or a list like \"2,5-8\"; empty = read-only")
		controlIGMP    = flag.Bool("control-igmp", envChain("", "DUI_CONTROL_IGMP", "STU_CONTROL_IGMP") == "1", "let the controller's per-network IGMP snooping setting drive the device (off by default)")
		controlNTP     = flag.Bool("control-ntp", envChain("", "DUI_CONTROL_NTP", "STU_CONTROL_NTP") == "1", "let the controller's NTP servers replace the device's (off by default)")
		controlSyslog  = flag.Bool("control-syslog", envChain("", "DUI_CONTROL_SYSLOG", "STU_CONTROL_SYSLOG") == "1", "let the controller's remote syslog host replace the device's (off by default)")
		controlReboot  = flag.Bool("control-reboot", envChain("", "DUI_CONTROL_REBOOT", "STU_CONTROL_REBOOT") == "1", "the controller's Restart really reloads the device (off by default: emulated)")
		controlSSH     = flag.Bool("control-ssh-keys", envChain("", "DUI_CONTROL_SSH_KEYS", "STU_CONTROL_SSH_KEYS") == "1", "install the SSH keys the controller pushes on the device's bridge user (off by default)")
		controlSNMP    = flag.Bool("control-snmp", envChain("", "DUI_CONTROL_SNMP", "STU_CONTROL_SNMP") == "1", "the controller's SNMP v1/v2c community is configured on the device (off by default)")

		// Controller REST API (names)
		unifiURL  = flag.String("unifi-url", envChain("", "DUI_UNIFI_URL", "STU_UNIFI_URL"), "controller URL for the REST API, e.g. https://unifi.example.net (needs STU_UNIFI_API_KEY)")
		unifiSite = flag.String("unifi-site", envChain("default", "DUI_UNIFI_SITE", "STU_UNIFI_SITE"), "controller site")
		provision = flag.Bool("provision-names", envChain("1", "DUI_PROVISION_NAMES", "STU_PROVISION_NAMES") != "0", "name the device \"<vendor> <model>\" and ports after the device's interfaces via the REST API, touching only controller-default names (needs -unifi-url)")

		// One-shots
		listModels  = flag.Bool("list-models", false, "print the switch models the bundled catalogue knows and exit")
		listDrivers = flag.Bool("list-drivers", false, "print the registered device drivers and exit")
		showBuild   = flag.Bool("build-version", false, "print this bridge's own build version and exit (-version is the firmware version reported to the controller)")
		collectOnce = flag.Bool("collect-once", false, "collect one snapshot from the device, print it as JSON with the suggested model, and exit")
	)
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
			if p, ok := emu.Profile(m); ok && p.Type == "usw" {
				fmt.Printf("%-10s %-40s %d ports\n", m, p.ModelDisplay, len(p.Ports))
			}
		}
		return
	}

	log.Printf("disunified %s starting", buildVersion)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *configPath != "" && !*collectOnce {
		f, err := config.Load(*configPath)
		if err != nil {
			log.Fatal(err)
		}
		runConfig(ctx, f)
		return
	}

	o := options{
		controller: *controller, informURL: *informURL, model: *model, udapiVersion: *udapiVer,
		interval: *interval, recordDir: *recordDir, stateFile: *stateFile,
		mac: *mac, serial: *serial, ip: *ip, hostname: *hostname, version: *version, uplink: *uplink,
		driver: *driverName, deviceURL: deviceURL, deviceSSH: deviceSSH,
		username:     envChain("", "DUI_DEVICE_USER", "STU_SWITCH_USER", "STU_EOS_USER"),
		password:     envChain("", "DUI_DEVICE_PASS", "STU_SWITCH_PASS", "STU_EOS_PASS"),
		controlPorts: *controlPorts, controlOutlets: *controlOutlets, controlIGMP: *controlIGMP, controlNTP: *controlNTP, controlSyslog: *controlSyslog,
		controlReboot: *controlReboot, controlSSH: *controlSSH, controlSNMP: *controlSNMP,
		unifiURL: *unifiURL, unifiSite: *unifiSite, unifiKey: envChain("", "DUI_UNIFI_API_KEY", "STU_UNIFI_API_KEY"), provision: *provision,
		collectOnce: *collectOnce, logger: log.Default(),
	}
	if err := runOne(ctx, o); err != nil {
		log.Fatal(err)
	}
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
	controlIGMP, controlNTP, controlSyslog     bool
	controlReboot, controlSSH, controlSNMP     bool
	allowInitialChanges, noSeed                bool
	unifiURL, unifiSite, unifiKey              string
	provision                                  bool
	collectOnce                                bool
	logger                                     *log.Logger
	stateDir                                   string
}

// runConfig runs every switch in the file concurrently; a switch that fails
// to start is logged and the others keep running.
func runConfig(ctx context.Context, f *config.File) {
	var wg sync.WaitGroup
	for _, sw := range f.Devices {
		sw := sw
		logger := log.New(os.Stderr, "["+sw.Name+"] ", log.LstdFlags|log.Lmicroseconds)
		o := options{
			controller: f.Controller.Host, informURL: f.Controller.InformURL,
			model: sw.Model, udapiVersion: sw.UDAPIVersion, interval: 10 * time.Second,
			recordDir: filepath.Join(f.StateDir, "inform-log", sw.Name),
			stateFile: filepath.Join(f.StateDir, "state", sw.Name, "device.json"),
			ip:        sw.IP, hostname: sw.Hostname, uplink: sw.UplinkPort,
			driver: sw.Driver, deviceURL: sw.URL, deviceSSH: sw.SSH, username: sw.Username, password: sw.Password,
			driverOptions: sw.Options,
			controlPorts:  sw.Control.Ports, controlOutlets: sw.Control.Outlets, controlIGMP: sw.Control.IGMP, controlNTP: sw.Control.NTP, controlSyslog: sw.Control.Syslog,
			controlReboot: sw.Control.Reboot, controlSSH: sw.Control.SSHKeys, controlSNMP: sw.Control.SNMP,
			allowInitialChanges: sw.Control.AllowInitialChanges, noSeed: sw.Control.NoSeed,
			unifiURL: f.Controller.APIURL, unifiSite: f.Controller.Site, unifiKey: f.Controller.APIKey(), provision: true,
			logger: logger, stateDir: filepath.Join(f.StateDir, "state", sw.Name),
		}
		if strings.EqualFold(o.controlPorts, "off") {
			o.controlPorts = ""
		}
		if strings.EqualFold(o.controlOutlets, "off") {
			o.controlOutlets = ""
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runOne(ctx, o); err != nil {
				logger.Printf("FAILED: %v", err)
			}
		}()
	}
	wg.Wait()
}

// openAndStart opens a driver and takes its first snapshot, closing the
// device again if Start fails so a retry begins from a clean connection.
func openAndStart(ctx context.Context, drv devicemodel.Driver, cfg devicemodel.DriverConfig) (devicemodel.Device, *devicemodel.Snapshot, error) {
	sw, err := drv.Open(ctx, cfg)
	if err != nil {
		return nil, nil, err
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

// runOne bridges one switch until ctx is cancelled.
func runOne(ctx context.Context, o options) error {
	log := o.logger

	// --- Device ---
	var (
		sw   devicemodel.Device
		snap *devicemodel.Snapshot
	)
	if o.deviceURL != "" || o.deviceSSH != "" {
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
		if o.stateDir != "" {
			opts["state_dir"] = o.stateDir // drivers with their own persistent state keep it next to device.json
		} else if o.stateFile != "" && !o.collectOnce {
			opts["state_dir"] = filepath.Dir(o.stateFile)
		}
		cfg := devicemodel.DriverConfig{URL: o.deviceURL, SSH: o.deviceSSH, Username: o.username, Password: o.password, Options: opts}
		// A device that does not answer at startup is retried, not dropped.
		// Start is meant to fail loudly on a wrong command or credential, and
		// it still does -- every attempt is logged -- but the same failure
		// also comes from a card that is rebooting, a switch mid-upgrade or
		// a lease that moved, and dropping the device for those meant it
		// stayed gone until someone restarted the container (the PDU,
		// 2026-09-22: one SNMP timeout at start, then nothing for hours).
		// -collect-once keeps failing fast: it is a probe, not a service.
		for attempt, wait := 1, 15*time.Second; ; attempt++ {
			sw, snap, err = openAndStart(ctx, drv, cfg)
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
		log.Printf("switch: %s %s serial %s, %s, %d ports", snap.System.Vendor, snap.System.Model, snap.System.Serial, snap.System.Version, len(snap.Ports))
	}

	if o.collectOnce {
		if snap == nil {
			return errors.New("-collect-once needs -device-url or -device-ssh")
		}
		out := map[string]any{"snapshot": snap, "layout": unifimodel.LayoutOf(snap)}
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

	// --- Identity ---
	if snap != nil {
		if o.mac == "" {
			o.mac = snap.System.MAC
		}
		if o.serial == "" {
			o.serial = snap.System.Serial
		}
		if o.hostname == "" {
			o.hostname = snap.System.Hostname
		}
		if o.ip == "" {
			o.ip = hostOf(o.deviceURL, o.deviceSSH)
		}
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
	if o.serial == "" {
		o.serial = strings.ToUpper(strings.ReplaceAll(macStr, ":", ""))
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
	if profile.Type != "usw" {
		return fmt.Errorf("model %q is a %q, not a switch", o.model, profile.Type)
	}
	if snap != nil && len(profile.Ports) != len(snap.Ports) {
		log.Printf("WARNING: model %s has %d ports, the switch has %d; ports beyond the profile are not drawn", o.model, len(profile.Ports), len(snap.Ports))
	}

	// --- Controller URL ---
	url := o.informURL
	if url == "" {
		if o.controller == "" {
			return errors.New("one of -controller or -inform is required")
		}
		ipLit, err := resolveIPv4(o.controller)
		if err != nil {
			return fmt.Errorf("resolve controller %q: %v", o.controller, err)
		}
		url = "http://" + ipLit + ":8080/inform"
	}

	desc, err := device.DescriptorFor(profile.Model, snap, device.Identity{
		MAC: macStr, Serial: o.serial, IP: o.ip, Hostname: o.hostname, Version: o.version,
		UDAPIVersion: o.udapiVersion, UplinkPort: o.uplink,
	})
	if err != nil {
		return err
	}
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
	if st.Adopted {
		log.Printf("resuming adopted state from %s (cfgversion %s, gcm=%v)", o.stateFile, st.CfgVersion, st.UseAESGCM)
	}
	sess := device.NewSession(desc, url, st, store, time.Now())
	sess.SetCapabilities(caps)
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
	var provision func(snap *devicemodel.Snapshot)
	if o.provision && o.unifiURL != "" {
		key := o.unifiKey
		if key == "" {
			log.Printf("provision-names: no UNIFI API key set (DUI_UNIFI_API_KEY), skipping")
		} else {
			api := unifiapi.New(o.unifiURL, key, o.unifiSite, true)
			seed := !o.noSeed && o.controlPorts != ""
			var last time.Time
			provision = func(snap *devicemodel.Snapshot) {
				if !sess.Adopted() || snap == nil || time.Since(last) < 10*time.Second {
					return
				}
				last = time.Now()
				pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
				r, err := api.Provision(pctx, macStr, snap, namer, append([]string{profile.ModelDisplay, "USW Leaf", profile.Model}, unifimodel.ControllerDisplayNames(profile.Model)...), isDefaultPortNameIn(defaultPortNames(desc.Ports, snap, namer)), seed)
				pcancel()
				for _, note := range r.Notes {
					log.Printf("provision: seed: %s", note)
				}
				switch {
				case err != nil:
					log.Printf("provision: %v (retried on the next layout change or held push)", err)
				case r.RenamedDevice || r.RenamedPorts > 0 || r.Seeded > 0 || r.Cleared > 0:
					log.Printf("provision: device renamed=%v, %d ports named after the switch, %d ports seeded from the switch's own config, %d released slots cleared", r.RenamedDevice, r.RenamedPorts, r.Seeded, r.Cleared)
				}
			}
			if snap != nil {
				provision(snap)
			}
		}
	}

	// --- Loop ---
	loopCfg := informloop.Config{
		Logger:     log,
		Interval:   o.interval,
		RecordDir:  o.recordDir,
		DeviceHost: hostOf(o.deviceURL, o.deviceSSH),
		GatewayIP:  o.controller,
		OnLayoutChange: func(snap *devicemodel.Snapshot) {
			if provision != nil {
				provision(snap)
			}
		},
		OnConnected: func(snap *devicemodel.Snapshot) {
			if provision != nil {
				provision(snap)
			}
		},
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
		loopCfg.ControlIGMP = o.controlIGMP
		loopCfg.ControlNTP = o.controlNTP
		loopCfg.ControlSyslog = o.controlSyslog
		loopCfg.ControlReboot = o.controlReboot
		loopCfg.ControlSSHKeys = o.controlSSH
		loopCfg.ControlSNMP = o.controlSNMP
		loopCfg.AllowInitialChanges = o.allowInitialChanges
		loopCfg.JumboAlwaysOn = true // every driver so far: EOS 7160 forwards jumbo at L2 unconditionally
		loopCfg.DefaultPortNames = defaults
		if allow == nil {
			log.Printf("control: writing controller config to ALL ports (igmp=%v)", o.controlIGMP)
		} else {
			log.Printf("control: writing controller config to ports %s (igmp=%v)", o.controlPorts, o.controlIGMP)
		}
	} else if o.controlOutlets == "" {
		log.Printf("control: read-only (no -control-ports); controller pushes are accepted but not written")
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

// hostOf extracts an IP literal from a switch URL or user@host target; ""
// when the host is a name (the operator then passes -ip).
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
