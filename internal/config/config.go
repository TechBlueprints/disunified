// Package config is the bridge's configuration file: one controller, any
// number of devices, secrets by environment-variable name. Kept small on
// purpose — the file names what to bridge; everything else is discovered
// from the device or the controller.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File is the whole YAML document.
//
// Devices is the list of things to bridge. The key is "devices:"; "switches:"
// is the pre-rename spelling and is still accepted (a file may use either,
// not both). After Load, read Devices — legacy is folded into it.
type File struct {
	Controller Controller `yaml:"controller"`
	Devices    []Device   `yaml:"devices"`
	// Switches is the legacy spelling of Devices, folded into it by Load.
	Switches []Device `yaml:"switches"`
	// StateDir holds state/<name>/device.json and inform-log/<name>/.
	StateDir string `yaml:"state_dir"`
}

// Controller is where informs go and, optionally, the REST API for naming.
type Controller struct {
	Host      string `yaml:"host"`        // inform target: IP or hostname (resolved to an IPv4 literal)
	InformURL string `yaml:"inform_url"`  // overrides host: full inform URL
	APIURL    string `yaml:"api_url"`     // optional: https://<controller> for the REST API (naming)
	APIKeyEnv string `yaml:"api_key_env"` // env var holding the API key (default DUI_UNIFI_API_KEY, then STU_UNIFI_API_KEY)
	Site      string `yaml:"site"`        // default "default"
}

// Device is one bridged device (a switch, today).
type Device struct {
	Name   string `yaml:"name"`   // required, unique: used for state and log paths
	Driver string `yaml:"driver"` // e.g. arista-eos (see -list-drivers)

	URL         string `yaml:"url"`          // API endpoint (driver-specific)
	SSH         string `yaml:"ssh"`          // user@host[:port]
	Username    string `yaml:"username"`     // or UsernameEnv
	UsernameEnv string `yaml:"username_env"` // env var name
	Password    string `yaml:"password"`     // discouraged; prefer PasswordEnv
	PasswordEnv string `yaml:"password_env"` // env var name
	// Auth: "" means a url needs username/password (an API endpoint without
	// them is the usual misconfiguration); "none" says the device has no
	// credentials at all (Modbus TCP on a UPS's SmartConnect port).
	Auth    string            `yaml:"auth"`
	Options map[string]string `yaml:"options"` // driver-specific knobs

	Model        string `yaml:"model"`         // UniFi model to claim; "auto" (default) picks by port layout
	IP           string `yaml:"ip"`            // reported device IP (default: the switch address when it is an IP literal)
	Hostname     string `yaml:"hostname"`      // reported hostname (default: the switch's)
	UplinkPort   int    `yaml:"uplink_port"`   // default: the port whose LLDP neighbour is the upstream switch
	UDAPIVersion string `yaml:"udapi_version"` // default "1.0.0"; "" omits (capabilities then not stored)
	// Firmware is the version string reported to the controller. Default: the
	// claimed model's catalogue version. The controller cannot upgrade a
	// bridged switch; an upgrade it requests is emulated and the requested
	// version reported (and persisted) from then on.
	Firmware string `yaml:"firmware"`

	Control Control `yaml:"control"`

	// ReplyLog caps this device's reply log, inform-log/<name>/<mac>.ndjson.
	ReplyLog ReplyLog `yaml:"reply_log"`

	// Children are devices reported below this one's ports: each child's MAC
	// is added to its port's MAC table and, when the port has no LLDP
	// neighbour of its own, reported as the port's neighbour, so the
	// controller can place a separately bridged device there (a breakout
	// cage's lane switch, docs/breakout-lag.md §6.4). Reported values only:
	// nothing is written to the device.
	Children []Child `yaml:"children"`
}

// Child is one device reported on a port (see Device.Children).
type Child struct {
	Port int    `yaml:"port"` // front-panel port index
	MAC  string `yaml:"mac"`  // the child's MAC, as its own bridge reports it
	Name string `yaml:"name"` // reported as the neighbour's system name (default: the MAC)
	// Report: "" = both entries; "lldp" or "mac" reports only that one (an
	// experiment's knob: which of the two the controller places by).
	Report string `yaml:"report"`
}

// Control says which of the device's ports and outlets the bridge writes
// from the controller: unset = every one the driver can write (a fresh
// adoption is seeded from the device and its first push held until it
// matches), "off" = none, or a list such as "2,5-8". Everything else
// UniFi configures on its own devices -- the device's address (static IP
// Settings), IGMP snooping, NTP, the remote syslog host, the hostname, SSH
// keys, SNMP, Restart -- follows the controller whenever the driver can
// apply it, with no per-setting opt-in. The retired keys (address, igmp,
// ntp, syslog, hostname, reboot, ssh_keys, snmp) still load and are ignored.
type Control struct {
	Ports string `yaml:"ports"` // unset/"all", "off", or "2,5-8"
	// Outlets is the power-device equivalent of Ports. An outlet carries
	// real load: the first outlet push of a run that would switch one is
	// held until the controller agrees with the device.
	Outlets string `yaml:"outlets"`
	// NUT is accepted for compatibility and no longer needed: every UPS
	// claims the NUT-server capability (the controller's UI cannot save a
	// UPS's settings otherwise), and the controller's "NUT Server" switch
	// alone starts and stops the listener.
	NUT bool `yaml:"nut"`
	// AllowInitialChanges lets the first push after adoption change ports
	// (default: held until the controller's config matches the switch — the
	// bridge seeds it from the switch when api_url is set).
	AllowInitialChanges bool `yaml:"allow_initial_changes"`
	// ApplyTimeout bounds one write to the device (a port/VLAN/device apply,
	// SSH keys, the address, a reboot), separately from the collect budget:
	// a Go duration, e.g. "90s" or "2m". Default: what the driver asks for
	// (sonic: 90s, since a SONiC switch writes through ~250 ms CLI calls and
	// a first push can be dozens of them), else the collect budget (20s).
	// An apply cut short is finished by the next reconcile.
	ApplyTimeout string `yaml:"apply_timeout"`
	// NoSeed skips seeding the controller's port config from the switch on
	// adoption (default: seed when api_url is set).
	NoSeed bool `yaml:"no_seed"`
}

// ReplyLog is the size cap on a device's reply log (every controller reply,
// one JSON line each). When a record would take the live file past MaxSize
// it is renamed to <mac>.ndjson.1 (.1 to .2 and so on, Keep generations, the
// oldest dropped) and a fresh file started; the live file keeps its name.
type ReplyLog struct {
	// MaxSize: bytes, or a number with a binary unit -- "8MB", "512KB",
	// "1GB" (K/M/G, KB/MB/GB and KiB/MiB/GiB all count in 1024s). Unset =
	// 8MB; "off" = no cap.
	MaxSize string `yaml:"max_size"`
	// Keep is the number of rotated files kept. Unset (0) = 3.
	Keep int `yaml:"keep"`
}

// Bytes is MaxSize as the inform loop takes it: 0 = its default (8 MB),
// -1 = no cap. Load has already validated it.
func (r ReplyLog) Bytes() int64 {
	n, _ := ParseSize(r.MaxSize)
	return n
}

// ParseSize reads a reply_log.max_size: "" = 0 (the default), "off" = -1,
// else a positive byte count, bare or with a binary unit (K, M, G, with or
// without a trailing B or iB).
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	switch t {
	case "":
		return 0, nil
	case "OFF":
		return -1, nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("%q is not a size (e.g. \"8MB\", \"512KB\", \"off\")", s)
	}
	return n * mult, nil
}

// Load reads and validates a config file.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Controller.Host == "" && f.Controller.InformURL == "" {
		return nil, fmt.Errorf("%s: controller.host (or inform_url) is required", path)
	}
	if f.Controller.APIKeyEnv == "" {
		// DUI_ is the project's prefix; STU_ is the pre-rename one, still read.
		if os.Getenv("DUI_UNIFI_API_KEY") != "" {
			f.Controller.APIKeyEnv = "DUI_UNIFI_API_KEY"
		} else {
			f.Controller.APIKeyEnv = "STU_UNIFI_API_KEY"
		}
	}
	if f.Controller.Site == "" {
		f.Controller.Site = "default"
	}
	if f.StateDir == "" {
		f.StateDir = "."
	}
	// Fold the legacy "switches:" key into Devices. A file may use one key
	// or the other, never both.
	if len(f.Switches) > 0 {
		if len(f.Devices) > 0 {
			return nil, fmt.Errorf("%s: use either devices: or switches: (the legacy spelling), not both", path)
		}
		f.Devices, f.Switches = f.Switches, nil
	}
	if len(f.Devices) == 0 {
		return nil, fmt.Errorf("%s: at least one device is required (devices:)", path)
	}
	seen := map[string]bool{}
	for i := range f.Devices {
		s := &f.Devices[i]
		if s.Name == "" {
			return nil, fmt.Errorf("%s: devices[%d]: name is required", path, i)
		}
		if strings.ContainsAny(s.Name, "/\\ ") {
			return nil, fmt.Errorf("%s: device %q: name must not contain slashes or spaces", path, s.Name)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("%s: duplicate device name %q", path, s.Name)
		}
		seen[s.Name] = true
		if s.Driver == "" {
			return nil, fmt.Errorf("%s: device %q: driver is required", path, s.Name)
		}
		if s.URL == "" && s.SSH == "" {
			return nil, fmt.Errorf("%s: device %q: url or ssh is required", path, s.Name)
		}
		if s.Model == "" {
			s.Model = "auto"
		}
		if s.Control.ApplyTimeout != "" {
			if d, err := time.ParseDuration(s.Control.ApplyTimeout); err != nil || d <= 0 {
				return nil, fmt.Errorf("%s: device %q: control.apply_timeout %q is not a positive duration (e.g. \"90s\")", path, s.Name, s.Control.ApplyTimeout)
			}
		}
		if _, err := ParseSize(s.ReplyLog.MaxSize); err != nil {
			return nil, fmt.Errorf("%s: device %q: reply_log.max_size: %w", path, s.Name, err)
		}
		if s.ReplyLog.Keep < 0 {
			return nil, fmt.Errorf("%s: device %q: reply_log.keep %d: must be 1 or more (unset = 3)", path, s.Name, s.ReplyLog.Keep)
		}
		ports := map[int]bool{}
		for j := range s.Children {
			c := &s.Children[j]
			if c.Port <= 0 {
				return nil, fmt.Errorf("%s: device %q: children[%d]: port must be a port index (1 or more)", path, s.Name, j)
			}
			if ports[c.Port] {
				return nil, fmt.Errorf("%s: device %q: children[%d]: port %d already has a child", path, s.Name, j, c.Port)
			}
			ports[c.Port] = true
			hw, err := net.ParseMAC(strings.TrimSpace(c.MAC))
			if err != nil || len(hw) != 6 {
				return nil, fmt.Errorf("%s: device %q: children[%d]: mac %q is not a 6-byte MAC", path, s.Name, j, c.MAC)
			}
			c.MAC = hw.String()
			if c.Report != "" && c.Report != "lldp" && c.Report != "mac" {
				return nil, fmt.Errorf("%s: device %q: children[%d]: report must be lldp, mac or unset (both), got %q", path, s.Name, j, c.Report)
			}
			if c.Name == "" {
				c.Name = c.MAC
			}
		}
		if s.UDAPIVersion == "" {
			s.UDAPIVersion = "1.0.0"
		}
		if s.UsernameEnv != "" {
			s.Username = os.Getenv(s.UsernameEnv)
		}
		if s.PasswordEnv != "" {
			s.Password = os.Getenv(s.PasswordEnv)
		}
		switch s.Auth {
		case "":
			if s.URL != "" && (s.Username == "" || s.Password == "") {
				return nil, fmt.Errorf("%s: device %q: url needs username/password (set username_env/password_env and export them, or auth: none for a device that has no credentials)", path, s.Name)
			}
		case "none":
			// A credential-less transport, declared rather than inferred.
		default:
			return nil, fmt.Errorf("%s: device %q: auth must be \"none\" or unset, got %q", path, s.Name, s.Auth)
		}
	}
	return &f, nil
}

// APIKey returns the controller API key from the environment ("" if unset).
func (c Controller) APIKey() string { return os.Getenv(c.APIKeyEnv) }
