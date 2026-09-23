package apcpdu

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The PowerNet OIDs this driver reads and writes. The two outlet tables are
// easy to confuse and one of them switches power, so they were mapped column
// by column against the live card (2026-09-21) rather than from a MIB
// listing:
//
//	12.3.3 is the CONTROL table (5 columns): index, name, phase, command, bank.
//	        Column 4 reads back the outlet's current state and is what you
//	        write to switch it. It is the only writable outlet object.
//	12.3.5 is the CONFIG table (6 columns): index, name, phase, power-on time,
//	        power-off time, reboot duration. Its name column is READ-ONLY.
const (
	oidSystem    = "1.3.6.1.2.1.1"
	oidSysDescr  = "1.3.6.1.2.1.1.1.0"
	oidSysUptime = "1.3.6.1.2.1.1.3.0"
	oidSysName   = "1.3.6.1.2.1.1.5.0"
	oidIfTable   = "1.3.6.1.2.1.2.2.1"
	oidIfPhysAdr = "1.3.6.1.2.1.2.2.1.6"

	// IP-MIB: the card's own address and mask, its default route, and its ARP
	// cache. Together they give the reachability fields a real UniFi device
	// reports (netmask, gateway_mac) -- the controller uses them to place the
	// device in a network. These tables index their rows by address.
	oidIPAddrTable  = "1.3.6.1.2.1.4.20.1"   // .1.<ip> = address, .3.<ip> = mask
	oidIPRouteNext  = "1.3.6.1.2.1.4.21.1.7" // .0.0.0.0 = default gateway
	oidIPNetToMedia = "1.3.6.1.2.1.4.22.1.2" // .<ifIndex>.<ip> = MAC
	oidDefaultRoute = "1.3.6.1.2.1.4.21.1.7.0.0.0.0"

	oidRPDU        = "1.3.6.1.4.1.318.1.1.12"
	oidIdent       = "1.3.6.1.4.1.318.1.1.12.1"
	oidIdentHWRev  = "1.3.6.1.4.1.318.1.1.12.1.2.0"
	oidIdentFWRev  = "1.3.6.1.4.1.318.1.1.12.1.3.0"
	oidIdentModel  = "1.3.6.1.4.1.318.1.1.12.1.5.0"
	oidIdentSerial = "1.3.6.1.4.1.318.1.1.12.1.6.0"
	oidIdentNumOut = "1.3.6.1.4.1.318.1.1.12.1.8.0"

	oidLoad            = "1.3.6.1.4.1.318.1.1.12.2"      // the phase/load subtree
	oidIdentRatingA    = "1.3.6.1.4.1.318.1.1.12.1.7.0"  // the PDU's rating in amps
	oidIdentVoltage    = "1.3.6.1.4.1.318.1.1.12.1.15.0" // configured line-to-line volts
	oidIdentPowerFactr = "1.3.6.1.4.1.318.1.1.12.1.17.0" // configured power factor x1000

	oidLoadPhaseMax = "1.3.6.1.4.1.318.1.1.12.2.1.1.0"     // amps
	oidLoadStatus   = "1.3.6.1.4.1.318.1.1.12.2.3.1.1.2.1" // tenths of an amp

	oidOutletCtlName = "1.3.6.1.4.1.318.1.1.12.3.3.1.1.2" // read-only
	oidOutletCtlCmd  = "1.3.6.1.4.1.318.1.1.12.3.3.1.1.4" // read state / write to switch
	oidOutletCfgName = "1.3.6.1.4.1.318.1.1.12.3.5.1.1.2" // read-only
)

// Outlet command values (rPDUOutletControlOutletCommand). Only the immediate
// forms are used: the delayed variants are the card's own scheduling, which
// the controller does not drive.
const (
	outletCmdOn     = 1
	outletCmdOff    = 2
	outletCmdReboot = 3
)

// Collector is an open connection to one APC rack PDU.
type Collector struct {
	r Runner

	// Outlets is the outlet count to present. 0 means "ask the card"
	// (rPDUIdentDeviceNumOutlets).
	Outlets int

	mu     sync.Mutex
	last   *devicemodel.Snapshot
	warned map[string]bool
	// dhcp is the card's addressing mode, read from its config file once at
	// Start (it is not in the MIB) and kept current by ApplyAddress.
	dhcp     bool
	dhcpRead bool
}

// NewCollector builds a collector over a runner.
func NewCollector(r Runner) *Collector {
	return &Collector{r: r, warned: map[string]bool{}}
}

// Start runs every read the driver will ever do, once, so a wrong community
// or an unreachable card fails here rather than on the first inform.
func (c *Collector) Start(ctx context.Context) (*devicemodel.Snapshot, error) {
	if b, err := c.r.GetConfig(ctx); err != nil {
		return nil, fmt.Errorf("apc-pdu: read config.ini: %w", err)
	} else {
		c.mu.Lock()
		c.dhcp, c.dhcpRead = configBootMode(b)
		c.mu.Unlock()
	}
	snap, err := c.Collect(ctx)
	if err != nil {
		return nil, err
	}
	if len(snap.Outlets) == 0 {
		return nil, fmt.Errorf("apc-pdu: the card reported no outlets; is %s a rack PDU?", snap.System.Model)
	}
	return snap, nil
}

func (c *Collector) Close() error { return nil }

// Collect reads the whole device in three walks: the system group, the
// interface table (for the card's own MAC) and the rPDU tree.
func (c *Collector) Collect(ctx context.Context) (*devicemodel.Snapshot, error) {
	// Walk only the subtrees that are read. SNMPv1 has no GETBULK, so every
	// OID is its own GETNEXT round trip: walking the whole rPDU tree costs
	// ~340 of them and took 15s against the real card, against an inform
	// cadence of 65-80s.
	sys, err := c.r.Walk(ctx, oidSystem)
	if err != nil {
		return nil, err
	}
	ifs, err := c.r.Walk(ctx, oidIfPhysAdr)
	if err != nil {
		return nil, err
	}
	ip := map[string]string{}
	for _, root := range []string{oidIPAddrTable, oidIPRouteNext, oidIPNetToMedia} {
		part, err := c.r.Walk(ctx, root)
		if err != nil {
			return nil, err
		}
		for k, v := range part {
			ip[k] = v
		}
	}
	rpdu := map[string]string{}
	for _, root := range []string{oidIdent, oidLoad, oidOutletCtlName, oidOutletCtlCmd} {
		part, err := c.r.Walk(ctx, root)
		if err != nil {
			return nil, err
		}
		for k, v := range part {
			rpdu[k] = v
		}
	}

	snap := &devicemodel.Snapshot{TakenAt: time.Now()}
	snap.System = c.system(sys, ifs, rpdu)
	c.reachability(&snap.System, ip)
	c.mu.Lock()
	snap.System.DHCP = c.dhcp
	c.mu.Unlock()
	snap.Outlets = c.outlets(rpdu)
	// The card has exactly one network interface, and it is the uplink. There
	// is no LLDP on this firmware, so the loop is told directly rather than
	// left to infer it from a neighbour it will never see.
	snap.Ports = []devicemodel.Port{c.uplinkPort(snap.System, ifs)}
	snap.UplinkHint = 1

	c.mu.Lock()
	c.last = snap
	c.mu.Unlock()
	return snap, nil
}

func (c *Collector) system(sys, ifs, rpdu map[string]string) devicemodel.System {
	out := devicemodel.System{
		Vendor:   "APC",
		Model:    strings.TrimSpace(rpdu[oidIdentModel]),
		Serial:   strings.TrimSpace(rpdu[oidIdentSerial]),
		Version:  strings.TrimPrefix(strings.TrimSpace(rpdu[oidIdentFWRev]), "v"),
		Hostname: strings.TrimSpace(sys[oidSysName]),
		MAC:      firstMAC(ifs),
	}
	if t, err := strconv.ParseInt(strings.TrimSpace(sys[oidSysUptime]), 10, 64); err == nil {
		// sysUpTime is in hundredths of a second.
		out.Uptime = time.Duration(t) * 10 * time.Millisecond
	}
	// A switched rack PDU meters the phase, not the outlet: there is one
	// aggregate current reading for the whole device. Watts are not measured
	// -- the card computes them from the current and the operator-configured
	// line voltage and power factor (its PDU Configuration screen), so the
	// same arithmetic is done here.
	if amps, ok := tenths(rpdu[oidLoadStatus]); ok {
		v := floatOr(rpdu[oidIdentVoltage], 0)
		pf := floatOr(rpdu[oidIdentPowerFactr], 1000) / 1000
		out.HasPowerDraw = true
		out.PowerCurrentA = amps
		out.PowerDrawW = amps * v * pf
		if rating := floatOr(rpdu[oidIdentRatingA], 0); rating > 0 && v > 0 {
			out.PowerBudgetW = rating * v
		}
	}
	// The AP7931 has no fans, no redundant supplies and no temperature sensor,
	// so those stay empty rather than being invented.
	return out
}

// uplinkPort presents the card's network interface as the single port, so the
// payload has something for `uplink` to resolve against in if_table.
func (c *Collector) uplinkPort(sys devicemodel.System, ifs map[string]string) devicemodel.Port {
	p := devicemodel.Port{
		Index:      1,
		IfName:     "eth0",
		Name:       "Network",
		Media:      devicemodel.MediaCopper1G,
		Present:    true,
		Enabled:    true,
		Up:         true,
		SpeedMbps:  100, // 10/100 card; the profile renders the icon
		FullDuplex: true,
		Lanes:      1,
	}
	return p
}

// outlets reads the control table: the name from its read-only name column and
// the state from the command column, which reads back what the relay is doing.
func (c *Collector) outlets(rpdu map[string]string) []devicemodel.Outlet {
	idx := map[int]*devicemodel.Outlet{}
	get := func(i int) *devicemodel.Outlet {
		o, ok := idx[i]
		if !ok {
			o = &devicemodel.Outlet{Index: i, Switchable: true}
			idx[i] = o
		}
		return o
	}
	for oid, v := range rpdu {
		switch {
		case strings.HasPrefix(oid, oidOutletCtlName+"."):
			if i, ok := trailingIndex(oid, oidOutletCtlName); ok {
				get(i).Name = strings.TrimSpace(v)
			}
		case strings.HasPrefix(oid, oidOutletCtlCmd+"."):
			if i, ok := trailingIndex(oid, oidOutletCtlCmd); ok {
				n, _ := strconv.Atoi(strings.TrimSpace(v))
				get(i).On = n == outletCmdOn
			}
		}
	}
	out := make([]devicemodel.Outlet, 0, len(idx))
	for _, o := range idx {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	if c.Outlets > 0 && len(out) > c.Outlets {
		out = out[:c.Outlets]
	}
	return out
}

// Capabilities: a rack PDU is not a switch. Claiming a switch feature here
// would put a control in the UI that this driver cannot honour.
func (c *Collector) Capabilities() devicemodel.Capabilities {
	return devicemodel.Capabilities{}
}

func trailingIndex(oid, prefix string) (int, bool) {
	rest := strings.TrimPrefix(oid, prefix+".")
	if rest == "" || strings.Contains(rest, ".") {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// firstMAC returns the first non-empty ifPhysAddress, which on this card is
// its single network interface.
func firstMAC(ifs map[string]string) string {
	best, bestIdx := "", 1<<30
	for oid, v := range ifs {
		if !strings.HasPrefix(oid, oidIfPhysAdr+".") {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		i, ok := trailingIndex(oid, oidIfPhysAdr)
		if !ok || i >= bestIdx {
			continue
		}
		best, bestIdx = normaliseMAC(v), i
	}
	return best
}

// normaliseMAC pads the unpadded octets snmpwalk prints (0:c0:b7:...) into the
// lower-case colon form the rest of the bridge uses.
func normaliseMAC(s string) string {
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return strings.ToLower(s)
	}
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
		parts[i] = strings.ToLower(parts[i])
	}
	return strings.Join(parts, ":")
}

// tenths parses a value the card reports in tenths of a unit (its load
// readings), returning whole units.
func tenths(s string) (float64, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return float64(n) / 10, true
}

// floatOr parses a numeric value, falling back to def when the card did not
// report it.
func floatOr(s string, def float64) float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	return n
}

// reachability fills the card's own addresses, its ARP cache and its
// gateway's MAC from the IP-MIB tables, whose rows are keyed by address.
func (c *Collector) reachability(sys *devicemodel.System, ip map[string]string) {
	masks := map[string]string{}
	for oid, v := range ip {
		switch {
		case strings.HasPrefix(oid, oidIPAddrTable+".3."):
			masks[strings.TrimPrefix(oid, oidIPAddrTable+".3.")] = strings.TrimSpace(v)
		}
	}
	for oid, v := range ip {
		switch {
		case strings.HasPrefix(oid, oidIPAddrTable+".1."):
			addr := strings.TrimSpace(v)
			if addr == "" || strings.HasPrefix(addr, "127.") {
				continue
			}
			sys.Addresses = append(sys.Addresses, devicemodel.IfAddress{
				Iface: "eth0", IP: addr, PrefixLen: prefixLen(masks[addr]),
			})
		case strings.HasPrefix(oid, oidIPNetToMedia+"."):
			// .<ifIndex>.<a>.<b>.<c>.<d>: the address is the last four labels.
			parts := strings.Split(strings.TrimPrefix(oid, oidIPNetToMedia+"."), ".")
			if len(parts) < 5 {
				continue
			}
			if sys.ARP == nil {
				sys.ARP = map[string]string{}
			}
			sys.ARP[strings.Join(parts[len(parts)-4:], ".")] = normaliseMAC(strings.TrimSpace(v))
		}
	}
	sort.Slice(sys.Addresses, func(i, j int) bool { return sys.Addresses[i].IP < sys.Addresses[j].IP })
	if gw := strings.TrimSpace(ip[oidDefaultRoute]); gw != "" {
		sys.Gateway = gw
		sys.GatewayMAC = sys.ARP[gw]
	}
}

// prefixLen turns a dotted netmask into a prefix length; 0 when unparseable.
func prefixLen(mask string) int {
	n := 0
	for _, part := range strings.Split(mask, ".") {
		v, err := strconv.Atoi(part)
		if err != nil {
			return 0
		}
		for ; v > 0; v >>= 1 {
			n += v & 1
		}
	}
	return n
}

// configBootMode reads BootMode out of the card's config.ini: true for the
// DHCP/BOOTP modes, false for Manual. ok is false when the key is absent.
func configBootMode(cfg []byte) (dhcp bool, ok bool) {
	for _, line := range strings.Split(string(cfg), "\n") {
		k, v, found := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !found || strings.TrimSpace(k) != "BootMode" {
			continue
		}
		return !strings.EqualFold(strings.TrimSpace(v), "Manual"), true
	}
	return false, false
}
