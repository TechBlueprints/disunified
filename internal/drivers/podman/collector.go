// Package podman presents a Podman host to the controller as a switch, the
// way the proxmox driver presents a node: the host is the switch (its
// uplink NIC's MAC, address and hostname), every container network
// endpoint is a port named after the container, and the NICs are the top
// ports with the default route's NIC as the uplink. Read over one SSH exec
// of collect.sh per poll (root on the host, key auth). Read-only.
//
// Two kinds of endpoint, told apart by the network's driver:
//
//   - macvlan/ipvlan ("real IP"): the container sits on the LAN with its own
//     MAC, so the port carries that MAC and the controller shows the
//     container as the client behind it.
//   - bridge (Podman's default, NAT behind the host): the container is
//     unreachable from the LAN under its own MAC, so the port is shown up and
//     counted but carries no client -- reporting the endpoint's MAC would
//     make the controller invent a wired client the LAN never sees.
//
// Slots (port indices) are assigned once per endpoint and kept in a JSON
// file on the host (the podman analogue of the proxmox driver's guest
// tags: the host, not the bridge, holds the mapping), so a container keeps
// its port across restarts and bridge redeploys. A removed container frees
// its slot; a stopped one keeps it and shows link down.
package podman

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

//go:embed collect.sh
var collectScript string

const (
	topSlot = 54 // the USW Leaf's 54 ports; NICs count down from here, every slot below them is open to containers

	// DefaultSlotsFile is where the host keeps endpoint -> port assignments.
	DefaultSlotsFile = "/var/lib/disunified/podman-slots.json"
)

// Collector reads a Podman host and builds the neutral snapshot.
type Collector struct {
	r sshrun.Runner
	// Uplink is the NIC presented as the uplink; "" = the default route's.
	Uplink string
	// SlotsFile is the host-side file of endpoint -> port assignments.
	SlotsFile string
	Log       *log.Logger

	mu         sync.Mutex
	host       string
	prevCPU    cpuTimes
	slots      map[string]int // endpoint key -> port index, as on the host
	slotsKnown bool
	last       *devicemodel.Snapshot
	nm         nmState // NetworkManager's view of the uplink, from the last poll
	uplinkNIC  string
	warned     map[string]bool
	knownNames map[int][]string
}

// NewCollector wraps a runner.
func NewCollector(r sshrun.Runner) *Collector {
	return &Collector{r: r, SlotsFile: DefaultSlotsFile, Log: log.Default(), warned: map[string]bool{}, knownNames: map[int][]string{}}
}

// Start runs the collector once and checks it saw a host with Podman.
func (c *Collector) Start(ctx context.Context) (*devicemodel.Snapshot, error) {
	snap, err := c.Collect(ctx)
	if err != nil {
		return nil, err
	}
	if snap.System.Version == "" {
		return nil, fmt.Errorf("podman: %s answered but reported no podman version; is podman installed and is this root?", c.host)
	}
	c.Log.Printf("podman: %s, %s, podman %s, uplink %s at port %d, %d container endpoints", snap.System.Hostname, snap.System.Model, snap.System.Version, c.uplinkName(snap), snap.UplinkHint, len(snap.Ports)-c.nicCount(snap)-c.freeCount(snap))
	return snap, nil
}

func (c *Collector) Close() error { return c.r.Close() }

// Capabilities: none. The UI offers port state and names only, and the
// driver honours neither yet (read-only).
func (c *Collector) Capabilities() devicemodel.Capabilities { return devicemodel.Capabilities{} }

// Collect runs collect.sh and builds the snapshot.
func (c *Collector) Collect(ctx context.Context) (*devicemodel.Snapshot, error) {
	out, err := c.r.Run(ctx, "bash -s -- "+sshrun.ShellQuote(c.Uplink)+" "+sshrun.ShellQuote(c.SlotsFile), collectScript)
	if err != nil {
		return nil, fmt.Errorf("podman: collect: %w", err)
	}
	snap, err := c.build(ctx, out, time.Now())
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.last = snap
	c.mu.Unlock()
	return snap, nil
}

// endpointKey is the stable identity of a port: the container's name and
// the network, which survive a container being recreated (its id does not).
func endpointKey(containerName, network string) string { return containerName + "/" + network }

type endpointInfo struct {
	key       string
	container container
	network   network
	ep        endpoint // from inspect; zero when the container is not running
	link      ipLink   // the interface inside the container's netns; zero when not running
	multi     bool     // the container has more than one endpoint
	hasLink   bool
	running   bool
	inspected bool
}

func (c *Collector) build(ctx context.Context, out string, now time.Time) (*devicemodel.Snapshot, error) {
	sec := sections(out)
	if _, ok := sec["end"]; !ok {
		return nil, fmt.Errorf("podman: collector script did not run to completion (%d bytes)", len(out))
	}
	for _, need := range []string{"hostname", "links", "addr", "route", "phys", "networks", "ps"} {
		if _, ok := sec[need]; !ok {
			return nil, fmt.Errorf("podman: collector output lacks section %q", need)
		}
	}
	var links []ipLink
	var addrs []ipAddr
	var routes []ipRoute
	var neigh []ipNeigh
	var fdb []fdbEntry
	var networks []network
	var containers []container
	var inspects []inspected
	for _, d := range []struct {
		name string
		v    any
	}{{"links", &links}, {"addr", &addrs}, {"route", &routes}, {"neigh", &neigh}, {"fdb", &fdb}, {"networks", &networks}, {"ps", &containers}, {"inspect", &inspects}} {
		if err := decodeJSON(d.name, sec[d.name], d.v); err != nil {
			return nil, err
		}
	}
	hostname := strings.TrimSpace(sec["hostname"])
	c.mu.Lock()
	c.host = hostname
	c.mu.Unlock()
	linkBy := map[string]ipLink{}
	for _, l := range links {
		linkBy[l.Ifname] = l
	}
	netBy := map[string]network{}
	for _, n := range networks {
		netBy[n.Name] = n
	}
	inspectBy := map[string]inspected{}
	for _, i := range inspects {
		inspectBy[i.ID] = i
		inspectBy[i.Name] = i
	}
	netnsLinks := map[string][]ipLink{} // container id (short or long) -> its interfaces
	if names, bodies := subsections(sec["netns"]); len(names) > 0 {
		for _, id := range names {
			var ls []ipLink
			if err := decodeJSON("netns "+id, bodies[id], &ls); err != nil {
				c.warnOnce("netns-"+id, "%v", err)
				continue
			}
			netnsLinks[id] = ls
		}
	}
	carrier := keyValues(sec["carrier"])
	dmi := keyValues(sec["dmi"])
	osr := keyValues(sec["osrelease"])

	// --- the uplink and the other NICs ---
	uplinkName := strings.TrimSpace(sec["uplink"])
	if c.Uplink != "" {
		uplinkName = c.Uplink
	}
	if uplinkName == "" && len(routes) > 0 {
		uplinkName = routes[0].Dev
	}
	var nics []string
	for _, line := range strings.Split(strings.TrimSpace(sec["phys"]), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			nics = append(nics, f[0])
		}
	}
	sort.Strings(nics)
	if uplinkName != "" {
		// The uplink first, at the top slot.
		rest := nics[:0]
		for _, n := range nics {
			if n != uplinkName {
				rest = append(rest, n)
			}
		}
		nics = append([]string{uplinkName}, rest...)
	}
	_, ethtoolBodies := subsections(sec["ethtool"])
	physBy := map[string]map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(sec["phys"]), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		kv := map[string]string{}
		for _, x := range f[1:] {
			if k, v, ok := strings.Cut(x, "="); ok {
				kv[k] = v
			}
		}
		physBy[f[0]] = kv
	}

	var ports []devicemodel.Port
	uplinkHint := 0
	if len(nics) >= topSlot {
		return nil, fmt.Errorf("podman: %d NICs, more than the %d slots", len(nics), topSlot)
	}
	guestSlots := topSlot - len(nics) // the slots below the NICs
	for i, n := range nics {
		idx := topSlot - i
		p := nicPort(idx, n, linkBy[n], parseEthtool(ethtoolBodies[n]), physBy[n], carrier)
		if n == uplinkName {
			uplinkHint = idx
		}
		ports = append(ports, p)
	}

	// --- container endpoints ---
	var eps []endpointInfo
	sort.Slice(containers, func(i, j int) bool {
		if containers[i].Created != containers[j].Created {
			return containers[i].Created < containers[j].Created
		}
		return containers[i].Name() < containers[j].Name()
	})
	for _, ct := range containers {
		nets := append([]string(nil), ct.Networks...)
		ins, hasIns := inspectBy[ct.ID]
		if hasIns {
			for n := range ins.Networks {
				found := false
				for _, x := range nets {
					if x == n {
						found = true
					}
				}
				if !found {
					nets = append(nets, n)
				}
			}
		}
		sort.Strings(nets)
		for _, n := range nets {
			e := endpointInfo{key: endpointKey(ct.Name(), n), container: ct, network: netBy[n], multi: len(nets) > 1, running: ct.State == "running", inspected: hasIns}
			if hasIns {
				e.ep = ins.Networks[n]
				for _, l := range netnsLinks[ct.ID[:12]] {
					if strings.EqualFold(l.Address, e.ep.MAC) {
						e.link, e.hasLink = l, true
					}
				}
				if !e.hasLink {
					for _, l := range netnsLinks[ct.ID] {
						if strings.EqualFold(l.Address, e.ep.MAC) {
							e.link, e.hasLink = l, true
						}
					}
				}
			}
			eps = append(eps, e)
		}
	}
	slots, changed := c.assignSlots(sec["slots"], eps, guestSlots)
	if changed {
		if err := c.writeSlots(ctx, slots); err != nil {
			c.warnOnce("slots-write", "slot file %s not written: %v (assignments held in memory)", c.SlotsFile, err)
		}
	}
	var macs []devicemodel.MACEntry
	used := map[int]bool{}
	for _, e := range eps {
		idx, ok := slots[e.key]
		if !ok {
			c.warnOnce("slot-"+e.key, "no free port for container endpoint %s; not presented", e.key)
			continue
		}
		used[idx] = true
		p := endpointPort(idx, e)
		if isLANNetwork(e.network) && e.ep.MAC != "" && e.running {
			m := devicemodel.MACEntry{MAC: strings.ToLower(e.ep.MAC), VLAN: 1, PortIndex: idx, LastMove: now}
			p.MACs = append(p.MACs, m)
			macs = append(macs, m)
		}
		c.remember(idx, p.Name)
		ports = append(ports, p)
	}
	for idx := 1; idx <= guestSlots; idx++ {
		if !used[idx] {
			ports = append(ports, emptyPort(idx))
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Index < ports[j].Index })

	// --- the host itself ---
	up := linkBy[uplinkName]
	sys := devicemodel.System{
		Vendor: "Podman", Model: strings.Trim(osr["PRETTY_NAME"], `"`), Version: strings.TrimSpace(sec["podman"]),
		Hostname: hostname, MAC: strings.ToLower(up.Address), STPMode: "disabled",
	}
	if sys.Model == "" {
		sys.Model = "Linux host"
	}
	for _, k := range []string{"product_serial", "product_uuid"} {
		if s := dmi[k]; s != "" && !strings.Contains(strings.ToLower(s), "not specified") && !strings.Contains(strings.ToLower(s), "to be filled") {
			sys.Serial = s
			break
		}
	}
	if sys.Serial == "" {
		sys.Serial = strings.TrimSpace(sec["machineid"])
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
	sys.LoadAvg = parseLoadAvg(sec["loadavg"])
	// Reachability: the uplink NIC's own addresses (the podman bridges'
	// gateway addresses are internal and would mislead placement), whether
	// the address is a lease, the default gateway and what it resolves to.
	for _, a := range addrs {
		if a.Ifname != uplinkName {
			continue
		}
		for _, ai := range a.AddrInfo {
			if ai.Family == "inet" && ai.Scope == "global" {
				sys.Addresses = append(sys.Addresses, devicemodel.IfAddress{Iface: a.Ifname, IP: ai.Local, PrefixLen: ai.PrefixLen})
				if ai.Dynamic {
					sys.DHCP = true
				}
			}
		}
	}
	for _, r := range routes {
		if r.Dst == "default" && r.Gateway != "" {
			sys.Gateway = r.Gateway
			break
		}
	}
	sys.ARP = map[string]string{}
	for _, n := range neigh {
		if n.LLAddr != "" && !strings.Contains(n.Dst, ":") && (uplinkName == "" || n.Dev == uplinkName) {
			sys.ARP[n.Dst] = strings.ToLower(n.LLAddr)
			if n.Dst == sys.Gateway {
				sys.GatewayMAC = strings.ToLower(n.LLAddr)
			}
		}
	}
	sort.Slice(macs, func(i, j int) bool { return macs[i].MAC < macs[j].MAC })
	c.mu.Lock()
	c.nm = parseNM(sec["nmconn"], sec["nmipv4"])
	c.uplinkNIC = uplinkName
	c.mu.Unlock()
	return &devicemodel.Snapshot{TakenAt: now, System: sys, Ports: ports, MACTable: macs, UplinkHint: uplinkHint}, nil
}

// isLANNetwork: the container is on the LAN under its own MAC.
func isLANNetwork(n network) bool {
	switch n.Driver {
	case "macvlan", "ipvlan":
		return true
	}
	return false
}

// assignSlots keeps the host's endpoint -> port map current: existing
// assignments stay, new endpoints take the lowest free slot in creation
// order, and endpoints whose container no longer exists are dropped.
// Returns the map and whether it changed (and so must be written back).
func (c *Collector) assignSlots(stored string, eps []endpointInfo, guestSlots int) (map[string]int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	slots := map[string]int{}
	if !c.slotsKnown {
		if s := strings.TrimSpace(stored); s != "" {
			if err := json.Unmarshal([]byte(s), &slots); err != nil {
				c.warnOnceLocked("slots-parse", "slot file %s not understood (%v); reassigning", c.SlotsFile, err)
				slots = map[string]int{}
			}
		}
		c.slots = slots
		c.slotsKnown = true
	}
	slots = c.slots
	changed := false
	present := map[string]bool{}
	for _, e := range eps {
		present[e.key] = true
	}
	for k := range slots {
		if !present[k] {
			delete(slots, k)
			changed = true
		}
	}
	used := map[int]bool{}
	for _, idx := range slots {
		used[idx] = true
	}
	for _, e := range eps {
		if _, ok := slots[e.key]; ok {
			continue
		}
		for idx := 1; idx <= guestSlots; idx++ {
			if !used[idx] {
				slots[e.key] = idx
				used[idx] = true
				changed = true
				break
			}
		}
	}
	out := make(map[string]int, len(slots))
	for k, v := range slots {
		out[k] = v
	}
	return out, changed
}

// writeSlots stores the map on the host, atomically.
func (c *Collector) writeSlots(ctx context.Context, slots map[string]int) error {
	b, err := json.MarshalIndent(slots, "", "  ")
	if err != nil {
		return err
	}
	dir := c.SlotsFile[:strings.LastIndex(c.SlotsFile, "/")]
	tmp := sshrun.ShellQuote(c.SlotsFile + ".tmp")
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s && mv -f %s %s", sshrun.ShellQuote(dir), tmp, tmp, sshrun.ShellQuote(c.SlotsFile))
	_, err = c.r.Run(ctx, cmd, string(b)+"\n")
	return err
}

// endpointPort renders one container endpoint. The port's counters are
// the container interface's seen from the switch side: what the container
// transmits arrives at the port (rx), what it receives left the port (tx).
// A memory-bound veth is shown as a 100G port, as the proxmox driver shows
// virtio guests.
func endpointPort(idx int, e endpointInfo) devicemodel.Port {
	label := e.container.Name()
	if e.multi {
		label += " " + e.network.Name
	}
	p := devicemodel.Port{
		Index: idx, IfName: label, Name: label, Description: label,
		Media: devicemodel.MediaQSFP28, Lanes: 1, Present: true, Enabled: true,
		SpeedMbps: 100000, FullDuplex: true, AutoNeg: true, SpeedCaps: []int{100000},
		STPState: "disabled", STPRole: "disabled", MTU: 1500,
		VLAN: devicemodel.PortVLAN{Mode: "trunk", NativeVLAN: 1, AllowAll: true},
	}
	if e.hasLink {
		p.Up = e.running && e.link.hasFlag("LOWER_UP")
		p.MTU = e.link.MTU
		p.Counters = devicemodel.Counters{
			RxBytes: e.link.Stats64.Tx.Bytes, TxBytes: e.link.Stats64.Rx.Bytes,
			RxPackets: e.link.Stats64.Tx.Packets, TxPackets: e.link.Stats64.Rx.Packets,
			RxErrors: e.link.Stats64.Tx.Errors, TxErrors: e.link.Stats64.Rx.Errors,
			RxDropped: e.link.Stats64.Tx.Dropped, TxDropped: e.link.Stats64.Rx.Dropped,
			RxMulticast: e.link.Stats64.Rx.Multicast,
		}
	}
	if p.Up {
		p.STPState, p.STPRole = "forwarding", "designated"
	} else {
		p.SpeedMbps = 0
	}
	return p
}

// nicPort renders a host NIC as a top port. A NIC whose speed the kernel
// does not know (virtio in a VM: speed -1) is shown as a 100G port, the
// way a guest is on the node that hosts it.
func nicPort(idx int, name string, l ipLink, et ethtoolInfo, phys map[string]string, carrier map[string]string) devicemodel.Port {
	speed := et.Speed
	if speed <= 0 {
		speed = atoiDefault(phys["speed"])
	}
	media := devicemodel.MediaQSFP28
	caps := []int{100000}
	switch {
	case speed <= 0:
		speed = 100000
	case speed >= 100000:
	case speed >= 25000:
		media, caps = devicemodel.MediaSFP28, []int{speed}
	case speed >= 10000:
		media, caps = devicemodel.MediaSFPPlus, []int{speed}
	default:
		media, caps = devicemodel.MediaCopper1G, []int{speed}
	}
	if len(et.Speeds) > 0 {
		caps = et.Speeds
	}
	p := devicemodel.Port{
		Index: idx, IfName: name, Name: name, Interfaces: []string{name}, Lanes: 1, Present: true,
		Media: media, Enabled: l.hasFlag("UP"), Up: l.hasFlag("LOWER_UP"), MTU: l.MTU,
		Counters: countersOf(l), AutoNeg: et.Autoneg || et.Speed <= 0, SpeedCaps: caps,
		FullDuplex: !strings.EqualFold(et.Duplex, "Half"), SpeedMbps: speed,
		STPState: "disabled", STPRole: "disabled",
		VLAN: devicemodel.PortVLAN{Mode: "trunk", NativeVLAN: 1, AllowAll: true},
	}
	if p.Up {
		p.STPState, p.STPRole = "forwarding", "designated"
	} else {
		p.SpeedMbps = 0
	}
	n, _ := strconv.ParseUint(carrier[name], 10, 64)
	p.Health.LinkChanges = n
	return p
}

// emptyPort is a free slot: present in the profile, nothing on it.
func emptyPort(idx int) devicemodel.Port {
	return devicemodel.Port{Index: idx, Media: devicemodel.MediaQSFP28, Lanes: 1, Enabled: false, AutoNeg: true,
		VLAN: devicemodel.PortVLAN{Mode: "trunk", NativeVLAN: 1, AllowAll: true}}
}

func countersOf(l ipLink) devicemodel.Counters {
	return devicemodel.Counters{
		RxBytes: l.Stats64.Rx.Bytes, TxBytes: l.Stats64.Tx.Bytes,
		RxPackets: l.Stats64.Rx.Packets, TxPackets: l.Stats64.Tx.Packets,
		RxErrors: l.Stats64.Rx.Errors, TxErrors: l.Stats64.Tx.Errors,
		RxDropped: l.Stats64.Rx.Dropped, TxDropped: l.Stats64.Tx.Dropped,
		RxMulticast: l.Stats64.Rx.Multicast,
	}
}

func (c *Collector) uplinkName(snap *devicemodel.Snapshot) string {
	for _, p := range snap.Ports {
		if p.Index == snap.UplinkHint {
			return p.IfName
		}
	}
	return "?"
}

func (c *Collector) nicCount(snap *devicemodel.Snapshot) int {
	n := 0
	for _, p := range snap.Ports {
		if len(p.Interfaces) > 0 {
			n++
		}
	}
	return n
}

func (c *Collector) freeCount(snap *devicemodel.Snapshot) int {
	n := 0
	for _, p := range snap.Ports {
		if !p.Present {
			n++
		}
	}
	return n
}

// remember keeps every name a slot has carried, for DefaultPortNames.
func (c *Collector) remember(idx int, label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.knownNames[idx] {
		if n == label {
			return
		}
	}
	c.knownNames[idx] = append(c.knownNames[idx], label)
}

func (c *Collector) warnOnce(key, format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.warnOnceLocked(key, format, args...)
}

func (c *Collector) warnOnceLocked(key, format string, args ...any) {
	if c.warned[key] {
		return
	}
	c.warned[key] = true
	c.Log.Printf("podman %s: "+format, append([]any{c.host}, args...)...)
}
