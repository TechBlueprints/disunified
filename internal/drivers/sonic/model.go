package sonic

import (
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The mapping from SONiC's tables onto the neutral model
// (docs/drivers/sonic.md §5.2).

// cagePort is a front-panel port plus what the namer needs.
type cagePort struct {
	devicemodel.Port
	alias string
}

type membership struct {
	vid    int
	tagged bool
}

// ifNum is the number in "Ethernet12", for ordering a cage's interfaces.
func ifNum(name string) int {
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(name[i:])
	if err != nil {
		return 1 << 30
	}
	return n
}

// buildPorts makes one Port per front-panel cage. A cage is every CONFIG_DB
// PORT entry sharing an `index`: one entry for an unsplit cage, the child
// ports (Ethernet0..3) of a broken-out one, folded the way the Arista driver
// folds lanes. An untagged membership of Vlan<native> is UniFi's native
// VLAN 1 (SONiC has no VLAN 1); every other membership keeps its id. plat
// is platform.json's interfaces block (nil where absent): a cage it lists
// claims every speed of its breakout modes, so the controller offers the
// split and the join.
func buildPorts(db map[string]table, native int, plat map[string]platformPort) []cagePort {
	platCaps := map[int][]int{}
	for _, pp := range plat {
		platCaps[pp.cageIndex()] = pp.speeds()
	}
	cfg := db["cfg_port"].byName("|")
	appl := db["appl_port"].byName(":")
	state := db["state_port"].byName("|")
	oids := db["counters_map"].first()
	counters := db["counters"].byName(":")
	lldp := db["lldp"].byName(":")
	xcvr := db["xcvr"].byName("|")
	dom := db["dom"].byName("|")

	vlans := map[string][]membership{}
	for k, f := range db["cfg_vlan_member"] {
		parts := strings.Split(k, "|") // VLAN_MEMBER|Vlan1000|Ethernet0
		if len(parts) != 3 {
			continue
		}
		vid := atoi(strings.TrimPrefix(parts[1], "Vlan"))
		if vid <= 0 {
			continue
		}
		tagged := f["tagging_mode"] == "tagged"
		if !tagged && vid == native {
			vid = 1
		}
		vlans[parts[2]] = append(vlans[parts[2]], membership{vid: vid, tagged: tagged})
	}
	lags := map[string]string{}
	for k := range db["cfg_lag_member"] {
		parts := strings.Split(k, "|") // PORTCHANNEL_MEMBER|PortChannel1|Ethernet0
		if len(parts) == 3 {
			lags[parts[2]] = parts[1]
		}
	}
	stpPort := map[string]map[string]string{}
	for k, f := range db["cfg_stp"] {
		if name, ok := strings.CutPrefix(k, "STP_PORT|"); ok {
			stpPort[name] = f
		}
	}
	storm := stormTable(db["cfg_storm"])

	byIndex := map[int][]string{}
	for name, f := range cfg {
		idx := atoi(f["index"])
		if idx <= 0 {
			idx = atoi(appl[name]["index"])
		}
		if idx <= 0 {
			continue
		}
		byIndex[idx] = append(byIndex[idx], name)
	}
	idxs := make([]int, 0, len(byIndex))
	for i := range byIndex {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)

	var out []cagePort
	for _, idx := range idxs {
		names := byIndex[idx]
		sort.Slice(names, func(i, j int) bool { return ifNum(names[i]) < ifNum(names[j]) })
		var cage cagePort
		cageLanes := 0
		var caps []int
		for i, name := range names {
			// A LAG member has no VLAN config of its own (SONiC refuses
			// it); it reports the PortChannel's, as the Arista's do.
			vl := vlans[name]
			if lag := lags[name]; lag != "" {
				vl = vlans[lag]
			}
			lp := lanePort(idx, name, cfg[name], appl[name], state[name], counters[oids[name]], vl, lags[name], stpPort[name])
			lp.StormCtrl = stormSpec(storm[name], atoi(cfg[name]["speed"]))
			// A member runs at its PortChannel's MTU.
			if lag := lags[name]; lag != "" {
				if m := atoi(db["cfg_lag"]["PORTCHANNEL|"+lag]["mtu"]); m > 0 {
					lp.MTU = m
				}
			}
			cageLanes += laneCount(cfg[name]["lanes"])
			caps = mergeSpeeds(caps, speedList(state[name]["supported_speeds"]))
			if n := lldp[name]; len(n) > 0 && lp.Neighbor == nil {
				lp.Neighbor = neighborOf(n)
			}
			if i == 0 {
				cage = cagePort{Port: lp, alias: cageAlias(cfg[name]["alias"], len(names) > 1)}
				continue
			}
			p := &cage.Port
			if lanesDiffer(p, &lp) {
				p.LanesDiverge = true
			}
			p.Up = p.Up || lp.Up
			p.Enabled = p.Enabled || lp.Enabled
			p.Present = p.Present || lp.Present
			if p.SpeedMbps == 0 {
				p.SpeedMbps = lp.SpeedMbps
			}
			p.FullDuplex = p.FullDuplex || lp.FullDuplex
			p.Counters.Add(lp.Counters)
			p.Health.LinkChanges += lp.Health.LinkChanges
			p.Health.FECCorrected += lp.Health.FECCorrected
			p.Health.FECUncorrected += lp.Health.FECUncorrected
			p.Interfaces = append(p.Interfaces, name)
			if p.Description == "" {
				p.Description = lp.Description
			}
			if p.Neighbor == nil {
				p.Neighbor = lp.Neighbor
			}
		}
		p := &cage.Port
		p.Lanes = len(names)
		firstCfg := cfg[names[0]]
		caps = mergeSpeeds(caps, platCaps[idx])
		if len(caps) == 0 {
			// No capability data (the VS): the SKU's configured speed only.
			if s := atoi(firstCfg["speed"]); s > 0 {
				caps = []int{s}
			}
		}
		p.SpeedCaps = caps
		p.Media = mediaOf(cageLanes, atoi(firstCfg["speed"]), laneCount(firstCfg["lanes"]), caps)
		// The optic sits in the cage; SONiC keys it by the first interface.
		if x := xcvr[names[0]]; len(x) > 0 {
			p.Present = true
			p.Optic = opticOf(x, dom[names[0]])
		}
		out = append(out, cage)
	}

	// Mirror sessions: the destination port records its (first) source.
	idxOf := map[string]int{}
	for _, c := range out {
		for _, n := range c.Interfaces {
			idxOf[n] = c.Index
		}
	}
	for _, f := range db["cfg_mirror"] {
		dst, ok := idxOf[f["dst_port"]]
		if !ok {
			continue
		}
		src, _, _ := strings.Cut(f["src_port"], ",")
		if s, ok := idxOf[strings.TrimSpace(src)]; ok {
			for i := range out {
				if out[i].Index == dst {
					out[i].MirrorFrom = s
				}
			}
		}
	}
	return out
}

// lanePort maps one SONiC interface.
func lanePort(idx int, name string, cfg, appl, state, ctr map[string]string, vl []membership, lag string, stp map[string]string) devicemodel.Port {
	p := devicemodel.Port{
		Index: idx, IfName: name, Interfaces: []string{name}, Lanes: 1,
		Description: cfg["description"],
		Enabled:     cfg["admin_status"] == "up",
		Up:          appl["oper_status"] == "up",
		AutoNeg:     isTrue(cfg["autoneg"]),
		FECConfig:   fecOf(cfg["fec"]),
		FEC:         fecOf(state["fec"]),
	}
	// The description is CONFIG_DB's alone: `hdel … description` leaves
	// APPL_DB's copy behind (seen on the VS), so APPL_DB is not read for it.
	if !p.AutoNeg {
		p.AdminSpeed = atoi(cfg["speed"])
	}
	for _, m := range []string{cfg["mtu"], appl["mtu"], state["mtu"]} {
		if n := atoi(m); n > 0 {
			p.MTU = n
			break
		}
	}
	if p.Up {
		// APPL_DB speed is in Mb/s ("100000" is 100G). STATE_DB's speed is
		// not used: the VS writes 4294967295 there.
		p.SpeedMbps = atoi(appl["speed"])
		if p.SpeedMbps == 0 {
			p.SpeedMbps = atoi(cfg["speed"])
		}
		p.FullDuplex = true
		p.Present = true
		p.STPState, p.STPRole = "forwarding", "designated"
	} else {
		p.STPState, p.STPRole = "disabled", "disabled"
	}
	p.Health.LinkChanges = atou(appl["flap_count"])
	if len(ctr) > 0 {
		c := func(k string) uint64 { return atou(ctr["SAI_PORT_STAT_"+k]) }
		p.Counters = devicemodel.Counters{
			RxBytes: c("IF_IN_OCTETS"), TxBytes: c("IF_OUT_OCTETS"),
			RxPackets: c("IF_IN_UCAST_PKTS") + c("IF_IN_NON_UCAST_PKTS"),
			TxPackets: c("IF_OUT_UCAST_PKTS") + c("IF_OUT_NON_UCAST_PKTS"),
			RxErrors:  c("IF_IN_ERRORS"), TxErrors: c("IF_OUT_ERRORS"),
			RxDropped: c("IF_IN_DISCARDS"), TxDropped: c("IF_OUT_DISCARDS"),
			RxMulticast: c("IF_IN_MULTICAST_PKTS"), TxMulticast: c("IF_OUT_MULTICAST_PKTS"),
			RxBroadcast: c("IF_IN_BROADCAST_PKTS"), TxBroadcast: c("IF_OUT_BROADCAST_PKTS"),
		}
		// FEC codeword counters exist only where the ASIC counts them
		// (PORT_COUNTER_CAPABILITIES; empty on the VS).
		cor, okC := ctr["SAI_PORT_STAT_IF_IN_FEC_CORRECTABLE_FRAMES"]
		unc, okU := ctr["SAI_PORT_STAT_IF_IN_FEC_NOT_CORRECTABLE_FRAMES"]
		if okC || okU {
			p.Health.HasFECCounters = true
			p.Health.FECCorrected, p.Health.FECUncorrected = atou(cor), atou(unc)
		}
	}
	p.VLAN = vlanOf(vl)
	if lag != "" {
		p.LAG, p.LAGID = lag, lagID(lag)
	}
	if stp != nil {
		p.BPDUGuard = isTrue(stp["bpdu_guard"])
		// "STP disabled on the port" in UniFi terms is portfast on and STP
		// off for the interface (§6.2).
		p.STPEdge = isTrue(stp["portfast"]) && stp["enabled"] == "false"
		p.STPPathCost = atoi(stp["path_cost"])
	}
	return p
}

// vlanOf: the untagged membership is the native VLAN, tagged ones are
// allowed; SONiC has no "all VLANs" trunk.
func vlanOf(vl []membership) devicemodel.PortVLAN {
	if len(vl) == 0 {
		return devicemodel.PortVLAN{}
	}
	v := devicemodel.PortVLAN{Mode: "access"}
	for _, m := range vl {
		if m.tagged {
			v.Mode = "trunk"
			v.Allowed = append(v.Allowed, m.vid)
		} else {
			v.NativeVLAN = m.vid
		}
	}
	if v.Mode == "trunk" && v.NativeVLAN > 0 {
		v.Allowed = append(v.Allowed, v.NativeVLAN)
	}
	sort.Ints(v.Allowed)
	return v
}

// lanesDiffer: a broken-out cage whose child ports carry different config.
func lanesDiffer(a, b *devicemodel.Port) bool {
	if a.AdminSpeed != b.AdminSpeed || a.FECConfig != b.FECConfig || a.MTU != b.MTU || a.Enabled != b.Enabled || a.AutoNeg != b.AutoNeg {
		return true
	}
	if a.VLAN.Mode != b.VLAN.Mode || a.VLAN.NativeVLAN != b.VLAN.NativeVLAN || len(a.VLAN.Allowed) != len(b.VLAN.Allowed) {
		return true
	}
	for i := range a.VLAN.Allowed {
		if a.VLAN.Allowed[i] != b.VLAN.Allowed[i] {
			return true
		}
	}
	return false
}

func mergeSpeeds(a, b []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, s := range append(append([]int(nil), a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Ints(out)
	return out
}

// mediaOf decides the cage type from its serdes lanes and lane rate: four
// lanes at 25G+ is QSFP28, four at 10G QSFP+; a single-lane port is an SFP
// of its rate.
func mediaOf(cageLanes, speed, ifLanes int, caps []int) devicemodel.Media {
	for _, s := range caps {
		if s >= 100000 {
			return devicemodel.MediaQSFP28
		}
	}
	if ifLanes <= 0 {
		ifLanes = 1
	}
	rate := speed / ifLanes
	switch {
	case cageLanes >= 4 && rate >= 25000:
		return devicemodel.MediaQSFP28
	case cageLanes >= 4:
		return devicemodel.MediaQSFPPlus
	case rate >= 25000:
		return devicemodel.MediaSFP28
	case rate >= 10000:
		return devicemodel.MediaSFPPlus
	case rate > 0:
		return devicemodel.MediaSFP
	}
	return devicemodel.MediaQSFP28
}

// neighborOf maps an APPL_DB LLDP_ENTRY_TABLE entry.
func neighborOf(f map[string]string) *devicemodel.Neighbor {
	n := &devicemodel.Neighbor{
		SystemName:      f["lldp_rem_sys_name"],
		SystemDesc:      f["lldp_rem_sys_desc"],
		ChassisID:       f["lldp_rem_chassis_id"],
		PortID:          f["lldp_rem_port_id"],
		PortDescription: f["lldp_rem_port_desc"],
	}
	if _, err := net.ParseMAC(n.ChassisID); err == nil {
		n.ChassisID = strings.ToLower(n.ChassisID)
	}
	// The management address is a comma-separated list (IPv4 and IPv6);
	// the first IPv4 wins, else the first entry.
	for i, a := range strings.Split(f["lldp_rem_man_addr"], ",") {
		a = strings.TrimSpace(a)
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			n.ManagementIP = a
			break
		}
		if i == 0 {
			n.ManagementIP = a
		}
	}
	n.IsBridge, n.IsRouter = lldpCaps(f["lldp_rem_sys_cap_enabled"])
	return n
}

// opticOf maps STATE_DB TRANSCEIVER_INFO / TRANSCEIVER_DOM_SENSOR (hardware
// only; nothing on the VS).
func opticOf(x, d map[string]string) *devicemodel.Optic {
	o := &devicemodel.Optic{Vendor: strings.TrimSpace(x["manufacturer"]), Part: strings.TrimSpace(x["model"]), Serial: strings.TrimSpace(x["serial"])}
	for _, k := range []string{"specification_compliance", "type"} {
		if known(x[k]) {
			o.MediaType = strings.TrimSpace(x[k])
			break
		}
	}
	if t, ok := atof(d["temperature"]); ok {
		o.TempC, o.HasDOM = t, true
		o.VoltageV, _ = atof(d["voltage"])
		o.TxBiasMA, _ = atof(d["tx1bias"])
		o.TxPowerDBm, _ = atof(d["tx1power"])
		o.RxPowerDBm, _ = atof(d["rx1power"])
	}
	return o
}

// buildMACTable reads STATE_DB FDB_TABLE: key FDB_TABLE|Vlan<id>:<mac>,
// value {port, type}. No timestamps. An address learned in Vlan<native> is
// on UniFi's VLAN 1, like the ports' untagged membership.
func buildMACTable(fdb table, byIfname map[string]int, native int) []devicemodel.MACEntry {
	var out []devicemodel.MACEntry
	for k, f := range fdb {
		_, rest, ok := strings.Cut(k, "|")
		if !ok {
			continue
		}
		vlan, mac, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		hw, err := net.ParseMAC(mac)
		if err != nil {
			continue
		}
		vid := atoi(strings.TrimPrefix(vlan, "Vlan"))
		if vid == native {
			vid = 1
		}
		out = append(out, devicemodel.MACEntry{MAC: hw.String(), VLAN: vid, PortIndex: byIfname[f["port"]]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MAC != out[j].MAC {
			return out[i].MAC < out[j].MAC
		}
		return out[i].VLAN < out[j].VLAN
	})
	return out
}

// vlanIDs lists the switch's VLANs, with 1 in place of Vlan<native> so the
// controller never sees the substitute id.
func vlanIDs(t table, native int) []int {
	var out []int
	for k, f := range t {
		id := atoi(f["vlanid"])
		if id == 0 {
			_, name, _ := strings.Cut(k, "|")
			id = atoi(strings.TrimPrefix(name, "Vlan"))
		}
		if id == native {
			id = 1
		}
		if id > 0 {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

// keyNames lists a table's entry names after the prefix ("NTP_SERVER|x" -> x).
func keyNames(t table, prefix string) []string {
	var out []string
	for k := range t {
		if n, ok := strings.CutPrefix(k, prefix); ok && n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// buildSystem maps identity, health and reachability. dial is the address
// (or the addresses a name resolved to) the bridge reaches the switch on.
func buildSystem(db map[string]table, sec map[string]string, addrs []ipAddr, routes []ipRoute, neigh []ipNeigh, dial []string) devicemodel.System {
	meta := db["metadata"].first()
	sys := devicemodel.System{
		Vendor:   vendorOf(meta["hwsku"], meta["platform"]),
		Model:    meta["hwsku"],
		Hostname: meta["hostname"],
		Timezone: meta["timezone"],
		Version:  yamlValue(sec["version"], "build_version"),
	}
	if sys.Hostname == "" {
		sys.Hostname = strings.TrimSpace(sec["hostname"])
	}
	if hw, err := net.ParseMAC(meta["mac"]); err == nil {
		sys.MAC = hw.String()
	}
	if s := db["state_metadata"].first()["chassis_serial_number"]; known(s) {
		sys.Serial = strings.TrimSpace(s)
	}
	if f := strings.Fields(sec["uptime"]); len(f) > 0 {
		if secs, err := strconv.ParseFloat(f[0], 64); err == nil {
			sys.Uptime = secondsDuration(secs)
		}
	}
	mem := meminfo(sec["meminfo"])
	sys.MemTotalKB = mem["MemTotal"]
	if mem["MemAvailable"] <= sys.MemTotalKB {
		sys.MemUsedKB = sys.MemTotalKB - mem["MemAvailable"]
	}
	sys.MemBufferKB = mem["Buffers"] + mem["Cached"]
	sys.LoadAvg = parseLoadAvg(sec["loadavg"])

	// Addresses: every configured IPv4 on the in-band and management
	// interfaces, a down one included; the dialled address first, then
	// in-band (Vlan*, Loopback*), then the management port.
	_, hasMgmtIntf := firstWithPrefix(db["cfg_mgmt"], "MGMT_INTERFACE|")
	isDialled := map[string]bool{}
	for _, d := range dial {
		isDialled[d] = true
	}
	rank := func(n string) int {
		switch {
		case strings.HasPrefix(n, "Vlan"):
			return 1
		case strings.HasPrefix(n, "Loopback"):
			return 2
		case n == "eth0":
			return 3
		}
		return -1
	}
	for _, a := range addrs {
		if rank(a.Ifname) < 0 {
			continue
		}
		var v4 string
		for _, ai := range a.AddrInfo {
			if ai.Family != "inet" || ai.Scope != "global" {
				continue
			}
			sys.Addresses = append(sys.Addresses, devicemodel.IfAddress{Iface: a.Ifname, IP: ai.Local, PrefixLen: ai.PrefixLen})
			if v4 == "" {
				v4 = ai.Local
			}
		}
		// DHCP describes the address the bridge dials: a lease, or eth0
		// left on its default (DHCP) with no MGMT_INTERFACE configured. A
		// stale lease on a down eth0 says nothing about the dialled address.
		for _, ai := range a.AddrInfo {
			if isDialled[ai.Local] && (ai.Dynamic || (a.Ifname == "eth0" && !hasMgmtIntf)) {
				sys.DHCP = true
			}
		}
		if a.Ifname == "eth0" {
			if hw, err := net.ParseMAC(a.Address); err == nil {
				sys.MgmtMAC = hw.String()
			}
			// Up is carrier, not the admin flag: an unplugged management
			// port is admin UP, NO-CARRIER.
			sys.OOBInterfaces = append(sys.OOBInterfaces, devicemodel.OOBInterface{Name: "eth0", Up: a.carrier(), IP: v4})
		}
	}
	addrRank := func(a devicemodel.IfAddress) int {
		if isDialled[a.IP] {
			return 0
		}
		return rank(a.Iface)
	}
	sort.SliceStable(sys.Addresses, func(i, j int) bool { return addrRank(sys.Addresses[i]) < addrRank(sys.Addresses[j]) })

	// The default gateway: the default route with the lowest metric whose
	// interface has carrier (a stale DHCP route stays on a down eth0).
	carrier := map[string]bool{}
	for _, a := range addrs {
		carrier[a.Ifname] = a.carrier()
	}
	gwDev := ""
	best := -1
	for _, r := range routes {
		if r.Dst != "default" || r.Gateway == "" || hasString(r.Flags, "linkdown") {
			continue
		}
		if up, known := carrier[r.Dev]; known && !up {
			continue
		}
		if best < 0 || r.Metric < best {
			best, sys.Gateway, gwDev = r.Metric, r.Gateway, r.Dev
		}
	}
	sys.ARP = map[string]string{}
	for _, n := range neigh {
		ip := net.ParseIP(n.Dst)
		if ip == nil || ip.To4() == nil {
			continue
		}
		hw, err := net.ParseMAC(n.LLAddr)
		if err != nil {
			continue
		}
		if _, seen := sys.ARP[n.Dst]; !seen || n.Dev == gwDev {
			sys.ARP[n.Dst] = hw.String()
		}
		if n.Dst == sys.Gateway && (sys.GatewayMAC == "" || n.Dev == gwDev) {
			sys.GatewayMAC = hw.String()
		}
	}

	// STP: "none" unless STP|GLOBAL is configured.
	sys.STPMode = "none"
	if g, ok := db["cfg_stp"]["STP|GLOBAL"]; ok {
		switch m := strings.ToLower(g["mode"]); m {
		case "mst":
			sys.STPMode = "mstp"
		case "pvst":
			sys.STPMode = "stp" // SONiC's PVST is per-VLAN 802.1D
		case "":
		default:
			sys.STPMode = m
		}
		sys.STPPriority = atoi(g["priority"])
	}
	sys.NTPServers = keyNames(db["cfg_ntp"], "NTP_SERVER|")
	// Syslog hosts as UniFi writes them: host, or host:port off the default.
	for _, h := range keyNames(db["cfg_syslog"], "SYSLOG_SERVER|") {
		if port := db["cfg_syslog"]["SYSLOG_SERVER|"+h]["port"]; port != "" && port != "514" {
			h += ":" + port
		}
		sys.SyslogHosts = append(sys.SyslogHosts, h)
	}
	sys.SNMPCommunities = keyNames(db["cfg_snmp"], "SNMP_COMMUNITY|")

	// Platform (pmon; hardware only -- absent or N/A on the VS).
	fans := db["fan"].byName("|")
	for _, name := range sortedKeys(fans) {
		f := fans[name]
		if !isTrue(f["presence"]) {
			continue
		}
		sys.Fans = append(sys.Fans, devicemodel.Fan{Label: name, SpeedPct: atoi(f["speed"]), OK: isTrue(f["status"])})
	}
	psus := db["psu"].byName("|")
	for _, name := range sortedKeys(psus) {
		f := psus[name]
		p := devicemodel.PSU{Slot: name, Model: f["model"], Present: isTrue(f["presence"]), OK: isTrue(f["status"])}
		if !known(p.Model) {
			p.Model = ""
		}
		p.OutputW, _ = atof(f["power"])
		if t, ok := atof(f["temp"]); ok {
			p.TempsC = []float64{t}
		}
		sys.PSUs = append(sys.PSUs, p)
	}
	temps := db["temp"].byName("|")
	for _, name := range sortedKeys(temps) {
		f := temps[name]
		t, ok := atof(f["temperature"])
		if !ok || t <= 0 {
			continue
		}
		if !sys.HasTemperature || t > sys.TemperatureC {
			sys.TemperatureC, sys.HasTemperature = t, true
		}
		if hi, ok := atof(f["high_threshold"]); ok && hi > 0 && t >= hi {
			sys.Overheating = true
		}
	}
	if f := db["asic_temp"].first(); f != nil {
		if t, ok := atof(f["maximum_temperature"]); ok && t > 0 && (!sys.HasTemperature || t > sys.TemperatureC) {
			sys.TemperatureC, sys.HasTemperature = t, true
		}
	}
	return sys
}

func firstWithPrefix(t table, prefix string) (string, bool) {
	for k := range t {
		if strings.HasPrefix(k, prefix) {
			return k, true
		}
	}
	return "", false
}

func sortedKeys(m map[string]map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
