package device

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

func macHeader(mac string) [6]byte {
	var out [6]byte
	if hw, err := net.ParseMAC(mac); err == nil && len(hw) == 6 {
		copy(out[:], hw)
	}
	return out
}

// portTable renders one entry per port in the model profile. Live values
// come from the snapshot's port with the same index; a profile port with no
// live counterpart (the claimed model has more ports than the switch) is
// reported down. Config the controller pushed for the port via setstate
// (name, portconf_id, ...) is used as the base so the controller sees its own
// provisioning echoed, with live status fields written over it.
//
// Field set = unifi-emu's minimum-to-adopt ∪ wvengen's switch-view fields.
// portHistory is what the session remembers per port from the previous
// inform, so growth-based anomaly bits mean "since the last report".
type portHistory struct {
	At             time.Time
	Counters       switchmodel.Counters
	LinkChanges    uint64
	STPChanges     int
	FECUncorrected uint64
	PCSErrBlocks   uint64
}

// portAnomalies derives the UniFi per-port anomaly bits plus a satisfaction
// score and reason from neutral port state.
//
// Bits are windowed where UniFi's are: error/drop/flap bits need growth since
// the previous inform. Satisfaction mirrors what this controller's own
// switches report (captured 2026-09-19 across 30 switches): 100, minus 10
// with reason bit 1 when the port has ever dropped packets, minus 15 with
// reason bit 2 when it has ever counted errors. An uplink negotiated below
// its top speed costs a further 10 (our own rule; no UniFi sample had one).
func portAnomalies(p switchmodel.Port, isUplink bool, prev *portHistory) (bits int, satisfaction int, reason int) {
	f := strings.ToLower(p.Fault)
	switch {
	case strings.Contains(f, "link-flap"):
		bits |= anomLinkFlap
	case strings.Contains(f, "bpduguard"):
		bits |= anomBPDUGuard
	case strings.Contains(f, "portsec"):
		bits |= anomPortLock
	case strings.Contains(f, "loopprotect"), strings.Contains(f, "loop-protect"):
		bits |= anomLoopKeepalive
	case strings.Contains(f, "xcvr"):
		bits |= anomSFPEEPROM
	}
	if p.Health.STPInconsistent {
		bits |= anomLoopSTP
	}
	if p.Health.OpticRxAlarm {
		bits |= anomSFPRxFault
	}
	if p.Health.OpticTxAlarm {
		bits |= anomSFPTxFault
	}
	if prev != nil {
		if d := p.Health.LinkChanges - prev.LinkChanges; d >= 2 && p.Health.LinkChanges > prev.LinkChanges {
			bits |= anomLinkFlap
		}
		if d := p.Health.STPChanges - prev.STPChanges; d >= 1 {
			bits |= anomTopologyFlap
			if d >= 3 {
				bits |= anomSTPFlap
			}
		}
	}
	if !p.Up {
		return bits, -1, 0
	}
	satisfaction = 100
	if isUplink && len(p.SpeedCaps) > 0 && p.SpeedMbps > 0 && p.SpeedMbps < p.SpeedCaps[len(p.SpeedCaps)-1] {
		bits |= anomLowUplinkSpeed
		satisfaction -= 10
	}
	if prev != nil {
		errs := p.Counters.RxErrors+p.Counters.TxErrors > prev.Counters.RxErrors+prev.Counters.TxErrors
		if p.Health.HasFECCounters && p.Health.FECUncorrected > prev.FECUncorrected {
			errs = true
		}
		if p.Health.HasPCSCounters && p.Health.PCSErrBlocks > prev.PCSErrBlocks {
			errs = true // PHY-layer errored blocks: bad cable/optic before frames are even lost
		}
		if errs || p.Health.PCSHighBER {
			bits |= anomTransmissionError
		}
		if p.Counters.RxDropped+p.Counters.TxDropped > prev.Counters.RxDropped+prev.Counters.TxDropped {
			bits |= anomDroppedTraffic
		}
	}
	if p.Counters.RxDropped+p.Counters.TxDropped > 0 {
		satisfaction -= 10
		reason |= 1
	}
	if p.Counters.RxErrors+p.Counters.TxErrors > 0 || (p.Health.HasFECCounters && p.Health.FECUncorrected > 0) {
		satisfaction -= 15
		reason |= 2
	}
	if satisfaction < 0 {
		satisfaction = 0
	}
	return bits, satisfaction, reason
}

func portTable(desc inform.Descriptor, snap *switchmodel.Snapshot, provisioned json.RawMessage, prev map[int]portHistory) []map[string]any {
	live := map[int]switchmodel.Port{}
	if snap != nil {
		for _, p := range snap.Ports {
			live[p.Index] = p
		}
	}
	pushed := map[int]map[string]any{}
	if len(provisioned) > 0 {
		var entries []map[string]any
		if json.Unmarshal(provisioned, &entries) == nil {
			for _, e := range entries {
				if idx, ok := e["port_idx"].(float64); ok {
					pushed[int(idx)] = e
				}
			}
		}
	}

	table := make([]map[string]any, 0, len(desc.Ports))
	for _, pp := range desc.Ports {
		e := map[string]any{}
		for k, v := range pushed[pp.PortIdx] {
			e[k] = v
		}
		e["ifname"] = pp.IfName
		if p, ok := live[pp.PortIdx]; ok && p.IfName != "" {
			// The vendor interface name, which is also what the switch puts in
			// its LLDP port ID; a parent's LLDP view of us names this port.
			e["ifname"] = p.IfName
		}
		e["port_idx"] = pp.PortIdx
		e["media"] = pp.Media
		if p, ok := live[pp.PortIdx]; ok {
			if m := unifiMedia(p.Media); m != "" {
				e["media"] = m // the port's real medium beats the claimed model's profile
			}
		}
		e["poe_caps"] = pp.PoECaps
		e["port_poe"] = false
		e["is_uplink"] = pp.IsUplink
		if p, ok := live[pp.PortIdx]; ok {
			var ph *portHistory
			if h, ok := prev[pp.PortIdx]; ok {
				ph = &h
			}
			bits, sat, reason := portAnomalies(p, pp.IsUplink, ph)
			e["anomalies"] = bits
			e["custom_anomalies"] = 0
			if sat >= 0 {
				e["satisfaction"] = sat
				e["satisfaction_reason"] = reason
			}
			// Byte rates since the previous inform (bytes/s), as real switches send.
			rx, tx := 0.0, 0.0
			if ph != nil && !ph.At.IsZero() {
				if secs := snap.TakenAt.Sub(ph.At).Seconds(); secs > 0 {
					rx = float64(p.Counters.RxBytes-ph.Counters.RxBytes) / secs
					tx = float64(p.Counters.TxBytes-ph.Counters.TxBytes) / secs
					if p.Counters.RxBytes < ph.Counters.RxBytes || p.Counters.TxBytes < ph.Counters.TxBytes {
						rx, tx = 0, 0 // counters reset
					}
				}
			}
			e["rx_bytes-r"], e["tx_bytes-r"], e["bytes-r"] = rx, tx, rx+tx
		}
		if _, hasName := e["name"]; !hasName {
			// A UniFi switch reports its physical port label here — the same
			// string it advertises as its LLDP port ID ("Port 13"). Ours is the
			// vendor interface name, which is also our LLDP port ID.
			e["name"] = pp.Name
			if p, ok := live[pp.PortIdx]; ok && p.IfName != "" {
				e["name"] = p.IfName
			}
		}

		p, ok := live[pp.PortIdx]
		if !ok {
			e["enable"] = false
			e["up"] = false
			e["speed"] = 0
			e["full_duplex"] = false
			e["stp_state"] = "disabled"
			for _, k := range []string{"rx_bytes", "tx_bytes", "rx_packets", "tx_packets", "rx_errors", "tx_errors", "rx_dropped", "tx_dropped"} {
				e[k] = 0
			}
			table = append(table, e)
			continue
		}
		if snap != nil && p.Description != "" {
			e["name"] = p.Description
		}
		e["enable"] = p.Enabled
		e["up"] = p.Up
		e["speed"] = p.SpeedMbps
		e["full_duplex"] = p.FullDuplex
		e["rx_bytes"] = p.Counters.RxBytes
		e["tx_bytes"] = p.Counters.TxBytes
		e["rx_packets"] = p.Counters.RxPackets
		e["tx_packets"] = p.Counters.TxPackets
		e["rx_errors"] = p.Counters.RxErrors
		e["tx_errors"] = p.Counters.TxErrors
		e["rx_dropped"] = p.Counters.RxDropped
		e["tx_dropped"] = p.Counters.TxDropped
		if p.MTU > 0 {
			e["mtu"] = p.MTU
		}
		switch {
		case p.STPState != "":
			e["stp_state"] = p.STPState
		case p.Up:
			e["stp_state"] = "forwarding"
		default:
			e["stp_state"] = "disabled"
		}
		e["rx_multicast"] = p.Counters.RxMulticast
		e["rx_broadcast"] = p.Counters.RxBroadcast
		e["tx_multicast"] = p.Counters.TxMulticast
		e["tx_broadcast"] = p.Counters.TxBroadcast
		e["autoneg"] = p.AutoNeg
		e["flowctrl_rx"] = p.FlowCtrlRx
		e["flowctrl_tx"] = p.FlowCtrlTx
		e["jumbo"] = p.MTU > 1518
		if p.STPPathCost > 0 {
			e["stp_pathcost"] = p.STPPathCost
		}
		if p.LAG != "" {
			e["op_mode"] = "aggregate"
			e["aggregated_by"] = true
		} else {
			if _, ok := e["op_mode"]; !ok {
				e["op_mode"] = "switch"
			}
			e["aggregated_by"] = false
		}
		if p.FEC != switchmodel.FECUnknown {
			// Real informs carry `fec` in the 802.3 clause vocabulary; the
			// controller derives fec_mode from it.
			switch p.FEC {
			case switchmodel.FECRS:
				e["fec"] = "cl-91"
			case switchmodel.FECFC:
				e["fec"] = "cl-74"
			}
		}
		if sc := speedCaps(p); sc != 0 {
			e["speed_caps"] = sc
		}
		if p.Media != switchmodel.MediaUnknown && !isCopper(p.Media) {
			e["sfp_found"] = p.Present
			if o := p.Optic; o != nil {
				e["sfp_vendor"] = o.Vendor
				e["sfp_part"] = o.Part
				e["sfp_serial"] = o.Serial
				e["sfp_compliance"] = o.MediaType
				if o.HasDOM {
					e["sfp_temperature"] = round1(o.TempC)
					e["sfp_voltage"] = round2(o.VoltageV)
					e["sfp_current"] = round2(o.TxBiasMA)
					e["sfp_txpower"] = round2(o.TxPowerDBm)
					e["sfp_rxpower"] = round2(o.RxPowerDBm)
				}
			}
		}
		if len(p.MACs) > 0 {
			e["mac_table"] = macEntries(p.MACs, false, snap.TakenAt)
		}
		// Counters and flags UniFi switches report per port.
		e["mac_table_count"] = len(p.MACs)
		e["link_down_count"] = p.Health.LinkChanges / 2 // EOS counts transitions; UniFi counts drops
		e["stp_state_change_count"] = []map[string]any{{"change_count": p.Health.STPChanges, "mst": 0}}
		if p.STPRole != "" {
			e["stp_role"] = p.STPRole
		} else if !p.Up {
			e["stp_role"] = "disabled"
		}
		e["dot1x_mode"] = "unknown" // no 802.1X: what a real switch reports with it off
		e["dot1x_status"] = "disabled"
		if p.Media != switchmodel.MediaUnknown && !isCopper(p.Media) {
			e["sfp_rxfault"] = p.Health.OpticRxAlarm
			e["sfp_txfault"] = p.Health.OpticTxAlarm
		}
		// Per-port settings a UniFi switch echoes from its own config; the
		// bridge applies these from system_cfg, so report the applied state
		// (or the only state the switch has, for knobs it cannot change).
		setDefault := func(k string, v any) {
			if _, ok := e[k]; !ok {
				e[k] = v
			}
		}
		setDefault("stp_port_mode", !p.STPEdge)
		setDefault("stp_edge_port", p.STPEdge)
		setDefault("lldpmed_enabled", true)
		setDefault("port_keepalive_enabled", false)
		setDefault("isolation", false)
		setDefault("egress_rate_limit_kbps_enabled", false)
		setDefault("port_security_enabled", false)
		setDefault("port_security_mac_address", []string{})
		setDefault("locating", false)
		table = append(table, e)
	}
	return table
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// macEntries renders learned addresses. age = seconds since the address was
// learned/moved (0 if unknown); withPort adds port_idx for the top-level table.
func macEntries(macs []switchmodel.MACEntry, withPort bool, now time.Time) []map[string]any {
	out := make([]map[string]any, 0, len(macs))
	for _, m := range macs {
		e := map[string]any{"mac": m.MAC, "vlan": m.VLAN, "age": 0, "uptime": 0}
		if !m.LastMove.IsZero() {
			secs := int(now.Sub(m.LastMove).Seconds())
			if secs < 0 {
				secs = 0
			}
			e["age"] = secs
			e["uptime"] = secs
		}
		if withPort {
			e["port_idx"] = m.PortIndex
		}
		out = append(out, e)
	}
	return out
}

// switchTables renders the switch-level fields derived from the snapshot.
func switchTables(desc inform.Descriptor, snap *switchmodel.Snapshot) map[string]any {
	m := map[string]any{}
	if snap == nil {
		return m
	}
	sys := snap.System
	if sys.STPMode != "" {
		m["stp_version"] = sys.STPMode
	}
	if sys.STPPriority > 0 {
		m["stp_priority"] = sys.STPPriority // a number, as real switches send it
	}
	if sys.STPRoot != "" {
		m["root_switch"] = sys.STPRoot // what UniFi switches report; the topology's STP root marker
	}
	m["sys_error_caps"] = SysErrorCaps
	m["overheating"] = sys.Overheating
	fanOK, psuOK := true, true
	// fan_table / psu_table use the keys real UniFi switches report (captured
	// from a USW-Aggregation-Pro on 10.6): the UI shows a supply as "Not
	// Installed" unless `present` is true, and reads `power`/`power_capacity`.
	if len(sys.Fans) > 0 {
		m["has_fan"] = true
		level := 0
		fans := make([]map[string]any, 0, len(sys.Fans))
		for i, f := range sys.Fans {
			if f.SpeedPct > level {
				level = f.SpeedPct
			}
			fanOK = fanOK && f.OK
			fans = append(fans, map[string]any{"index": i + 1, "label": f.Label, "present": true, "status": f.OK,
				"speed": f.SpeedPct, "target_speed": f.SpeedPct, "critical_state": 0, "emergency": false})
		}
		m["fan_level"] = level
		m["fan_ok"] = fanOK
		m["fan_table"] = fans
	}
	if len(sys.PSUs) > 0 {
		var capW, outW float64
		psus := make([]map[string]any, 0, len(sys.PSUs))
		for i, p := range sys.PSUs {
			capW += p.CapacityW
			outW += p.OutputW
			psuOK = psuOK && (!p.Present || p.OK) // an empty slot is not a fault
			idx := i + 1
			if n, err := strconv.Atoi(p.Slot); err == nil {
				idx = n
			}
			e := map[string]any{"psu_idx": idx, "label": "Psu" + strconv.Itoa(idx), "present": p.Present, "online": p.OK,
				"power": round1(p.OutputW), "watt": round1(p.OutputW), "power_capacity": p.CapacityW, "maxwatt": p.CapacityW,
				"part_no": p.Model, "psu_stat": ""}
			if len(p.TempsC) > 0 {
				temps := map[string]any{}
				for j, t := range p.TempsC {
					temps[strconv.Itoa(j)] = t
				}
				e["temp"] = temps
			}
			psus = append(psus, e)
		}
		m["total_max_power"] = int(capW)
		m["power_consumption"] = round1(outW)
		m["psu_table"] = psus
	}
	// sys_error: the fault bits currently raised (same vocabulary as the claim).
	sysErr := 0
	if sys.Overheating {
		sysErr |= sysErrOverheating
	}
	if !fanOK {
		sysErr |= sysErrFanIssue
	}
	if !psuOK {
		sysErr |= sysErrPSUIssue
	}
	m["sys_error"] = sysErr
	jumbo, flow := false, false
	for _, p := range snap.Ports {
		jumbo = jumbo || p.MTU > 1518
		flow = flow || p.FlowCtrlRx || p.FlowCtrlTx
	}
	m["jumboframe_enabled"] = jumbo
	m["flowctrl_enabled"] = flow
	m["dot1x_portctrl_enabled"] = false

	var live []switchmodel.MACEntry
	for _, e := range snap.MACTable {
		if e.PortIndex > 0 {
			live = append(live, e)
		}
	}
	m["mac_table"] = macEntries(live, true, snap.TakenAt)

	if up := snap.UplinkPort(); up > 0 {
		for _, p := range snap.Ports {
			if p.Index != up {
				continue
			}
			// A UniFi switch reports `uplink` as the NAME of its management
			// interface in if_table (a string, "eth0"), never as an object;
			// the controller composes the uplink record from that interface,
			// port_table[].is_uplink and LLDP. (Seen in real informs, 2026-09-20.)
			m["uplink"] = "eth0"
			m["if_table"] = []map[string]any{{
				"name": "eth0", "mac": desc.MAC, "ip": desc.IP, "netmask": netmaskFor(snap, desc.IP), "num_port": len(desc.Ports),
				"up": p.Up, "speed": p.SpeedMbps, "full_duplex": p.FullDuplex,
				"rx_bytes": p.Counters.RxBytes, "tx_bytes": p.Counters.TxBytes,
				"rx_packets": p.Counters.RxPackets, "tx_packets": p.Counters.TxPackets,
				"rx_errors": p.Counters.RxErrors, "tx_errors": p.Counters.TxErrors,
				"rx_dropped": p.Counters.RxDropped, "tx_dropped": p.Counters.TxDropped,
				"rx_multicast": p.Counters.RxMulticast,
			}}
		}
	}
	// STP topology changes across ports, as a switch-wide count.
	stpChanges := 0
	macsInUse := 0
	for _, p := range snap.Ports {
		stpChanges += p.Health.STPChanges
		macsInUse += len(p.MACs)
	}
	m["stp_topology_change_count"] = stpChanges
	m["total_mac_in_used"] = macsInUse
	return m
}

func mediaLabel(desc inform.Descriptor, idx int) string {
	for _, pp := range desc.Ports {
		if pp.PortIdx == idx {
			return pp.Media
		}
	}
	return ""
}

// trailingInt returns the number at the end of s ("one00GigE48" -> 48), 0 if none.
// remotePortIndex turns an LLDP port ID into the neighbour's 1-based port
// index. UniFi devices advertise "Port N" (already 1-based) or an interface
// name with a zero-based number ("twenty5GigE41" is port 42, "one00GigE48"
// is port 49); anything else keeps its trailing number.
func remotePortIndex(portID string) int {
	if n, err := strconv.Atoi(strings.TrimPrefix(portID, "Port ")); err == nil {
		return n
	}
	if strings.HasSuffix(strings.ToLower(portID), "gige"+strconv.Itoa(trailingInt(portID))) {
		return trailingInt(portID) + 1
	}
	return trailingInt(portID)
}

func trailingInt(s string) int {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(s[i:])
	return n
}

// unifiMedia maps the neutral media vocabulary onto the controller's port
// media labels (seen in its hardware DB: GE, 2.5GbE, 10GbE, SFP, SFP+,
// SFP28, QSFP28). "" = keep the profile's label.
func unifiMedia(m switchmodel.Media) string {
	switch m {
	case switchmodel.MediaCopper1G:
		return "GE"
	case switchmodel.MediaCopper2G5:
		return "2.5GbE"
	case switchmodel.MediaCopper10G:
		return "10GbE"
	case switchmodel.MediaSFP:
		return "SFP"
	case switchmodel.MediaSFPPlus:
		return "SFP+"
	case switchmodel.MediaSFP28:
		return "SFP28"
	case switchmodel.MediaQSFPPlus:
		return "QSFP+"
	case switchmodel.MediaQSFP28:
		return "QSFP28"
	}
	return ""
}

func isCopper(m switchmodel.Media) bool {
	switch m {
	case switchmodel.MediaCopper1G, switchmodel.MediaCopper2G5, switchmodel.MediaCopper10G:
		return true
	}
	return false
}

// netmaskFor returns the dotted netmask of the switch address ip, from the
// snapshot's own interface addresses; "" if unknown.
func netmaskFor(snap *switchmodel.Snapshot, ip string) string {
	if snap == nil {
		return ""
	}
	for _, a := range snap.System.Addresses {
		if a.IP == ip && a.PrefixLen > 0 && a.PrefixLen <= 32 {
			m := net.CIDRMask(a.PrefixLen, 32)
			return net.IP(m).String()
		}
	}
	return ""
}

// ethernetTable lists the device's own interfaces as UniFi switches do:
// eth0 (the switch, system MAC) and srv0, the service/management interface
// with its own MAC, when the switch has one.
func ethernetTable(desc inform.Descriptor, snap *switchmodel.Snapshot) []map[string]any {
	t := []map[string]any{{
		"mac":        desc.MAC,
		"name":       "eth0",
		"num_port":   len(desc.Ports),
		"other_macs": []string{},
	}}
	if mac := serviceMAC(snap, desc.MAC); mac != "" {
		t = append(t, map[string]any{"mac": mac, "name": "srv0"})
	}
	return t
}

// serviceMAC is the management interface's MAC to report as the service
// interface — but only while that port is unplugged. A UniFi switch's
// service interface is never seen on the wire; if ours is cabled, another
// UniFi switch has that MAC as a client on one of its ports, and naming it
// the service MAC gives the controller a second location for the device.
func serviceMAC(snap *switchmodel.Snapshot, deviceMAC string) string {
	if snap == nil || snap.System.MgmtMAC == "" || snap.System.MgmtMAC == deviceMAC {
		return ""
	}
	for _, o := range snap.System.OOBInterfaces {
		if o.Up {
			return ""
		}
	}
	return snap.System.MgmtMAC
}

// lldpTable reports LLDP neighbours in the controller's shape.
func lldpTable(desc inform.Descriptor, snap *switchmodel.Snapshot) []map[string]any {
	if snap == nil {
		return nil
	}
	ifname := map[int]string{}
	for _, pp := range desc.Ports {
		ifname[pp.PortIdx] = pp.IfName
	}
	var table []map[string]any
	for _, p := range snap.Ports {
		if p.Neighbor == nil {
			continue
		}
		name := ifname[p.Index]
		if p.IfName != "" {
			name = p.IfName
		}
		e := map[string]any{
			"local_port_idx":  p.Index,
			"local_port_name": name,
			"chassis_id":      p.Neighbor.ChassisID,
			"port_id":         p.Neighbor.PortID,
			"is_wired":        true,
		}
		// UniFi devices advertise zero-based interface names ("twenty5GigE41"
		// is port 42; "one00GigE48" is port 49). The controller maps the
		// SFP28 form for its own switches; report the 1-based "Port N" form
		// it always understands, so a neighbour on a QSFP28 port resolves.
		if n := remotePortIndex(p.Neighbor.PortID); n > 0 && strings.HasSuffix(strings.ToLower(p.Neighbor.PortID), "gige"+strconv.Itoa(n-1)) {
			e["port_id"] = "Port " + strconv.Itoa(n)
		}
		if p.Neighbor.ManagementIP != "" {
			e["mgmt_ips"] = []string{p.Neighbor.ManagementIP} // as UniFi switches report their neighbours
		}
		table = append(table, e)
	}
	sort.Slice(table, func(i, j int) bool { return table[i]["local_port_idx"].(int) < table[j]["local_port_idx"].(int) })
	return table
}

// sysStats reports CPU/memory. Memory is in bytes on the wire; the
// snapshot carries kB. Without a snapshot, unifi-emu's placeholder values.
func sysStats(snap *switchmodel.Snapshot) map[string]any {
	if snap == nil || snap.System.MemTotalKB == 0 {
		return map[string]any{"cpu": 1.5, "mem_total": 134217728, "mem_used": 67108864, "mem_buffer": 16777216}
	}
	s := snap.System
	m := map[string]any{
		"cpu":        s.CPUPercent,
		"mem_total":  s.MemTotalKB * 1024,
		"mem_used":   s.MemUsedKB * 1024,
		"mem_buffer": s.MemBufferKB * 1024,
	}
	if len(s.LoadAvg) == 3 {
		m["loadavg_1"] = fmt.Sprintf("%.2f", s.LoadAvg[0])
		m["loadavg_5"] = fmt.Sprintf("%.2f", s.LoadAvg[1])
		m["loadavg_15"] = fmt.Sprintf("%.2f", s.LoadAvg[2])
	}
	return m
}

// systemStats is the `system-stats` object UniFi switches send alongside
// sys_stats: percentages as strings. The UI's "Memory Usage" reads mem here.
func systemStats(snap *switchmodel.Snapshot, uptime int64) map[string]any {
	if snap == nil || snap.System.MemTotalKB == 0 {
		return map[string]any{}
	}
	s := snap.System
	return map[string]any{
		"cpu":    fmt.Sprintf("%.1f", s.CPUPercent),
		"mem":    fmt.Sprintf("%.1f", 100*float64(s.MemUsedKB)/float64(s.MemTotalKB)),
		"uptime": strconv.FormatInt(uptime, 10),
	}
}

// macTableCapability mirrors what UniFi switches report for the MAC table
// pressure card; thresholds follow the ratios seen on a real switch
// (warning at 66%, critical at 79% of capacity).
func macTableCapability(snap *switchmodel.Snapshot) map[string]any {
	if snap == nil || snap.System.MACTableCapacity == 0 {
		return nil
	}
	c := snap.System.MACTableCapacity
	return map[string]any{
		"capacity": c, "count": snap.System.MACTableUsed,
		"threshold_warning": c * 66 / 100, "threshold_critical": c * 79 / 100,
	}
}
