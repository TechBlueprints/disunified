// Package sonic presents a community SONiC switch to the controller. The
// switch state lives in SONiC's Redis databases; one SSH exec per poll runs
// collect.py (`python3 -`, embedded) as the login user, which reads the
// tables the driver needs through swsscommon and prints them as tagged
// JSON sections, plus a few Linux facts. Writes are in control.go.
//
// Verified on SONiC 202505.1238524-ff3c0c9f6 (the virtual switch, hardware
// SKU Seastone-DX010); see docs/drivers/sonic.md §5 for the specification
// and docs/fixtures/sonic-202505.1238524-ff3c0c9f6/ for the capture.
package sonic

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

//go:embed collect.py
var collectScript string

const (
	// collectCommand is the one command a poll runs; the script is its stdin.
	collectCommand = "python3 -"

	// DefaultNativeVLAN is the SONiC VLAN presented as UniFi's VLAN 1 when
	// the native_vlan option is not set (docs/drivers/sonic.md §5.2a).
	DefaultNativeVLAN = 4094
)

// Collector reads a SONiC switch and builds the neutral snapshot.
type Collector struct {
	r sshrun.Runner
	// NativeVLAN is the SONiC VLAN that stands in for UniFi's untagged
	// Default network (VLAN 1, which SONiC refuses to create): an untagged
	// member of Vlan<NativeVLAN> is reported as native VLAN 1, and the VLAN
	// list carries 1 in its place. 2..4094; DefaultNativeVLAN unless set.
	NativeVLAN int
	// Dial is the host the bridge reaches the switch on (the host part of
	// the ssh target, as given: an address or a name, resolved each poll).
	// System.DHCP describes that address; "" = unknown, DHCP false.
	Dial string
	Log  *log.Logger

	mu      sync.Mutex
	host    string
	prevCPU cpuTimes
	aliases map[int]string   // port index -> SONiC alias (the faceplate label, "Eth1")
	ifnames map[int][]string // port index -> every interface folded into it
	last    *devicemodel.Snapshot
	state   *swState // CONFIG_DB as the control side diffs it: the last collect plus the writes since
	// siteVLANs are the controller's VLANs in SONiC ids (native_vlan
	// excluded), from the last EnsureVLANs: what "Allow All" tags.
	siteVLANs []int
	warned    map[string]bool

	// Redial opens a runner to the switch at another address, with the same
	// user, key and known_hosts (set by Open; tests set a fixture's). The
	// address controller proves a new address through it.
	Redial func(host string) sshrun.Runner
	// StateDir is where the driver keeps what it owns on the switch (the
	// managed NTP/syslog/SNMP entries), next to device.json; "" = memory only.
	StateDir string
	// Resolve turns an NTP/syslog hostname into an address (default: the
	// system resolver); tests set their own.
	Resolve func(host string) (string, error)

	pinned        map[string]string // NTP/syslog hostname -> the address it was first resolved to
	configHost    string            // the address the config file names (a fallback dial target)
	savedDial     string            // the dial target an address move adopted (persisted)
	pendingDial   string            // the address a move was switching to when it was cut short
	managedNTP    map[string]bool
	managedSyslog map[string]bool
	managedSNMP   string

	zones []zone // the switch's time zones and their POSIX rules, read once per run (zones.go)

	// Locate (locate.go): the status LED blink.
	BlinkInterval time.Duration // between LED toggles; default 1 s
	blinkStop     func()        // stops the running blink and waits for it; nil when none
	noLED         bool          // the platform API could not set the status LED: report only
}

// NewCollector wraps a runner.
func NewCollector(r sshrun.Runner) *Collector {
	return &Collector{r: r, NativeVLAN: DefaultNativeVLAN, Log: log.Default(), aliases: map[int]string{}, ifnames: map[int][]string{}, warned: map[string]bool{},
		managedNTP: map[string]bool{}, managedSyslog: map[string]bool{}, pinned: map[string]string{}}
}

// runner is the current transport (the address controller may swap it).
func (c *Collector) runner() sshrun.Runner {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.r
}

// warnOnce logs a message the first time it is seen in this run.
func (c *Collector) warnOnce(key, format string, args ...any) {
	c.mu.Lock()
	seen := c.warned[key]
	c.warned[key] = true
	host := c.host
	c.mu.Unlock()
	if !seen {
		c.Log.Printf("sonic %s: "+format, append([]any{host}, args...)...)
	}
}

// Start runs the collector once and checks it saw a SONiC switch.
func (c *Collector) Start(ctx context.Context) (*devicemodel.Snapshot, error) {
	snap, err := c.Collect(ctx)
	if err != nil {
		return nil, err
	}
	if snap.System.Version == "" {
		return nil, fmt.Errorf("sonic: %s answered but reported no SONiC build (no /etc/sonic/sonic_version.yml); is this a SONiC switch?", c.host)
	}
	if len(snap.Ports) == 0 {
		return nil, fmt.Errorf("sonic: %s reported no ports in CONFIG_DB PORT; is the database readable by this user?", c.host)
	}
	up := 0
	for _, p := range snap.Ports {
		if p.Up {
			up++
		}
	}
	if err := c.verifyCommands(ctx); err != nil {
		return nil, err
	}
	c.Log.Printf("sonic: %s, %s %s, SONiC %s, %d ports (%d up), %d MACs, uplink hint %d", snap.System.Hostname, snap.System.Vendor, snap.System.Model, snap.System.Version, len(snap.Ports), up, len(snap.MACTable), snap.UplinkHint)
	return snap, nil
}

// Close stops a locate blink and drops the connection.
func (c *Collector) Close() error {
	c.stopBlink()
	return c.runner().Close()
}

// Capabilities are computed from the switch (§6.1): FEC, jumbo, LACP,
// storm control and SNMP always; one mirror session and eight aggregates
// (what is tested); STP, BPDU guard and port cost only when the image has
// the STP daemon (FEATURE|stp). IGMP snooping, LLDP-MED, DHCP snooping and
// isolation never: community SONiC has none of them.
func (c *Collector) Capabilities() devicemodel.Capabilities {
	caps := devicemodel.Capabilities{FEC: true, Jumbo: true, LACP: true, StormControl: true, SNMP: true, MirrorSessions: 1, AggregateSessions: 8}
	c.mu.Lock()
	st := c.state
	c.mu.Unlock()
	if st != nil && st.stpFeature {
		caps.STP, caps.BPDUGuard, caps.STPPortCost = true, true, true
	}
	return caps
}

// Collect runs collect.py and builds the snapshot.
func (c *Collector) Collect(ctx context.Context) (*devicemodel.Snapshot, error) {
	out, err := c.run(ctx, collectCommand, collectScript)
	if err != nil {
		return nil, fmt.Errorf("sonic: collect: %w", err)
	}
	snap, err := c.build(out, time.Now())
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.last = snap
	c.mu.Unlock()
	return snap, nil
}

// required sections: without these the output is not a collector run.
var required = []string{"metadata", "cfg_port", "appl_port", "version", "addr"}

// redisSections is every Redis section collect.py prints.
var redisSections = []string{
	"metadata", "state_metadata", "cfg_port", "appl_port", "state_port", "cfg_vlan", "cfg_vlan_member",
	"cfg_lag", "cfg_lag_member", "appl_lag", "cfg_mgmt", "cfg_stp", "cfg_mirror", "cfg_ntp", "cfg_syslog",
	"cfg_snmp", "cfg_route", "cfg_dns", "cfg_storm", "cfg_breakout", "cfg_feature", "lldp_loc", "lldp", "fdb", "counters_map", "counters", "rates",
	"xcvr", "dom", "fan", "psu", "temp", "chassis", "asic_temp",
}

func (c *Collector) build(out string, now time.Time) (*devicemodel.Snapshot, error) {
	sec := sections(out)
	if _, ok := sec["end"]; !ok {
		return nil, fmt.Errorf("sonic: collector script did not run to completion (%d bytes)", len(out))
	}
	for _, need := range required {
		if _, ok := sec[need]; !ok {
			return nil, fmt.Errorf("sonic: collector output lacks section %q", need)
		}
	}
	db := map[string]table{}
	for _, name := range redisSections {
		t, err := parseTable(name, sec[name])
		if err != nil {
			return nil, err
		}
		db[name] = t
	}
	var addrs []ipAddr
	var routes []ipRoute
	var neigh []ipNeigh
	for _, d := range []struct {
		name string
		v    any
	}{{"addr", &addrs}, {"route", &routes}, {"neigh", &neigh}} {
		if err := decodeJSON(d.name, sec[d.name], d.v); err != nil {
			return nil, err
		}
	}

	_, plat := parsePlatform(sec["platform_json"])
	ports := buildPorts(db, c.NativeVLAN, plat)
	if len(ports) == 0 {
		return nil, fmt.Errorf("sonic: CONFIG_DB PORT is empty")
	}
	byIfname := map[string]int{}
	for _, p := range ports {
		for _, n := range p.Interfaces {
			byIfname[n] = p.Index
		}
	}
	// A LAG's MACs are reported on its lowest member's port.
	for name, idx := range lagPorts(db, byIfname) {
		byIfname[name] = idx
	}
	macs := buildMACTable(db["fdb"], byIfname, c.NativeVLAN)
	pi := map[int]int{}
	for i := range ports {
		pi[ports[i].Index] = i
	}
	for _, m := range macs {
		if i, ok := pi[m.PortIndex]; ok {
			ports[i].MACs = append(ports[i].MACs, m)
		}
	}

	sys := buildSystem(db, sec, addrs, routes, neigh, dialAddrs(c.CurrentAddress()))
	sys.MACTableUsed = len(macs)
	cpu := parseCPUTimes(sec["stat"])
	c.mu.Lock()
	if c.prevCPU.total > 0 && cpu.total > c.prevCPU.total {
		dt := float64(cpu.total - c.prevCPU.total)
		di := float64(cpu.idle - c.prevCPU.idle)
		sys.CPUPercent = float64(int((100*(1-di/dt))*10+0.5)) / 10
	}
	c.prevCPU = cpu
	c.host = sys.Hostname
	c.state = newState(db, sec)
	for _, p := range ports {
		c.aliases[p.Index] = p.alias
		c.ifnames[p.Index] = append([]string(nil), p.Interfaces...)
	}
	c.mu.Unlock()

	snap := &devicemodel.Snapshot{TakenAt: now, System: sys, MACTable: macs, VLANs: vlanIDs(db["cfg_vlan"], c.NativeVLAN)}
	for _, p := range ports {
		p.Port.Name = c.PortName(p.Port)
		snap.Ports = append(snap.Ports, p.Port)
	}
	snap.UplinkHint = uplinkHint(snap)
	return snap, nil
}

// dialAddrs resolves the dialled host as given: an address is itself, a
// name is whatever it resolves to now (nothing if it does not).
func dialAddrs(host string) []string {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return []string{ip.String()}
	}
	ips, err := net.LookupHost(host)
	if err != nil {
		return nil
	}
	return ips
}

// uplinkHint: the port whose LLDP neighbour is a bridge; failing that, the
// up port on which the default gateway's MAC was learned (the switch's way
// out, which is its uplink when the management address is in-band); else 0.
func uplinkHint(snap *devicemodel.Snapshot) int {
	for _, p := range snap.Ports {
		if p.Up && p.Neighbor != nil && p.Neighbor.IsBridge {
			return p.Index
		}
	}
	if gw := snap.System.GatewayMAC; gw != "" {
		for _, m := range snap.MACTable {
			if m.MAC == gw && m.PortIndex > 0 {
				for _, p := range snap.Ports {
					if p.Index == m.PortIndex && p.Up {
						return p.Index
					}
				}
			}
		}
	}
	return 0
}

// lagID is the number in "PortChannel12".
func lagID(name string) int {
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(name[i:])
	return n
}

// lagPorts maps each PortChannel to the index of its lowest member port.
func lagPorts(db map[string]table, byIfname map[string]int) map[string]int {
	out := map[string]int{}
	for k := range db["cfg_lag_member"] {
		parts := strings.Split(k, "|")
		if len(parts) != 3 {
			continue
		}
		idx, ok := byIfname[parts[2]]
		if !ok {
			continue
		}
		if cur, seen := out[parts[1]]; !seen || idx < cur {
			out[parts[1]] = idx
		}
	}
	return out
}
