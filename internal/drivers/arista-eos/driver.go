package aristaeos

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Driver is the Arista EOS driver: eAPI (JSON-RPC over HTTPS) or SSH.
//
// Config: URL = https://<switch>/command-api with Username/Password (eAPI,
// preferred), or SSH = user@host[:port] (key auth via ssh-agent or
// ~/.ssh/id_*, host key from ~/.ssh/known_hosts; no password support).
// Options: ssh_key_user = EOS user that receives the controller's SSH keys
// (default: the bridge's own user); dhcp_guard = exact (default) | edge,
// which ports the DHCP guard reaches, the 7160 matching no VLAN in an ACL
// (docs/drivers/arista-eos.md §9.2).
//
// Written against EOS 4.26.14M on a DCS-7160-48TC6-F; see
// docs/drivers/arista-eos.md for what that version does and does not have.
type Driver struct{}

func init() { devicemodel.RegisterDriver(Driver{}) }

func (Driver) Name() string { return "arista-eos" }

// Capabilities is what EOS 4.26 on the 7160 honours: STP (priority, path
// cost, BPDU guard), jumbo, FEC, LACP, storm control (percent), IGMP
// snooping, LLDP-MED, SNMP, DHCP snooping as the driver's per-port guard
// ACLs and the controller's IP rules in the same lists (acl.go). Not
// claimed: MAC rules (below), port isolation (no protected-port equivalent), L3, dot1x, MC-LAG, PTP. Mirror
// and aggregate session counts mirror what real switches report (the ECS
// reports them) so the UI offers both.
func (c *Collector) Capabilities() devicemodel.Capabilities {
	return devicemodel.Capabilities{
		STP: true, BPDUGuard: true, STPPortCost: true, Jumbo: true, FEC: true, LACP: true,
		StormControl: true, IGMPSnooping: true, LLDPMED: true, SNMP: true,
		// DHCPSnooping: EOS 4.26's own DHCP snooping is Option-82 insertion
		// with no trusted-port model, so the guard is an ingress ACL per
		// interface, programmed and read back on the switch's test ports
		// 2026-11-14 (docs/drivers/arista-eos.md §9). The 7160 matches no
		// VLAN in an ACL, so options.dhcp_guard decides which ports it
		// reaches (§9.2).
		DHCPSnooping: true,
		// IPACLs: the controller's IP rules go into every interface's list
		// after its guard, sized from the chip's counter pool and entry
		// capacity, measured on the test ports 2026-11-14 (acl.go
		// maxIPRules, docs/drivers/arista-eos.md §9.4). Not MACACLs: no
		// MAC ACL binds to a switchport on the 7160 (§9.2).
		IPACLs:         maxIPRules,
		MirrorSessions: 1, AggregateSessions: 8,
	}
}

var _ devicemodel.Capable = (*Collector)(nil)

func (Driver) Describe() string {
	return "Arista EOS over eAPI (URL + username/password) or SSH (user@host, key auth); verified on 4.26.14M"
}

// Open builds the transport, then runs every command once via Start so a
// command this EOS version lacks — or a bad credential — fails here.
func (Driver) Open(ctx context.Context, cfg devicemodel.DriverConfig) (devicemodel.Device, error) {
	var (
		t      Transport
		host   string
		redial func(string) Transport
	)
	switch {
	case cfg.URL != "":
		if cfg.Username == "" || cfg.Password == "" {
			return nil, errors.New("arista-eos: eAPI needs a username and password")
		}
		t = NewEAPI(cfg.URL, cfg.Username, cfg.Password, true, 20*time.Second)
		host, redial = eapiHost(cfg.URL), func(h string) Transport {
			return NewEAPI(replaceHost(cfg.URL, h), cfg.Username, cfg.Password, true, 20*time.Second)
		}
	case cfg.SSH != "":
		user, host, ok := strings.Cut(cfg.SSH, "@")
		if !ok {
			return nil, fmt.Errorf("arista-eos: SSH target must be user@host, got %q", cfg.SSH)
		}
		t = NewSSH(host, user)
		sshHost := host
		host, redial = strings.Split(sshHost, ":")[0], func(h string) Transport {
			if _, port, ok := strings.Cut(sshHost, ":"); ok {
				h += ":" + port
			}
			return NewSSH(h, user)
		}
	default:
		return nil, errors.New("arista-eos: set an eAPI URL or an SSH target")
	}
	c := NewCollector(t)
	c.host, c.redial = host, redial
	c.mgmtPort = mgmtPort(cfg)
	if cfg.Username != "" {
		c.SetSSHKeyUser(cfg.Username) // controller SSH keys land on the bridge's own user
	} else if cfg.SSH != "" {
		user, _, _ := strings.Cut(cfg.SSH, "@")
		c.SetSSHKeyUser(user)
	}
	if u := cfg.Options["ssh_key_user"]; u != "" {
		c.SetSSHKeyUser(u)
	}
	switch m := cfg.Options["dhcp_guard"]; m {
	case "", guardExact:
	case guardEdge:
		c.dhcpGuard = guardEdge
	default:
		t.Close()
		return nil, fmt.Errorf("arista-eos: options.dhcp_guard %q: want %s or %s", m, guardExact, guardEdge)
	}
	return c, nil
}

// Compile-time checks: the collector is a full read/write Device.
var (
	_ devicemodel.Device           = (*Collector)(nil)
	_ devicemodel.Controller       = (*Collector)(nil)
	_ devicemodel.DeviceController = (*Collector)(nil)
	_ devicemodel.VLANController   = (*Collector)(nil)
	_ devicemodel.PortCycler       = (*Collector)(nil)
	_ devicemodel.Rebooter         = (*Collector)(nil)
	_ devicemodel.SSHKeyInstaller  = (*Collector)(nil)
)

// mgmtPort is the TCP port the bridge reaches the switch on: the eAPI
// URL's (443 for https, 80 for http, when it names none), or the SSH
// target's (22 when it names none).
func mgmtPort(cfg devicemodel.DriverConfig) int {
	if cfg.URL != "" {
		u, err := url.Parse(cfg.URL)
		if err != nil {
			return 0
		}
		if p, err := strconv.Atoi(u.Port()); err == nil {
			return p
		}
		if u.Scheme == "http" {
			return 80
		}
		return 443
	}
	if _, hp, ok := strings.Cut(cfg.SSH, "@"); ok {
		if _, p, ok := strings.Cut(hp, ":"); ok {
			if n, err := strconv.Atoi(p); err == nil {
				return n
			}
		}
	}
	return 22
}

// eapiHost is the host part of an eAPI URL; replaceHost puts another host in
// its place, keeping scheme, port and path.
func eapiHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func replaceHost(rawURL, host string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if p := u.Port(); p != "" {
		u.Host = net.JoinHostPort(host, p)
	} else {
		u.Host = host
	}
	return u.String()
}

var _ devicemodel.AddressController = (*Collector)(nil)
var _ devicemodel.ApplyBudget = (*Collector)(nil)
var _ devicemodel.Planner = (*Collector)(nil)
