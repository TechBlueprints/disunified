package aristaeos

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// The structs below are the subset of each command's JSON that the collector
// reads, with field names exactly as EOS 4.26.14M emits them (see
// docs/fixtures/arista-eos-4.26.14M). Unknown fields are ignored by encoding/json.

type showVersion struct {
	ModelName        string  `json:"modelName"`
	SerialNumber     string  `json:"serialNumber"`
	SystemMacAddress string  `json:"systemMacAddress"`
	Version          string  `json:"version"`
	Uptime           float64 `json:"uptime"`
	MemTotal         uint64  `json:"memTotal"`
	MemFree          uint64  `json:"memFree"`
}

type showHostname struct {
	Hostname string `json:"hostname"`
	FQDN     string `json:"fqdn"`
}

type showInterfaces struct {
	Interfaces map[string]eosInterface `json:"interfaces"`
}

type eosInterface struct {
	Name               string `json:"name"`
	InterfaceStatus    string `json:"interfaceStatus"`    // connected | notconnect | disabled | errdisabled | ...
	LineProtocolStatus string `json:"lineProtocolStatus"` // up | down | notPresent
	Bandwidth          int64  `json:"bandwidth"`          // bits/s
	Duplex             string `json:"duplex"`             // duplexFull | duplexHalf | duplexUnknown
	MTU                int    `json:"mtu"`
	Hardware           string `json:"hardware"`
	Description        string `json:"description"`
	PhysicalAddress    string `json:"physicalAddress"`
	InterfaceAddress   []struct {
		PrimaryIP struct {
			Address string `json:"address"`
			MaskLen int    `json:"maskLen"`
		} `json:"primaryIp"`
	} `json:"interfaceAddress"`
	Counters *eosCounters `json:"interfaceCounters"`
}

type eosCounters struct {
	InOctets          uint64 `json:"inOctets"`
	OutOctets         uint64 `json:"outOctets"`
	InUcastPkts       uint64 `json:"inUcastPkts"`
	InMulticastPkts   uint64 `json:"inMulticastPkts"`
	InBroadcastPkts   uint64 `json:"inBroadcastPkts"`
	OutUcastPkts      uint64 `json:"outUcastPkts"`
	OutMulticastPkts  uint64 `json:"outMulticastPkts"`
	OutBroadcastPkts  uint64 `json:"outBroadcastPkts"`
	InDiscards        uint64 `json:"inDiscards"`
	OutDiscards       uint64 `json:"outDiscards"`
	TotalInErrors     uint64 `json:"totalInErrors"`
	TotalOutErrors    uint64 `json:"totalOutErrors"`
	LinkStatusChanges uint64 `json:"linkStatusChanges"`
}

type showLLDPNeighborsDetail struct {
	LLDPNeighbors map[string]struct {
		Info []struct {
			ChassisID           string `json:"chassisId"`
			SystemName          string `json:"systemName"`
			SystemDescription   string `json:"systemDescription"`
			ManagementAddresses []struct {
				Address     string `json:"address"`
				AddressType string `json:"addressType"`
			} `json:"managementAddresses"`
			NeighborInterfaceInfo struct {
				InterfaceIDv2        string `json:"interfaceId_v2"`
				InterfaceID          string `json:"interfaceId"`
				InterfaceDescription string `json:"interfaceDescription"`
			} `json:"neighborInterfaceInfo"`
			SystemCapabilities struct {
				Bridge bool `json:"bridge"`
				Router bool `json:"router"`
			} `json:"systemCapabilities"`
		} `json:"lldpNeighborInfo"`
	} `json:"lldpNeighbors"`
}

type showProcessesTop struct {
	TimeInfo struct {
		LoadAvg []float64 `json:"loadAvg"`
	} `json:"timeInfo"`
	CPUInfo struct {
		CPUs struct {
			Idle float64 `json:"idle"`
		} `json:"%Cpu(s)"`
	} `json:"cpuInfo"`
	MemInfo struct {
		Physical struct {
			MemTotal  uint64 `json:"memTotal"`
			MemUsed   uint64 `json:"memUsed"`
			MemFree   uint64 `json:"memFree"`
			MemBuffer uint64 `json:"memBuffer"`
		} `json:"physicalMem"`
	} `json:"memInfo"`
}

type showSystemEnvTemperature struct {
	SystemStatus string `json:"systemStatus"`
	TempSensors  []struct {
		Name               string  `json:"name"`
		CurrentTemperature float64 `json:"currentTemperature"`
	} `json:"tempSensors"`
}

// ethRe matches front-panel names: Ethernet<slot> or Ethernet<slot>/<lane>.
var ethRe = regexp.MustCompile(`^Ethernet(\d+)(?:/(\d+))?$`)

// parseEthName returns (slot, lane, ok). lane is 0 for an unsplit port.
func parseEthName(name string) (int, int, bool) {
	m := ethRe.FindStringSubmatch(name)
	if m == nil {
		return 0, 0, false
	}
	slot, _ := strconv.Atoi(m[1])
	lane := 0
	if m[2] != "" {
		lane, _ = strconv.Atoi(m[2])
	}
	return slot, lane, true
}

// mediaFor maps EOS's interfaceType (from `show interfaces status`) plus the
// configured bandwidth onto the neutral Media vocabulary. When no optic is
// present EOS says "Not Present" (or "N/A" for breakout lanes), so the cage
// type is inferred from the configured speed instead.
func mediaFor(interfaceType string, bandwidth int64) switchmodel.Media {
	t := strings.ToUpper(interfaceType)
	switch {
	case strings.HasSuffix(t, "BASE-T") || t == "10/100/1000" || strings.HasSuffix(t, "BASE-TX"):
		switch {
		case strings.HasPrefix(t, "10G"):
			return switchmodel.MediaCopper10G
		case strings.HasPrefix(t, "2.5G"):
			return switchmodel.MediaCopper2G5
		default:
			return switchmodel.MediaCopper1G
		}
	case strings.HasPrefix(t, "100GBASE"), strings.HasPrefix(t, "100G"):
		return switchmodel.MediaQSFP28
	case strings.HasPrefix(t, "40GBASE"), strings.HasPrefix(t, "40G"):
		return switchmodel.MediaQSFPPlus
	case strings.HasPrefix(t, "25GBASE"), strings.HasPrefix(t, "25G"):
		return switchmodel.MediaSFP28
	case strings.HasPrefix(t, "10GBASE"), strings.HasPrefix(t, "10G"):
		return switchmodel.MediaSFPPlus
	case strings.HasPrefix(t, "1000BASE"), strings.HasPrefix(t, "1G"):
		return switchmodel.MediaSFP
	}
	// "Not Present", "N/A", "" — infer from configured speed.
	switch {
	case bandwidth >= 100_000_000_000:
		return switchmodel.MediaQSFP28
	case bandwidth >= 40_000_000_000:
		return switchmodel.MediaQSFPPlus
	case bandwidth >= 25_000_000_000:
		return switchmodel.MediaSFP28
	case bandwidth >= 10_000_000_000:
		return switchmodel.MediaSFPPlus
	case bandwidth > 0:
		return switchmodel.MediaSFP
	}
	return switchmodel.MediaUnknown
}

// portsFromInterfaces builds the front-panel port list from `show interfaces`,
// folding breakout lanes (Ethernet50/1..4) into their slot. media is keyed by
// EOS interface name (from `show interfaces status`, captured at startup).
func portsFromInterfaces(si showInterfaces, media map[string]switchmodel.Media) ([]switchmodel.Port, error) {
	type slotAcc struct {
		port  switchmodel.Port
		lanes []string
	}
	slots := map[int]*slotAcc{}

	for name, ifc := range si.Interfaces {
		slot, lane, ok := parseEthName(name)
		if !ok {
			continue // Management1, Port-ChannelN, VlanN, ...
		}
		lanePort := switchmodel.Port{
			Index:       slot,
			IfName:      name,
			Interfaces:  []string{name},
			Description: ifc.Description,
			Media:       media[name],
			Lanes:       1,
			Present:     ifc.LineProtocolStatus != "notPresent",
			Enabled:     ifc.InterfaceStatus != "disabled",
			Up:          ifc.InterfaceStatus == "connected" || ifc.LineProtocolStatus == "up",
			FullDuplex:  ifc.Duplex == "duplexFull",
			MTU:         ifc.MTU,
		}
		if lanePort.Up {
			lanePort.SpeedMbps = int(ifc.Bandwidth / 1_000_000)
		}
		if c := ifc.Counters; c != nil {
			lanePort.Health.LinkChanges = c.LinkStatusChanges
			lanePort.Counters = switchmodel.Counters{
				RxBytes:   c.InOctets,
				TxBytes:   c.OutOctets,
				RxPackets: c.InUcastPkts + c.InMulticastPkts + c.InBroadcastPkts,
				TxPackets: c.OutUcastPkts + c.OutMulticastPkts + c.OutBroadcastPkts,
				RxErrors:  c.TotalInErrors,
				TxErrors:  c.TotalOutErrors,
				RxDropped: c.InDiscards,
				TxDropped: c.OutDiscards,

				RxMulticast: c.InMulticastPkts,
				RxBroadcast: c.InBroadcastPkts,
				TxMulticast: c.OutMulticastPkts,
				TxBroadcast: c.OutBroadcastPkts,
			}
		}

		acc, seen := slots[slot]
		if !seen {
			acc = &slotAcc{port: lanePort}
			slots[slot] = acc
		} else {
			// Fold: any lane up => slot up; counters summed; speed is the
			// lane speed (the controller shows one port, and "4x25G" is
			// better read as 25G than as 100G).
			p := &acc.port
			p.Up = p.Up || lanePort.Up
			p.Present = p.Present || lanePort.Present
			p.Enabled = p.Enabled || lanePort.Enabled
			if p.SpeedMbps == 0 {
				p.SpeedMbps = lanePort.SpeedMbps
			}
			p.FullDuplex = p.FullDuplex || lanePort.FullDuplex
			p.Counters.Add(lanePort.Counters)
			p.Health.LinkChanges += lanePort.Health.LinkChanges
			p.Interfaces = append(p.Interfaces, name)
			if p.Media == switchmodel.MediaUnknown {
				p.Media = lanePort.Media
			}
			if p.Description == "" {
				p.Description = lanePort.Description
			}
		}
		acc.lanes = append(acc.lanes, name)
		_ = lane
	}

	ports := make([]switchmodel.Port, 0, len(slots))
	for _, acc := range slots {
		p := acc.port
		sort.Strings(p.Interfaces)
		if len(acc.lanes) > 1 {
			sort.Strings(acc.lanes)
			p.Lanes = len(acc.lanes)
			p.IfName = fmt.Sprintf("Ethernet%d", p.Index) // the slot, not lane 1
			// Lanes only exist in a QSFP cage; the per-lane media guess
			// (bandwidth-based, e.g. "SFP28" for a 25G lane) is wrong for
			// the cage as a whole.
			switch p.Media {
			case switchmodel.MediaQSFP28, switchmodel.MediaQSFPPlus:
			default:
				p.Media = switchmodel.MediaQSFP28
			}
		}
		if p.Name = p.Description; p.Name == "" {
			p.Name = p.IfName
		}
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Index < ports[j].Index })
	return ports, nil
}

// applyLLDP attaches neighbours to ports. Lane names (Ethernet50/2) are
// mapped onto their folded slot.
func applyLLDP(ports []switchmodel.Port, ld showLLDPNeighborsDetail) {
	byIndex := map[int]*switchmodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	for ifname, entry := range ld.LLDPNeighbors {
		slot, _, ok := parseEthName(ifname)
		if !ok || len(entry.Info) == 0 {
			continue
		}
		p, ok := byIndex[slot]
		if !ok || p.Neighbor != nil {
			continue
		}
		in := entry.Info[0]
		n := &switchmodel.Neighbor{
			SystemName:      in.SystemName,
			SystemDesc:      in.SystemDescription,
			ChassisID:       strings.Trim(in.ChassisID, `"`),
			PortID:          in.NeighborInterfaceInfo.InterfaceIDv2,
			PortDescription: in.NeighborInterfaceInfo.InterfaceDescription,
			IsBridge:        in.SystemCapabilities.Bridge,
			IsRouter:        in.SystemCapabilities.Router,
		}
		if n.PortID == "" {
			n.PortID = strings.Trim(in.NeighborInterfaceInfo.InterfaceID, `"`)
		}
		for _, a := range in.ManagementAddresses {
			if a.AddressType == "ipv4" {
				n.ManagementIP = a.Address
				break
			}
		}
		p.Neighbor = n
	}
}

// applySTP sets STPState from whichever instance lists the port; ports STP
// does not mention are left "" (the presentation layer decides a default).
func applySTPStates(ports []switchmodel.Port, st showSpanningTreeFull) {
	states := map[int]string{}
	for _, inst := range st.Instances {
		for ifname, e := range inst.Interfaces {
			if slot, _, ok := parseEthName(ifname); ok {
				if cur, seen := states[slot]; !seen || cur != "forwarding" {
					states[slot] = e.State
				}
			}
		}
	}
	for i := range ports {
		ports[i].STPState = states[ports[i].Index]
	}
}

func decodeInto(raw json.RawMessage, cmd string, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("parsing %q output: %w", cmd, err)
	}
	return nil
}

func uptime(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}
