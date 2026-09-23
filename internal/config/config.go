// Package config is the bridge's configuration file: one controller, any
// number of devices, secrets by environment-variable name. Kept small on
// purpose — the file names what to bridge; everything else is discovered
// from the device or the controller.
package config

import (
	"fmt"
	"os"
	"strings"

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

	URL         string            `yaml:"url"`          // API endpoint (driver-specific)
	SSH         string            `yaml:"ssh"`          // user@host[:port]
	Username    string            `yaml:"username"`     // or UsernameEnv
	UsernameEnv string            `yaml:"username_env"` // env var name
	Password    string            `yaml:"password"`     // discouraged; prefer PasswordEnv
	PasswordEnv string            `yaml:"password_env"` // env var name
	Options     map[string]string `yaml:"options"`      // driver-specific knobs

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
}

// Control says what UniFi owns on this switch. Ports "" or "off" = read-only.
type Control struct {
	Ports string `yaml:"ports"` // "all", or "2,5-8"
	// Outlets is the power-device equivalent of Ports: "all", or "2,5-8".
	// An outlet carries real load, so it is off by default like everything
	// else that writes.
	Outlets string `yaml:"outlets"`
	// Address lets the controller's IP Settings for the device set the
	// device's own management address (static or DHCP). Off by default: a
	// wrong address takes the device off the network, and only a driver that
	// can write its device's TCP/IP config honours it.
	Address bool `yaml:"address"`
	IGMP    bool `yaml:"igmp"`
	NTP     bool `yaml:"ntp"`
	Syslog  bool `yaml:"syslog"`
	Reboot  bool `yaml:"reboot"`
	SSHKeys bool `yaml:"ssh_keys"`
	SNMP    bool `yaml:"snmp"`
	// AllowInitialChanges lets the first push after adoption change ports
	// (default: held until the controller's config matches the switch — the
	// bridge seeds it from the switch when api_url is set).
	AllowInitialChanges bool `yaml:"allow_initial_changes"`
	// NoSeed skips seeding the controller's port config from the switch on
	// adoption (default: seed when api_url is set).
	NoSeed bool `yaml:"no_seed"`
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
		if s.UDAPIVersion == "" {
			s.UDAPIVersion = "1.0.0"
		}
		if s.UsernameEnv != "" {
			s.Username = os.Getenv(s.UsernameEnv)
		}
		if s.PasswordEnv != "" {
			s.Password = os.Getenv(s.PasswordEnv)
		}
		if s.URL != "" && (s.Username == "" || s.Password == "") {
			return nil, fmt.Errorf("%s: device %q: url needs username/password (set username_env/password_env and export them)", path, s.Name)
		}
	}
	return &f, nil
}

// APIKey returns the controller API key from the environment ("" if unset).
func (c Controller) APIKey() string { return os.Getenv(c.APIKeyEnv) }
