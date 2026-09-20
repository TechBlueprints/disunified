// Package proxmox presents a Proxmox VE node's Linux bridge (vmbr0) as a
// switch: every guest NIC on the bridge is a port, the physical NICs the
// bridge uplinks through take the last ports, and the controller's per-port
// state and VLAN config are written back as the guest's net options.
//
// Written against Proxmox VE 9.1 (Debian 13, kernel 6.17) with a VLAN-aware
// bridge (`bridge-vlan-aware yes`); fixtures in docs/fixtures/proxmox-9.1.6.
// Everything is read over one SSH exec per poll (collect.sh) and written
// with `qm set` / `pct set`, so the node's own tooling keeps its config
// consistent and persistent. See docs/proxmox.md.
package proxmox

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// Driver is the Proxmox VE driver.
//
// Config: SSH = root@<node> (key auth; the ssh-agent, or ssh_key). Options:
//
//	bridge        the bridge to present (default vmbr0)
//	ports         total ports to present (default 54, the USW Leaf's)
//	uplink_ports  how many of the last ports are physical uplinks (default 6)
//	ssh_key       private key file (default: agent, ~/.ssh/id_ed25519, id_rsa)
//	known_hosts   known_hosts file (default ~/.ssh/known_hosts)
//	manage_lldpd  "false" leaves lldpd alone (default: the driver keeps
//	              /etc/lldpd.d/switch-to-unifi.conf current, docs/proxmox.md §4)
//	state_dir     where the guest-to-port map persists (set by the bridge)
type Driver struct{}

func init() { switchmodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "proxmox" }

func (Driver) Describe() string {
	return "Proxmox VE node bridge (vmbr0) over SSH (root@node, key auth); guests are ports; verified on PVE 9.1"
}

// Open builds the SSH runner and the collector; Start then runs the
// collector script once so a missing tool or bad key fails here.
func (Driver) Open(ctx context.Context, cfg switchmodel.DriverConfig) (switchmodel.Switch, error) {
	if cfg.SSH == "" {
		return nil, errors.New("proxmox: set ssh: user@node (key auth)")
	}
	user, host, ok := strings.Cut(cfg.SSH, "@")
	if !ok {
		return nil, fmt.Errorf("proxmox: SSH target must be user@host, got %q", cfg.SSH)
	}
	t := NewSSH(host, user)
	t.KeyFile = cfg.Options["ssh_key"]
	t.KnownHosts = cfg.Options["known_hosts"]
	c := NewCollector(t)
	if b := cfg.Options["bridge"]; b != "" {
		c.Bridge = b
	}
	if v := cfg.Options["ports"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 2 {
			return nil, fmt.Errorf("proxmox: ports must be a number >= 2, got %q", v)
		}
		c.Ports = n
	}
	if v := cfg.Options["uplink_ports"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n >= c.Ports {
			return nil, fmt.Errorf("proxmox: uplink_ports must be between 1 and ports-1, got %q", v)
		}
		c.UplinkPorts = n
	}
	if cfg.Options["manage_lldpd"] == "false" {
		c.ManageLLDP = false
	}
	pm, err := loadPortMap(cfg.Options["state_dir"])
	if err != nil {
		return nil, fmt.Errorf("proxmox: port map: %w", err)
	}
	c.ports = pm
	c.cycleDelay = 3 * time.Second
	return c, nil
}

// Compile-time checks.
var (
	_ switchmodel.Switch           = (*Collector)(nil)
	_ switchmodel.Controller       = (*Collector)(nil)
	_ switchmodel.SwitchController = (*Collector)(nil)
	_ switchmodel.VLANController   = (*Collector)(nil)
	_ switchmodel.PortCycler       = (*Collector)(nil)
	_ switchmodel.Capable          = (*Collector)(nil)
	_ switchmodel.Namer            = (*Collector)(nil)
)
