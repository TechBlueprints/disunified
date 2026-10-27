package sonic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// Driver presents a community SONiC switch: CONFIG_DB PORT entries are the
// ports (a broken-out cage folded into one), the rest from the Redis
// tables listed in docs/drivers/sonic.md §5.1.
//
// Config: SSH = admin@<host> (key auth; the ssh-agent, or ssh_key). The
// login user needs no sudo: collect.py reads Redis through swsscommon.
// Options:
//
//	ssh_key      private key file (default: agent, ~/.ssh/id_ed25519, id_rsa)
//	known_hosts  known_hosts file (default ~/.ssh/known_hosts)
//
// Verified on SONiC 202505.1238524-ff3c0c9f6 (virtual switch, hardware SKU
// Seastone-DX010). Read-only: no port control yet.
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "sonic" }

func (Driver) Describe() string {
	return "community SONiC switch over SSH (admin@host, key auth); Redis tables via swsscommon; read-only"
}

// Open builds the SSH runner and the collector; Start then runs the
// collector once so a bad key or a host that is not SONiC fails there.
func (Driver) Open(ctx context.Context, cfg devicemodel.DriverConfig) (devicemodel.Device, error) {
	if cfg.SSH == "" {
		return nil, errors.New("sonic: set ssh: admin@host (key auth)")
	}
	user, host, ok := strings.Cut(cfg.SSH, "@")
	if !ok || user == "" || host == "" {
		return nil, fmt.Errorf("sonic: SSH target must be user@host, got %q", cfg.SSH)
	}
	t := sshrun.New(host, user)
	t.KeyFile = cfg.Options["ssh_key"]
	t.KnownHosts = cfg.Options["known_hosts"]
	return NewCollector(t), nil
}

// Compile-time contract: what this driver is, and deliberately is not.
var (
	_ devicemodel.Driver  = Driver{}
	_ devicemodel.Device  = (*Collector)(nil)
	_ devicemodel.Capable = (*Collector)(nil)
	_ devicemodel.Namer   = (*Collector)(nil)
)
