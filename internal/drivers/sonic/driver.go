package sonic

import (
	"context"
	"errors"
	"fmt"
	"net"
	osuser "os/user"
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
// With transport local the bridge runs on the switch itself as that user,
// and <host> is the switch's own in-band address (the one reported, and
// the one an address move starts from); nothing is dialled
// (docs/drivers/sonic-on-switch.md). Options:
//
//	transport    ssh (default): reach the switch over SSH; local: run every
//	             command on this host (bash -c), as the process's user,
//	             which must be the user ssh: names; ssh_key and
//	             known_hosts are then ignored
//	probe        host:port the switch must keep reaching (TCP connect),
//	             which proves an address move (bound to the new address)
//	             and the first STP enable when there is nothing to dial;
//	             required with transport local, and filled by the bridge
//	             from the controller's inform address unless set
//	ssh_key      private key file, or a .pub that selects that one ssh-agent
//	             key (default: every agent key one at a time, then
//	             ~/.ssh/id_ed25519, id_rsa)
//	known_hosts  known_hosts file (default ~/.ssh/known_hosts)
//	native_vlan  the SONiC VLAN that is UniFi's untagged Default network,
//	             2..4094 (default 4094). SONiC refuses to create VLAN 1, so
//	             a port that is an untagged member of Vlan<native_vlan> is
//	             reported with native VLAN 1, an address learned there is on
//	             VLAN 1, and the VLAN list shows 1 in its place. Pick an id
//	             no UniFi network uses (docs/drivers/sonic.md §5.2a).
//	l2d          off: the bridge installs nothing on the switch for spanning
//	             tree. By default (on) an image without FEATURE|stp gets
//	             RSTP, BPDU guard, auto edge and loop protection through
//	             disunified-l2d -- mstpd built on the switch, its port
//	             states mirrored into orchagent's StpOrch (docs/drivers/
//	             sonic.md §11.5) -- and claims them, so the controller's
//	             settings apply as on its own switches.
//
// Verified on SONiC 202505.1238524-ff3c0c9f6 (virtual switch, hardware SKU
// Seastone-DX010). Control (docs/drivers/sonic.md §6: ports, VLANs, STP,
// NTP, syslog, SNMP, SSH keys, reboot, the static address; §7.5: time
// zone, hostname, locate via the status LED, dynamic port breakout -- the
// last two hardware-untested) writes `sudo config …` commands, so the login user needs passwordless sudo (the admin
// user has it). The managed NTP/syslog/SNMP entries are remembered in
// <state_dir>/sonic-managed.json (state_dir is set by the bridge, next to
// device.json). Over SSH an address change dials the new address with the
// same key and known_hosts, so known_hosts must carry the switch's host key
// for the new address before the change; on the switch (transport local)
// it connects to the probe address from the new one instead.
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "sonic" }

func (Driver) Describe() string {
	return "community SONiC switch over SSH (admin@host, key auth, passwordless sudo), or on the switch itself (options.transport: local, as the login user); Redis tables via swsscommon; port control via config"
}

// Open builds the runner (SSH, or local on the switch itself) and the
// collector; Start then runs the collector once so a bad key or a host
// that is not SONiC fails there.
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
	transport := strings.ToLower(strings.TrimSpace(cfg.Options["transport"]))
	var c *Collector
	switch transport {
	case "", "ssh":
		c = openSSH(user, host, cfg.Options)
	case "local":
		if c, err = openLocal(user, host, cfg.Options); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("sonic: transport must be ssh or local, got %q", transport)
	}
	c.NativeVLAN = native
	c.StateDir = strings.TrimSpace(cfg.Options["state_dir"])
	switch v := strings.ToLower(strings.TrimSpace(cfg.Options["l2d"])); v {
	case "", "on", "true", "yes":
	case "off", "false", "no":
		c.L2 = false
	default:
		return nil, fmt.Errorf("sonic: l2d must be on or off, got %q", v)
	}
	c.configHost = c.Dial
	if err := c.loadManaged(); err != nil {
		return nil, err
	}
	// An address move adopted another address: dial that one (the config
	// file's stays a fallback target).
	if c.savedDial != "" && c.savedDial != c.Dial {
		c.Log.Printf("sonic: dialling %s, the address an earlier move adopted (%s/%s), not the configured %s (which stays a fallback)", c.savedDial, c.StateDir, managedFile, c.Dial)
		c.r, c.Dial = c.Redial(c.savedDial), c.savedDial
	}
	return c, nil
}

// openSSH is the collector for a switch reached over SSH: a fresh dial is
// how a new address and a first STP enable are proven.
func openSSH(user, host string, opts map[string]string) *Collector {
	t := sshrun.New(host, user)
	t.KeyFile = opts["ssh_key"]
	t.KnownHosts = opts["known_hosts"]
	c := NewCollector(t)
	logf := func(f string, a ...any) { c.Log.Printf(f, a...) }
	t.Logf = logf
	c.Dial = host
	port := ""
	if h, p, err := net.SplitHostPort(host); err == nil {
		c.Dial, port = h, p
	}
	keyFile, knownHosts := opts["ssh_key"], opts["known_hosts"]
	c.Redial = func(h string) sshrun.Runner {
		addr := h
		if port != "" {
			addr = net.JoinHostPort(h, port)
		}
		r := sshrun.New(addr, user)
		r.KeyFile, r.KnownHosts, r.Logf = keyFile, knownHosts, logf
		return r
	}
	return c
}

// openLocal is the collector for the bridge on the switch itself
// (docs/drivers/sonic-on-switch.md §2): commands run as the service's own
// user, which must be the login user ssh: names -- the controller's SSH
// keys go into that user's authorized_keys and collect.py reads its home --
// and the host part of ssh: stays the switch's own address, the one the
// loop reports and an address move starts from. With nothing to dial, a
// TCP connect to options.probe is the proof (§3).
func openLocal(user, host string, opts map[string]string) (*Collector, error) {
	me, err := osuser.Current()
	if err != nil {
		return nil, fmt.Errorf("sonic: transport local: who runs this bridge? %w", err)
	}
	if me.Username != user {
		return nil, fmt.Errorf("sonic: transport local runs as %s, but ssh: names %s; the service must run as the login user", me.Username, user)
	}
	probe := strings.TrimSpace(opts["probe"])
	if probe == "" {
		return nil, errors.New("sonic: transport local proves an address move and the first STP enable by reaching options.probe; set options.probe host:port, or let the bridge fill it from the controller")
	}
	if _, _, err := net.SplitHostPort(probe); err != nil {
		return nil, fmt.Errorf("sonic: options.probe must be host:port, got %q", probe)
	}
	c := NewCollector(&sshrun.Local{})
	c.Redial = func(string) sshrun.Runner { return &sshrun.Local{} }
	c.Dial = host
	if h, _, err := net.SplitHostPort(host); err == nil {
		c.Dial = h
	}
	c.Probe, c.ProbeAddr = tcpProbe, probe
	for _, k := range []string{"ssh_key", "known_hosts"} {
		if opts[k] != "" {
			c.Log.Printf("sonic: transport local: options.%s is ignored (nothing is dialled)", k)
		}
	}
	c.Log.Printf("sonic: transport local: commands run on this switch as %s; reachability is proven against %s", user, probe)
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
	_ devicemodel.AddressReporter   = (*Collector)(nil)
	_ devicemodel.Locator           = (*Collector)(nil)
	_ devicemodel.ApplyBudget       = (*Collector)(nil)
)
