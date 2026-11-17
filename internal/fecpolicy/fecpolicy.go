// Package fecpolicy says which forward error correction a link may run, by
// what is plugged in and the speed it runs at, as the standards define it
// (docs/fec-by-standard.md §1 is the table and its sources). It knows no
// vendor and no controller: a driver names the medium from what its
// platform reads out of the module, asks for the modes the standard
// permits, and chooses within what its port can do.
package fecpolicy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Medium is a class of module or cable assembly, as far as FEC goes. One
// EEPROM code often covers a 100G four-lane use and a 25G single-lane one
// ("100GBASE-SR4 or 25GBASE-SR"): the medium is the code's class and the
// speed passed to Allowed picks the use.
type Medium int

const (
	// Unknown: nothing the table can speak for. No opinion at any speed.
	Unknown Medium = iota
	// CopperCAN: a passive copper assembly of class CA-25G-N (SFF-8024
	// 0Dh: 25GBASE-CR CA-25G-N, or 50GBASE-CR2 with no FEC).
	CopperCAN
	// CopperCAS: class CA-25G-S (0Ch: 25GBASE-CR CA-25G-S, or 50GBASE-CR2
	// with BASE-R FEC).
	CopperCAS
	// CopperCAL: class CA-25G-L (0Bh: 100GBASE-CR4, 25GBASE-CR CA-25G-L,
	// or 50GBASE-CR2 with RS-FEC).
	CopperCAL
	// CopperCR4: a 100GBASE-CR4 assembly whose 25G class is not known (a
	// platform that names the cable but not its code).
	CopperCR4
	// Copper40G: a 40GBASE-CR4 assembly.
	Copper40G
	// SR, LR, ER: 100GBASE-SR4 / 25GBASE-SR (02h), 100GBASE-LR4 /
	// 25GBASE-LR (03h), 100GBASE-ER4 / 25GBASE-ER (04h).
	SR
	LR
	ER
	// ActiveCable5e5: an active optical or copper cable with a worst BER
	// of 5e-5 (01h, 08h: 100G AOC/ACC or 25GAUI C2M).
	ActiveCable5e5
	// ActiveCable1e12: the same at a BER of 1e-12 or better (18h, 19h).
	ActiveCable1e12
	// The 100G-only optics the table speaks for.
	CWDM4
	PSM4
	SWDM4
	PAM4BiDi
	// Other: a module that is present and named, with no use at 25G a
	// lane or above that the table speaks for: 40G and 10G optics and
	// cables, and the 100G codes the table leaves open (CLR4: optional).
	// It has an answer only at 40G and 10G, where no optic runs a FEC.
	Other
)

func (m Medium) String() string {
	switch m {
	case CopperCAN:
		return "25G copper CA-25G-N"
	case CopperCAS:
		return "25G copper CA-25G-S"
	case CopperCAL:
		return "copper CA-25G-L / 100GBASE-CR4"
	case CopperCR4:
		return "100GBASE-CR4 copper (25G class unknown)"
	case Copper40G:
		return "40GBASE-CR4 copper"
	case SR:
		return "100GBASE-SR4 / 25GBASE-SR"
	case LR:
		return "100GBASE-LR4 / 25GBASE-LR"
	case ER:
		return "100GBASE-ER4 / 25GBASE-ER"
	case ActiveCable5e5:
		return "active cable, BER 5e-5"
	case ActiveCable1e12:
		return "active cable, BER 1e-12"
	case CWDM4:
		return "100G CWDM4"
	case PSM4:
		return "100G PSM4"
	case SWDM4:
		return "100G SWDM4"
	case PAM4BiDi:
		return "100G PAM4 BiDi"
	case Other:
		return "a module with no 25G-a-lane rating"
	}
	return "unknown"
}

const (
	none = devicemodel.FECDisabled
	fc   = devicemodel.FECFC // BASE-R, IEEE 802.3 Clause 74
	rs   = devicemodel.FECRS // RS(528,514): Clause 91 at 100G, Clause 108 at 25G
)

func (m Medium) copper() bool {
	switch m {
	case CopperCAN, CopperCAS, CopperCAL, CopperCR4, Copper40G:
		return true
	}
	return false
}

// Allowed is the set of FEC modes the standard permits for m at an
// interface speed (Mb/s), the mode the medium is specified for first. Nil
// means the table has no answer for the pair -- an unknown medium, a
// speed the medium is not rated for, a speed below 10G -- and a caller
// then writes nothing.
//
// The rows are docs/fec-by-standard.md §1:
//
//   - 10G: no FEC is defined for a pluggable.
//   - 40G: RS does not exist; FC is optional on copper (CR4, by
//     auto-negotiation), nothing on optics.
//   - 25G copper, by the cable's class (IEEE 802.3 clause 110): CA-N
//     runs with none, FC or RS; CA-S with FC or RS; CA-L with RS only.
//   - 25G SR, LR, ER and an active cable at BER 5e-5: RS required, no FC
//     is defined for optics. An active cable at 1e-12 needs none (RS over
//     it is still a 25GAUI the standard defines: second, for a port that
//     cannot run without FEC).
//   - 50G on two 25G lanes (25G/50G Consortium): by the cable, as at 25G.
//   - 100G on four 25G lanes: RS for CR4, SR4, CWDM4, PSM4, SWDM4, PAM4
//     BiDi and a 5e-5 active cable; none for LR4 and ER4; FC is not
//     defined.
func Allowed(m Medium, speedMbps int) []devicemodel.FEC {
	if m == Unknown {
		return nil
	}
	switch speedMbps {
	case 10000:
		return []devicemodel.FEC{none}
	case 40000:
		if m.copper() {
			return []devicemodel.FEC{none, fc}
		}
		return []devicemodel.FEC{none}
	case 25000:
		switch m {
		case CopperCAN:
			return []devicemodel.FEC{none, fc, rs}
		case CopperCAS:
			return []devicemodel.FEC{fc, rs}
		case CopperCAL, SR, LR, ER, ActiveCable5e5:
			return []devicemodel.FEC{rs}
		case ActiveCable1e12:
			return []devicemodel.FEC{none, rs}
		}
	case 50000:
		switch m {
		case CopperCAN:
			return []devicemodel.FEC{none, fc, rs}
		case CopperCAS:
			return []devicemodel.FEC{fc, rs}
		case CopperCAL:
			return []devicemodel.FEC{rs}
		}
	case 100000:
		switch m {
		case CopperCAL, CopperCR4, SR, CWDM4, PSM4, SWDM4, PAM4BiDi, ActiveCable5e5:
			return []devicemodel.FEC{rs}
		case LR, ER:
			return []devicemodel.FEC{none}
		case ActiveCable1e12:
			return []devicemodel.FEC{none, rs}
		}
	}
	return nil
}

// Choose picks the FEC for m at the speed on a port that can run the
// modes in supported: the first allowed mode the port supports. A nil
// supported means the port's list is not known, and the first allowed
// mode is the choice. ok is false, with the reason, when the table has no
// answer or when no allowed mode is one the port can run: then the pair
// cannot link as the standard defines it and nothing should be written.
func Choose(m Medium, speedMbps int, supported []devicemodel.FEC) (f devicemodel.FEC, ok bool, reason string) {
	allowed := Allowed(m, speedMbps)
	if len(allowed) == 0 {
		return "", false, fmt.Sprintf("no FEC is known for %s at %d Mb/s", m, speedMbps)
	}
	if supported == nil {
		return allowed[0], true, ""
	}
	for _, a := range allowed {
		if slices.Contains(supported, a) {
			return a, true, ""
		}
	}
	return "", false, fmt.Sprintf("%s at %d Mb/s runs only with FEC %s by the standard and the port supports %s: the two cannot link within the standard",
		m, speedMbps, Names(allowed), Names(supported))
}

// Names prints a list of modes ("fc, rs"; "none" for disabled).
func Names(fs []devicemodel.FEC) string {
	if len(fs) == 0 {
		return "nothing"
	}
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = Name(f)
	}
	return strings.Join(out, ", ")
}

// Name is a mode's short name: rs, fc, none.
func Name(f devicemodel.FEC) string {
	switch f {
	case rs:
		return "rs"
	case fc:
		return "fc"
	case none:
		return "none"
	}
	return "unknown"
}

// FromSFF8024 is the medium of an SFF-8024 extended specification
// compliance code (Table 4-4; the byte a QSFP28 keeps at 192 and an SFP28
// at 36), for the codes the table speaks for. Other for any other code
// that names something; Unknown for 0 (unspecified).
func FromSFF8024(code int) Medium {
	switch code {
	case 0x00:
		return Unknown
	case 0x01, 0x08:
		return ActiveCable5e5
	case 0x02:
		return SR
	case 0x03:
		return LR
	case 0x04:
		return ER
	case 0x06:
		return CWDM4
	case 0x07:
		return PSM4
	case 0x0B:
		return CopperCAL
	case 0x0C:
		return CopperCAS
	case 0x0D:
		return CopperCAN
	case 0x18, 0x19:
		return ActiveCable1e12
	case 0x20:
		return SWDM4
	case 0x21:
		return PAM4BiDi
	}
	return Other
}
