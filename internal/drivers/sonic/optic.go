package sonic

import (
	"fmt"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The module in a cage (docs/drivers/sonic.md §10.3, §10.4): its name as
// xcvrd prints it, and the FEC it calls for.

// complianceName names a module from TRANSCEIVER_INFO's
// specification_compliance. For an SFF-8636 module xcvrd stores Python's
// str() of a dict ("{'10/40G Ethernet Compliance Code': 'Extended', …,
// 'Extended Specification Compliance': '100G CWDM4'}"): the name is the
// extended code when the 10/40G code says Extended, else the 10/40G code;
// another dict (an SFP's codes) gives its first value that names
// something; a plain string is the name. "" when nothing does.
func complianceName(s string) string {
	s = strings.TrimSpace(s)
	d, ok := parsePyDict(s)
	if !ok {
		if known(s) && !vague(s) {
			return s
		}
		return ""
	}
	if code := d.m["10/40G Ethernet Compliance Code"]; strings.Contains(code, "Extended") {
		if ext := d.m["Extended Specification Compliance"]; known(ext) && !vague(ext) {
			return ext
		}
		if base, _, _ := strings.Cut(code, ","); base != "Extended" && known(base) {
			return base // "40GBASE-CR4,Extended"
		}
	} else if known(code) && !vague(code) {
		return code
	}
	for _, k := range d.keys {
		if v := d.m[k]; known(v) && !vague(v) {
			return v
		}
	}
	return ""
}

// vague: the values xcvrd prints for a code that names nothing.
func vague(s string) bool {
	switch strings.TrimSpace(s) {
	case "Unknown", "Unspecified", "Extended":
		return true
	}
	return false
}

// reprDict is a parsed Python dict literal of strings, keys in source order.
type reprDict struct {
	keys []string
	m    map[string]string
}

// parsePyDict reads the repr of a dict of strings: {'k': 'v', "k2": "v2"}
// (Python quotes with " when the text holds a '). False for anything else.
func parsePyDict(s string) (reprDict, bool) {
	p := pyParser{s: s}
	p.space()
	if !p.eat('{') {
		return reprDict{}, false
	}
	d := reprDict{m: map[string]string{}}
	p.space()
	if p.eat('}') {
		return d, p.end()
	}
	for {
		p.space()
		k, ok := p.str()
		if !ok {
			return reprDict{}, false
		}
		p.space()
		if !p.eat(':') {
			return reprDict{}, false
		}
		p.space()
		v, ok := p.str()
		if !ok {
			return reprDict{}, false
		}
		if _, dup := d.m[k]; !dup {
			d.keys = append(d.keys, k)
		}
		d.m[k] = v
		p.space()
		if p.eat('}') {
			return d, p.end()
		}
		if !p.eat(',') {
			return reprDict{}, false
		}
	}
}

type pyParser struct {
	s string
	i int
}

func (p *pyParser) space() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n') {
		p.i++
	}
}

func (p *pyParser) eat(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *pyParser) end() bool { p.space(); return p.i == len(p.s) }

// str reads one quoted string with Python's backslash escapes for \\, \',
// \" and \n; any other escape is kept as written.
func (p *pyParser) str() (string, bool) {
	if p.i >= len(p.s) || (p.s[p.i] != '\'' && p.s[p.i] != '"') {
		return "", false
	}
	q := p.s[p.i]
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == q:
			p.i++
			return b.String(), true
		case c == '\\' && p.i+1 < len(p.s):
			n := p.s[p.i+1]
			switch n {
			case '\\', '\'', '"':
				b.WriteByte(n)
			case 'n':
				b.WriteByte('\n')
			default:
				b.WriteByte(c)
				b.WriteByte(n)
			}
			p.i += 2
		default:
			b.WriteByte(c)
			p.i++
		}
	}
	return "", false
}

// moduleFEC is the FEC a module calls for at an interface speed (Mb/s), by
// its compliance name (sonic-platform-common 202505: sff8024.py
// EXT_SPEC_COMPLIANCE, sff8636.py ETHERNET_10_40G_COMPLIANCE) and what the
// IEEE clause or MSA for it requires of the host. False when the table
// does not know the pair: then nothing is written.
func moduleFEC(name string, speed int) (devicemodel.FEC, bool) {
	switch speed {
	case 40000, 10000:
		if name != "" {
			return devicemodel.FECDisabled, true // no RS-FEC below 25G a lane
		}
		return "", false
	case 100000:
		switch name {
		case "100G CWDM4", // verified live on the DX010 (2026-10-31)
			"100GBASE-SR4 or 25GBASE-SR",
			"100G PSM4 Parallel SMF",
			"100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS",
			"100G AOC (Active Optical Cable) or 25GAUI C2M AOC", // BER 5x10^-5
			"100G ACC (Active Copper Cable) or 25GAUI C2M ACC",  // BER 5x10^-5
			"100G SWDM4",
			"100G PAM4 BiDi":
			return devicemodel.FECRS, true
		case "100GBASE-LR4 or 25GBASE-LR",
			"100GBASE-ER4 or 25GBASE-ER",
			"100G AOC or 25GAUI C2M AOC", // BER 10^-12
			"100G ACC or 25GAUI C2M ACC": // BER 10^-12
			return devicemodel.FECDisabled, true
		}
	case 25000:
		switch name {
		case "100GBASE-SR4 or 25GBASE-SR",
			"100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS",
			"100G AOC (Active Optical Cable) or 25GAUI C2M AOC",
			"100G ACC (Active Copper Cable) or 25GAUI C2M ACC":
			return devicemodel.FECRS, true
		case "25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R":
			return devicemodel.FECFC, true
		case "25GBASE-CR CA-25G-N or 50GBASE-CR2 with no FEC",
			"100GBASE-LR4 or 25GBASE-LR",
			"100GBASE-ER4 or 25GBASE-ER",
			"100G AOC or 25GAUI C2M AOC",
			"100G ACC or 25GAUI C2M ACC":
			return devicemodel.FECDisabled, true
		}
	case 50000:
		switch name {
		case "100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS":
			return devicemodel.FECRS, true
		case "25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R":
			return devicemodel.FECFC, true
		case "25GBASE-CR CA-25G-N or 50GBASE-CR2 with no FEC":
			return devicemodel.FECDisabled, true
		}
	}
	return "", false
}

// laneCfg is one way to run a cage: split into ports interfaces of speed
// Mb/s each (1 x 100000 unsplit, 4 x 25000 a 4x25G breakout).
type laneCfg struct{ ports, speed int }

func (l laneCfg) String() string { return fmt.Sprintf("%dx%dG", l.ports, l.speed/1000) }

var (
	cfg1x100 = laneCfg{1, 100000}
	cfg4x25  = laneCfg{4, 25000}
	cfg2x50  = laneCfg{2, 50000}
	cfg1x40  = laneCfg{1, 40000}
	cfg4x10  = laneCfg{4, 10000}
)

// moduleRuns is what a module runs in a cage, its native configuration
// first (docs/drivers/sonic.md §10.9), by its compliance name. xcvrd's
// SFF-8636 info has no "far end broken out" field, so a code that fits both
// a straight cable and a breakout (CR4 / CA-25G-L, the AOC/ACC codes,
// SR4-or-25GBASE-SR) runs both; the 25G/50G-only codes in a QSFP28 cage are
// breakout cables. False for a module the table does not know.
func moduleRuns(name string) ([]laneCfg, bool) {
	switch name {
	case "100G CWDM4", "100GBASE-LR4 or 25GBASE-LR", "100GBASE-ER4 or 25GBASE-ER", "100G SWDM4",
		"100G PAM4 BiDi", "100G CLR4", "100GBASE-DR", "100G-FR or 100GBASE-FR1", "100G-LR or 100GBASE-LR1":
		return []laneCfg{cfg1x100}, true
	case "100GBASE-SR4 or 25GBASE-SR", "100G PSM4 Parallel SMF",
		"100G AOC (Active Optical Cable) or 25GAUI C2M AOC", "100G ACC (Active Copper Cable) or 25GAUI C2M ACC",
		"100G AOC or 25GAUI C2M AOC", "100G ACC or 25GAUI C2M ACC":
		return []laneCfg{cfg1x100, cfg4x25}, true
	case "100GBASE-CR4, 25GBASE-CR CA-25G-L or 50GBASE-CR2 with RS":
		return []laneCfg{cfg1x100, cfg4x25, cfg2x50}, true
	case "25GBASE-CR CA-25G-S or 50GBASE-CR2 with BASE-R", "25GBASE-CR CA-25G-N or 50GBASE-CR2 with no FEC":
		return []laneCfg{cfg4x25, cfg2x50}, true
	case "40GBASE-LR4", "40GBASE-ER4", "40G SWDM4":
		return []laneCfg{cfg1x40}, true
	case "40GBASE-SR4", "40GBASE-CR4", "40G Active Cable (XLPPI)", "40G PSM4 Parallel SMF":
		return []laneCfg{cfg1x40, cfg4x10}, true
	case "4 x 10GBASE-SR":
		return []laneCfg{cfg4x10}, true
	}
	return nil, false
}
