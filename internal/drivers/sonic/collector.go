// Package sonic presents a community SONiC switch to the controller. The
// switch state lives in SONiC's Redis databases; one SSH exec per poll runs
// collect.py (`python3 -`, embedded) as the login user, which reads the
// tables the driver needs through swsscommon and prints them as tagged
// JSON sections, plus a few Linux facts. Read-only.
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

//go:embed collect.py
var collectScript string

// collectCommand is the one command a poll runs; the script is its stdin.
const collectCommand = "python3 -"

// Collector reads a SONiC switch and builds the neutral snapshot.
type Collector struct {
	r   sshrun.Runner
	Log *log.Logger

	mu      sync.Mutex
	host    string
	prevCPU cpuTimes
	aliases map[int]string   // port index -> SONiC alias (the faceplate label, "Eth1")
	ifnames map[int][]string // port index -> every interface folded into it
	last    *devicemodel.Snapshot
}

// NewCollector wraps a runner.
func NewCollector(r sshrun.Runner) *Collector {
	return &Collector{r: r, Log: log.Default(), aliases: map[int]string{}, ifnames: map[int][]string{}}
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
	c.Log.Printf("sonic: %s, %s %s, SONiC %s, %d ports (%d up), %d MACs, uplink hint %d", snap.System.Hostname, snap.System.Vendor, snap.System.Model, snap.System.Version, len(snap.Ports), up, len(snap.MACTable), snap.UplinkHint)
	return snap, nil
}

func (c *Collector) Close() error { return c.r.Close() }

// Capabilities: none yet. The read side honours no control; the write side
// will claim STP, FEC, LACP and mirroring as it implements them.
func (c *Collector) Capabilities() devicemodel.Capabilities { return devicemodel.Capabilities{} }

// Collect runs collect.py and builds the snapshot.
func (c *Collector) Collect(ctx context.Context) (*devicemodel.Snapshot, error) {
	out, err := c.r.Run(ctx, collectCommand, collectScript)
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
	"cfg_snmp", "cfg_breakout", "cfg_feature", "lldp_loc", "lldp", "fdb", "counters_map", "counters", "rates",
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

	ports := buildPorts(db)
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
	macs := buildMACTable(db["fdb"], byIfname)
	pi := map[int]int{}
	for i := range ports {
		pi[ports[i].Index] = i
	}
	for _, m := range macs {
		if i, ok := pi[m.PortIndex]; ok {
			ports[i].MACs = append(ports[i].MACs, m)
		}
	}

	sys := buildSystem(db, sec, addrs, routes, neigh)
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
	for _, p := range ports {
		c.aliases[p.Index] = p.alias
		c.ifnames[p.Index] = append([]string(nil), p.Interfaces...)
	}
	c.mu.Unlock()

	snap := &devicemodel.Snapshot{TakenAt: now, System: sys, MACTable: macs, VLANs: vlanIDs(db["cfg_vlan"])}
	for _, p := range ports {
		p.Port.Name = c.PortName(p.Port)
		snap.Ports = append(snap.Ports, p.Port)
	}
	snap.UplinkHint = uplinkHint(snap)
	return snap, nil
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
