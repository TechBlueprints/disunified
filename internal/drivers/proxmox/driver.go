// Package proxmox presents a Proxmox VE node's Linux bridge (vmbr0) as a
// switch: every guest NIC on the bridge is a port, the physical NICs the
// bridge uplinks through take the last ports, and the controller's per-port
// state and VLAN config are written back as the guest's net options.
//
// Written against Proxmox VE 9.1 (Debian 13, kernel 6.17) with a VLAN-aware
// bridge (`bridge-vlan-aware yes`); fixtures in docs/fixtures/proxmox-9.1.6.
// Everything is read over one SSH exec per poll (collect.sh) and written
// with `qm set` / `pct set`, so the node's own tooling keeps its config
// consistent and persistent. See docs/drivers/proxmox.md.
package proxmox

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Driver is the Proxmox VE driver.
//
// Config: SSH = root@<node> (key auth; the ssh-agent, or ssh_key). Options:
//
//	bridge        the bridge to present (default vmbr0)
//	ports         total ports to present (default 54, the USW Leaf's); the
//	              node's physical NICs take the top ones, guests the rest
//	ssh_key       private key file (default: agent, ~/.ssh/id_ed25519, id_rsa)
//	known_hosts   known_hosts file (default ~/.ssh/known_hosts)
//	numbering     "cluster" (default: every guest in the cluster has the same
//	              port on every node; 48 guest NICs cluster-wide) or "node"
//	              (only this node's guests; 48 per node; a migrated guest takes
//	              a free slot on the destination). Only "cluster" has been
//	              run live. A NIC's port is recorded in the guest's own
//	              Proxmox tags (tags.go); the bridge keeps no file.
//	manage_lldpd  "false" leaves lldpd alone (default: the driver keeps
//	              /etc/lldpd.d/disunified.conf current, docs/drivers/proxmox.md §4)
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "proxmox" }

func (Driver) Describe() string {
	return "Proxmox VE node bridge (vmbr0) over SSH (root@node, key auth); guests are ports; verified on PVE 9.1"
}

// Open builds the SSH runner and the collector; Start then runs the
// collector script once so a missing tool or bad key fails here.
func (Driver) Open(ctx context.Context, cfg devicemodel.DriverConfig) (devicemodel.Device, error) {
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
	if cfg.Options["manage_lldpd"] == "false" {
		c.ManageLLDP = false
	}
	switch cfg.Options["numbering"] {
	case "", "cluster":
	case "node":
		c.NodeNumbering = true
	default:
		return nil, fmt.Errorf("proxmox: numbering must be \"cluster\" or \"node\", got %q", cfg.Options["numbering"])
	}
	c.cycleDelay = 3 * time.Second
	return c, nil
}

// Compile-time checks.
var (
	_ devicemodel.Device           = (*Collector)(nil)
	_ devicemodel.Controller       = (*Collector)(nil)
	_ devicemodel.Planner          = (*Collector)(nil)
	_ devicemodel.DeviceController = (*Collector)(nil)
	_ devicemodel.VLANController   = (*Collector)(nil)
	_ devicemodel.PortCycler       = (*Collector)(nil)
	_ devicemodel.Capable          = (*Collector)(nil)
	_ devicemodel.Namer            = (*Collector)(nil)
)
