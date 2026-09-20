// Command switch-to-unifi presents a non-UniFi switch to a UniFi Network
// controller as if it were a UniFi switch.
//
// A driver (-driver) reads the switch and, when writes are enabled, applies
// the controller's port and switch settings to it. The bridge claims a
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

	"github.com/TechBlueprints/switch-to-unifi/internal/config"
	"github.com/TechBlueprints/switch-to-unifi/internal/device"
	"github.com/TechBlueprints/switch-to-unifi/internal/informloop"
	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
	"github.com/TechBlueprints/switch-to-unifi/internal/unifiapi"
	"github.com/TechBlueprints/switch-to-unifi/internal/unifimodel"
	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/inform"

	// Drivers register themselves; add a blank import per driver.
	_ "github.com/TechBlueprints/switch-to-unifi/internal/drivers/aristaeos"
)

func main() {
	var (
		configPath = flag.String("config", envOr("STU_CONFIG", ""), "YAML config file (deploy/config.example.yaml): one controller, any number of switches; the flags below then apply only as one-shots")

		// Controller side
		controller = flag.String("controller", envOr("STU_CONTROLLER", ""), "controller host or IP; the inform URL becomes http://<ip>:8080/inform")
		informURL  = flag.String("inform", envOr("STU_INFORM_URL", ""), "full inform URL (overrides -controller); must contain an IP literal")
		model      = flag.String("model", envOr("STU_MODEL", "auto"), "UniFi model string to claim, or \"auto\" to pick the best catalogue match for the switch's port layout")
		udapiVer   = flag.String("udapi-version", envOr("STU_UDAPI_VERSION", "1.0.0"), "UDAPI schema version to report; the controller only stores capability claims when this is set (\"\" = omit)")
		interval   = flag.Duration("interval", envDuration("STU_INTERVAL", 10*time.Second), "initial inform interval; the controller then sets its own")
		recordDir  = flag.String("record-dir", envOr("STU_RECORD_DIR", "inform-log"), "directory for the NDJSON reply log (\"\" disables)")
		stateFile  = flag.String("state", envOr("STU_STATE", "state/device.json"), "file holding the adopted key and provisioned config across restarts")

		// Identity (defaults come from the switch when a driver is configured)
		mac      = flag.String("mac", envOr("STU_MAC", ""), "device MAC to present (default: the switch's system MAC)")
		serial   = flag.String("serial", envOr("STU_SERIAL", ""), "device serial (default: the switch's serial)")
		ip       = flag.String("ip", envOr("STU_IP", ""), "device IP to report (default: the switch address from -switch-url/-switch-ssh when it is an IP literal)")
		hostname = flag.String("hostname", envOr("STU_HOSTNAME", ""), "device hostname to report (default: the switch's hostname)")
		version  = flag.String("version", envOr("STU_VERSION", ""), "firmware version to report (default: the model profile's)")
		uplink   = flag.Int("uplink-port", envInt("STU_UPLINK_PORT", 0), "port_idx to flag as the uplink (0 = the port whose LLDP neighbour is the upstream switch)")

		// Switch side
		driverName    = flag.String("driver", envOr("STU_DRIVER", "arista-eos"), "switch driver (see -list-drivers)")
		switchURL     = flag.String("switch-url", envOr("STU_SWITCH_URL", envOr("STU_EOS_URL", "")), "switch API endpoint (driver-specific), with STU_SWITCH_USER/STU_SWITCH_PASS")
		switchSSH     = flag.String("switch-ssh", envOr("STU_SWITCH_SSH", envOr("STU_EOS_SSH", "")), "switch SSH target user@host[:port] (key auth), used when -switch-url is empty")
		controlPorts  = flag.String("control-ports", envOr("STU_CONTROL_PORTS", ""), "write controller port config to the switch: \"all\", or a list like \"2,5-8\"; empty = read-only")
		controlIGMP   = flag.Bool("control-igmp", envOr("STU_CONTROL_IGMP", "") == "1", "let the controller's per-network IGMP snooping setting drive the switch (off by default)")
		controlNTP    = flag.Bool("control-ntp", envOr("STU_CONTROL_NTP", "") == "1", "let the controller's NTP servers replace the switch's (off by default)")
		controlSyslog = flag.Bool("control-syslog", envOr("STU_CONTROL_SYSLOG", "") == "1", "let the controller's remote syslog host replace the switch's (off by default)")
		controlReboot = flag.Bool("control-reboot", envOr("STU_CONTROL_REBOOT", "") == "1", "the controller's Restart really reloads the switch (off by default: emulated)")
		controlSSH    = flag.Bool("control-ssh-keys", envOr("STU_CONTROL_SSH_KEYS", "") == "1", "install the SSH keys the controller pushes on the switch's bridge user (off by default)")
		controlSNMP   = flag.Bool("control-snmp", envOr("STU_CONTROL_SNMP", "") == "1", "the controller's SNMP v1/v2c community is configured on the switch (off by default)")

		// Controller REST API (names)
		unifiURL  = flag.String("unifi-url", envOr("STU_UNIFI_URL", ""), "controller URL for the REST API, e.g. https://unifi.example.net (needs STU_UNIFI_API_KEY)")
		unifiSite = flag.String("unifi-site", envOr("STU_UNIFI_SITE", "default"), "controller site")
		provision = flag.Bool("provision-names", envOr("STU_PROVISION_NAMES", "1") != "0", "name the device \"<vendor> <model>\" and ports after the switch's interfaces via the REST API, touching only controller-default names (needs -unifi-url)")

		// One-shots
		listModels  = flag.Bool("list-models", false, "print the switch models the bundled catalogue knows and exit")
		listDrivers = flag.Bool("list-drivers", false, "print the registered switch drivers and exit")
		collectOnce = flag.Bool("collect-once", false, "collect one snapshot from the switch, print it as JSON with the suggested model, and exit")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if *listDrivers {
		for _, d := range switchmodel.Drivers() {
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
		driver: *driverName, switchURL: *switchURL, switchSSH: *switchSSH,
		username:     envOr("STU_SWITCH_USER", os.Getenv("STU_EOS_USER")),
		password:     envOr("STU_SWITCH_PASS", os.Getenv("STU_EOS_PASS")),
		controlPorts: *controlPorts, controlIGMP: *controlIGMP, controlNTP: *controlNTP, controlSyslog: *controlSyslog,
		controlReboot: *controlReboot, controlSSH: *controlSSH, controlSNMP: *controlSNMP,
		unifiURL: *unifiURL, unifiSite: *unifiSite, unifiKey: os.Getenv("STU_UNIFI_API_KEY"), provision: *provision,
		collectOnce: *collectOnce, logger: log.Default(),
	}
	if err := runOne(ctx, o); err != nil {
		log.Fatal(err)
	}
}

// options is everything one bridged switch needs, from flags or a config entry.
type options struct {
	controller, informURL, model, udapiVersion string
	interval                                   time.Duration
	recordDir, stateFile                       string
	mac, serial, ip, hostname, version         string
	uplink                                     int
	driver, switchURL, switchSSH               string
	username, password                         string
	driverOptions                              map[string]string
	controlPorts                               string
	controlIGMP, controlNTP, controlSyslog     bool
	controlReboot, controlSSH, controlSNMP     bool
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
	for _, sw := range f.Switches {
		sw := sw
		logger := log.New(os.Stderr, "["+sw.Name+"] ", log.LstdFlags|log.Lmicroseconds)
		o := options{
			controller: f.Controller.Host, informURL: f.Controller.InformURL,
			model: sw.Model, udapiVersion: sw.UDAPIVersion, interval: 10 * time.Second,
			recordDir: filepath.Join(f.StateDir, "inform-log", sw.Name),
			stateFile: filepath.Join(f.StateDir, "state", sw.Name, "device.json"),
			ip:        sw.IP, hostname: sw.Hostname, uplink: sw.UplinkPort,
			driver: sw.Driver, switchURL: sw.URL, switchSSH: sw.SSH, username: sw.Username, password: sw.Password,
			driverOptions: sw.Options,
			controlPorts:  sw.Control.Ports, controlIGMP: sw.Control.IGMP, controlNTP: sw.Control.NTP, controlSyslog: sw.Control.Syslog,
			controlReboot: sw.Control.Reboot, controlSSH: sw.Control.SSHKeys, controlSNMP: sw.Control.SNMP,
			unifiURL: f.Controller.APIURL, unifiSite: f.Controller.Site, unifiKey: f.Controller.APIKey(), provision: true,
			logger: logger, stateDir: filepath.Join(f.StateDir, "state", sw.Name),
		}
		if strings.EqualFold(o.controlPorts, "off") {
			o.controlPorts = ""
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

// runOne bridges one switch until ctx is cancelled.
func runOne(ctx context.Context, o options) error {
	log := o.logger

	// --- Switch ---
	var (
		sw   switchmodel.Switch
		snap *switchmodel.Snapshot
	)
	if o.switchURL != "" || o.switchSSH != "" {
		drv, err := switchmodel.LookupDriver(o.driver)
		if err != nil {
			return err
		}
		cfg := switchmodel.DriverConfig{URL: o.switchURL, SSH: o.switchSSH, Username: o.username, Password: o.password, Options: o.driverOptions}
		sw, err = drv.Open(ctx, cfg)
		if err != nil {
			return err
		}
		defer sw.Close()
		started := time.Now()
		snap, err = sw.Start(ctx)
		if err != nil {
			return err
		}
		log.Printf("switch: %s %s serial %s, %s, %d ports (%s)", snap.System.Vendor, snap.System.Model, snap.System.Serial, snap.System.Version, len(snap.Ports), time.Since(started).Round(time.Millisecond))
	}

	if o.collectOnce {
		if snap == nil {
			return errors.New("-collect-once needs -switch-url or -switch-ssh")
		}
		out := map[string]any{"snapshot": snap, "layout": unifimodel.LayoutOf(snap)}
		if c, err := unifimodel.Suggest(unifimodel.LayoutOf(snap)); err == nil {
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
			o.ip = hostOf(o.switchURL, o.switchSSH)
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
		c, err := unifimodel.Suggest(unifimodel.LayoutOf(snap))
		if err != nil {
			return err
		}
		o.model = c.Model
		log.Printf("model: auto-selected %s (%s) for switch layout %+v", c.Model, c.Display, unifimodel.LayoutOf(snap))
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

	ports := make([]inform.Port, len(profile.Ports))
	copy(ports, profile.Ports)
	if o.uplink > 0 {
		for i := range ports {
			ports[i].IsUplink = ports[i].PortIdx == o.uplink
		}
	}
	if o.version == "" {
		o.version = profile.Version
	}
	desc := inform.Descriptor{
		MAC:          macStr,
		Serial:       o.serial,
		Model:        profile.Model,
		ModelDisplay: profile.ModelDisplay,
		Version:      o.version,
		IP:           o.ip,
		Hostname:     o.hostname,
		Type:         profile.Type,
		FWCaps:       device.FWCaps,
		UDAPIVersion: o.udapiVersion,
		Ports:        ports,
	}

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
	if snap != nil {
		sess.SetSnapshot(snap)
	} else {
		log.Printf("no -switch-url/-switch-ssh: reporting the model's synthetic port table")
	}

	// --- Names: defaults, and provisioning through the REST API ---
	namer := switchmodel.Namer(switchmodel.DefaultNamer{})
	if sw != nil {
		namer = switchmodel.NamerFor(sw)
	}
	defaults := defaultPortNames(ports, snap, namer)
	isDefaultPortName := func(idx int, name string) bool {
		for _, d := range defaults[idx] {
			if name == d {
				return true
			}
		}
		return false
	}
	var provisionNames func(snap *switchmodel.Snapshot)
	if o.provision && o.unifiURL != "" {
		key := o.unifiKey
		if key == "" {
			log.Printf("provision-names: STU_UNIFI_API_KEY not set, skipping")
		} else {
			api := unifiapi.New(o.unifiURL, key, o.unifiSite, true)
			provisionNames = func(snap *switchmodel.Snapshot) {
				if !sess.Adopted() {
					return
				}
				pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
				dev, nports, err := api.ProvisionNames(pctx, macStr, snap, namer, []string{profile.ModelDisplay, "USW Leaf", profile.Model}, isDefaultPortName)
				pcancel()
				switch {
				case err != nil:
					log.Printf("provision-names: %v", err)
				case dev || nports > 0:
					log.Printf("provision-names: device renamed=%v, %d ports named after the switch", dev, nports)
				}
			}
			if snap != nil {
				provisionNames(snap)
			}
		}
	}

	// --- Loop ---
	loopCfg := informloop.Config{
		Logger:     log,
		Interval:   o.interval,
		RecordDir:  o.recordDir,
		SwitchHost: hostOf(o.switchURL, o.switchSSH),
		GatewayIP:  o.controller,
		OnLayoutChange: func(snap *switchmodel.Snapshot) {
			if provisionNames != nil {
				provisionNames(snap)
			}
		},
	}
	if sw != nil {
		loopCfg.Collector = sw
	}
	if o.controlPorts != "" {
		ctl, ok := sw.(switchmodel.Controller)
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
		loopCfg.JumboAlwaysOn = true // every driver so far: EOS 7160 forwards jumbo at L2 unconditionally
		loopCfg.DefaultPortNames = defaults
		if allow == nil {
			log.Printf("control: writing controller config to ALL ports (igmp=%v)", o.controlIGMP)
		} else {
			log.Printf("control: writing controller config to ports %s (igmp=%v)", o.controlPorts, o.controlIGMP)
		}
	} else {
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
func defaultPortNames(ports []inform.Port, snap *switchmodel.Snapshot, namer switchmodel.Namer) map[int][]string {
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
func hostOf(switchURL, switchSSH string) string {
	host := ""
	if switchURL != "" {
		if u, err := url.Parse(switchURL); err == nil {
			host = u.Hostname()
		}
	} else if switchSSH != "" {
		_, h, _ := strings.Cut(switchSSH, "@")
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

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
