package device

import "github.com/TechBlueprints/disunified/internal/devicemodel"

// Capability bitmaps the controller gates UI features on. Values from
// unifi-emu's capability_bits.json. The rule:
// claim only what the bridge can honour on the switch, because the
// controller offers every claimed feature and a claim it cannot service is
// a silent lie in the UI.

// fw_caps (top-level)
const (
	fwCapSSH    = 1 // unifi-emu's placeholder bits 0-1: SSH | STA_STAT
	fwCapSTAT   = 2
	fwCapUTERM  = 4 // device terminal (Manage → Debug). NOT claimed: the terminal is WebRTC (build-ssh-session), see branch ssh-gateway
	fwCapLAG    = 128
	fwCapSNMP   = 8192
	fwCapSNMPv3 = 16384
	fwCapLLDP   = 1073741824
)

// switch_caps.feature_caps
const (
	swCapPortIsolation = 4
	swCapStormControl  = 8
	swCapLLDPMED       = 16
	swCapSTP           = 64
	swCapDHCPSnooping  = 128
	swCapIGMPSnooping  = 256
	swCapLACP          = 8192
	swCapFEC           = 65536
	swCapJumbo         = 524288
)

// switch_caps.vlan_caps
const (
	vlanCapAccess  = 2
	vlanCapTagging = 4
)

// switch_caps.stp_caps
const (
	stpCapBPDUGuard = 1
	stpCapPortCost  = 8
)

// switch_caps.storm_control_caps
const stormCapInPercentage = 1

// port_table[].speed_caps
const (
	speedCapFull10   = 2
	speedCapFull100  = 8
	speedCapFull1000 = 32
	speedCapFull2G5  = 64
	speedCapFull5G   = 128
	speedCapFull10G  = 256
	speedCapFull25G  = 8192
	speedCapFull40G  = 16384
	speedCapFull50G  = 32768
	speedCapFull100G = 65536
	speedCapAutoNeg  = 1048576
	speedCapFEC      = 2097152
)

// sys_error_caps: which system faults the device can report. Claiming them
// lets the controller alert on overheating / fan / PSU failures.
const (
	sysErrOverheating = 2
	sysErrFanIssue    = 4
	sysErrPSUIssue    = 8
)

// hw_caps says what hardware the device physically has. The controller
// believes it over anything else the payload claims: a device that reports an
// outlet table without declaring the outlet bit has that table discarded
// without a word in the log, so it adopts, reports its outlets on every
// inform, and shows none (unifi-emu, 2026-09-21).
//
// Only the outlet bit is claimed here. The three PoE bits in this field are a
// three-value enum matched by exact equality, not independent flags, and
// setting more than one provisions the device into a low-performance path.
const (
	hwCapScreen = 1
	hwCapLEDBar = 2
	hwCapLCM    = 8
	hwCapRPS    = 16
	hwCapOutlet = 128
)

// OutletIndexBase is the index of a claimed model's first AC outlet.
//
// The controller merges its own stored outlet names onto the rows a device
// reports, BY INDEX. The USP-PDU-Pro's own layout puts four USB outlets at
// 1-4 and its sixteen AC outlets at 5-20, so a 16-outlet rack PDU that
// reports its outlets at 1..16 is adopted and then labelled "USB Outlet 1"
// through "USB Outlet 4" (seen on Network 10.6.106, 2026-09-21). Reporting
// the AC outlets at the model's AC positions lines them up.
func OutletIndexBase(model string) int {
	switch model {
	case "USPPDUP":
		return 5
	}
	return 1
}

// outlet_table[].outlet_caps bits, and the outlet_type a rack PDU sends
// alongside them. A value at or above the AC class bit (65536) would select
// the newer encoding; this bridge sends the rack-PDU form, so the values stay
// small and outlet_type is what the controller reads to classify the outlet.
const (
	outletCapHasRelay   = 1
	outletCapPowerMeter = 2

	outletTypeAC  = 0
	outletTypeUSB = 1
)

// HWCapsOutlet is the hw_caps value a power device reports.
const HWCapsOutlet = hwCapOutlet

// SysErrorCaps is the sys_error_caps claim.
const SysErrorCaps = sysErrOverheating | sysErrFanIssue | sysErrPSUIssue

// FWCaps is the fw_caps value the bridge reports. fwCapUTERM is left out on
// purpose: claiming it shows a Debug terminal entry that needs a WebRTC
// session the bridge does not build (branch ssh-gateway has the notes).
const FWCaps = fwCapSSH | fwCapSTAT | fwCapLAG | fwCapSNMP | fwCapSNMPv3 | fwCapLLDP

// DefaultCapabilities is what a driver that declares no Capabilities
// claims: nothing. The UI then offers only what every switch has (port
// state, names, VLANs, speed from the port table). Each driver states
// what its switch honours (devicemodel.Capable); claims must be true.
var DefaultCapabilities = devicemodel.Capabilities{}

// switchCaps renders switch_caps from a driver's capabilities.
func switchCaps(c devicemodel.Capabilities) map[string]any {
	feat := 0
	set := func(on bool, bit int) {
		if on {
			feat |= bit
		}
	}
	set(c.STP, swCapSTP)
	set(c.Jumbo, swCapJumbo)
	set(c.FEC, swCapFEC)
	set(c.LACP, swCapLACP)
	set(c.StormControl, swCapStormControl)
	set(c.IGMPSnooping, swCapIGMPSnooping)
	set(c.LLDPMED, swCapLLDPMED)
	set(c.DHCPSnooping, swCapDHCPSnooping)
	set(c.PortIsolation, swCapPortIsolation)
	stp := 0
	if c.BPDUGuard {
		stp |= stpCapBPDUGuard
	}
	if c.STPPortCost {
		stp |= stpCapPortCost
	}
	storm := 0
	if c.StormControl {
		storm = stormCapInPercentage
	}
	return map[string]any{
		"feature_caps":           feat,
		"max_mirror_sessions":    c.MirrorSessions,
		"max_aggregate_sessions": c.AggregateSessions,
		"vlan_caps":              vlanCapAccess | vlanCapTagging | 1,
		"stp_caps":               stp,
		"storm_control_caps":     storm,
		"igmp_snoop_caps":        0,
	}
}

// FWCapsFor renders fw_caps for a driver's capabilities: the base bits
// (SSH, STA_STAT, LLDP) plus LAG and SNMP when honoured.
func FWCapsFor(c devicemodel.Capabilities) int {
	bits := fwCapSSH | fwCapSTAT | fwCapLLDP
	if c.LACP {
		bits |= fwCapLAG
	}
	if c.SNMP {
		bits |= fwCapSNMP | fwCapSNMPv3
	}
	return bits
}

// speedCaps renders a port's speed_caps bitmap from its capabilities.
func speedCaps(p devicemodel.Port) int {
	bits := 0
	for _, mbps := range p.SpeedCaps {
		switch mbps {
		case 10:
			bits |= speedCapFull10
		case 100:
			bits |= speedCapFull100
		case 1000:
			bits |= speedCapFull1000
		case 2500:
			bits |= speedCapFull2G5
		case 5000:
			bits |= speedCapFull5G
		case 10000:
			bits |= speedCapFull10G
		case 25000:
			bits |= speedCapFull25G
		case 40000:
			bits |= speedCapFull40G
		case 50000:
			bits |= speedCapFull50G
		case 100000:
			bits |= speedCapFull100G
		}
	}
	if bits == 0 {
		return 0
	}
	bits |= speedCapAutoNeg
	if p.FECCapable {
		bits |= speedCapFEC
	}
	return bits
}

// Per-port `anomalies` bits (port_table), the values the Network 10.6 UI
// understands. Real switches send 0 when healthy; a missing key leaves the
// UI's Anomaly column blank.
const (
	anomSFPRxFault        = 1
	anomSFPTxFault        = 2
	anomSFPEEPROM         = 4
	anomLoopSTP           = 8
	anomSTPFlap           = 16
	anomTopologyFlap      = 32
	anomLinkFlap          = 64
	anomMCLAGNegotiation  = 128
	anomPoEBudget         = 256
	anomLoopKeepalive     = 512
	anomLowUplinkSpeed    = 1024
	anomBPDUGuard         = 2048
	anomTransmissionError = 4096
	anomDroppedTraffic    = 8192
	anomPortLock          = 32768
)
