package aristaeos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// Driver is the Arista EOS driver: eAPI (JSON-RPC over HTTPS) or SSH.
//
// Config: URL = https://<switch>/command-api with Username/Password (eAPI,
// preferred), or SSH = user@host[:port] (key auth via ssh-agent or
// ~/.ssh/id_*, host key from ~/.ssh/known_hosts; no password support).
// Options: ssh_key_user = EOS user that receives the controller's SSH keys
// (default: the bridge's own user).
//
// Written against EOS 4.26.14M on a DCS-7160-48TC6-F; see
// docs/drivers/arista-eos.md for what that version does and does not have.
type Driver struct{}

func init() { switchmodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "arista-eos" }

// Capabilities is what EOS 4.26 on the 7160 honours: STP (priority, path
// cost, BPDU guard), jumbo, FEC, LACP, storm control (percent), IGMP
// snooping, LLDP-MED, SNMP. Not claimed: DHCP snooping (below), port isolation
// (no protected-port equivalent), L3, dot1x, MC-LAG, PTP. Mirror and
// aggregate session counts mirror what real switches report (the ECS
// reports them) so the UI offers both.
func (c *Collector) Capabilities() switchmodel.Capabilities {
	return switchmodel.Capabilities{
		STP: true, BPDUGuard: true, STPPortCost: true, Jumbo: true, FEC: true, LACP: true,
		StormControl: true, IGMPSnooping: true, LLDPMED: true, SNMP: true,
		// Not DHCPSnooping: UniFi's "Rogue DHCP Server Detection" blocks DHCP
		// servers on non-uplink ports, and EOS 4.26 has no trusted-port model
		// (`ip dhcp snooping trust` is invalid); its snooping is Option-82
		// insertion only. Unclaimed, the controller never pushes the key
		// (verified 2026-09-20); if it does, ApplySwitch logs and ignores it.
		MirrorSessions: 1, AggregateSessions: 8,
	}
}

var _ switchmodel.Capable = (*Collector)(nil)

func (Driver) Describe() string {
	return "Arista EOS over eAPI (URL + username/password) or SSH (user@host, key auth); verified on 4.26.14M"
}

// Open builds the transport, then runs every command once via Start so a
// command this EOS version lacks — or a bad credential — fails here.
func (Driver) Open(ctx context.Context, cfg switchmodel.DriverConfig) (switchmodel.Switch, error) {
	var t Transport
	switch {
	case cfg.URL != "":
		if cfg.Username == "" || cfg.Password == "" {
			return nil, errors.New("arista-eos: eAPI needs a username and password")
		}
		t = NewEAPI(cfg.URL, cfg.Username, cfg.Password, true, 20*time.Second)
	case cfg.SSH != "":
		user, host, ok := strings.Cut(cfg.SSH, "@")
		if !ok {
			return nil, fmt.Errorf("arista-eos: SSH target must be user@host, got %q", cfg.SSH)
		}
		t = NewSSH(host, user)
	default:
		return nil, errors.New("arista-eos: set an eAPI URL or an SSH target")
	}
	c := NewCollector(t)
	if cfg.Username != "" {
		c.SetSSHKeyUser(cfg.Username) // controller SSH keys land on the bridge's own user
	} else if cfg.SSH != "" {
		user, _, _ := strings.Cut(cfg.SSH, "@")
		c.SetSSHKeyUser(user)
	}
	if u := cfg.Options["ssh_key_user"]; u != "" {
		c.SetSSHKeyUser(u)
	}
	return c, nil
}

// Compile-time checks: the collector is a full read/write Switch.
var (
	_ switchmodel.Switch           = (*Collector)(nil)
	_ switchmodel.Controller       = (*Collector)(nil)
	_ switchmodel.SwitchController = (*Collector)(nil)
	_ switchmodel.VLANController   = (*Collector)(nil)
	_ switchmodel.PortCycler       = (*Collector)(nil)
	_ switchmodel.Rebooter         = (*Collector)(nil)
	_ switchmodel.SSHKeyInstaller  = (*Collector)(nil)
)
