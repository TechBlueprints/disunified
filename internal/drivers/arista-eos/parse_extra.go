package aristaeos

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Shapes of the second wave of commands, all verified on EOS 4.26.14M
// (docs/fixtures/arista-eos-4.26.14M, captured 2026-09-19).

type showMACAddressTable struct {
	UnicastTable struct {
		Entries []struct {
			MACAddress string  `json:"macAddress"`
			Interface  string  `json:"interface"`
			VLANID     int     `json:"vlanId"`
			EntryType  string  `json:"entryType"` // dynamic | static
			LastMove   float64 `json:"lastMove"`
		} `json:"tableEntries"`
	} `json:"unicastTable"`
}

type showInterfacesTransceiver struct {
	Interfaces map[string]struct {
		MediaType   string  `json:"mediaType"`
		VendorSn    string  `json:"vendorSn"`
		Temperature float64 `json:"temperature"`
		Voltage     float64 `json:"voltage"`
		TxBias      float64 `json:"txBias"`
		TxPower     float64 `json:"txPower"`
		RxPower     float64 `json:"rxPower"`
	} `json:"interfaces"`
}

type showInventory struct {
	XcvrSlots map[string]struct {
		MfgName     string `json:"mfgName"`
		ModelName   string `json:"modelName"`
		SerialNum   string `json:"serialNum"`
		HardwareRev string `json:"hardwareRev"` // the module's vendor revision ("1", "A"), "" for a cable that has none
	} `json:"xcvrSlots"`
}

type showInterfacesErrorCorrection struct {
	Statuses map[string]struct {
		Status string `json:"errCorrEncodingStatus"` // disabled | reedSolomon | fireCode
	} `json:"interfaceErrCorrStatuses"`
}

type showInterfaceFlowControl struct {
	Interfaces map[string]struct {
		RxOper string `json:"rxOperState"` // on | off
		TxOper string `json:"txOperState"`
	} `json:"interfaceFlowControls"`
}

type showPortChannelSummary struct {
	PortChannels map[string]struct {
		Protocol      string         `json:"protocol"` // "lacp" | "static"
		Ports         map[string]any `json:"ports"`    // 4.26.14M: members keyed by interface name, each with lagMember (bundled)
		ActivePorts   map[string]any `json:"activePorts"`
		InactivePorts map[string]any `json:"inactivePorts"`
	} `json:"portChannels"`
}

type showSystemEnvCooling struct {
	SystemStatus string `json:"systemStatus"`
	// OverrideFanSpeed is `environment fan-speed override`'s percentage,
	// 0 under auto (4.26.14M capture: coolingMode "automatic", 0).
	OverrideFanSpeed int `json:"overrideFanSpeed"`
	FanTraySlots     []struct {
		Label  string `json:"label"`
		Speed  int    `json:"speed"`
		Status string `json:"status"`
	} `json:"fanTraySlots"`
}

type showSystemEnvPower struct {
	PowerSupplies map[string]struct {
		ModelName   string  `json:"modelName"`
		State       string  `json:"state"` // ok | powerLoss | notInserted | ...
		OutputPower float64 `json:"outputPower"`
		Capacity    float64 `json:"capacity"`
		TempSensors map[string]struct {
			Temperature float64 `json:"temperature"`
		} `json:"tempSensors"`
	} `json:"powerSupplies"`
}

// showSpanningTree (parse.go) also carries protocol and bridge priority:
type showSpanningTreeFull struct {
	Instances map[string]struct {
		Protocol string `json:"protocol"` // rstp | mstp | rapidPvst ...
		Bridge   struct {
			Priority int `json:"priority"`
		} `json:"bridge"`
		Interfaces map[string]struct {
			State string `json:"state"`
			Role  string `json:"role"` // root | designated | alternate | backup | disabled
			Cost  int    `json:"cost"`
		} `json:"interfaces"`
	} `json:"spanningTreeInstances"`
}

// showTransceiverThresholds is `show interfaces transceiver dom thresholds`:
// per optic, each DOM parameter's per-channel value and alarm thresholds.
// Cages without a DOM-capable optic (DACs, empty) have no parameters. A
// channel is nil where EOS printed a value JSON does not have (finiteJSON):
// -Infinity dBm, a power of zero.
type showTransceiverThresholds struct {
	Interfaces map[string]struct {
		Parameters map[string]struct {
			Channels  map[string]*float64 `json:"channels"`
			Threshold *struct {
				HighAlarm float64 `json:"highAlarm"`
				LowAlarm  float64 `json:"lowAlarm"`
			} `json:"threshold"`
		} `json:"parameters"`
	} `json:"interfaces"`
}

// showSTPTopologyStatus is `show spanning-tree topology status detail`: the
// topology-change count per interface per topology ("Cist" for RSTP/MSTP).
type showSTPTopologyStatus struct {
	Topologies map[string]struct {
		Interfaces map[string]struct {
			NumChanges int `json:"numChanges"`
		} `json:"interfaces"`
	} `json:"topologies"`
}

// applyOpticAlarms flags ports whose optic reports a receive or transmit
// parameter outside its own alarm thresholds (any lane, any channel).
func applyOpticAlarms(ports []devicemodel.Port, th showTransceiverThresholds, key ifKey) {
	byIndex := map[int]*devicemodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	for name, e := range th.Interfaces {
		slot, _, ok := key(name)
		if !ok || byIndex[slot] == nil {
			continue
		}
		out := func(param string) bool {
			p, ok := e.Parameters[param]
			if !ok || p.Threshold == nil {
				return false
			}
			for _, v := range p.Channels {
				if v == nil {
					// No finite reading. For a power that is no light at
					// all, below any low alarm; for anything else there
					// is nothing to compare.
					if strings.HasSuffix(param, "Power") {
						return true
					}
					continue
				}
				if *v < p.Threshold.LowAlarm || *v > p.Threshold.HighAlarm {
					return true
				}
			}
			return false
		}
		port := byIndex[slot]
		port.Health.OpticRxAlarm = port.Health.OpticRxAlarm || out("rxPower")
		port.Health.OpticTxAlarm = port.Health.OpticTxAlarm || out("txPower") || out("txBias")
	}
}

// stpChangeCounts is each Ethernet interface's state-change count in the
// Cist topology (falling back to whatever topology there is). EOS keeps the
// count per link-up: an interface whose link is down is not listed at all
// (live 2026-11-15: Ethernet2 and Ethernet54/1 absent), and a link that
// comes up again starts from nothing (the 2026-09-19 capture: Ethernet49/1
// at 18 link transitions, 9 rises, and 3 state changes; Ethernet1 at 3
// after its one rise, live 2026-11-15). So the raw count falls to 0 with
// every drop and must be held (Collector.holdSTPChanges).
func stpChangeCounts(ts showSTPTopologyStatus) map[string]int {
	topo, ok := ts.Topologies["Cist"]
	if !ok {
		for _, t := range ts.Topologies {
			topo = t
			break
		}
	}
	out := map[string]int{}
	for name, e := range topo.Interfaces {
		if _, _, ok := parseEthName(name); ok {
			out[name] = e.NumChanges
		}
	}
	return out
}

// stpHold is one interface's STP change count as the bridge reports it:
// raw is EOS's count at the last collect, drops the interface's link drops
// then, total what has been reported (never less than before).
type stpHold struct {
	raw, total int
	drops      uint64
}

// holdSTPChanges turns EOS's per-link-up counts into a count that never
// falls while the bridge runs: each collect adds the growth of the current
// link-up's count, or, when that link-up is a new one (the count fell, or
// the interface's link dropped since), its whole count. An interface that
// is down (not listed) keeps its total. drops is linkDrops per interface.
// The controller's STP-flap alert is a rate between two informs (CLAUDE.md
// §5): a count that fell to 0 while a port was down and came back whole
// would look like every change of that link-up happening at once. A
// bridge restart starts from EOS's counts again (one fall, which raises
// nothing). Returns the totals by interface, every interface ever seen.
func (c *Collector) holdSTPChanges(raw map[string]int, drops map[string]uint64) map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stpHeld == nil {
		c.stpHeld = map[string]stpHold{}
	}
	names := map[string]bool{}
	for n := range raw {
		names[n] = true
	}
	for n := range c.stpHeld {
		names[n] = true
	}
	out := make(map[string]int, len(names))
	for n := range names {
		r, d := raw[n], drops[n]
		h, seen := c.stpHeld[n]
		switch {
		case !seen:
			h.total = r
		case r < h.raw || d > h.drops:
			h.total += r // a new link-up: its count started from 0
		default:
			h.total += r - h.raw
		}
		h.raw, h.drops = r, d
		c.stpHeld[n] = h
		out[n] = h.total
	}
	return out
}

// applySTPChanges sums each port's held STP change count over its lanes
// (holdSTPChanges); an interface the switch no longer has (a lane of a
// cage since joined) still counts towards its cage, so a join does not
// lower the cage's count either.
func applySTPChanges(ports []devicemodel.Port, totals map[string]int, key ifKey) {
	byIndex := map[int]*devicemodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	for name, n := range totals {
		if slot, _, ok := key(name); ok && byIndex[slot] != nil {
			byIndex[slot].Health.STPChanges += n
		}
	}
}

// applySTPRoles sets each port's STP role.
func applySTPRoles(ports []devicemodel.Port, st showSpanningTreeFull, key ifKey) {
	byIndex := map[int]*devicemodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	for _, inst := range st.Instances {
		for name, e := range inst.Interfaces {
			slot, _, ok := key(name)
			if !ok || byIndex[slot] == nil {
				continue
			}
			if e.Role != "" && byIndex[slot].STPRole == "" {
				byIndex[slot].STPRole = e.Role
			}
		}
	}
}

var (
	fecCorrectedRe   = regexp.MustCompile(`FEC corrected codewords\s+(\d+)`)
	fecUncorrectedRe = regexp.MustCompile(`FEC uncorrected codewords\s+(\d+)`)
)

// phyDetail is what `show interfaces <name> phy detail` (text; EOS 4.26 has
// no JSON for these counters) yields per lane. The first number on a line is
// the counter; the rest is change bookkeeping.
type phyDetail struct {
	HasFEC                  bool
	FECCorrected, FECUncorr uint64
}

func parsePHYDetail(text string) phyDetail {
	var d phyDetail
	if c, u := fecCorrectedRe.FindStringSubmatch(text), fecUncorrectedRe.FindStringSubmatch(text); c != nil && u != nil {
		d.HasFEC = true
		d.FECCorrected, _ = strconv.ParseUint(c[1], 10, 64)
		d.FECUncorr, _ = strconv.ParseUint(u[1], 10, 64)
	}
	return d
}

// parseFECCounters is kept for callers that only want the codewords.
func parseFECCounters(text string) (corrected, uncorrected uint64, ok bool) {
	d := parsePHYDetail(text)
	return d.FECCorrected, d.FECUncorr, d.HasFEC
}

// showSpanningTreeRoot is `show spanning-tree root detail`: the root bridge
// per instance (this switch's own bridge ID when it is the root).
type showSpanningTreeRoot struct {
	Instances map[string]struct {
		RootBridge struct {
			MACAddress string `json:"macAddress"`
			Priority   int    `json:"priority"`
		} `json:"rootBridge"`
	} `json:"instances"`
}

// stpRoot returns the root bridge MAC of the first instance, lower-cased.
func stpRoot(r showSpanningTreeRoot) string {
	for _, inst := range r.Instances {
		return strings.ToLower(inst.RootBridge.MACAddress)
	}
	return ""
}

// showInterfacesStatus also carries autoneg (used at startup):
type showInterfacesStatusFull struct {
	InterfaceStatuses map[string]struct {
		InterfaceType     string `json:"interfaceType"`
		Bandwidth         int64  `json:"bandwidth"`
		AutoNegotiateOper bool   `json:"autoNegotiateActive"`
	} `json:"interfaceStatuses"`
}

// portStatic is what refreshStatic learns per EOS interface.
type portStatic struct {
	media   devicemodel.Media
	autoNeg bool
}

func staticFromStatus(ss showInterfacesStatusFull) map[string]portStatic {
	m := make(map[string]portStatic, len(ss.InterfaceStatuses))
	for name, st := range ss.InterfaceStatuses {
		if _, _, ok := parseEthName(name); ok {
			m[name] = portStatic{media: mediaFor(st.InterfaceType, st.Bandwidth), autoNeg: st.AutoNegotiateOper}
		}
	}
	return m
}

func fecFor(status string) devicemodel.FEC {
	switch status {
	case "reedSolomon", "reedSolomon544":
		return devicemodel.FECRS
	case "fireCode":
		return devicemodel.FECFC
	case "disabled":
		return devicemodel.FECDisabled
	}
	return devicemodel.FECUnknown
}

// applyExtras attaches the second-wave data to ports (by slot; lane data is
// taken from lane 1 or, for FEC/flow control, from any lane that reports it).
func applyExtras(ports []devicemodel.Port, static map[string]portStatic,
	xcvr showInterfacesTransceiver, inv showInventory, fec showInterfacesErrorCorrection,
	fc showInterfaceFlowControl, pc showPortChannelSummary, stp showSpanningTreeFull, key ifKey) {
	byIndex := map[int]*devicemodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	for name, st := range static {
		if slot, lane, ok := key(name); ok && lane <= 1 {
			if p := byIndex[slot]; p != nil {
				p.AutoNeg = st.autoNeg
			}
		}
	}
	for name, x := range xcvr.Interfaces {
		slot, lane, ok := key(name)
		if !ok || lane > 1 {
			continue
		}
		p := byIndex[slot]
		if p == nil || isCopperMedia(p.Media) || !p.Present {
			continue
		}
		o := &devicemodel.Optic{MediaType: x.MediaType, Serial: x.VendorSn}
		if x.Temperature != 0 || x.RxPower != 0 || x.TxPower != 0 {
			o.HasDOM = true
			o.TempC, o.VoltageV, o.TxBiasMA, o.TxPowerDBm, o.RxPowerDBm = x.Temperature, x.Voltage, x.TxBias, x.TxPower, x.RxPower
		}
		if slotInv, ok := inv.XcvrSlots[strconv.Itoa(slot)]; ok {
			o.Vendor, o.Part, o.Revision = slotInv.MfgName, slotInv.ModelName, slotInv.HardwareRev
			if o.Serial == "" {
				o.Serial = slotInv.SerialNum
			}
		}
		p.Optic = o
	}
	for name, st := range fec.Statuses {
		if slot, _, ok := key(name); ok {
			if p := byIndex[slot]; p != nil && (p.FEC == devicemodel.FECUnknown || p.FEC == devicemodel.FECDisabled) {
				p.FEC = fecFor(st.Status)
			}
		}
	}
	for name, st := range fc.Interfaces {
		if slot, _, ok := key(name); ok {
			if p := byIndex[slot]; p != nil {
				p.FlowCtrlRx = p.FlowCtrlRx || st.RxOper == "on"
				p.FlowCtrlTx = p.FlowCtrlTx || st.TxOper == "on"
			}
		}
	}
	for lag, ch := range pc.PortChannels {
		id, _ := strconv.Atoi(strings.TrimPrefix(lag, "Port-Channel"))
		for _, members := range []map[string]any{ch.Ports, ch.ActivePorts, ch.InactivePorts} {
			for name, v := range members {
				if slot, _, ok := key(name); ok {
					if p := byIndex[slot]; p != nil {
						p.LAG = lag
						p.LAGID = id
						// lagMember: LACP has bundled the interface. The
						// partner's system id needs `show lacp neighbor`,
						// which has no capture yet.
						if m, ok := v.(map[string]any); ok && ch.Protocol == "lacp" {
							if lm, ok := m["lagMember"].(bool); ok {
								if p.LACP == nil {
									p.LACP = &devicemodel.LACP{}
								}
								p.LACP.Active = p.LACP.Active || lm // a cage is bundled when a lane is
							}
						}
					}
				}
			}
		}
	}
	for _, inst := range stp.Instances {
		for name, e := range inst.Interfaces {
			if slot, _, ok := key(name); ok {
				if p := byIndex[slot]; p != nil && p.STPPathCost == 0 {
					p.STPPathCost = e.Cost
				}
			}
		}
	}
}

func isCopperMedia(m devicemodel.Media) bool {
	switch m {
	case devicemodel.MediaCopper1G, devicemodel.MediaCopper2G5, devicemodel.MediaCopper10G:
		return true
	}
	return false
}

// macTable converts the unicast table. Entries on non-front-panel ports
// (Cpu, Port-Channel, Vlan) keep PortIndex 0.
func macTable(mt showMACAddressTable, key ifKey) []devicemodel.MACEntry {
	out := make([]devicemodel.MACEntry, 0, len(mt.UnicastTable.Entries))
	for _, e := range mt.UnicastTable.Entries {
		if e.EntryType == "static" && e.Interface == "Cpu" {
			continue // the switch's own address
		}
		m := devicemodel.MACEntry{MAC: strings.ToLower(e.MACAddress), VLAN: e.VLANID}
		if slot, _, ok := key(e.Interface); ok {
			m.PortIndex = slot
		}
		// lastMove is when EOS learned the address on this interface (or
		// moved it there): the entry's learn time. EOS 4.26's table
		// carries no last-seen time, so age is reported 0.
		if e.LastMove > 0 {
			m.LearnedAt = time.Unix(int64(e.LastMove), 0)
		}
		out = append(out, m)
	}
	return out
}

func stpSystem(stp showSpanningTreeFull) (mode string, priority int) {
	for _, inst := range stp.Instances {
		switch inst.Protocol {
		case "rstp":
			mode = "rstp"
		case "mstp":
			mode = "mstp"
		case "rapidPvst", "pvst":
			mode = "rstp"
		default:
			mode = inst.Protocol
		}
		priority = inst.Bridge.Priority
		return
	}
	return "none", 0
}

func fans(c showSystemEnvCooling) []devicemodel.Fan {
	var out []devicemodel.Fan
	for _, t := range c.FanTraySlots {
		out = append(out, devicemodel.Fan{Label: t.Label, SpeedPct: t.Speed, OK: t.Status == "ok"})
	}
	return out
}

func psus(p showSystemEnvPower) []devicemodel.PSU {
	slots := make([]string, 0, len(p.PowerSupplies))
	for slot := range p.PowerSupplies {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	out := make([]devicemodel.PSU, 0, len(slots))
	for _, slot := range slots {
		s := p.PowerSupplies[slot]
		psu := devicemodel.PSU{Slot: slot, Model: s.ModelName, Present: s.State != "notInserted" && s.State != "absent",
			OK: s.State == "ok", OutputW: s.OutputPower, CapacityW: s.Capacity}
		names := make([]string, 0, len(s.TempSensors))
		for n := range s.TempSensors {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			psu.TempsC = append(psu.TempsC, s.TempSensors[n].Temperature)
		}
		out = append(out, psu)
	}
	return out
}
