// Package modulelayout says how a QSFP28 cage can be run for the module
// seated in it -- one port or a breakout, and at what speed -- from the
// module's own SFF-8636 compliance codes (docs/module-layout.md §1 is the
// table). It knows no vendor and no controller and does no I/O: a driver
// reads the two codes from its platform, asks Runs, and decides what to
// write; internal/fecpolicy is its FEC counterpart.
//
// The code numbers are SFF-8024 Table 4-4 and SFF-8636 byte 131 as
// sonic-platform-common lists them (sonic_xcvr/codes/public/sff8024.py
// EXT_SPEC_COMPLIANCE, sff8636.py ETHERNET_10_40G_COMPLIANCE).
package modulelayout

import "fmt"

// Module is an SFF-8636 module's two compliance bytes.
type Module struct {
	Ethernet int // byte 131: the 10/40G Ethernet compliance bits; bit 7 = "Extended"
	Extended int // byte 192: the SFF-8024 extended specification compliance code
}

// EthernetExtended is byte 131's bit 7: the extended code (byte 192) says
// what the module is.
const EthernetExtended = 0x80

// Layout is one way to run a cage: Ports interfaces of SpeedMbps each.
type Layout struct{ Ports, SpeedMbps int }

func (l Layout) String() string { return fmt.Sprintf("%dx%dG", l.Ports, l.SpeedMbps/1000) }

// The layouts the table uses.
var (
	L1x100 = Layout{1, 100000}
	L4x25  = Layout{4, 25000}
	L2x50  = Layout{2, 50000}
	L1x40  = Layout{1, 40000}
	L4x10  = Layout{4, 10000}
)

// byExtended: SFF-8024 extended specification compliance codes. A code
// that fits a straight cable and a breakout runs both (the EEPROM has no
// "far end broken out" field); the 25G/50G-only copper codes in a QSFP28
// cage are breakout cables.
var byExtended = map[int][]Layout{
	0x03: {L1x100}, // 100GBASE-LR4 or 25GBASE-LR
	0x04: {L1x100}, // 100GBASE-ER4 or 25GBASE-ER
	0x06: {L1x100}, // 100G CWDM4
	0x17: {L1x100}, // 100G CLR4
	0x20: {L1x100}, // 100G SWDM4
	0x21: {L1x100}, // 100G PAM4 BiDi
	0x25: {L1x100}, // 100GBASE-DR
	0x26: {L1x100}, // 100G-FR or 100GBASE-FR1
	0x27: {L1x100}, // 100G-LR or 100GBASE-LR1

	0x02: {L1x100, L4x25}, // 100GBASE-SR4 or 25GBASE-SR
	0x07: {L1x100, L4x25}, // 100G PSM4 Parallel SMF
	0x01: {L1x100, L4x25}, // 100G AOC (Active Optical Cable) or 25GAUI C2M AOC (BER 5x10^-5)
	0x08: {L1x100, L4x25}, // 100G ACC (Active Copper Cable) or 25GAUI C2M ACC (BER 5x10^-5)
	0x18: {L1x100, L4x25}, // 100G AOC or 25GAUI C2M AOC (BER 10^-12)
	0x19: {L1x100, L4x25}, // 100G ACC or 25GAUI C2M ACC (BER 10^-12)

	0x0B: {L1x100, L4x25, L2x50}, // 100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS

	0x0C: {L4x25, L2x50}, // 25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R
	0x0D: {L4x25, L2x50}, // 25GBASE-CR CA-25G-N or 50GBASE-CR2 with no FEC

	0x10: {L1x40}, // 40GBASE-ER4
	0x1F: {L1x40}, // 40G SWDM4

	0x12: {L1x40, L4x10}, // 40G PSM4 Parallel SMF

	0x11: {L4x10}, // 4 x 10GBASE-SR
}

// Byte 131's 10/40G bits.
const (
	Eth40GActiveCable = 0x01 // 40G Active Cable (XLPPI)
	Eth40GBASELR4     = 0x02
	Eth40GBASESR4     = 0x04
	Eth40GBASECR4     = 0x08
)

// Runs is what the module runs in a QSFP28 cage, its native layout first.
// ok is false for a module the table does not know. The extended code
// speaks when byte 131 says Extended and the table knows the code;
// otherwise the 10/40G bits do (a "40GBASE-CR4,Extended" module whose
// extended code names nothing is a 40G CR4).
func Runs(m Module) (layouts []Layout, ok bool) {
	if m.Ethernet&EthernetExtended != 0 {
		if l, ok := byExtended[m.Extended]; ok {
			return append([]Layout(nil), l...), true
		}
	}
	switch {
	case m.Ethernet&(Eth40GBASESR4|Eth40GBASECR4|Eth40GActiveCable) != 0:
		return []Layout{L1x40, L4x10}, true
	case m.Ethernet&Eth40GBASELR4 != 0:
		return []Layout{L1x40}, true
	}
	return nil, false
}

// Extended reports the module of an extended compliance code.
func Extended(code int) Module { return Module{Ethernet: EthernetExtended, Extended: code} }
