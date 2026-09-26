package apcups

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The register map is APC's Smart-UPS Modbus map as NUT's apc_modbus driver
// reads it (drivers/apc_modbus.c, NUT 2.8.5), which is the same map the
// SmartConnect port serves: firmware UPS 01.3 for ID 1026 "added support for
// Modbus over TCP on SmartConnect port" (APC's SMTL series release notes).
// The map is sparse, so reads are done in the four blocks the reference
// driver uses; a read straddling an undefined address is refused with an
// exception, and a burst of single-register reads made a real unit refuse
// connections (2026-09-26).
//
// Values are fixed-point: the register holds value * 2^bits. The bit counts
// are the reference driver's, checked against a live unit (load 80.97 % =
// 20728/256; input 115.77 V = 7409/64).
const (
	blockStatus, blockStatusLen   = 0, 27
	blockDynamic, blockDynamicLen = 128, 44
	blockStatic, blockStaticLen   = 516, 120
	blockConfig, blockConfigLen   = 1026, 22

	regStatus         = 0   // UPSStatus_BF, 2 registers
	regChangeCause    = 2   // UPSStatusChangeCause
	regOutletGroup0   = 3   // OutletStatus_BF for group 0, 2 registers; group i at 3+3i
	regSimpleSignal   = 18  // SimpleSignalingStatus_BF
	regRuntimeCal     = 24  // RunTimeCalibrationStatus_BF
	regRuntime        = 128 // seconds, 2 registers
	regCharge         = 130 // %, 9 bits
	regBatteryVoltage = 131 // V, signed, 5 bits
	regBatteryTemp    = 135 // C, signed, 7 bits
	regLoadPct        = 136 // % of real-power rating, 8 bits
	regApparentPct    = 138 // % of apparent-power rating, 8 bits
	regOutputCurrent  = 140 // A, 5 bits
	regOutputVoltage  = 142 // V, 6 bits
	regOutputFreq     = 144 // Hz, 7 bits
	regInputStatus    = 150 // InputStatus_BF
	regInputVoltage   = 151 // V, 6 bits; 0xffff = not applicable
	regEfficiency     = 154 // %, signed, 7 bits; negative = a reason, not a value
	regFirmware       = 516 // 8 registers, ASCII
	regModel          = 532 // 16 registers, ASCII
	regSerial         = 564 // 8 registers, ASCII
	regApparentRating = 588 // VA
	regRealRating     = 589 // W
	regSOGConfig      = 590 // SOGRelayConfigSetting_BF: which outlet groups exist
	regGroupName0     = 604 // 8 registers per group, 4 groups
	regTransferHigh   = 1026
	regTransferLow    = 1027
	regShutdownDelay  = 1029 // seconds, signed
)

// UPSStatus_BF bits (the low 32 bits of registers 0-1).
const (
	statusOnline    = 1 << 1
	statusOnBattery = 1 << 2
	statusBypass    = 1 << 3
	statusOff       = 1 << 4
	statusFault     = 1 << 5
	statusInputBad  = 1 << 6
	statusTest      = 1 << 7
	statusECO       = 1 << 13
	statusOverload  = 1 << 21

	outletGroupOn = 1 << 0 // OutletStatus_BF

	simpleShutdownImminent = 1 << 1 // SimpleSignalingStatus_BF
	calibrationInProgress  = 1 << 1 // RunTimeCalibrationStatus_BF
	inputBoost             = 1 << 5 // InputStatus_BF
	inputTrim              = 1 << 6

	sogGroupPresent = 1 << 0 // SOGRelayConfigSetting_BF: bit i = group i present
)

// changeCause names the values of UPSStatusChangeCause that have been seen
// on a live unit; anything else is reported by number.
var changeCause = map[uint16]string{
	8: "AcceptableInput",
}

// Collector is an open connection to one APC Smart-UPS.
type Collector struct {
	r Runner
	// Addr is the address dialled, reported as the device's own so the
	// controller has somewhere to place it; MAC is the SmartConnect port's
	// address when the operator supplied it.
	Addr string
	MAC  string
	// Netmask and GatewayMAC are the unit's network as the operator knows it.
	// Modbus carries no IP configuration, and the controller places a device
	// by its netmask and gateway MAC (a device without them has no parent in
	// the topology -- the lesson that cost a day on the Arista).
	Netmask    string
	GatewayMAC string
	// Resolve turns the dialled host into addresses when it is a name rather
	// than a literal. The UPS's address is held only by a DHCP lease (nothing
	// can push one onto it), and the gateway keeps a DNS name for the lease
	// itself -- one that survives adoption deleting the client record. Dialling
	// that name follows the lease; the resolved address is what is reported.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)

	mu       sync.Mutex
	last     *devicemodel.Snapshot
	resolved string // last address the name resolved to, for change logging
}

// NewCollector builds a collector over a runner.
func NewCollector(r Runner) *Collector { return &Collector{r: r} }

// Start reads the whole unit once, so an unreachable port, Modbus left
// disabled at the LCD, or a unit that is not a Smart-UPS fails here rather
// than on the first inform.
func (c *Collector) Start(ctx context.Context) (*devicemodel.Snapshot, error) {
	snap, err := c.Collect(ctx)
	if err != nil {
		return nil, err
	}
	b := snap.System.Battery
	if snap.System.Model == "" || b == nil || b.RealPowerRatingW <= 0 {
		return nil, fmt.Errorf("apc-ups: %s answered Modbus but reported no Smart-UPS identity or rating; is it a Smart-UPS with Modbus enabled?", c.Addr)
	}
	log.Printf("apc-ups: %s %s, %s, rated %.0f W / %.0f VA", snap.System.Vendor, snap.System.Model, snap.System.Version, b.RealPowerRatingW, b.ApparentRatingVA)
	return snap, nil
}

func (c *Collector) Close() error { return nil }

// Collect reads the four register blocks and builds the snapshot.
func (c *Collector) Collect(ctx context.Context) (*devicemodel.Snapshot, error) {
	status, err := c.r.ReadRegisters(ctx, blockStatus, blockStatusLen)
	if err != nil {
		return nil, err
	}
	dyn, err := c.r.ReadRegisters(ctx, blockDynamic, blockDynamicLen)
	if err != nil {
		return nil, err
	}
	static, err := c.r.ReadRegisters(ctx, blockStatic, blockStaticLen)
	if err != nil {
		return nil, err
	}
	cfg, err := c.r.ReadRegisters(ctx, blockConfig, blockConfigLen)
	if err != nil {
		return nil, err
	}

	snap := &devicemodel.Snapshot{TakenAt: time.Now()}
	snap.System = c.system(static)
	c.fillAddress(ctx, &snap.System)
	snap.System.Battery = c.battery(status, dyn, static, cfg)
	// The UPS measures its whole output; this is the aggregate the overview's
	// power card reads, the same fields a metered PDU fills.
	if b := snap.System.Battery; b.HasOutput {
		snap.System.HasPowerDraw = true
		snap.System.PowerDrawW = b.RealPowerW
		snap.System.PowerCurrentA = b.OutputCurrentA
		snap.System.PowerBudgetW = b.RealPowerRatingW
	}
	// The SmartConnect port is the unit's one network interface and its
	// uplink. There is no LLDP on it, so the loop is told directly.
	snap.Ports = []devicemodel.Port{c.uplinkPort()}
	snap.UplinkHint = 1
	// Outlet groups are read (OutletGroups) but not yet presented as outlets:
	// presenting them would offer the controller a relay, and switching one
	// is a follow-on that waits on a discussion with the operator
	// (docs/drivers/apc-ups.md).

	c.mu.Lock()
	c.last = snap
	c.mu.Unlock()
	return snap, nil
}

func (c *Collector) system(static []uint16) devicemodel.System {
	sys := devicemodel.System{
		Vendor:  "APC",
		Model:   regString(static, regModel-blockStatic, 16),
		Serial:  regString(static, regSerial-blockStatic, 8),
		Version: strings.TrimPrefix(regString(static, regFirmware-blockStatic, 8), "UPS "),
		MAC:     c.MAC,
	}
	if sys.MAC == "" {
		sys.MAC = syntheticMAC(sys.Serial)
	}
	sys.GatewayMAC = strings.ToLower(c.GatewayMAC)
	return sys
}

func (c *Collector) battery(status, dyn, static, cfg []uint16) *devicemodel.Battery {
	b := &devicemodel.Battery{}

	// Two-register values are big-endian by register: the first holds the
	// high word. Confirmed against a live unit, whose 0x2002/0x0000 decoded to
	// online + ECO mode exactly as NUT reported it.
	st := uint32(status[regStatus])<<16 | uint32(status[regStatus+1])
	b.OnBattery = st&statusOnBattery != 0
	b.Bypass = st&statusBypass != 0
	b.OutputOff = st&statusOff != 0
	b.Fault = st&statusFault != 0
	b.InputBad = st&statusInputBad != 0
	b.Testing = st&statusTest != 0
	b.ECOMode = st&statusECO != 0
	b.Overload = st&statusOverload != 0
	b.LowBattery = status[regSimpleSignal]&simpleShutdownImminent != 0
	b.Calibrating = status[regRuntimeCal]&calibrationInProgress != 0
	if name, ok := changeCause[status[regChangeCause]]; ok {
		b.TransferCause = name
	} else {
		b.TransferCause = fmt.Sprintf("cause %d", status[regChangeCause])
	}

	b.Runtime = time.Duration(uint32(dyn[regRuntime-blockDynamic])<<16|uint32(dyn[regRuntime-blockDynamic+1])) * time.Second
	b.ChargePct = int(fixed(dyn[regCharge-blockDynamic], 9) + 0.5)
	b.VoltageV = sfixed(dyn[regBatteryVoltage-blockDynamic], 5)
	b.TemperatureC = sfixed(dyn[regBatteryTemp-blockDynamic], 7)
	b.HasTemperature = true

	b.RealPowerRatingW = float64(static[regRealRating-blockStatic])
	b.ApparentRatingVA = float64(static[regApparentRating-blockStatic])
	b.LoadPct = fixed(dyn[regLoadPct-blockDynamic], 8)
	b.RealPowerW = b.LoadPct / 100 * b.RealPowerRatingW
	b.ApparentPowerVA = fixed(dyn[regApparentPct-blockDynamic], 8) / 100 * b.ApparentRatingVA
	b.OutputCurrentA = fixed(dyn[regOutputCurrent-blockDynamic], 5)
	b.OutputVoltageV = fixed(dyn[regOutputVoltage-blockDynamic], 6)
	b.OutputFrequencyHz = fixed(dyn[regOutputFreq-blockDynamic], 7)
	b.HasOutput = true

	if raw := dyn[regInputVoltage-blockDynamic]; raw != 0xffff {
		b.InputVoltageV = fixed(raw, 6)
		b.HasInput = true
	}
	if eff := sfixed(dyn[regEfficiency-blockDynamic], 7); eff >= 0 {
		b.EfficiencyPct = eff
		b.HasEfficiency = true
	}
	in := dyn[regInputStatus-blockDynamic]
	b.Boost = in&inputBoost != 0
	b.Trim = in&inputTrim != 0
	// The map has no charging bit; a unit on mains below full charge is
	// charging, which is what the HID feed reported as CHRG for the same unit.
	b.Charging = !b.OnBattery && b.ChargePct < 100

	b.TransferHighV = int(cfg[regTransferHigh-blockConfig])
	b.TransferLowV = int(cfg[regTransferLow-blockConfig])
	b.ShutdownDelay = time.Duration(int16(cfg[regShutdownDelay-blockConfig])) * time.Second
	return b
}

// OutletGroups reports the unit's outlet groups from the last snapshot's
// registers: the name the unit gives each present group and whether it is
// on. Informational in this version; switching them is a follow-on.
func OutletGroups(status, static []uint16) []string {
	var out []string
	present := static[regSOGConfig-blockStatic]
	for i := 0; i < 4; i++ {
		if present&(sogGroupPresent<<i) == 0 {
			continue
		}
		name := regString(static, regGroupName0-blockStatic+8*i, 8)
		on := status[regOutletGroup0+3*i+1]&outletGroupOn != 0 // low word of the 2-register field
		state := "off"
		if on {
			state = "on"
		}
		out = append(out, fmt.Sprintf("%d:%s:%s", i, name, state))
	}
	return out
}

// uplinkPort presents the SmartConnect port as the single port, so the
// payload has something for `uplink` to resolve against in if_table.
func (c *Collector) uplinkPort() devicemodel.Port {
	return devicemodel.Port{
		Index: 1, IfName: "eth0", Name: "SmartConnect", Media: devicemodel.MediaCopper1G,
		Present: true, Enabled: true, Up: true, SpeedMbps: 100, FullDuplex: true, Lanes: 1,
	}
}

// Capabilities: a UPS is not a switch. Claiming a switch feature here would
// put a control in the UI that this driver cannot honour.
func (c *Collector) Capabilities() devicemodel.Capabilities {
	return devicemodel.Capabilities{}
}

// fixed decodes an unsigned fixed-point register with bits fractional bits.
func fixed(raw uint16, bits uint) float64 { return float64(raw) / float64(uint32(1)<<bits) }

// sfixed decodes a signed one.
func sfixed(raw uint16, bits uint) float64 { return float64(int16(raw)) / float64(uint32(1)<<bits) }

// regString decodes n big-endian registers of ASCII, trimming padding.
func regString(regs []uint16, off, n int) string {
	if off < 0 || off+n > len(regs) {
		return ""
	}
	b := make([]byte, 0, 2*n)
	for _, r := range regs[off : off+n] {
		b = append(b, byte(r>>8), byte(r))
	}
	return strings.TrimRight(strings.TrimSpace(strings.TrimRight(string(b), "\x00")), " ")
}

// prefixLen turns a dotted netmask into a prefix length; 0 when absent or
// unparseable, which the payload reports as no netmask.
func prefixLen(mask string) int {
	ip := net.ParseIP(mask)
	if ip == nil {
		return 0
	}
	ones, _ := net.IPMask(ip.To4()).Size()
	return ones
}

// syntheticMAC derives a stable locally-administered unicast address from the
// unit's serial, for a bridged device adopted under its own identity rather
// than the SmartConnect port's. Adopting under the port's real MAC replaces
// the unit's client record in the controller, and any fixed-IP reservation
// on it; a synthetic address leaves both alone.
func syntheticMAC(serial string) string {
	h := sha256.Sum256([]byte("apc-ups " + serial))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", h[0], h[1], h[2], h[3], h[4])
}

// fillAddress reports the unit's address: the dialled literal, or what the
// dialled name resolves to right now. A name that stops resolving leaves
// the address unreported rather than stale.
func (c *Collector) fillAddress(ctx context.Context, sys *devicemodel.System) {
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		host = c.Addr
	}
	ip := host
	if net.ParseIP(host) == nil {
		resolve := c.Resolve
		if resolve == nil {
			resolve = func(ctx context.Context, h string) ([]net.IP, error) {
				addrs, err := net.DefaultResolver.LookupIPAddr(ctx, h)
				if err != nil {
					return nil, err
				}
				out := make([]net.IP, 0, len(addrs))
				for _, a := range addrs {
					out = append(out, a.IP)
				}
				return out, nil
			}
		}
		ips, err := resolve(ctx, host)
		ip = ""
		for _, a := range ips {
			if v4 := a.To4(); v4 != nil {
				ip = v4.String()
				break
			}
		}
		if err != nil || ip == "" {
			log.Printf("apc-ups: %s does not resolve to an IPv4 address (%v); address unreported this cycle", host, err)
			return
		}
		c.mu.Lock()
		if c.resolved != "" && c.resolved != ip {
			log.Printf("apc-ups: %s now resolves to %s (was %s): following the lease", host, ip, c.resolved)
		}
		c.resolved = ip
		c.mu.Unlock()
	}
	sys.Addresses = []devicemodel.IfAddress{{Iface: "eth0", IP: ip, PrefixLen: prefixLen(c.Netmask)}}
}
