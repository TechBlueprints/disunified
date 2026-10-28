package sonic

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
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
//	native_vlan  the SONiC VLAN that is UniFi's untagged Default network,
//	             2..4094 (default 4094). SONiC refuses to create VLAN 1, so
//	             a port that is an untagged member of Vlan<native_vlan> is
//	             reported with native VLAN 1, an address learned there is on
//	             VLAN 1, and the VLAN list shows 1 in its place. Pick an id
//	             no UniFi network uses (docs/drivers/sonic.md §5.2a).
//
// Verified on SONiC 202505.1238524-ff3c0c9f6 (virtual switch, hardware SKU
// Seastone-DX010). Control (docs/drivers/sonic.md §6: ports, VLANs, STP,
// NTP, syslog, SNMP, SSH keys, reboot, the static address) writes `sudo
// config …` commands, so the login user needs passwordless sudo (the admin
// user has it). The managed NTP/syslog/SNMP entries are remembered in
// <state_dir>/sonic-managed.json (state_dir is set by the bridge, next to
// device.json). An address change dials the new address with the same
// key and known_hosts, so known_hosts must carry the switch's host key for
// the new address before the change.
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "sonic" }

func (Driver) Describe() string {
	return "community SONiC switch over SSH (admin@host, key auth, passwordless sudo); Redis tables via swsscommon; port control via config"
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
	native, err := nativeVLANOption(cfg.Options["native_vlan"])
	if err != nil {
		return nil, err
	}
	t := sshrun.New(host, user)
	t.KeyFile = cfg.Options["ssh_key"]
	t.KnownHosts = cfg.Options["known_hosts"]
	c := NewCollector(t)
	c.NativeVLAN = native
	c.Dial = host
	port := ""
	if h, p, err := net.SplitHostPort(host); err == nil {
		c.Dial, port = h, p
	}
	keyFile, knownHosts := cfg.Options["ssh_key"], cfg.Options["known_hosts"]
	c.Redial = func(h string) sshrun.Runner {
		addr := h
		if port != "" {
			addr = net.JoinHostPort(h, port)
		}
		r := sshrun.New(addr, user)
		r.KeyFile, r.KnownHosts = keyFile, knownHosts
		return r
	}
	c.StateDir = strings.TrimSpace(cfg.Options["state_dir"])
	c.configHost = c.Dial
	if err := c.loadManaged(); err != nil {
		return nil, err
	}
	// An address move adopted another address: dial that one (the config
	// file's stays a fallback target).
	if c.savedDial != "" && c.savedDial != c.Dial {
		c.r, c.Dial = c.Redial(c.savedDial), c.savedDial
	}
	return c, nil
}

// nativeVLANOption parses options.native_vlan: unset is DefaultNativeVLAN;
// otherwise an integer SONiC can create, 2..4094.
func nativeVLANOption(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultNativeVLAN, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 2 || n > 4094 {
		return 0, fmt.Errorf("sonic: native_vlan must be a VLAN id SONiC can create, 2..4094 (VLAN 1 does not exist on SONiC; UniFi's VLAN 1 is mapped onto this one), got %q", s)
	}
	return n, nil
}

// Compile-time contract: what this driver is, and deliberately is not.
var (
	_ devicemodel.Driver  = Driver{}
	_ devicemodel.Device  = (*Collector)(nil)
	_ devicemodel.Capable = (*Collector)(nil)
	_ devicemodel.Namer   = (*Collector)(nil)

	_ devicemodel.Controller     = (*Collector)(nil)
	_ devicemodel.Planner        = (*Collector)(nil)
	_ devicemodel.VLANController = (*Collector)(nil)
	_ devicemodel.JumboSwitcher  = (*Collector)(nil)

	_ devicemodel.DeviceController  = (*Collector)(nil)
	_ devicemodel.SSHKeyInstaller   = (*Collector)(nil)
	_ devicemodel.Rebooter          = (*Collector)(nil)
	_ devicemodel.AddressController = (*Collector)(nil)
)
