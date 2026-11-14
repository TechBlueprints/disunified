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
	swCapL3Routing     = 1
	swCapPortIsolation = 4
	swCapStormControl  = 8
	swCapLLDPMED       = 16
	swCapEgressRate    = 32 // EGRESS_RATE_LIMIT: switch.port.N egress rate limit
	swCapSTP           = 64
	swCapDHCPSnooping  = 128
	swCapIGMPSnooping  = 256
	swCapPortSecurity  = 512 // PORT_SECURITY: the per-port MAC allow list
	swCapLACP          = 8192
	swCapFEC           = 65536
	swCapJumbo         = 524288
	swCapLoopProtect   = 1048576  // KEEPALIVE_LOOP_PROTECTION: the UI's per-port Loop Protection (switch.port.N.keepalive)
	swCapBGP           = 33554432 // BGP: Settings -> Routing -> BGP offers the switch as a Device (switch.bgp.1.*)
	swCapIGMPAdvanced  = 67108864 // IGMP_SNOOPING_ADVANCED: the per-VLAN querier, timers and switch.multicast.* (docs/drivers/sonic.md §8.10)
)

// switch_caps.feature_caps2 (upstream unifi-emu's capability_bits.json:
// LINK_DEBOUNCE 1, ROUTES_OSPFV2 4, PORT_ROUTING 16; the ECS's own inform
// claims 128; the names POE_STAGING 32, TRUSTED_PORT 64 and
// MAC_TABLE_PRESSURE 128 are not captured). 4 and 16 verified live on the
// DX010 (docs/drivers/sonic.md §11.14, §11.16). Sent only when a bit is
// claimed, so no driver's payload changes without one.
const (
	swCap2OSPF        = 4  // OSPF: Settings -> Routing -> OSPF offers the switch as a Device (switch.ospf.*)
	swCap2PortRouting = 16 // PORT_ROUTING: the port editor's Routing and Routing + Aggregating operations
)

// sys_fan_mode.caps: the fan modes a device can be set to (DEFAULT 1,
// QUIET 2; upstream unifi-emu's capability_bits.json). The controller keeps
// the block for no switch (live on the DX010, docs/drivers/sonic.md §11.15;
// that it keeps it for a gateway is not captured); it is reported
// anyway because it is true, in the gateway's shape: caps and the current
// mode, which the UI reads as sys_fan_mode.mode.
const (
	sysFanModeDefault = 1
	sysFanModeQuiet   = 2
)

// sysFanMode is the inform's sys_fan_mode for a device whose driver can set
// its fan mode; nil when it can set none.
func sysFanMode(snap *devicemodel.Snapshot) map[string]any {
	if snap == nil || len(snap.System.FanModes) == 0 {
		return nil
	}
	caps := 0
	for _, m := range snap.System.FanModes {
		switch m {
		case devicemodel.FanModeDefault:
			caps |= sysFanModeDefault
		case devicemodel.FanModeQuiet:
			caps |= sysFanModeQuiet
		}
	}
	if caps == 0 {
		return nil
	}
	out := map[string]any{"caps": caps}
	if snap.System.FanMode != "" {
		out["mode"] = string(snap.System.FanMode)
	}
	return out
}

// switch_caps.vlan_caps
const (
	vlanCapAccess  = 2
	vlanCapTagging = 4
)

// switch_caps.stp_caps
const (
	stpCapBPDUGuard = 1
	stpCapAutoEdge  = 2 // Edge Port auto/enabled/disabled (switch.port.N.stp.edge)
	stpCapPortCost  = 8
)

// switch_caps.storm_control_caps
const stormCapInPercentage = 1

// igmp_snoop_caps: unknown multicast to the router ports (1), flooding the
// reserved groups (2), the query interval (4), fast leave per port (8) and
// one more bit without a name in the controller's bundle (16). 31 is what
// the ECS Aggregation, Ubiquiti's own SONiC switch, claims; a driver claims
// it with IGMPSnoopingAdvanced and logs a key it cannot honour.
const igmpSnoopCapsAdvanced = 1 | 2 | 4 | 8 | 16

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

// psu_table[].psu_caps (unifi-emu capability_bits.json: CHARGE_CTRL 1,
// POWER_METER 2, HOT_SWAPPABLE 4). The ECS Aggregation claims 4 alone,
// though its supplies meter their power, so that is all a supply here
// claims; what POWER_METER changes in the UI is not known.
const psuCapHotSwappable = 4

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

// OutletCount is how many outlet slots the controller draws for a claimed
// battery-backed model -- the pinned catalogue carries no outlets for these,
// so the counts come from Ubiquiti's fingerprint DB (standard + surge). 0 =
// pad nothing. A device reporting fewer rows than the picture has slots
// leaves the rest at the controller's default, which looks switchable; they
// are reported instead as present-but-off with no relay (see outletTable).
func OutletCount(model string) int {
	switch model {
	case "USWDA23", "USWDA24":
		return 10
	case "USWDA25", "USWDA26", "USPDA2B":
		return 8
	case "USPDA2C":
		return 9
	}
	return 0
}

// PlaceholderOutlets lists the controller-side outlet indices the claimed
// model draws but the device does not have: the slots below the index base
// (a USP-PDU-Pro's USB 1-4) and the slots past the device's last outlet up
// to the model's count (a UPS 2U's six spare positions). They are the rows
// outletTable reports present-but-off with no relay. The controller's own
// default for an outlet it has no override for is "on", so its picture
// shows them powered until an override says otherwise; the provisioner
// seeds those overrides off from this list. Nil when there are none.
func PlaceholderOutlets(model string, snap *devicemodel.Snapshot) []int {
	if snap == nil || len(snap.Outlets) == 0 {
		return nil
	}
	base := OutletIndexBase(model)
	highest := 0
	for _, o := range snap.Outlets {
		if i := o.Index + base - 1; i > highest {
			highest = i
		}
	}
	var out []int
	for i := 1; i < base; i++ {
		out = append(out, i)
	}
	for i := highest + 1; i <= OutletCount(model); i++ {
		out = append(out, i)
	}
	return out
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

// smart_power_caps says which power-device controls the controller may push
// (each bit unlocks a system_cfg key; unifi-emu's capability_bits.json names
// them). The UPS driver is read-only, so it claims none: a claimed bit whose
// push is then ignored is a silent lie in the UI.
const (
	SmartPowerCapNUTInfo      = 1  // nut_server block: claimed by the bridge's NUT server (control.nut)
	smartPowerCapACRecovery   = 2  // power_cycle_on_ac_recovery.*
	smartPowerCapBuzzer       = 4  // beep.status
	smartPowerCapSafeShutdown = 8  // safe-shutdown timing
	smartPowerCapEPO          = 16 // epo.status
	smartPowerCapInputTHD     = 64 // ac_input_thd.level
	SmartPowerCapsNone        = 0
)

// bms_run_anomaly bits: each set bit raises an operator-visible alert, so
// only bits the device can justify from a measurement are ever set.
//
// The four are one enum in unifi-emu's capability_bits.json, which files it
// under "unplaced" -- v0.5.5 recovered the bit names but could not tie them
// to a document field. This bridge's live captures are what place them on
// bms_run_anomaly, so the mapping is ours and is not in that file.
const (
	bmsAnomalyOverloadOver120  = 64
	bmsAnomalyOverload100to120 = 1024
	bmsAnomalyBatteryLow       = 4096
	// DISCHARGE_TIME_LIMIT, the fourth bit. Never set: the bridge has no
	// discharge-limit policy for a UPS to breach. Listed so the next
	// reader knows the bitmap is fully enumerated.
	bmsAnomalyDischargeTimeLimit = 16384
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
	set(c.PortSecurity, swCapPortSecurity)
	set(c.EgressRateLimit, swCapEgressRate)
	set(c.L3Routing, swCapL3Routing)
	set(c.LoopProtection, swCapLoopProtect)
	set(c.L3Routing && c.BGP, swCapBGP)
	set(c.IGMPSnooping && c.IGMPSnoopingAdvanced, swCapIGMPAdvanced)
	igmp := 0
	if c.IGMPSnooping && c.IGMPSnoopingAdvanced {
		igmp = igmpSnoopCapsAdvanced
	}
	stp := 0
	if c.BPDUGuard {
		stp |= stpCapBPDUGuard
	}
	if c.STPAutoEdge {
		stp |= stpCapAutoEdge
	}
	if c.STPPortCost {
		stp |= stpCapPortCost
	}
	storm := 0
	if c.StormControl {
		storm = stormCapInPercentage
	}
	caps := map[string]any{
		"feature_caps":           feat,
		"max_mirror_sessions":    c.MirrorSessions,
		"max_aggregate_sessions": c.AggregateSessions,
		"vlan_caps":              vlanCapAccess | vlanCapTagging | 1,
		"stp_caps":               stp,
		"storm_control_caps":     storm,
		"igmp_snoop_caps":        igmp,
	}
	// Custom ACLs have no feature bit: a UniFi switch claims them with
	// these counts (the ECS Aggregation 512 each, docs/drivers/sonic.md
	// §11.7.5). Sent only by a driver that honours some, so no other
	// driver's payload changes. Which count gates the controller's push is
	// recorded in §11.7.
	// Layer 3: the ECS Aggregation claims max_l3_intf 64 and
	// max_static_routes 256 beside the L3_ROUTING bit.
	if c.L3Routing {
		caps["max_l3_intf"] = c.L3Interfaces
		caps["max_static_routes"] = c.StaticRoutes
		feat2 := 0
		if c.OSPF {
			feat2 |= swCap2OSPF
		}
		if c.PortRouting {
			feat2 |= swCap2PortRouting
		}
		if feat2 != 0 {
			caps["feature_caps2"] = feat2
		}
	}
	if c.IPACLs > 0 || c.MACACLs > 0 {
		caps["max_custom_ip_acls"] = c.IPACLs
		caps["max_custom_mac_acls"] = c.MACACLs
	}
	return caps
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

// Per-port `anomalies` (port_table[].anomalies): conditions the switch
// itself detected. The controller's table for this field: 1 broadcast storm,
// 2 multicast storm, 4 unicast storm, 32 DHCP packet rate exceeded, 64 rogue
// DHCP server, 128 802.1X authentication failed, 256 MAC locking, 512 loop
// protection, 1024 DoS, 2048 PHY failure, 4096 PoE failure. Each set bit
// raises the matching alert (64 and 32 seen live 2026-11-13), so a bit is set
// only for a condition a driver really detected; nothing is mapped onto this
// table by a similar name. Real switches send 0 when healthy. The storm,
// DHCP-rate and rogue-DHCP bits follow the growth of the port's own drop
// counters (Port.Health) since the previous inform; no driver fills those
// counters yet.
//
// custom_anomalies is a different table (1/2/4 SFP rx/tx/EEPROM, 8 loop by
// STP, 16 STP flap, 32 topology flap, 64 link flap, ... 32768 port lock
// violation) and a different owner: the controller computes it per port from
// raw fields (link_down_count growth, sfp_found/sfp_rxfault/sfp_txfault) and
// stores it. The bridge does not send it.
const (
	portAnomalyBroadcastStorm = 1    // the port's storm control dropped broadcast frames
	portAnomalyMulticastStorm = 2    // ... multicast frames
	portAnomalyUnicastStorm   = 4    // ... unicast frames
	portAnomalyDHCPRate       = 32   // the port's DHCP rate limit dropped DHCP frames
	portAnomalyRogueDHCP      = 64   // DHCP guarding dropped server replies on the port
	portAnomalyDot1X          = 128  // a port shut down after failed 802.1X authentication
	portAnomalyMACLocking     = 256  // a port shut down for a MAC not on its allowed list
	portAnomalyLoopProtection = 512  // a port held down by loop protection
	portAnomalyDoS            = 1024 // a port shut down by the switch's denial-of-service protection
)
