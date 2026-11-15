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
	fan     *fanState        // the last collect's fancontrol section (fan.go); nil = none
	aliases map[int]string   // port index -> SONiC alias (the faceplate label, "Eth1")
	ifnames map[int][]string // port index -> every interface folded into it
	last    *devicemodel.Snapshot
	state   *swState // CONFIG_DB as the control side diffs it: the last collect plus the writes since
	// siteVLANs are the controller's VLANs in SONiC ids (native_vlan
	// excluded), from the last EnsureVLANs: what "Allow All" tags.
	siteVLANs []int
	// siteKnown: an EnsureVLANs of this run has set siteVLANs from a push,
	// the condition for deleting a VLAN the push no longer names
	// (pruneVLANs). laneVLANs are an attached lane switch's own list per
	// cage (EnsureLaneVLANs), kept by the prune and tagged by its lanes'
	// Allow All.
	siteKnown bool
	laneVLANs map[int][]int
	// aclFrom is the device intent the ACL rules were last derived from
	// (aclSet): the DHCP guard's rules follow the VLANs on the switch.
	aclFrom *devicemodel.DeviceDesired
	warned    map[string]bool
	// krSeen is the snapshot state the kernel default route was last
	// checked against, krMisses how many collects in a row found it missing
	// while FRR reports it installed (kroute.go).
	krSeen   *swState
	krMisses int

	// Redial opens a runner to the switch at another address, with the same
	// user, key and known_hosts (set by Open; tests set a fixture's). The
	// address controller proves a new address through it.
	Redial func(host string) sshrun.Runner
	// Probe proves the switch still reaches the network where there is
	// nothing to dial (transport local, probe.go): ProbeAddr is the
	// host:port it connects to. Probe is nil on the SSH transport.
	Probe     probeFunc
	ProbeAddr string
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
	// managedL3 is what the driver added for routed networks (l3.go):
	// interface addresses, routes, relays, DHCP servers and the transit
	// hop of the default route.
	managedL3 l3Managed
	// l3 is the routing the last push asked for (ApplyDevice), the
	// reconcile's target; nil before the first push.
	l3 *devicemodel.DeviceDesired
	// managedBGP is the BGP file the driver loaded into FRR and the
	// objects it added (bgp.go); bgp the BGP router the last push asked
	// for, nil before the first push.
	managedBGP bgpManaged
	bgp        *devicemodel.DeviceDesired
	// ospf is the OSPF router the last push asked for (ospf.go), nil
	// before the first push.
	ospf *devicemodel.DeviceDesired

	zones []zone // the switch's time zones and their POSIX rules, read once per run (zones.go)

	// chrony left offline by its boot hook (§10.6, chronyNudge): the
	// snapshot the last `chronyc onoffline` ran for (once per collect), and
	// whether an episode of every source unreachable is on (logged once).
	chronyNudged *devicemodel.Snapshot
	chronyStuck  bool

	// Locate (locate.go): the status LED blink.
	BlinkInterval time.Duration // between LED toggles; default 1 s
	blinkStop     func()        // stops the running blink and waits for it; nil when none
	noLED         bool          // the platform API could not set the status LED: report only

	// Spanning tree and loop protection through disunified-l2d (l2.go):
	// on unless options.l2d is off (docs/drivers/sonic.md §11.5).
	L2 bool
	l2 l2Want
	// L2Wait is the pause before each check that a first STP enable left
	// the switch reachable (default 5 s; tests set 0).
	L2Wait *time.Duration
	// StartWait bounds how long Start waits for a switch that is still
	// starting, collecting again every StartPoll (starting.go, §6.4.2);
	// starting is why the last collect found it still starting, "" if not.
	StartWait, StartPoll time.Duration
	starting             string

	// DHCP guarding and the IP ACL rules (acl.go): the tables and rules the
	// last push asks for, and whether a write awaits orchagent's verdict
	// (logged by the next collect).
	acl        aclWant
	aclWritten bool
	// macACL: this image's orchagent has the L2 table type with the outer
	// MAC matches (the image kit's sonic-swss 0003), checked once at Start
	// (macACLCheckCommand); MAC rules are claimed and written only then.
	macACL bool
	// l2mcFixed: this image's orchagent has the image kit's sonic-swss 0004
	// (a port's L2MC group members go with its VLAN membership and its
	// bridge port), checked once at Start (l2mcFixCheckCommand). Without
	// it a port that leaves its last VLAN waits for its bridge port to go
	// (bridgePortWait, §8.15).
	l2mcFixed bool
	// IGMP snooping through l2mcd (igmp.go): off unless options.igmp is on
	// (docs/drivers/sonic.md §8.14: the claim waits for the traffic test),
	// and the snooping intent of the last push.
	IGMP bool
	igmp igmpWant
	// sshFrom / sshTo are the last collect's SSH connection as the switch
	// saw it (collect.py's ssh section): the bridge's address and the
	// switch address it dialled; "" when not reported.
	sshFrom, sshTo string

	// Lane switches (lanes.go, docs/drivers/breakout.md): each split cage's
	// lanes as of the last collect, the lane switches attached to cages,
	// the cages the parent's own config has disabled, and the switch's own
	// LLDP chassis id (what a lane switch sees as its uplink's neighbour).
	lanes     map[int][]cagePort
	children  map[int]devicemodel.Attachment
	cageDown  map[int]bool
	chassisID string
	// applySem serialises the writes and the collect: a lane switch's apply
	// runs from its own loop, beside this device's (lockApply).
	applySem  chan struct{}
	applyOnce sync.Once
}

// NewCollector wraps a runner.
func NewCollector(r sshrun.Runner) *Collector {
	return &Collector{r: r, NativeVLAN: DefaultNativeVLAN, L2: true, Log: log.Default(), aliases: map[int]string{}, ifnames: map[int][]string{}, warned: map[string]bool{},
		managedNTP: map[string]bool{}, managedSyslog: map[string]bool{}, pinned: map[string]string{},
		StartWait: defaultStartWait, StartPoll: defaultStartPoll}
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

// Start runs the collector once the switch has finished starting
// (startSnapshot) and checks it saw a SONiC switch.
func (c *Collector) Start(ctx context.Context) (*devicemodel.Snapshot, error) {
	snap, err := c.startSnapshot(ctx)
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
	c.checkMACACL(ctx)
	c.checkL2MCFix(ctx)
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
// (what is tested). STP, BPDU guard and port cost through the image's STP
// daemon where it has one (FEATURE|stp, PVST); otherwise RSTP, BPDU guard,
// auto edge, port cost and loop protection through disunified-l2d, where
// the switch can run it (mstpd installed, or docker to build it; §11.5).
// DHCP snooping (as hardware DHCP guarding) and IP ACL rules through the
// driver's own ACL tables wherever the collector reads them (§11.7.5); MAC
// ACL rules where the image's orchagent has the L2 table type (§11.7.8;
// the stock orchagent has none). IGMP
// snooping, advanced (querier, timers, reserved groups) as the ECS claims
// it, through l2mcd on an image with the series when options.igmp is on
// (igmp.go). Port isolation, the MAC filter, the egress limit and
// LLDP-MED where the collector reads their state (portfeat.go, §11.12);
// L3 routing where FRR runs (l3.go, §11.13).
func (c *Collector) Capabilities() devicemodel.Capabilities {
	caps := devicemodel.Capabilities{FEC: true, Jumbo: true, LACP: true, StormControl: true, SNMP: true, MirrorSessions: 1, AggregateSessions: 8}
	c.mu.Lock()
	st := c.state
	c.mu.Unlock()
	switch {
	case st == nil:
	case st.stpFeature:
		caps.STP, caps.BPDUGuard, caps.STPPortCost = true, true, true
	case c.L2 && st.l2.canRun():
		caps.STP, caps.BPDUGuard, caps.STPAutoEdge, caps.STPPortCost, caps.LoopProtection = true, true, true, true, true
	}
	if st != nil && st.aclKnown {
		caps.DHCPSnooping, caps.IPACLs = true, ipACLRules
		c.mu.Lock()
		if c.macACL {
			caps.MACACLs = macACLRules
		}
		c.mu.Unlock()
	}
	if c.igmpOn(st) {
		caps.IGMPSnooping, caps.IGMPSnoopingAdvanced = true, true
	}
	// Isolation, port security and egress shaping need the collector's
	// sections (portfeat.go); LLDP-MED needs lldpcli to answer.
	if st != nil && st.pf.known {
		caps.PortIsolation, caps.PortSecurity, caps.EgressRateLimit = true, true, true
	}
	if st != nil && st.pf.medKnown {
		caps.LLDPMED = true
	}
	// 802.1X: hostapd under disunified-l2d (§11.26), the ports closed
	// through the MAC filter's learn mode, so the collector's sections for
	// both.
	if st != nil && !st.stpFeature && c.L2 && st.l2.canDot1X() && st.pf.known {
		caps.Dot1X = true
	}
	// L3 routing needs FRR (l3.go): STATIC_ROUTE reaches the kernel and
	// the ASIC only through bgpcfgd and zebra.
	if st != nil && st.bgpOn() {
		caps.L3Routing, caps.L3Interfaces, caps.StaticRoutes = true, l3Interfaces, l3Routes
		caps.BGP, caps.OSPF = true, true
		caps.PortRouting = true
	}
	return caps
}

// Collect runs collect.py and builds the snapshot.
func (c *Collector) Collect(ctx context.Context) (*devicemodel.Snapshot, error) {
	// Not while a write runs: a lane switch's apply comes from its own loop,
	// and a collect read mid-batch would become the state the next plan
	// diffs against.
	unlock, err := c.lockApply(ctx)
	if err != nil {
		return nil, fmt.Errorf("sonic: collect: %w", err)
	}
	defer unlock()
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
	st := c.state
	c.mu.Unlock()
	c.verifyACL(st)
	return snap, nil
}

// required sections: without these the output is not a collector run.
var required = []string{"metadata", "cfg_port", "appl_port", "version", "addr"}

// redisSections is every Redis section collect.py prints.
var redisSections = []string{
	"metadata", "state_metadata", "cfg_port", "appl_port", "state_port", "cfg_vlan", "cfg_vlan_member",
	"cfg_lag", "cfg_lag_member", "appl_lag", "cfg_mgmt", "cfg_stp", "cfg_mirror", "cfg_ntp", "cfg_ntp_global", "cfg_syslog",
	"cfg_snmp", "cfg_route", "cfg_dns", "cfg_storm", "cfg_breakout", "cfg_feature", "lldp_loc", "lldp", "fdb", "counters_map", "counters", "rates",
	"xcvr", "dom", "fan", "psu", "temp", "chassis", "asic_temp",
}

// laterSections joined collect.py on 2026-11-01 (docs/drivers/sonic.md
// §11.4, the hardware tables; §11.5, disunified-l2d's tables; §11.7.5, the
// driver's ACL tables); the captures before that lack them, which reads as
// empty. state_switch_cap (the SAI's SWITCH_CAPABILITY, §11.8) joined on
// 2026-11-02, cfg_l2mc (IGMP snooping's tables, §8.10) on 2026-11-03,
// l2_storm (l2d's storm detection, §11.21), l2_dhcp_rate (its DHCP rate
// detector, §11.17), l2_maclock (MAC locking's count, §11.18.2) and
// asic_bridge_port (each port's bridge port in the chip, §8.15) on
// 2026-11-14, and appl_intf (orchagent's interface address rows, §11.22)
// the same day.
var laterSections = []string{"state_lag_member", "crm", "hwmon", "l2_bridge", "l2_port", "l2_guard", "l2_dhcp_rate", "l2_maclock", "cfg_acl_table", "cfg_acl_rule", "state_acl", "crm_acl", "state_switch_cap", "cfg_l2mc", "appl_iso", "appl_fdb", "cfg_qos", "l2_storm", "asic_bridge_port", "appl_intf", "l2_dot1x"}

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
	for _, name := range append(redisSections[:len(redisSections):len(redisSections)], laterSections...) {
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
	dot1xRead(ports, db, parseL2Inst(sec["l2d"]).Conf)
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
	applPort, applLAG := db["appl_port"].byName(":"), db["appl_lag"].byName(":")
	linkDown := func(port string) bool {
		if f, ok := applLAG[port]; ok {
			return f["oper_status"] != "up"
		}
		return applPort[port]["oper_status"] != "up"
	}
	macs := buildMACTable(db["fdb"], byIfname, c.NativeVLAN, linkDown)
	laneMACs(db, ports, c.NativeVLAN, linkDown)
	pi := map[int]int{}
	for i := range ports {
		pi[ports[i].Index] = i
	}
	for _, m := range macs {
		if i, ok := pi[m.PortIndex]; ok {
			ports[i].MACs = append(ports[i].MACs, m)
		}
	}

	dial := dialAddrs(c.CurrentAddress())
	starting := startingWhy(db, addrs, dial)
	sys := buildSystem(db, sec, addrs, routes, neigh, dial)
	if _, pvst := db["cfg_feature"]["FEATURE|stp"]; !pvst {
		l2System(&sys, db, now)
	}
	sys.IGMPSnooping = igmpReport(db["cfg_l2mc"], c.NativeVLAN)
	if sys.MACTableCapacity == 0 {
		sys.MACTableUsed = len(macs) // CRM's count goes with its capacity
	}
	fan := parseFan(sec["fancontrol"])
	c.fanReport(fan, &sys)
	cpu := parseCPUTimes(sec["stat"])
	c.mu.Lock()
	c.fan = fan
	if c.prevCPU.total > 0 && cpu.total > c.prevCPU.total {
		dt := float64(cpu.total - c.prevCPU.total)
		di := float64(cpu.idle - c.prevCPU.idle)
		sys.CPUPercent = float64(int((100*(1-di/dt))*10+0.5)) / 10
	}
	c.prevCPU = cpu
	c.host = sys.Hostname
	c.starting = starting
	c.state = newState(db, sec)
	c.state.l3Hop = c.managedL3.Hop
	macsBy := staticMACs(c.state.pf)
	for i := range ports {
		readBackPortFeat(c.state, &ports[i].Port, macsBy)
	}
	rogueDHCPDrops(c.state, ports) // DHCP guarding's count per port (acl_count.go)
	c.sshFrom, c.sshTo = "", ""
	if f := strings.Fields(sec["ssh"]); len(f) == 2 {
		c.sshFrom, c.sshTo = f[0], f[1]
	}
	for _, p := range ports {
		c.aliases[p.Index] = p.alias
		c.ifnames[p.Index] = append([]string(nil), p.Interfaces...)
	}
	c.lanes = map[int][]cagePort{}
	for _, p := range ports {
		if len(p.lanes) > 0 {
			rogueDHCPDrops(c.state, p.lanes)
			c.lanes[p.Index] = p.lanes
		}
	}
	c.chassisID = strings.ToLower(db["lldp_loc"].first()["lldp_loc_chassis_id"])
	children := make(map[int]devicemodel.Attachment, len(c.children))
	for k, v := range c.children {
		children[k] = v
	}
	down := sortedKeysOf(c.state.bridgeDown)
	c.mu.Unlock()
	for _, iface := range down {
		c.warnOnce("bridge down "+iface, "sonic: %s's bridge port is held admin down in the chip: orchagent refused to remove it (L2MC references; the kit's sonic-swss 0004 fixes the cause, docs/drivers/sonic.md §8.15), and the port learns nothing; a swss restart or a reboot clears it", iface)
	}
	macs = reportChildren(ports, macs, children, now)

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
		if p.Up && p.Neighbor != nil && p.Neighbor.IsBridge && !p.Neighbor.Child {
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

// applyBudget is the time one write batch gets (devicemodel.ApplyBudget): a
// `config` call is ~250 ms on the VS and a first push dozens of them (§6.6).
const applyBudget = 90 * time.Second

// ApplyTimeout implements devicemodel.ApplyBudget.
func (c *Collector) ApplyTimeout() time.Duration { return applyBudget }
