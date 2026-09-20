package aristaeos

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// Shapes of the second wave of commands, all verified on EOS 4.26.14M
// (docs/fixtures/eos-4.26.14M, captured 2026-09-19).

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
		MfgName   string `json:"mfgName"`
		ModelName string `json:"modelName"`
		SerialNum string `json:"serialNum"`
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
		Ports         map[string]any `json:"ports"` // 4.26.14M: members keyed by interface name
		ActivePorts   map[string]any `json:"activePorts"`
		InactivePorts map[string]any `json:"inactivePorts"`
	} `json:"portChannels"`
}

type showSystemEnvCooling struct {
	SystemStatus string `json:"systemStatus"`
	FanTraySlots []struct {
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
			State        string          `json:"state"`
			Role         string          `json:"role"` // root | designated | alternate | backup | disabled
			Cost         int             `json:"cost"`
			Inconsistent map[string]bool `json:"inconsistentFeatures"` // loopGuard, rootGuard, bridgeAssurance, mstPvstBorder
		} `json:"interfaces"`
	} `json:"spanningTreeInstances"`
}

// showTransceiverThresholds is `show interfaces transceiver dom thresholds`:
// per optic, each DOM parameter's per-channel value and alarm thresholds.
// Cages without a DOM-capable optic (DACs, empty) have no parameters.
type showTransceiverThresholds struct {
	Interfaces map[string]struct {
		Parameters map[string]struct {
			Channels  map[string]float64 `json:"channels"`
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
func applyOpticAlarms(ports []switchmodel.Port, th showTransceiverThresholds) {
	byIndex := map[int]*switchmodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	for name, e := range th.Interfaces {
		slot, _, ok := parseEthName(name)
		if !ok || byIndex[slot] == nil {
			continue
		}
		out := func(param string) bool {
			p, ok := e.Parameters[param]
			if !ok || p.Threshold == nil {
				return false
			}
			for _, v := range p.Channels {
				if v < p.Threshold.LowAlarm || v > p.Threshold.HighAlarm {
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

// applySTPChanges sums each port's topology-change count over its lanes,
// from the Cist topology (falling back to whatever topology lists the port).
func applySTPChanges(ports []switchmodel.Port, ts showSTPTopologyStatus) {
	byIndex := map[int]*switchmodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	topo, ok := ts.Topologies["Cist"]
	if !ok {
		for _, t := range ts.Topologies {
			topo = t
			break
		}
	}
	for name, e := range topo.Interfaces {
		if slot, _, ok := parseEthName(name); ok && byIndex[slot] != nil {
			byIndex[slot].Health.STPChanges += e.NumChanges
		}
	}
}

// applySTPInconsistent flags ports an STP guard holds inconsistent.
func applySTPInconsistent(ports []switchmodel.Port, st showSpanningTreeFull) {
	byIndex := map[int]*switchmodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	for _, inst := range st.Instances {
		for name, e := range inst.Interfaces {
			slot, _, ok := parseEthName(name)
			if !ok || byIndex[slot] == nil {
				continue
			}
			for _, v := range e.Inconsistent {
				if v {
					byIndex[slot].Health.STPInconsistent = true
				}
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
	pcsErrBlocksRe   = regexp.MustCompile(`PCS err blocks\s+(\d+)`)
	pcsHighBERRe     = regexp.MustCompile(`PCS high BER\s+(\S+)`)
)

// phyDetail is what `show interfaces <name> phy detail` (text; EOS 4.26 has
// no JSON for these counters) yields per lane. The first number on a line is
// the counter; the rest is change bookkeeping.
type phyDetail struct {
	HasFEC                  bool
	FECCorrected, FECUncorr uint64
	HasPCS                  bool
	PCSErrBlocks            uint64 // summed over the PCS sections (lanes)
	PCSHighBER              bool   // any section not "ok"
}

func parsePHYDetail(text string) phyDetail {
	var d phyDetail
	if c, u := fecCorrectedRe.FindStringSubmatch(text), fecUncorrectedRe.FindStringSubmatch(text); c != nil && u != nil {
		d.HasFEC = true
		d.FECCorrected, _ = strconv.ParseUint(c[1], 10, 64)
		d.FECUncorr, _ = strconv.ParseUint(u[1], 10, 64)
	}
	for _, m := range pcsErrBlocksRe.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.ParseUint(m[1], 10, 64)
		d.PCSErrBlocks += n
		d.HasPCS = true
	}
	for _, m := range pcsHighBERRe.FindAllStringSubmatch(text, -1) {
		d.HasPCS = true
		if m[1] != "ok" {
			d.PCSHighBER = true
		}
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
	media   switchmodel.Media
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

func fecFor(status string) switchmodel.FEC {
	switch status {
	case "reedSolomon", "reedSolomon544":
		return switchmodel.FECRS
	case "fireCode":
		return switchmodel.FECFC
	case "disabled":
		return switchmodel.FECDisabled
	}
	return switchmodel.FECUnknown
}

// applyExtras attaches the second-wave data to ports (by slot; lane data is
// taken from lane 1 or, for FEC/flow control, from any lane that reports it).
func applyExtras(ports []switchmodel.Port, static map[string]portStatic,
	xcvr showInterfacesTransceiver, inv showInventory, fec showInterfacesErrorCorrection,
	fc showInterfaceFlowControl, pc showPortChannelSummary, stp showSpanningTreeFull) {
	byIndex := map[int]*switchmodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	for name, st := range static {
		if slot, lane, ok := parseEthName(name); ok && lane <= 1 {
			if p := byIndex[slot]; p != nil {
				p.AutoNeg = st.autoNeg
			}
		}
	}
	for name, x := range xcvr.Interfaces {
		slot, lane, ok := parseEthName(name)
		if !ok || lane > 1 {
			continue
		}
		p := byIndex[slot]
		if p == nil || isCopperMedia(p.Media) || !p.Present {
			continue
		}
		o := &switchmodel.Optic{MediaType: x.MediaType, Serial: x.VendorSn}
		if x.Temperature != 0 || x.RxPower != 0 || x.TxPower != 0 {
			o.HasDOM = true
			o.TempC, o.VoltageV, o.TxBiasMA, o.TxPowerDBm, o.RxPowerDBm = x.Temperature, x.Voltage, x.TxBias, x.TxPower, x.RxPower
		}
		if slotInv, ok := inv.XcvrSlots[strconv.Itoa(slot)]; ok {
			o.Vendor, o.Part = slotInv.MfgName, slotInv.ModelName
			if o.Serial == "" {
				o.Serial = slotInv.SerialNum
			}
		}
		p.Optic = o
	}
	for name, st := range fec.Statuses {
		if slot, _, ok := parseEthName(name); ok {
			if p := byIndex[slot]; p != nil && (p.FEC == switchmodel.FECUnknown || p.FEC == switchmodel.FECDisabled) {
				p.FEC = fecFor(st.Status)
			}
		}
	}
	for name, st := range fc.Interfaces {
		if slot, _, ok := parseEthName(name); ok {
			if p := byIndex[slot]; p != nil {
				p.FlowCtrlRx = p.FlowCtrlRx || st.RxOper == "on"
				p.FlowCtrlTx = p.FlowCtrlTx || st.TxOper == "on"
			}
		}
	}
	for lag, ch := range pc.PortChannels {
		id, _ := strconv.Atoi(strings.TrimPrefix(lag, "Port-Channel"))
		for _, members := range []map[string]any{ch.Ports, ch.ActivePorts, ch.InactivePorts} {
			for name := range members {
				if slot, _, ok := parseEthName(name); ok {
					if p := byIndex[slot]; p != nil {
						p.LAG = lag
						p.LAGID = id
					}
				}
			}
		}
	}
	for _, inst := range stp.Instances {
		for name, e := range inst.Interfaces {
			if slot, _, ok := parseEthName(name); ok {
				if p := byIndex[slot]; p != nil && p.STPPathCost == 0 {
					p.STPPathCost = e.Cost
				}
			}
		}
	}
}

func isCopperMedia(m switchmodel.Media) bool {
	switch m {
	case switchmodel.MediaCopper1G, switchmodel.MediaCopper2G5, switchmodel.MediaCopper10G:
		return true
	}
	return false
}

// macTable converts the unicast table. Entries on non-front-panel ports
// (Cpu, Port-Channel, Vlan) keep PortIndex 0.
func macTable(mt showMACAddressTable) []switchmodel.MACEntry {
	out := make([]switchmodel.MACEntry, 0, len(mt.UnicastTable.Entries))
	for _, e := range mt.UnicastTable.Entries {
		if e.EntryType == "static" && e.Interface == "Cpu" {
			continue // the switch's own address
		}
		m := switchmodel.MACEntry{MAC: strings.ToLower(e.MACAddress), VLAN: e.VLANID}
		if slot, _, ok := parseEthName(e.Interface); ok {
			m.PortIndex = slot
		}
		if e.LastMove > 0 {
			m.LastMove = time.Unix(int64(e.LastMove), 0)
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

func fans(c showSystemEnvCooling) []switchmodel.Fan {
	var out []switchmodel.Fan
	for _, t := range c.FanTraySlots {
		out = append(out, switchmodel.Fan{Label: t.Label, SpeedPct: t.Speed, OK: t.Status == "ok"})
	}
	return out
}

func psus(p showSystemEnvPower) []switchmodel.PSU {
	slots := make([]string, 0, len(p.PowerSupplies))
	for slot := range p.PowerSupplies {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	out := make([]switchmodel.PSU, 0, len(slots))
	for _, slot := range slots {
		s := p.PowerSupplies[slot]
		psu := switchmodel.PSU{Slot: slot, Model: s.ModelName, Present: s.State != "notInserted" && s.State != "absent",
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
