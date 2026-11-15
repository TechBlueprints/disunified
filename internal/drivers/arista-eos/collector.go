package aristaeos

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The command sets. Every one of these exists on EOS 4.26.14M (verified live
// 2026-09-19); do not add a command without checking it against that version
// — `show poe`, `show environment temperature` and friends are not there.
var (
	// startupCmds run once: identity, and the media map.
	startupCmds = []string{"show version", "show hostname", "show interfaces status", "show inventory", "show interfaces transceiver properties", "show interfaces hardware", "show lldp local-info"}
	// pollCmds run every Collect.
	pollCmds = []string{
		"show version",
		"show interfaces",
		"show lldp neighbors detail",
		"show spanning-tree",
		"show processes top once",
		"show system environment temperature",
		"show mac address-table",
		"show interfaces transceiver",
		"show interfaces error-correction",
		"show interfaces flow-control", // full spelling: eAPI does not expand "interface"; `show interfaces flowcontrol` is gone on 4.26
		"show port-channel summary",
		"show system environment cooling",
		"show system environment power",
		"show interfaces switchport",
		"show vlan",
		"show storm-control",
		"show ip igmp snooping",
		"show running-config",
		"show interfaces status errdisabled",
		"show spanning-tree root detail",
		"show interfaces transceiver dom thresholds",
		"show spanning-tree topology status detail",
		"show ip arp",
		"show hardware capacity",
	}
)

// mediaRefreshEvery is how many polls between re-reading `show interfaces
// status` (an optic being inserted changes a port's media).
const mediaRefreshEvery = 30

// Collector turns EOS command output into a devicemodel.Snapshot.
type Collector struct {
	t   Transport
	Log *log.Logger // config batches are logged here when set

	// host is the address the bridge reaches the switch at; the interface
	// carrying it is the management interface an address change targets.
	// redial opens a transport to the switch at another address, used to
	// confirm a move before it becomes permanent; nil when the transport
	// cannot be re-dialled (a fixture without one, or an unknown target).
	host   string
	redial func(host string) Transport
	// mgmtPort is the TCP port the bridge connects on (eAPI's or SSH's);
	// 0 = eAPI's default, 443.
	mgmtPort int

	mu        sync.Mutex
	hostname  string
	static    map[string]portStatic
	inventory showInventory
	props     showInterfacesTransceiverProperties
	hardware  map[string]portHardware
	polls     int
	// refresh makes the next collect re-read the startup commands: a
	// written speed is read back from `show interfaces transceiver
	// properties`, one of them.
	refresh bool
	last    *devicemodel.Snapshot
	// nameServers and mgmtDHCP are the running config's view of the
	// switch's own addressing, kept for ApplyAddress's diff.
	nameServers []string
	mgmtDHCP    bool

	portChannels map[string]devicemodel.PortVLAN // Port-ChannelN -> its switchport state
	channelCfg   map[int][]string                // every Port-Channel in the running config: number -> its lines (portchannel.go)
	managedSNMP  string                          // the SNMP community this bridge last configured
	macDrops     map[string]bool                 // the running config's static MAC drop entries, by line (block.go)
	blockNoted   map[string]bool                 // the client Block's notes already logged

	sshKeyUser    string   // user that receives the controller's SSH keys
	installedKeys []string // what InstallSSHKeys last wrote (per process)

	// acl is the running config's view of the driver's own ACLs and their
	// bindings (acl.go). aclHeld names a change not sent again until the
	// controller's rules change -- one that left the switch unreachable, or
	// one the hardware could not program -- and aclHeldWhy says which;
	// aclSettle is the wait between a timed commit and the dial that
	// confirms it; dhcpGuard is the guard mode (options.dhcp_guard).
	acl        aclState
	aclHeld    string
	aclHeldWhy string
	aclSettle  time.Duration
	dhcpGuard  string
	aclStatus  string // the last ACL read-back problem logged (acl_counters.go)

	stpHeld map[string]stpHold // per interface: holdSTPChanges

	// Lane switches (lanes.go): the split cages' lanes as of the last
	// collect (by cage, Index 1..N), the attached lane switches, the cages
	// the controller has disabled, this switch's LLDP chassis id, and the
	// plan notes already logged.
	lanes     map[int][]devicemodel.Port
	children  map[int]devicemodel.Attachment
	cageDown  map[int]bool
	chassisID string
	noted     map[string]bool
	// applySem serialises collects and writes (lockApply): a lane switch
	// applies from its own loop, beside this device's.
	applyOnce sync.Once
	applySem  chan struct{}
}

// SetSSHKeyUser names the EOS user that receives controller SSH keys.
func (c *Collector) SetSSHKeyUser(user string) { c.sshKeyUser = user }

// NewCollector wraps a transport. Call Start before Collect.
func NewCollector(t Transport) *Collector {
	return &Collector{t: t, Log: log.Default(), aclSettle: aclSettleDefault, dhcpGuard: guardExact}
}

// Start runs the startup commands and then one full poll, so a command this
// EOS version lacks — or bad credentials — fails here, loudly, rather than
// producing an empty port table forever.
func (c *Collector) Start(ctx context.Context) (*devicemodel.Snapshot, error) {
	if err := c.refreshStatic(ctx); err != nil {
		return nil, fmt.Errorf("eos startup: %w", err)
	}
	snap, err := c.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("eos first poll: %w", err)
	}
	return snap, nil
}

// run executes cmds in privileged mode: eAPI (and a fresh SSH session) start
// at privilege 1 whatever the user's level, and some show commands — `show
// interface flow-control` on 4.26 — refuse to run there. The `enable` result
// is dropped so callers index results by their own command list.
func (c *Collector) run(ctx context.Context, cmds []string) ([]json.RawMessage, error) {
	out, err := c.t.Run(ctx, append([]string{"enable"}, cmds...))
	if err != nil {
		return nil, err
	}
	return out[1:], nil
}

func (c *Collector) refreshStatic(ctx context.Context) error {
	out, err := c.run(ctx, startupCmds)
	if err != nil {
		return err
	}
	var hn showHostname
	if err := decodeInto(out[1], startupCmds[1], &hn); err != nil {
		return err
	}
	var ss showInterfacesStatusFull
	if err := decodeInto(out[2], startupCmds[2], &ss); err != nil {
		return err
	}
	var inv showInventory
	if err := decodeInto(out[3], startupCmds[3], &inv); err != nil {
		return err
	}
	var props showInterfacesTransceiverProperties
	if err := decodeInto(out[4], startupCmds[4], &props); err != nil {
		return err
	}
	var hw showInterfacesHardware
	if err := decodeInto(out[5], startupCmds[5], &hw); err != nil {
		return err
	}
	var li struct {
		ChassisID string `json:"chassisId"`
	}
	if err := decodeInto(out[6], startupCmds[6], &li); err != nil {
		return err
	}
	c.mu.Lock()
	c.chassisID = strings.ToLower(li.ChassisID)
	c.hostname = hn.Hostname
	c.static = staticFromStatus(ss)
	c.inventory = inv
	c.props = props
	c.hardware = hardwareByInterface(hw)
	c.mu.Unlock()
	return nil
}

// Collect runs the poll commands and assembles a Snapshot.
func (c *Collector) Collect(ctx context.Context) (*devicemodel.Snapshot, error) {
	unlock, err := c.lockApply(ctx)
	if err != nil {
		return nil, fmt.Errorf("eos collect: %w", err)
	}
	defer unlock()
	c.mu.Lock()
	c.polls++
	needRefresh := c.static == nil || c.refresh || c.polls%mediaRefreshEvery == 0
	c.refresh = false
	c.mu.Unlock()
	if needRefresh {
		if err := c.refreshStatic(ctx); err != nil {
			return nil, err
		}
	}

	out, err := c.run(ctx, pollCmds)
	if err != nil {
		return nil, err
	}
	var (
		ver  showVersion
		ifs  showInterfaces
		lldp showLLDPNeighborsDetail
		stp  showSpanningTreeFull
		top  showProcessesTop
		temp showSystemEnvTemperature
		mt   showMACAddressTable
		xcvr showInterfacesTransceiver
		fec  showInterfacesErrorCorrection
		fc   showInterfaceFlowControl
		pc   showPortChannelSummary
		cool showSystemEnvCooling
		pwr  showSystemEnvPower
		sp   showInterfacesSwitchport
		vl   showVLAN
		sc   showStormControl
		igmp showIGMPSnooping
		rc   showRunningConfig
		ed   showErrdisabled
		root showSpanningTreeRoot
		th   showTransceiverThresholds
		ts   showSTPTopologyStatus
		arp  showIPARP
		hwc  showHardwareCapacity
	)
	for i, v := range []any{&ver, &ifs, &lldp, &stp, &top, &temp, &mt, &xcvr, &fec, &fc, &pc, &cool, &pwr, &sp, &vl, &sc, &igmp, &rc, &ed, &root, &th, &ts, &arp, &hwc} {
		if err := decodeInto(out[i], pollCmds[i], v); err != nil {
			return nil, err
		}
	}

	c.mu.Lock()
	static, hostname, inv, props, hw := c.static, c.hostname, c.inventory, c.props, c.hardware
	c.mu.Unlock()

	media := make(map[string]devicemodel.Media, len(static))
	for k, v := range static {
		media[k] = v.media
	}
	stpTotals := c.holdSTPChanges(stpChangeCounts(ts), interfaceDrops(ifs, ver.BootupTimestamp))
	// build runs every per-interface reading for one way of keying the
	// interfaces: the fold (a port per front-panel slot, lanes folded) or
	// the lanes of the split cages, each a port of its own (lanes.go). sys
	// is filled by the fold only.
	build := func(key ifKey, sessions func(int) bool, sys *devicemodel.System) ([]devicemodel.Port, []devicemodel.MACEntry, error) {
		ports, err := portsFromInterfaces(ifs, media, ver.BootupTimestamp, key)
		if err != nil {
			return nil, nil, err
		}
		applyLLDP(ports, lldp, key)
		applySTPStates(ports, stp, key)
		applyExtras(ports, static, xcvr, inv, fec, fc, pc, stp, key)
		applyVLANState(ports, sp, props, key)
		applyHardwareCaps(ports, hw)
		applyStormControl(ports, sc, key)
		macs := macTable(mt, key)
		for i := range ports {
			for _, m := range macs {
				if m.PortIndex == ports[i].Index {
					ports[i].MACs = append(ports[i].MACs, m)
				}
			}
		}
		applyRunningConfig(ports, sys, rc, key, sessions)
		applyErrdisabled(ports, ed, key)
		if _, text := c.t.(TextRunner); !text {
			applyOpticAlarms(ports, th, key) // with text, applyPHYDetail reads the module's own flags
		}
		applySTPChanges(ports, stpTotals, key)
		applySTPRoles(ports, stp, key)
		return ports, macs, nil
	}
	var sys devicemodel.System
	ports, macs, err := build(parseEthName, ownSession, &sys)
	if err != nil {
		return nil, err
	}
	lanes, err := c.buildLanes(ports, build)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.portChannels = portChannelVLANs(sp)
	c.channelCfg = portChannelSections(rc)
	c.macDrops = macDropsFrom(rc)
	c.mu.Unlock()
	stpMode, stpPrio := stpSystem(stp)

	snap := &devicemodel.Snapshot{
		TakenAt: time.Now(),
		System: devicemodel.System{
			Vendor:      "Arista",
			Model:       ver.ModelName,
			Serial:      ver.SerialNumber,
			MAC:         strings.ToLower(ver.SystemMacAddress),
			Version:     ver.Version,
			Hostname:    hostname,
			Uptime:      uptime(ver.Uptime),
			CPUPercent:  100 - top.CPUInfo.CPUs.Idle,
			MemTotalKB:  top.MemInfo.Physical.MemTotal,
			MemUsedKB:   top.MemInfo.Physical.MemUsed,
			MemBufferKB: top.MemInfo.Physical.MemBuffer,
		},
		Ports:    ports,
		MACTable: macs,
		VLANs:    vlanIDs(vl),
	}
	snap.System.STPMode, snap.System.STPPriority = stpMode, stpPrio
	// What the running config said about the switch (NTP, syslog, SNMP,
	// DHCP snooping, STP priority and mode).
	snap.System.DHCPSnooping, snap.System.SNMPCommunities, snap.System.NTPServers, snap.System.SyslogHosts = sys.DHCPSnooping, sys.SNMPCommunities, sys.NTPServers, sys.SyslogHosts
	if sys.STPPriority != 0 {
		snap.System.STPPriority = sys.STPPriority
	}
	if sys.STPMode != "" {
		snap.System.STPMode = sys.STPMode
	}
	if m, ok := ifs.Interfaces["Management1"]; ok && m.PhysicalAddress != "" {
		snap.System.MgmtMAC = strings.ToLower(m.PhysicalAddress) // the OOB port has its own MAC (system MAC - 1 on the 7160)
	}
	snap.System.OOBInterfaces = oobInterfaces(ifs)
	snap.System.Addresses = ifAddresses(ifs)
	if iface, _ := managementInterface(snap, c.host); iface != "" {
		gw, dns, dhcp := addressingFromRunningConfig(rc, iface)
		snap.System.Gateway, snap.System.DHCP = gw, dhcp
		c.mu.Lock()
		c.nameServers, c.mgmtDHCP = dns, dhcp
		c.mu.Unlock()
	}
	snap.System.ARP = arpTable(arp)
	snap.System.LoadAvg = top.TimeInfo.LoadAvg
	snap.System.MACTableCapacity, snap.System.MACTableUsed = fdbCapacity(hwc)
	snap.System.STPRoot = stpRoot(root)
	snap.System.IGMPSnooping = igmpSnooping(igmp)
	flat := flatLanes(lanes)
	c.readACLs(ctx, rc, ports, flat)
	if tr, ok := c.t.(TextRunner); ok {
		c.applyPHYDetail(ctx, tr, ports, flat)
	}
	unflatLanes(lanes, flat)
	c.finishLanes(snap)
	if c.sshKeyUser != "" && c.installedKeys == nil {
		c.mu.Lock()
		c.installedKeys = sshKeysFromRunningConfig(rc, c.sshKeyUser)
		c.mu.Unlock()
	}
	snap.System.Overheating = temp.SystemStatus != "" && temp.SystemStatus != "temperatureOk"
	snap.System.Fans = fans(cool)
	snap.System.FanModes, snap.System.FanMode = fanModes, fanModeOf(cool)
	snap.System.PSUs = psus(pwr)
	if snap.System.MemTotalKB == 0 {
		snap.System.MemTotalKB = ver.MemTotal
		snap.System.MemUsedKB = ver.MemTotal - ver.MemFree
	}
	// Report the hottest sensor; the controller shows one number.
	for _, s := range temp.TempSensors {
		if !snap.System.HasTemperature || s.CurrentTemperature > snap.System.TemperatureC {
			snap.System.TemperatureC = s.CurrentTemperature
			snap.System.HasTemperature = true
		}
	}
	c.mu.Lock()
	c.last, c.lanes = snap, lanes
	c.mu.Unlock()
	return snap, nil
}

// interfaceDrops is linkDrops for every Ethernet interface.
func interfaceDrops(ifs showInterfaces, bootAt float64) map[string]uint64 {
	out := map[string]uint64{}
	for name, ifc := range ifs.Interfaces {
		if _, _, ok := parseEthName(name); ok {
			out[name] = linkDrops(ifc.Counters, ifUp(ifc), bootAt)
		}
	}
	return out
}

// Close releases the transport.
func (c *Collector) Close() error { return c.t.Close() }

// applyPHYDetail reads `show interfaces X phy detail` (text: EOS 4.26 only
// prints it) for two things. The FEC codeword counters of every up port that
// runs FEC (the satisfaction score counts uncorrected codewords as errors),
// and the module's own RX_LOS and TX_FAULT flags of every optical cage with a
// module seated, up or down, which are sent as sfp_rxfault and sfp_txfault
// (a UniFi switch's meaning: docs/drivers/arista-eos.md §4). One text
// command per lane interface of those ports; a cage takes any of its lanes'
// flags, a split cage's lane port its own. The up ports are one batch and
// the seated down cages another, so a cage the switch cannot read (eAPI
// stops a batch at its first error) costs no FEC counter. A failure is
// logged, not fatal, and a transport without text support skips it (the
// flags then come from the DOM thresholds, applyOpticAlarms).
func (c *Collector) applyPHYDetail(ctx context.Context, tr TextRunner, ports, lanes []devicemodel.Port) {
	laneOf := map[string]int{}
	for i, l := range lanes {
		laneOf[laneNames(l)[0]] = i
	}
	var up, seated []int
	for i, p := range ports {
		switch {
		case p.Up:
			up = append(up, i)
		case p.Present && p.Media != devicemodel.MediaUnknown && !isCopperMedia(p.Media):
			seated = append(seated, i)
		}
	}
	for _, batch := range [][]int{up, seated} {
		var cmds []string
		var owners []int // ports index per command
		var names []string
		for _, i := range batch {
			for _, n := range laneNames(ports[i]) {
				cmds = append(cmds, "show interfaces "+n+" phy detail")
				owners = append(owners, i)
				names = append(names, n)
			}
		}
		if len(cmds) == 0 {
			continue
		}
		out, err := tr.RunText(ctx, append([]string{"enable"}, cmds...))
		if err != nil {
			c.Log.Printf("eos phy detail: %v", err)
			continue
		}
		for j, text := range out[1:] {
			d := parsePHYDetail(text)
			owner := &ports[owners[j]]
			hs := []*devicemodel.PortHealth{&owner.Health}
			if i, ok := laneOf[names[j]]; ok {
				hs = append(hs, &lanes[i].Health)
			}
			for _, h := range hs {
				if d.HasFEC && owner.Up {
					h.HasFECCounters = true
					h.FECCorrected += d.FECCorrected
					h.FECUncorrected += d.FECUncorr
				}
				if d.HasXcvrFlags {
					h.OpticRxAlarm = h.OpticRxAlarm || d.RxLOS
					h.OpticTxAlarm = h.OpticTxAlarm || d.TxFault
				}
			}
		}
	}
}

// oobInterfaces reports the ManagementN ports: link state and address.
func oobInterfaces(ifs showInterfaces) []devicemodel.OOBInterface {
	names := make([]string, 0, 2)
	for name := range ifs.Interfaces {
		if strings.HasPrefix(name, "Management") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]devicemodel.OOBInterface, 0, len(names))
	for _, name := range names {
		m := ifs.Interfaces[name]
		o := devicemodel.OOBInterface{Name: name, Up: m.InterfaceStatus == "connected" || m.LineProtocolStatus == "up"}
		if len(m.InterfaceAddress) > 0 && m.InterfaceAddress[0].PrimaryIP.Address != "0.0.0.0" {
			o.IP = m.InterfaceAddress[0].PrimaryIP.Address
		}
		out = append(out, o)
	}
	return out
}

// ifAddresses lists every interface's primary IPv4 address.
func ifAddresses(ifs showInterfaces) []devicemodel.IfAddress {
	var out []devicemodel.IfAddress
	for name, m := range ifs.Interfaces {
		if len(m.InterfaceAddress) == 0 {
			continue
		}
		p := m.InterfaceAddress[0].PrimaryIP
		if p.Address == "" || p.Address == "0.0.0.0" {
			continue
		}
		out = append(out, devicemodel.IfAddress{Iface: name, IP: p.Address, PrefixLen: p.MaskLen})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Iface < out[j].Iface })
	return out
}

// showIPARP is `show ip arp`.
type showIPARP struct {
	IPv4Neighbors []struct {
		Address   string `json:"address"`
		HwAddress string `json:"hwAddress"` // aabb.ccdd.eeff on EOS
	} `json:"ipV4Neighbors"`
}

func arpTable(a showIPARP) map[string]string {
	out := make(map[string]string, len(a.IPv4Neighbors))
	for _, n := range a.IPv4Neighbors {
		out[n.Address] = colonMAC(n.HwAddress)
	}
	return out
}

// colonMAC turns EOS's aabb.ccdd.eeff (or any hex run) into aa:bb:cc:dd:ee:ff.
func colonMAC(s string) string {
	hex := strings.NewReplacer(".", "", ":", "", "-", "").Replace(strings.ToLower(s))
	if len(hex) != 12 {
		return strings.ToLower(s)
	}
	parts := make([]string, 6)
	for i := range parts {
		parts[i] = hex[2*i : 2*i+2]
	}
	return strings.Join(parts, ":")
}

// showHardwareCapacity is `show hardware capacity`; the L2 "FDB" table is
// the MAC address table (131072 entries on the 7160).
type showHardwareCapacity struct {
	Tables []struct {
		Feature  string `json:"feature"`
		Table    string `json:"table"`
		MaxLimit int    `json:"maxLimit"`
		Used     int    `json:"used"`
	} `json:"tables"`
}

func fdbCapacity(h showHardwareCapacity) (capacity, used int) {
	for _, t := range h.Tables {
		if t.Feature == "L2" && t.Table == "FDB" {
			return t.MaxLimit, t.Used
		}
	}
	return 0, 0
}
