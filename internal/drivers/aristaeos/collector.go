package aristaeos

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// The command sets. Every one of these exists on EOS 4.26.14M (verified live
// 2026-09-19); do not add a command without checking it against that version
// — `show poe`, `show environment temperature` and friends are not there.
var (
	// startupCmds run once: identity, and the media map.
	startupCmds = []string{"show version", "show hostname", "show interfaces status", "show inventory", "show interfaces transceiver properties", "show interfaces hardware"}
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
	}
)

// mediaRefreshEvery is how many polls between re-reading `show interfaces
// status` (an optic being inserted changes a port's media).
const mediaRefreshEvery = 30

// Collector turns EOS command output into a switchmodel.Snapshot.
type Collector struct {
	t   Transport
	Log *log.Logger // config batches are logged here when set

	mu        sync.Mutex
	hostname  string
	static    map[string]portStatic
	inventory showInventory
	props     showInterfacesTransceiverProperties
	hardware  map[string]portHardware
	polls     int
	last      *switchmodel.Snapshot

	portChannels map[string]switchmodel.PortVLAN // Port-ChannelN -> its switchport state
	managedSNMP  string                          // the SNMP community this bridge last configured

	sshKeyUser    string   // user that receives the controller's SSH keys
	installedKeys []string // what InstallSSHKeys last wrote (per process)
}

// SetSSHKeyUser names the EOS user that receives controller SSH keys.
func (c *Collector) SetSSHKeyUser(user string) { c.sshKeyUser = user }

// NewCollector wraps a transport. Call Start before Collect.
func NewCollector(t Transport) *Collector {
	return &Collector{t: t, Log: log.Default()}
}

// Start runs the startup commands and then one full poll, so a command this
// EOS version lacks — or bad credentials — fails here, loudly, rather than
// producing an empty port table forever.
func (c *Collector) Start(ctx context.Context) (*switchmodel.Snapshot, error) {
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
	c.mu.Lock()
	c.hostname = hn.Hostname
	c.static = staticFromStatus(ss)
	c.inventory = inv
	c.props = props
	c.hardware = hardwareByInterface(hw)
	c.mu.Unlock()
	return nil
}

// Collect runs the poll commands and assembles a Snapshot.
func (c *Collector) Collect(ctx context.Context) (*switchmodel.Snapshot, error) {
	c.mu.Lock()
	c.polls++
	needRefresh := c.static == nil || c.polls%mediaRefreshEvery == 0
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
	)
	for i, v := range []any{&ver, &ifs, &lldp, &stp, &top, &temp, &mt, &xcvr, &fec, &fc, &pc, &cool, &pwr, &sp, &vl, &sc, &igmp, &rc, &ed, &root, &th, &ts} {
		if err := decodeInto(out[i], pollCmds[i], v); err != nil {
			return nil, err
		}
	}

	c.mu.Lock()
	static, hostname, inv, props, hw := c.static, c.hostname, c.inventory, c.props, c.hardware
	c.mu.Unlock()

	media := make(map[string]switchmodel.Media, len(static))
	for k, v := range static {
		media[k] = v.media
	}
	ports, err := portsFromInterfaces(ifs, media)
	if err != nil {
		return nil, err
	}
	applyLLDP(ports, lldp)
	applySTPStates(ports, stp)
	applyExtras(ports, static, xcvr, inv, fec, fc, pc, stp)
	applyVLANState(ports, sp, props)
	c.mu.Lock()
	c.portChannels = portChannelVLANs(sp)
	c.mu.Unlock()
	applyHardwareCaps(ports, hw)
	applyStormControl(ports, sc)
	macs := macTable(mt)
	for i := range ports {
		for _, m := range macs {
			if m.PortIndex == ports[i].Index {
				ports[i].MACs = append(ports[i].MACs, m)
			}
		}
	}
	stpMode, stpPrio := stpSystem(stp)

	snap := &switchmodel.Snapshot{
		TakenAt: time.Now(),
		System: switchmodel.System{
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
	snap.System.STPRoot = stpRoot(root)
	snap.System.IGMPSnooping = igmpSnooping(igmp)
	applyRunningConfig(ports, &snap.System, rc)
	applyErrdisabled(ports, ed)
	applyOpticAlarms(ports, th)
	applySTPChanges(ports, ts)
	applySTPInconsistent(ports, stp)
	if tr, ok := c.t.(TextRunner); ok {
		c.applyPHYDetail(ctx, tr, ports)
	}
	if c.sshKeyUser != "" && c.installedKeys == nil {
		c.mu.Lock()
		c.installedKeys = sshKeysFromRunningConfig(rc, c.sshKeyUser)
		c.mu.Unlock()
	}
	snap.System.Overheating = temp.SystemStatus != "" && temp.SystemStatus != "temperatureOk"
	snap.System.Fans = fans(cool)
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
	c.last = snap
	c.mu.Unlock()
	return snap, nil
}

// Close releases the transport.
func (c *Collector) Close() error { return c.t.Close() }

// applyPHYDetail reads the PHY-layer counters for every up port: FEC
// codewords (ports running FEC) and PCS errored blocks / high-BER state
// (every port, copper included). EOS 4.26 only prints them (`show
// interfaces X phy detail`, text), so this is one text command per lane; a
// failure is logged, not fatal, and a transport without text support skips it.
func (c *Collector) applyPHYDetail(ctx context.Context, tr TextRunner, ports []switchmodel.Port) {
	var cmds []string
	var owners []int // ports index per command
	for i, p := range ports {
		if !p.Up {
			continue
		}
		names := p.Interfaces
		if len(names) == 0 {
			names = []string{p.IfName}
		}
		for _, n := range names {
			cmds = append(cmds, "show interfaces "+n+" phy detail")
			owners = append(owners, i)
		}
	}
	if len(cmds) == 0 {
		return
	}
	out, err := tr.RunText(ctx, append([]string{"enable"}, cmds...))
	if err != nil {
		c.Log.Printf("eos phy detail: %v", err)
		return
	}
	for j, text := range out[1:] {
		d := parsePHYDetail(text)
		h := &ports[owners[j]].Health
		if d.HasFEC {
			h.HasFECCounters = true
			h.FECCorrected += d.FECCorrected
			h.FECUncorrected += d.FECUncorr
		}
		if d.HasPCS {
			h.HasPCSCounters = true
			h.PCSErrBlocks += d.PCSErrBlocks
			h.PCSHighBER = h.PCSHighBER || d.PCSHighBER
		}
	}
}
