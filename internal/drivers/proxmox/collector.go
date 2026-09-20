package proxmox

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

//go:embed collect.sh
var collectScript string

// Collector turns one node's bridge into a switchmodel.Snapshot.
type Collector struct {
	r   Runner
	Log *log.Logger

	Bridge      string // vmbr0
	Ports       int    // total ports presented
	UplinkPorts int    // the last UplinkPorts ports: the primary NIC (last), the host (last-1), other NICs

	cycleDelay time.Duration
	// ManageLLDP: keep lldpd on the node announcing this switch's identity
	// (chassis ID = the derived device MAC, port ID = the NIC's port number,
	// on the primary uplink NIC only). Default on; the driver writes
	// /etc/lldpd.d/switch-to-unifi.conf and restarts lldpd when it differs.
	ManageLLDP bool

	mu         sync.Mutex
	ports      *portMap
	node       string // this node's hostname
	last       *switchmodel.Snapshot
	prevCPU    cpuTimes
	nics       map[string]guestNIC // key -> guest NIC, cluster-wide, at the last collect
	keyOf      map[int]string      // port index -> guest key
	knownNames map[int][]string    // labels a slot has carried (for renames)
	bridgeVIDs []int               // bridge-vids from /etc/network/interfaces
	ntpManaged string              // contents of the managed chrony sources file
	snooping   bool                // bridge multicast_snooping
	stpOn      bool
	warned     map[string]bool
}

// NewCollector wraps a runner with the defaults (vmbr0, 54 ports, the top
// 6 for the host and its NICs: the USW Leaf layout).
func NewCollector(r Runner) *Collector {
	return &Collector{r: r, Log: log.Default(), Bridge: "vmbr0", Ports: 54, UplinkPorts: 6, ManageLLDP: true,
		ports: &portMap{Slots: map[int]*slot{}}, knownNames: map[int][]string{}, warned: map[string]bool{}}
}

// Start runs the collector once so a missing tool, a bad key or a bridge
// that does not exist fails here.
func (c *Collector) Start(ctx context.Context) (*switchmodel.Snapshot, error) {
	snap, err := c.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxmox startup: %w", err)
	}
	return snap, nil
}

// Close releases the runner.
func (c *Collector) Close() error { return c.r.Close() }

// Capabilities: the bridge honours port state and VLANs; IGMP snooping is
// bridge-wide. No STP (bridge-stp off), no LAG/mirror/storm/FEC control.
func (c *Collector) Capabilities() switchmodel.Capabilities {
	return switchmodel.Capabilities{IGMPSnooping: true}
}

// Collect runs the script and assembles the snapshot.
func (c *Collector) Collect(ctx context.Context) (*switchmodel.Snapshot, error) {
	out, err := c.r.Run(ctx, "bash -s -- "+shellQuote(c.Bridge), collectScript)
	if err != nil {
		return nil, err
	}
	return c.build(out, time.Now())
}

// build is Collect without the transport (tests feed it fixture output).
func (c *Collector) build(out string, now time.Time) (*switchmodel.Snapshot, error) {
	sec := sections(out)
	if _, ok := sec["end"]; !ok {
		return nil, fmt.Errorf("proxmox: collector script did not run to completion (%d bytes)", len(out))
	}
	for _, need := range []string{"hostname", "links", "brlink", "vlan", "fdb", "bridge", "phys", "qemu"} {
		if _, ok := sec[need]; !ok {
			return nil, fmt.Errorf("proxmox: collector output lacks section %q", need)
		}
	}
	bridge := keyValues(sec["bridge"])
	if bridge["address"] == "" {
		return nil, fmt.Errorf("proxmox: bridge %s not found on the node", c.Bridge)
	}

	var links []ipLink
	var brlinks []ipLink
	var vlans []brVLANs
	var fdb []fdbEntry
	var vml vmList
	var lldp lldpJSON0
	var addrs []ipAddr
	var neigh []ipNeigh
	if err := decodeJSON("links", sec["links"], &links); err != nil {
		return nil, err
	}
	if err := decodeJSON("brlink", sec["brlink"], &brlinks); err != nil {
		return nil, err
	}
	if err := decodeJSON("vlan", sec["vlan"], &vlans); err != nil {
		return nil, err
	}
	if err := decodeJSON("fdb", sec["fdb"], &fdb); err != nil {
		return nil, err
	}
	if err := decodeJSON("vmlist", sec["vmlist"], &vml); err != nil {
		return nil, err
	}
	if err := decodeJSON("addr", sec["addr"], &addrs); err != nil {
		return nil, err
	}
	if err := decodeJSON("neigh", sec["neigh"], &neigh); err != nil {
		return nil, err
	}
	if err := decodeJSON("lldp", sec["lldp"], &lldp); err != nil {
		c.warnOnce("lldp-parse", "lldpcli output not understood: %v", err)
	}
	qemu, _ := parseGuestConfigs(sec["qemu"], "qemu")
	lxc, _ := parseGuestConfigs(sec["lxc"], "lxc")
	carrier := keyValues(sec["carrier"])
	bonds := parseBonding(sec["bonding"])
	hwmon := parseHwmon(sec["hwmon"])
	dmi := keyValues(sec["dmi"])

	linkBy := map[string]ipLink{}
	for _, l := range links {
		linkBy[l.Ifname] = l
	}
	brBy := map[string]ipLink{}
	for _, l := range brlinks {
		brBy[l.Ifname] = l
	}
	vlanBy := map[string]brVLANs{}
	for _, v := range vlans {
		vlanBy[v.Ifname] = v
	}
	fdbBy := map[string][]fdbEntry{}
	for _, e := range fdb {
		fdbBy[e.Ifname] = append(fdbBy[e.Ifname], e)
	}
	neighbors := neighborsByInterface(lldp)
	hostname := strings.TrimSpace(sec["hostname"])

	// --- guest ports ---
	var nics []guestNIC
	for _, n := range append(qemu, lxc...) {
		if n.Bridge == c.Bridge && !n.Template {
			nics = append(nics, n)
		}
	}
	sort.Slice(nics, func(i, j int) bool {
		if nics[i].VMID != nics[j].VMID {
			return nics[i].VMID < nics[j].VMID
		}
		if nics[i].Kind != nics[j].Kind {
			return nics[i].Kind < nics[j].Kind
		}
		return nics[i].Index < nics[j].Index
	})
	perGuest := map[string]int{}
	for _, n := range nics {
		perGuest[fmt.Sprintf("%s/%d", n.Kind, n.VMID)]++
	}
	keys := make([]string, 0, len(nics))
	nicBy := map[string]guestNIC{}
	for _, n := range nics {
		keys = append(keys, n.Key())
		nicBy[n.Key()] = n
	}
	vmSlots := c.Ports - c.UplinkPorts
	c.mu.Lock()
	slotOf, changed := c.ports.assign(keys, vmSlots, now)
	if changed {
		if err := c.ports.save(); err != nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("proxmox: saving port map: %w", err)
		}
	}
	keyOf := map[int]string{}
	for k, idx := range slotOf {
		keyOf[idx] = k
	}
	c.mu.Unlock()
	if len(keys) > vmSlots {
		c.warnOnce("too-many-guests", "%d guest NICs on %s but only %d guest ports (ports=%d, uplink_ports=%d): the newest are not shown", len(keys), c.Bridge, vmSlots, c.Ports, c.UplinkPorts)
	}

	ports := make([]switchmodel.Port, 0, c.Ports)
	var macs []switchmodel.MACEntry
	vlanSet := map[int]bool{1: true}
	for idx := 1; idx <= vmSlots; idx++ {
		key, ok := keyOf[idx]
		if !ok {
			ports = append(ports, emptyPort(idx))
			continue
		}
		n := nicBy[key]
		p := guestPort(idx, n, perGuest[fmt.Sprintf("%s/%d", n.Kind, n.VMID)] > 1)
		if n.Tag > 0 {
			vlanSet[n.Tag] = true
		}
		for _, t := range n.Trunks {
			vlanSet[t] = true
		}
		if n.Node == hostname {
			if l, ok := linkBy[n.HostIface()]; ok {
				p.Interfaces = []string{n.HostIface()}
				p.Up = !n.LinkDown && l.hasFlag("UP")
				p.MTU = l.MTU
				p.Counters = countersOf(l)
				if p.Up {
					p.STPState = "forwarding"
				}
				if st := brBy[n.BridgeMember()].Linkinfo.InfoSlaveData.State; st != "" && p.Up {
					p.STPState = st
				}
				if st := brBy[n.BridgeMember()]; st.Ifname != "" {
					p.STPPathCost = st.Linkinfo.InfoSlaveData.Cost
				}
				p.Health.LinkChanges = carrierChanges(carrier, n.HostIface())
				if v, ok := vlanBy[n.BridgeMember()]; ok {
					p.VLAN = portVLANOf(v)
				}
				for _, e := range fdbBy[n.BridgeMember()] {
					m := macEntry(e, idx, now)
					p.MACs = append(p.MACs, m)
					macs = append(macs, m)
					vlanSet[e.VLAN] = true
				}
			}
		}
		if !p.Up {
			p.SpeedMbps = 0
		}
		c.mu.Lock()
		c.remember(idx, p.Description)
		c.mu.Unlock()
		ports = append(ports, p)
	}

	// --- the host and its NICs: the top slots, filled from the last port down ---
	//
	// Last port = the primary NIC (the uplink); last-1 = the host itself
	// (vmbr0's own address and traffic); further NICs below that. The
	// host is an ordinary client behind its own switch, which needs the
	// switch to identify itself with a *different* MAC (see deviceMAC).
	phys := physicalMembers(sec["phys"], linkBy, bonds, c.Bridge)
	nicSlots := c.UplinkPorts - 1 // one of the top slots is the host port
	if len(phys) > nicSlots {
		c.warnOnce("too-many-nics", "bridge %s has %d uplinks but uplink_ports=%d leaves room for %d: %v not shown", c.Bridge, len(phys), c.UplinkPorts, nicSlots, uplinkNames(phys[nicSlots:]))
		phys = phys[:nicSlots]
	}
	_, ethBodies := subsections(sec["ethtool"])
	_, modBodies := subsections(sec["ethtoolm"])
	uplinkHint := 0
	hostMAC := strings.ToLower(bridge["address"])
	slotFor := func(i int) int { // uplink i -> port index: 0 -> last, 1 -> last-2, 2 -> last-3, ...
		if i == 0 {
			return c.Ports
		}
		return c.Ports - 1 - i
	}
	nicPorts := map[int]switchmodel.Port{}
	for i, u := range phys {
		idx := slotFor(i)
		l := linkBy[u.Active]
		et := parseEthtool(ethBodies[u.Active])
		mod := parseEthtoolModule(modBodies[u.Active])
		p := physicalPort(idx, u, linkBy[u.Member], l, et, mod, brBy, vlanBy)
		p.Health.LinkChanges = carrierChanges(carrier, u.Active)
		if nb, ok := neighbors[u.Active]; ok {
			nbc := nb
			p.Neighbor = &nbc
		}
		var learned []fdbEntry
		if u.First {
			learned = fdbBy[u.Member]
		}
		for _, e := range learned {
			if strings.EqualFold(e.MAC, hostMAC) {
				continue // the host is on its own port
			}
			m := macEntry(e, idx, now)
			p.MACs = append(p.MACs, m)
			macs = append(macs, m)
			vlanSet[e.VLAN] = true
		}
		if uplinkHint == 0 && p.Up {
			uplinkHint = idx
		}
		nicPorts[idx] = p
	}
	if c.ManageLLDP {
		c.ensureLLDP(hostname, sec["lldpdconf"], phys, slotFor, deviceMAC(hostMAC))
	}
	for idx := vmSlots + 1; idx <= c.Ports; idx++ {
		switch {
		case idx == c.Ports-1:
			hp, hm := hostPort(idx, hostname, hostMAC, linkBy[c.Bridge], now)
			ports = append(ports, hp)
			macs = append(macs, hm)
		default:
			if p, ok := nicPorts[idx]; ok {
				ports = append(ports, p)
			} else {
				ports = append(ports, emptyPort(idx))
			}
		}
	}

	// --- system ---
	sys := switchmodel.System{
		Vendor:   "Proxmox",
		Version:  pveVersion(sec["pveversion"]),
		Hostname: hostname,
		MAC:      deviceMAC(hostMAC),
	}
	sys.Model = "VE " + sys.Version
	if pn := dmi["product_name"]; pn != "" {
		sys.Model += " (" + strings.TrimSpace(dmi["sys_vendor"]+" "+pn) + ")"
	}
	if s := dmi["product_serial"]; s != "" && !strings.Contains(strings.ToLower(s), "not specified") && !strings.Contains(strings.ToLower(s), "to be filled") {
		sys.Serial = s
	}
	if f := strings.Fields(sec["uptime"]); len(f) > 0 {
		if secs, err := strconv.ParseFloat(f[0], 64); err == nil {
			sys.Uptime = time.Duration(secs * float64(time.Second))
		}
	}
	cpu := parseCPUTimes(sec["stat"])
	c.mu.Lock()
	if c.prevCPU.total > 0 && cpu.total > c.prevCPU.total {
		dt := float64(cpu.total - c.prevCPU.total)
		di := float64(cpu.idle - c.prevCPU.idle)
		sys.CPUPercent = float64(int((100*(1-di/dt))*10+0.5)) / 10
	}
	c.prevCPU = cpu
	c.mu.Unlock()
	mem := keyValues(strings.ReplaceAll(sec["meminfo"], ":", "="))
	sys.MemTotalKB = kb(mem["MemTotal"])
	sys.MemUsedKB = sys.MemTotalKB - kb(mem["MemAvailable"])
	sys.MemBufferKB = kb(mem["Buffers"]) + kb(mem["Cached"])
	applyHwmon(&sys, hwmon)
	stpOn := bridge["stp_state"] != "" && bridge["stp_state"] != "0"
	sys.STPMode = "disabled"
	if stpOn {
		sys.STPMode = "stp"
	}
	if n, err := strconv.Atoi(bridge["priority"]); err == nil {
		sys.STPPriority = n
	}
	snoop := bridge["multicast_snooping"] == "1"
	sys.IGMPSnooping = map[int]bool{1: snoop}
	// Reachability, for the controller to place the switch in a network:
	// the bridge's own addresses (netmask) and the neighbours it has
	// resolved (the gateway's MAC), plus the load averages for the graphs.
	for _, a := range addrs {
		for _, ai := range a.AddrInfo {
			if ai.Family == "inet" && ai.Scope == "global" {
				sys.Addresses = append(sys.Addresses, switchmodel.IfAddress{Iface: a.Ifname, IP: ai.Local, PrefixLen: ai.PrefixLen})
			}
		}
	}
	sys.ARP = map[string]string{}
	for _, n := range neigh {
		if n.LLAddr != "" && !strings.Contains(n.Dst, ":") {
			sys.ARP[n.Dst] = strings.ToLower(n.LLAddr)
		}
	}
	sys.LoadAvg = parseLoadAvg(sec["loadavg"])
	sys.NTPServers = ntpServers(sec["chrony"])
	if s := strings.TrimSpace(sec["ntpunifi"]); s != "" {
		sys.NTPServers = append(sys.NTPServers, ntpServers(s)...)
	}

	vlanIDs := make([]int, 0, len(vlanSet))
	for v := range vlanSet {
		if v > 0 {
			vlanIDs = append(vlanIDs, v)
		}
	}
	sort.Ints(vlanIDs)
	sort.Slice(macs, func(i, j int) bool { return macs[i].MAC < macs[j].MAC })

	snap := &switchmodel.Snapshot{TakenAt: now, System: sys, Ports: ports, MACTable: macs, VLANs: vlanIDs, UplinkHint: uplinkHint}
	c.mu.Lock()
	c.node = hostname
	c.last = snap
	c.nics = nicBy
	c.keyOf = keyOf
	c.bridgeVIDs = parseVLANRanges(strings.ReplaceAll(strings.TrimSpace(bridge["vids"]), " ", ";"))
	c.ntpManaged = strings.TrimSpace(sec["ntpunifi"])
	c.snooping = snoop
	c.stpOn = stpOn
	c.mu.Unlock()
	return snap, nil
}

func (c *Collector) remember(idx int, label string) {
	if label == "" {
		return
	}
	for _, n := range c.knownNames[idx] {
		if n == label {
			return
		}
	}
	c.knownNames[idx] = append(c.knownNames[idx], label)
}

func (c *Collector) warnOnce(key, format string, args ...any) {
	c.mu.Lock()
	seen := c.warned[key]
	c.warned[key] = true
	c.mu.Unlock()
	if !seen {
		c.Log.Printf("proxmox: "+format, args...)
	}
}

// emptyPort is a slot with nothing assigned: an empty cage.
func emptyPort(idx int) switchmodel.Port {
	return switchmodel.Port{Index: idx, Media: switchmodel.MediaQSFP28, Lanes: 1, Enabled: true, AutoNeg: true,
		VLAN: switchmodel.PortVLAN{Mode: "trunk", NativeVLAN: 1, AllowAll: true}}
}

// guestPort is the static part of a guest NIC's port: what is known from
// the config alone, before the node's live interface (if any) is applied.
// Speed is what a guest can push across the bridge, not the 10 Mb/s the
// kernel prints for a tap: virtio (and vmxnet3) are memory-bound, so they
// are shown as 100G ports on a 100G switch; emulated NICs get their
// nominal speed.
func guestPort(idx int, n guestNIC, multi bool) switchmodel.Port {
	speed := 100000
	switch n.Model {
	case "e1000", "e1000e":
		speed = 1000
	case "rtl8139", "ne2k_pci", "pcnet":
		speed = 100
	}
	ifname := fmt.Sprintf("vm%d-net%d", n.VMID, n.Index)
	if n.Kind == "lxc" {
		ifname = fmt.Sprintf("ct%d-net%d", n.VMID, n.Index)
	}
	p := switchmodel.Port{
		Index: idx, IfName: ifname, Description: guestLabel(n, multi), Name: guestLabel(n, multi),
		Media: switchmodel.MediaQSFP28, Lanes: 1, Present: true, Enabled: !n.LinkDown,
		SpeedMbps: speed, FullDuplex: true, AutoNeg: true, SpeedCaps: []int{speed},
		STPState: "disabled", MTU: n.MTU,
		VLAN: vlanFromConfig(n),
	}
	if p.MTU == 0 {
		p.MTU = 1500
	}
	return p
}

// vlanFromConfig is the 802.1Q state a NIC's tag/trunks options mean on a
// VLAN-aware bridge: no tag and no trunks = every VLAN tagged with 1
// untagged; tag=X alone = access on X; trunks = those VLANs tagged.
func vlanFromConfig(n guestNIC) switchmodel.PortVLAN {
	v := switchmodel.PortVLAN{Mode: "trunk", NativeVLAN: 1}
	if n.Tag > 0 {
		v.NativeVLAN = n.Tag
	}
	switch {
	case n.Tag > 0 && len(n.Trunks) == 0:
		v.Mode = "access"
	case n.Tag == 0 && len(n.Trunks) == 0:
		v.AllowAll = true
	default:
		v.Allowed = uniqueSorted(append(append([]int(nil), n.Trunks...), v.NativeVLAN))
	}
	return v
}

// portVLANOf reads the live `bridge vlan` state of a bridge member.
func portVLANOf(v brVLANs) switchmodel.PortVLAN {
	out := switchmodel.PortVLAN{Mode: "trunk", NativeVLAN: 1}
	var allowed []int
	count := 0
	for _, e := range v.VLANs {
		end := e.VLANEnd
		if end == 0 {
			end = e.VLAN
		}
		native := false
		for _, f := range e.Flags {
			if f == "PVID" {
				native = true
			}
		}
		if native {
			out.NativeVLAN = e.VLAN
		}
		count += end - e.VLAN + 1
		if end-e.VLAN < 64 {
			for x := e.VLAN; x <= end; x++ {
				allowed = append(allowed, x)
			}
		}
	}
	switch {
	case count >= 4000:
		out.AllowAll = true
	case count <= 1:
		out.Mode = "access"
	default:
		out.Allowed = uniqueSorted(allowed)
	}
	return out
}

func countersOf(l ipLink) switchmodel.Counters {
	return switchmodel.Counters{
		RxBytes: l.Stats64.Rx.Bytes, TxBytes: l.Stats64.Tx.Bytes,
		RxPackets: l.Stats64.Rx.Packets, TxPackets: l.Stats64.Tx.Packets,
		RxErrors: l.Stats64.Rx.Errors, TxErrors: l.Stats64.Tx.Errors,
		RxDropped: l.Stats64.Rx.Dropped, TxDropped: l.Stats64.Tx.Dropped,
		RxMulticast: l.Stats64.Rx.Multicast,
	}
}

func carrierChanges(carrier map[string]string, name string) uint64 {
	n, _ := strconv.ParseUint(carrier[name], 10, 64)
	return n
}

func macEntry(e fdbEntry, idx int, now time.Time) switchmodel.MACEntry {
	return switchmodel.MACEntry{MAC: strings.ToLower(e.MAC), VLAN: e.VLAN, PortIndex: idx, LastMove: now.Add(-time.Duration(e.Updated) * time.Second)}
}

// uplink is one physical path out of the bridge, as one switch port:
//
//   - a NIC in the bridge: itself;
//   - an active-backup (or other failover-only) bond: one link, showing
//     the active slave's speed, optic and LLDP neighbour with the bond's
//     counters (UniFi has no notion of an active-standby pair, and a bond
//     is one link to the network; Clint's call, 2026-09-20);
//   - an LACP (802.3ad) or balance-* bond: one port per member, all in
//     the same LAG, the way UniFi shows an aggregate;
//   - a VLAN device on any of those (bond0.10 as the bridge port): the
//     device underneath.
//
// Member is the bridge port (what `bridge vlan`/fdb key on); Active is the
// NIC whose speed, optic and LLDP neighbour the port reports; Ifaces are
// the NICs lldpd names with this port's number.
type uplink struct {
	Name   string   // the port's name
	Member string   // the bridge member this belongs to (bond, NIC or VLAN device)
	Ifaces []string // the NICs behind it
	Active string   // the NIC carrying traffic now
	LAG    string   // aggregate name when the port is one member of a LAG
	First  bool     // first port of its member: carries the member's MAC table
}

func uplinkNames(us []uplink) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.Name)
	}
	return out
}

// physicalMembers lists the bridge's uplink ports in kernel order.
func physicalMembers(physBody string, linkBy map[string]ipLink, bonds []bond, bridge string) []uplink {
	isPhys := map[string]bool{}
	for _, line := range strings.Split(physBody, "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			isPhys[f[0]] = true
		}
	}
	bondBy := map[string]bond{}
	for _, b := range bonds {
		bondBy[b.Name] = b
	}
	var members []ipLink
	for _, l := range linkBy {
		if l.Master == bridge {
			members = append(members, l)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Ifindex < members[j].Ifindex })
	var out []uplink
	for _, m := range members {
		// A VLAN (or macvlan) device rides on its parent: look through it.
		lower := m.Ifname
		for hops := 0; hops < 4; hops++ {
			l, ok := linkBy[lower]
			if !ok || l.Link == "" || (l.Linkinfo.InfoKind != "vlan" && l.Linkinfo.InfoKind != "macvlan") {
				break
			}
			lower = l.Link
		}
		if b, ok := bondBy[lower]; ok {
			var slaves []string
			for _, sl := range b.Slaves {
				slaves = append(slaves, sl.Name)
			}
			if len(slaves) == 0 {
				continue
			}
			if strings.Contains(b.Mode, "802.3ad") || strings.Contains(b.Mode, "balance") || strings.Contains(b.Mode, "broadcast") {
				for i, sl := range slaves {
					out = append(out, uplink{Name: sl, Member: m.Ifname, Ifaces: []string{sl}, Active: sl, LAG: b.Name, First: i == 0})
				}
				continue
			}
			active := b.ActiveSlave
			if active == "" {
				active = slaves[0]
			}
			out = append(out, uplink{Name: m.Ifname, Member: m.Ifname, Ifaces: slaves, Active: active, First: true})
			continue
		}
		if isPhys[lower] {
			out = append(out, uplink{Name: m.Ifname, Member: m.Ifname, Ifaces: []string{lower}, Active: lower, First: true})
		}
	}
	return out
}

// physicalPort renders one uplink: link state and counters from the bridge
// member (the bond, or the NIC), speed/optic/capabilities from the active NIC.
func physicalPort(idx int, u uplink, member, active ipLink, et ethtoolInfo, mod ethtoolModule, brBy map[string]ipLink, vlanBy map[string]brVLANs) switchmodel.Port {
	stats := member
	if u.LAG != "" {
		stats = active // a LAG member reports its own link and counters
	}
	p := switchmodel.Port{
		Index: idx, IfName: u.Name, Interfaces: u.Ifaces, Name: u.Name, Lanes: 1,
		Enabled: stats.hasFlag("UP"), Up: stats.hasFlag("LOWER_UP") && active.hasFlag("LOWER_UP"), MTU: member.MTU,
		Counters: countersOf(stats), AutoNeg: et.Autoneg, SpeedCaps: et.Speeds, FECCapable: et.FEC,
		FullDuplex: strings.EqualFold(et.Duplex, "Full"), SpeedMbps: et.Speed,
		STPState: "disabled",
	}
	p.Media, p.Present = mediaOf(et, mod)
	if mod.Present {
		p.Optic = &switchmodel.Optic{Vendor: mod.Vendor, Part: mod.Part, Serial: mod.Serial, MediaType: mod.Type,
			TempC: mod.TempC, VoltageV: mod.VoltageV, HasDOM: mod.HasDOM}
	}
	if u.LAG != "" {
		p.LAG = u.LAG
		p.LAGID = 1
	}
	if p.Up {
		p.STPState = "forwarding"
		if st := brBy[u.Member].Linkinfo.InfoSlaveData.State; st != "" {
			p.STPState = st
		}
	}
	if b := brBy[u.Member]; b.Ifname != "" {
		p.STPPathCost = b.Linkinfo.InfoSlaveData.Cost
	}
	if v, ok := vlanBy[u.Member]; ok {
		p.VLAN = portVLANOf(v)
	} else {
		p.VLAN = switchmodel.PortVLAN{Mode: "trunk", NativeVLAN: 1, AllowAll: true}
	}
	if !p.Up {
		p.SpeedMbps = 0
	}
	return p
}

// mediaOf classifies a NIC: the transceiver's own identifier when a module
// answers, else the port type and the fastest supported mode.
func mediaOf(et ethtoolInfo, mod ethtoolModule) (switchmodel.Media, bool) {
	top := 0
	if len(et.Speeds) > 0 {
		top = et.Speeds[len(et.Speeds)-1]
	}
	if mod.Present {
		switch {
		case strings.HasPrefix(mod.Identifier, "QSFP28"), strings.HasPrefix(mod.Identifier, "QSFP-DD"):
			return switchmodel.MediaQSFP28, true
		case strings.HasPrefix(mod.Identifier, "QSFP"):
			return switchmodel.MediaQSFPPlus, true
		case top >= 25000:
			return switchmodel.MediaSFP28, true
		case top >= 10000:
			return switchmodel.MediaSFPPlus, true
		default:
			return switchmodel.MediaSFP, true
		}
	}
	if strings.Contains(et.Port, "Twisted Pair") || strings.Contains(et.Port, "TP") {
		switch {
		case top >= 10000:
			return switchmodel.MediaCopper10G, true
		case top >= 2500:
			return switchmodel.MediaCopper2G5, true
		default:
			return switchmodel.MediaCopper1G, true
		}
	}
	switch {
	case top >= 100000:
		return switchmodel.MediaQSFP28, false
	case top >= 40000:
		return switchmodel.MediaQSFPPlus, false
	case top >= 25000:
		return switchmodel.MediaSFP28, false
	case top >= 10000:
		return switchmodel.MediaSFPPlus, false
	case top > 0:
		return switchmodel.MediaSFP, false
	}
	return switchmodel.MediaUnknown, false
}

// neighborsByInterface flattens lldpd's json0 output.
func neighborsByInterface(l lldpJSON0) map[string]switchmodel.Neighbor {
	out := map[string]switchmodel.Neighbor{}
	for _, top := range l.LLDP {
		for _, iface := range top.Interface {
			var n switchmodel.Neighbor
			for _, ch := range iface.Chassis {
				for _, id := range ch.ID {
					n.ChassisID = id.Value
				}
				for _, v := range ch.Name {
					n.SystemName = v.Value
				}
				for _, v := range ch.Descr {
					n.SystemDesc = v.Value
				}
				for _, v := range ch.MgmtIP {
					if n.ManagementIP == "" && !strings.Contains(v.Value, ":") {
						n.ManagementIP = v.Value
					}
				}
				for _, cp := range ch.Capability {
					switch cp.Type {
					case "Bridge":
						n.IsBridge = n.IsBridge || cp.Enabled
					case "Router":
						n.IsRouter = n.IsRouter || cp.Enabled
					}
				}
			}
			for _, port := range iface.Port {
				for _, id := range port.ID {
					n.PortID = id.Value
				}
				for _, v := range port.Descr {
					n.PortDescription = v.Value
				}
			}
			if n.ChassisID != "" {
				out[iface.Name] = n
			}
		}
	}
	return out
}

// applyHwmon fills temperature and fans: the CPU package sensor (coretemp /
// k10temp) is the switch temperature; a chip with fanN_input rows (Dell's
// dell_smm, most board sensors) gives the fans, as percent of fanN_max.
func applyHwmon(sys *switchmodel.System, chips []hwmonChip) {
	for _, ch := range chips {
		switch ch.Name {
		case "coretemp", "k10temp", "zenpower", "cpu_thermal":
			if t, ok := ch.float("temp1_input", 1000); ok {
				sys.TemperatureC, sys.HasTemperature = t, true
				if max, ok := ch.float("temp1_max", 1000); ok && max > 0 && t >= max {
					sys.Overheating = true
				} else if crit, ok := ch.float("temp1_crit", 1000); ok && crit > 0 && t >= crit {
					sys.Overheating = true
				}
			}
		}
	}
	if !sys.HasTemperature {
		for _, ch := range chips {
			if ch.Name == "nvme" {
				continue
			}
			if t, ok := ch.float("temp1_input", 1000); ok {
				sys.TemperatureC, sys.HasTemperature = t, true
				break
			}
		}
	}
	for _, ch := range chips {
		for i := 1; i <= 16; i++ {
			rpm, ok := ch.float(fmt.Sprintf("fan%d_input", i), 1)
			if !ok {
				continue
			}
			f := switchmodel.Fan{Label: fmt.Sprintf("Fan %d", i), OK: rpm > 0}
			if l := ch.Values[fmt.Sprintf("fan%d_label", i)]; l != "" {
				f.Label = l
			}
			if max, ok := ch.float(fmt.Sprintf("fan%d_max", i), 1); ok && max > 0 {
				f.SpeedPct = int(rpm/max*100 + 0.5)
			} else if rpm > 0 {
				f.SpeedPct = int(rpm / 60) // no maximum known: a rough percent
			}
			if f.SpeedPct > 100 {
				f.SpeedPct = 100
			}
			sys.Fans = append(sys.Fans, f)
		}
	}
}

func pveVersion(s string) string {
	// "pve-manager/9.1.6/71482d1833ded40a (running kernel: ...)"
	f := strings.Split(strings.TrimSpace(s), "/")
	if len(f) >= 2 {
		return f[1]
	}
	return strings.TrimSpace(s)
}

func kb(s string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(s), " kB"), 10, 64)
	return n
}

func ntpServers(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && (f[0] == "server" || f[0] == "pool") {
			out = append(out, f[1])
		}
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// deviceMAC is the identity the switch presents to the controller: the
// bridge's MAC with the locally-administered bit flipped (98:f2:.. -> 9a:f2:..).
// A MAC is either a device or a client to the controller, never both, so
// using the host's own MAC would make the host itself vanish from the
// client list and the topology; with a derived identity the host is an
// ordinary client behind its own switch (port Ports-1), keeping its name,
// IP and DNS record. lldpd on the node must advertise the same value
// (docs/proxmox.md §4).
func deviceMAC(hostMAC string) string {
	hw, err := net.ParseMAC(hostMAC)
	if err != nil || len(hw) != 6 {
		return hostMAC
	}
	hw[0] ^= 0x02
	return hw.String()
}

// hostPort is the port the node itself sits behind: vmbr0's own interface
// (the host's traffic through the bridge) with the host's MAC learned on it.
func hostPort(idx int, hostname, hostMAC string, br ipLink, now time.Time) (switchmodel.Port, switchmodel.MACEntry) {
	p := switchmodel.Port{
		Index: idx, IfName: "host", Description: hostname, Name: hostname,
		Media: switchmodel.MediaQSFP28, Lanes: 1, Present: true, Enabled: true,
		Up: br.hasFlag("UP"), SpeedMbps: 100000, FullDuplex: true, AutoNeg: true, SpeedCaps: []int{100000},
		MTU: br.MTU, Counters: countersOf(br), STPState: "forwarding",
		VLAN: switchmodel.PortVLAN{Mode: "access", NativeVLAN: 1},
	}
	if p.MTU == 0 {
		p.MTU = 1500
	}
	// The bridge interface's RX is what the host received; from the switch's
	// point of view the host port received what the host sent.
	p.Counters.RxBytes, p.Counters.TxBytes = p.Counters.TxBytes, p.Counters.RxBytes
	p.Counters.RxPackets, p.Counters.TxPackets = p.Counters.TxPackets, p.Counters.RxPackets
	p.Counters.RxErrors, p.Counters.TxErrors = p.Counters.TxErrors, p.Counters.RxErrors
	p.Counters.RxDropped, p.Counters.TxDropped = p.Counters.TxDropped, p.Counters.RxDropped
	if !p.Up {
		p.SpeedMbps = 0
	}
	m := switchmodel.MACEntry{MAC: hostMAC, VLAN: 1, PortIndex: idx, LastMove: now}
	p.MACs = []switchmodel.MACEntry{m}
	return p, m
}

// lldpdConfig is the lldpd configuration this switch needs on its node:
// announce only on each uplink's active NIC (with both slaves of an
// active-backup bond announcing, the controller drew the nodes under the
// backup link's switch), with the switch's device MAC as chassis ID and
// the uplink's port number as port ID on every NIC behind it, so a
// failover keeps the same port.
func lldpdConfig(phys []uplink, slotFor func(int) int, devMAC string) (config string, announce []string) {
	if len(phys) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("# managed by switch-to-unifi: this node's bridge as a UniFi switch\n")
	for _, u := range phys {
		announce = append(announce, u.Active)
	}
	fmt.Fprintf(&b, "configure system interface pattern %s\n", strings.Join(announce, ","))
	fmt.Fprintf(&b, "configure system chassisid %s\n", devMAC)
	b.WriteString("configure lldp portidsubtype ifname\n")
	for i, u := range phys {
		for _, n := range u.Ifaces {
			fmt.Fprintf(&b, "configure ports %s lldp portidsubtype local \"Port %d\"\n", n, slotFor(i))
			fmt.Fprintf(&b, "configure ports %s lldp portdescription \"%s\"\n", n, u.Name)
		}
	}
	// LACP members each announce their own port; an active-backup bond
	// announces on its active slave only (see above).
	return b.String(), announce
}

// ensureLLDP writes the lldpd config and restarts lldpd when the node's
// differs; a node without lldpd gets one warning naming the package.
func (c *Collector) ensureLLDP(node, section string, phys []uplink, slotFor func(int) int, devMAC string) {
	present, current, _ := strings.Cut(section, "\n")
	if strings.TrimSpace(present) != "present" {
		c.warnOnce("lldpd-missing", "lldpd is not installed on %s: the controller cannot place this switch in the topology without it (apt-get install lldpd; the bridge configures it)", node)
		return
	}
	want, announce := lldpdConfig(phys, slotFor, devMAC)
	if want == "" || strings.TrimSpace(current) == strings.TrimSpace(want) {
		return
	}
	c.Log.Printf("proxmox %s: configuring lldpd (chassis %s, announcing on %s as Port %d)", node, devMAC, strings.Join(announce, ","), slotFor(0))
	cmd := "install -m 644 /dev/stdin /etc/lldpd.d/switch-to-unifi.conf && systemctl restart lldpd"
	if _, err := c.r.Run(context.Background(), cmd, want); err != nil {
		c.warnOnce("lldpd-write", "configuring lldpd failed: %v", err)
	}
}
