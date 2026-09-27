package podman

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// Driver presents a Podman host as a switch: the host is the switch, its
// container network endpoints are the ports, its NICs the top ports.
//
// Config: SSH = root@<host> (key auth; the ssh-agent, or ssh_key). Options:
//
//	ssh_key      private key file (default: agent, ~/.ssh/id_ed25519, id_rsa)
//	known_hosts  known_hosts file (default ~/.ssh/known_hosts)
//	uplink       the NIC to present as the uplink (default: the default route's)
//	slots_file   where the host keeps endpoint -> port assignments
//	             (default /var/lib/disunified/podman-slots.json)
//
// Verified on Podman 5.8 / AlmaLinux 10 (rootful, netavark). Read-only:
// no port control yet.
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "podman" }

func (Driver) Describe() string {
	return "Podman host over SSH (root@host, key auth); container network endpoints are ports, NICs the uplinks; read-only"
}

// Open builds the SSH runner and the collector; Start then runs the
// collector once so a bad key or a host without podman fails there.
func (Driver) Open(ctx context.Context, cfg devicemodel.DriverConfig) (devicemodel.Device, error) {
	if cfg.SSH == "" {
		return nil, errors.New("podman: set ssh: user@host (key auth)")
	}
	user, host, ok := strings.Cut(cfg.SSH, "@")
	if !ok || user == "" || host == "" {
		return nil, fmt.Errorf("podman: SSH target must be user@host, got %q", cfg.SSH)
	}
	t := sshrun.New(host, user)
	t.KeyFile = cfg.Options["ssh_key"]
	t.KnownHosts = cfg.Options["known_hosts"]
	c := NewCollector(t)
	c.Uplink = strings.TrimSpace(cfg.Options["uplink"])
	if f := strings.TrimSpace(cfg.Options["slots_file"]); f != "" {
		if !strings.HasPrefix(f, "/") {
			return nil, fmt.Errorf("podman: slots_file must be an absolute path on the host, got %q", f)
		}
		c.SlotsFile = f
	}
	return c, nil
}

// Compile-time contract: what this driver is, and deliberately is not.
var (
	_ devicemodel.Driver  = Driver{}
	_ devicemodel.Device  = (*Collector)(nil)
	_ devicemodel.Capable = (*Collector)(nil)
	_ devicemodel.Namer   = (*Collector)(nil)
)
