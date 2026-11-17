// Package proxmox presents a Proxmox VE node's Linux bridge (vmbr0) as a
// switch: every guest NIC on the bridge is a port, the physical NICs the
// bridge uplinks through take the last ports, and the controller's per-port
// state and VLAN config are written back as the guest's net options.
//
// Written against Proxmox VE 9.1 (Debian 13, kernel 6.17) with a VLAN-aware
// bridge (`bridge-vlan-aware yes`); fixtures in docs/fixtures/proxmox-9.1.6.
// Everything is read with one command per poll (collect.sh) and written
// with `qm set` / `pct set`, so the node's own tooling keeps its config
// consistent and persistent. The commands run over SSH as root@node, or,
// with options.transport local, on the node itself under `bash -c` as the
// service's own user (docs/drivers/proxmox-on-node.md). See
// docs/drivers/proxmox.md.
package proxmox

import (
	"context"
	"errors"
	"fmt"
	osuser "os/user"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// Driver is the Proxmox VE driver.
//
// Config: SSH = root@<node> (key auth; the ssh-agent, or ssh_key). With
// transport local the bridge runs on the node itself as root and ssh: is
// not needed; nothing is dialled (docs/drivers/proxmox-on-node.md). The
// options.probe and options.state_dir the bridge sets for every driver are
// not used. Options:
//
//	transport     ssh (default): reach the node over SSH; local: run every
//	              command on this host (bash -c) as the process's user,
//	              which must be the user ssh: names if it is set; ssh_key
//	              and known_hosts are then ignored
//	bridge        the bridge to present (default vmbr0)
//	ports         total ports to present (default 54, the USW Leaf's); the
//	              node's physical NICs take the top ones, guests the rest
//	guest_ports   the number of guest ports, 1..guest_ports (default: ports
//	              less the node's physical NICs, 51 on a node with three);
//	              set it when the nodes' NIC counts differ, so every node
//	              reads the same tags. Start fails when the node's NICs do
//	              not fit above it
//	ssh_key       private key file, or a .pub that selects that one ssh-agent
//	              key (default: every agent key one at a time, then
//	              ~/.ssh/id_ed25519, id_rsa)
//	known_hosts   known_hosts file (default ~/.ssh/known_hosts)
//	numbering     "cluster" (default: every guest in the cluster has the same
//	              port on every node; the guest ports are shared cluster-wide)
//	              or "node" (only this node's guests; the guest ports per node;
//	              a migrated guest takes a free slot on the destination).
//	              Only "cluster" has been run live. A NIC's port is recorded
//	              in the guest's own Proxmox tags (tags.go); the bridge keeps
//	              no file.
//	manage_lldpd  "false" leaves lldpd alone (default: the driver keeps
//	              /etc/lldpd.d/disunified.conf current, docs/drivers/proxmox.md §4)
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "proxmox" }

func (Driver) Describe() string {
	return "Proxmox VE node bridge (vmbr0) over SSH (root@node, key auth), or on the node itself (options.transport: local, as root); guests are ports; verified on PVE 9.1"
}

// Open builds the runner (SSH, or local on the node itself) and the
// collector; Start then runs the collector script once so a missing tool or
// bad key fails here.
func (Driver) Open(ctx context.Context, cfg devicemodel.DriverConfig) (devicemodel.Device, error) {
	c, err := openTransport(cfg)
	if err != nil {
		return nil, err
	}
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
	if v := cfg.Options["guest_ports"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n >= c.Ports {
			return nil, fmt.Errorf("proxmox: guest_ports must be a number from 1 to ports-1 (%d), got %q", c.Ports-1, v)
		}
		c.GuestPorts = n
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

// openTransport builds the collector on the runner options.transport names.
func openTransport(cfg devicemodel.DriverConfig) (*Collector, error) {
	switch v := strings.ToLower(strings.TrimSpace(cfg.Options["transport"])); v {
	case "", "ssh":
		return openSSH(cfg)
	case "local":
		return openLocal(cfg)
	default:
		return nil, fmt.Errorf("proxmox: transport must be ssh or local, got %q", v)
	}
}

// openSSH is the collector for a node reached over SSH (root@node).
func openSSH(cfg devicemodel.DriverConfig) (*Collector, error) {
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
	t.Logf = func(f string, a ...any) { c.Log.Printf(f, a...) }
	return c, nil
}

// openLocal is the collector for the bridge on the node itself
// (docs/drivers/proxmox-on-node.md §4.2): every command runs under bash -c
// as the service's own user. ssh: is optional; if set, its user must be
// that user (its host is not used). There is no probe: this driver moves
// no address and arms no dead-man timer.
func openLocal(cfg devicemodel.DriverConfig) (*Collector, error) {
	me, err := osuser.Current()
	if err != nil {
		return nil, fmt.Errorf("proxmox: transport local: who runs this bridge? %w", err)
	}
	if cfg.SSH != "" {
		user, _, ok := strings.Cut(cfg.SSH, "@")
		if !ok || user == "" {
			return nil, fmt.Errorf("proxmox: SSH target must be user@host, got %q", cfg.SSH)
		}
		if me.Username != user {
			return nil, fmt.Errorf("proxmox: transport local runs as %s, but ssh: names %s; the service must run as the login user", me.Username, user)
		}
	}
	c := NewCollector(&sshrun.Local{})
	var ignored []string
	for _, k := range []string{"ssh_key", "known_hosts"} {
		if cfg.Options[k] != "" {
			ignored = append(ignored, "options."+k)
		}
	}
	if len(ignored) > 0 {
		c.Log.Printf("proxmox: transport local: %s ignored (nothing is dialled)", strings.Join(ignored, " and "))
	}
	c.Log.Printf("proxmox: transport local: commands run on this node as %s", me.Username)
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
