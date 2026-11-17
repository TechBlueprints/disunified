package aristaeos

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/fecpolicy"
)

// Forward error correction (docs/drivers/arista-eos.md §13).
//
// EOS 4.26.14M, captured on the 7160 (docs/fixtures/arista-eos-4.26.14M/
// fec-auto/): `error-correction encoding <reed-solomon|fire-code>` is
// additive -- a second encoding joins the first, and with both configured
// the lane ran Reed-Solomon; `no error-correction encoding <x>` takes one
// out (the last one out leaves the default); `no error-correction
// encoding` alone disables FEC and replaces every encoding; an encoding
// written over that replaces it; `default error-correction encoding` goes
// back to EOS's own choice for the module and speed. `show interfaces
// error-correction` reports, per interface, what is configured, what the
// interface can run with the module seated, and what it runs.
//
// A FEC the controller pushes ends with exactly that mode configured.
// Where the controller says auto (no FEC in the push), the mode is the
// standard's for the module at the port's speed (internal/fecpolicy),
// chosen within what the interface lists as available; the driver writes
// only when the configuration and EOS's own default do not already give
// it. Auto always means the preferred mode (Clint, 2026-11-17): a port
// on another mode is rewritten, link or no link, which drops a link whose
// far end stays on the other mode; a mode set in UniFi overrides auto.

// errCorr is one interface's row of `show interfaces error-correction`.
type errCorr struct {
	Status string `json:"errCorrEncodingStatus"` // disabled | reedSolomon | reedSolomon544 | fireCode

	CfgDisabled bool `json:"fecEncodingConfigDisabled"`
	CfgFC       bool `json:"fecEncodingConfigFireCodeEnabled"`
	CfgRS       bool `json:"fecEncodingConfigReedSolomonEnabled"`

	AvailFC bool `json:"fecEncodingFireCodeAvailable"`
	AvailRS bool `json:"fecEncodingReedSolomonAvailable"`
}

// fecRowsFrom is every interface's row with its configuration taken from
// the running config, where the driver reads every other setting it
// writes (the row's own fecEncodingConfig* flags say the same: fec-auto/).
func fecRowsFrom(fec showInterfacesErrorCorrection, rc showRunningConfig) map[string]errCorr {
	rows := make(map[string]errCorr, len(fec.Statuses))
	for name, row := range fec.Statuses {
		row.CfgDisabled, row.CfgFC, row.CfgRS = false, false, false
		if body := rc.Cmds["interface "+name]; body != nil {
			_, row.CfgDisabled = body.Cmds["no error-correction encoding"]
			_, row.CfgFC = body.Cmds["error-correction encoding fire-code"]
			_, row.CfgRS = body.Cmds["error-correction encoding reed-solomon"]
		}
		rows[name] = row
	}
	return rows
}

// configured is the row's configuration as the modes it names: nothing
// for EOS's default.
func (e errCorr) configured() []devicemodel.FEC {
	var out []devicemodel.FEC
	if e.CfgDisabled {
		out = append(out, devicemodel.FECDisabled)
	}
	if e.CfgFC {
		out = append(out, devicemodel.FECFC)
	}
	if e.CfgRS {
		out = append(out, devicemodel.FECRS)
	}
	return out
}

// available is what the interface can run: the encodings EOS lists, and
// none, which `show interfaces hardware` lists at every speed of a cage
// with a module ("disabled(10G(default),25G,40G(default),50G,100G)").
func (e errCorr) available() []devicemodel.FEC {
	out := []devicemodel.FEC{devicemodel.FECDisabled}
	if e.AvailFC {
		out = append(out, devicemodel.FECFC)
	}
	if e.AvailRS {
		out = append(out, devicemodel.FECRS)
	}
	return out
}

// only reports whether the configuration is exactly f.
func (e errCorr) only(f devicemodel.FEC) bool {
	c := e.configured()
	return len(c) == 1 && c[0] == f
}

// set records a written mode in the row, as the switch then reports it
// (fec-auto/: the configuration flags, and the operating mode following
// a configured one).
func (e errCorr) set(f devicemodel.FEC) errCorr {
	e.CfgDisabled, e.CfgFC, e.CfgRS = f == devicemodel.FECDisabled, f == devicemodel.FECFC, f == devicemodel.FECRS
	switch f {
	case devicemodel.FECRS:
		e.Status = "reedSolomon"
	case devicemodel.FECFC:
		e.Status = "fireCode"
	case devicemodel.FECDisabled:
		e.Status = "disabled"
	}
	return e
}

func fecEncoding(f devicemodel.FEC) string {
	switch f {
	case devicemodel.FECRS:
		return "reed-solomon"
	case devicemodel.FECFC:
		return "fire-code"
	}
	return ""
}

// fecSetCommands are the interface commands that leave exactly want
// configured on an interface whose row is e. An encoding is added before
// another is taken out, so the interface never passes through a third
// mode on the way.
func fecSetCommands(e errCorr, want devicemodel.FEC) []string {
	if e.only(want) {
		return nil
	}
	if want == devicemodel.FECDisabled {
		return []string{"no error-correction encoding"} // replaces every encoding
	}
	enc := fecEncoding(want)
	if enc == "" {
		return nil
	}
	var cmds []string
	have := false
	for _, c := range e.configured() {
		have = have || c == want
	}
	if !have {
		cmds = append(cmds, "error-correction encoding "+enc) // replaces a `no error-correction encoding`
	}
	for _, c := range e.configured() {
		if other := fecEncoding(c); other != "" && c != want {
			cmds = append(cmds, "no error-correction encoding "+other)
		}
	}
	return cmds
}

// showTransceiverEEPROM is `show interfaces transceiver eeprom`, reduced to
// the two compliance bytes of an SFF-8636 module: `Ethernet` is byte 131
// (bit 7 "Extended", bit 3 40GBASE-CR4, the others 40G and 10G optics and
// the 40G active cable) and `Extended` byte 192, the SFF-8024 extended
// specification compliance code. Keyed by cage ("Ethernet50").
type showTransceiverEEPROM struct {
	Interfaces map[string]struct {
		Fields struct {
			Compliance struct {
				Subfields map[string]struct {
					Value struct {
						Raw json.Number `json:"raw"`
					} `json:"value"`
				} `json:"subfields"`
			} `json:"Compliance"`
		} `json:"fields"`
	} `json:"interfaces"`
}

const eepromCmd = "show interfaces transceiver eeprom"

// moduleCodes are a cage's compliance bytes; ok is false when the read
// did not give them.
type moduleCodes struct {
	ethernet, extended int
	ok                 bool
}

func eepromCodes(e showTransceiverEEPROM) map[int]moduleCodes {
	out := map[int]moduleCodes{}
	for name, v := range e.Interfaces {
		slot, _, ok := parseEthName(name)
		if !ok {
			continue
		}
		sub := v.Fields.Compliance.Subfields
		eth, err1 := strconv.Atoi(sub["Ethernet"].Value.Raw.String())
		ext, err2 := strconv.Atoi(sub["Extended"].Value.Raw.String())
		if err1 != nil || err2 != nil {
			continue
		}
		out[slot] = moduleCodes{ethernet: eth, extended: ext, ok: true}
	}
	return out
}

// readEEPROM reads the modules' compliance codes. Not one of the startup
// commands: a switch or capture that cannot answer it loses only the
// cable's class (the media type still names the 100G optics), so a failure
// is logged once and the codes are left empty.
func (c *Collector) readEEPROM(ctx context.Context) map[int]moduleCodes {
	out, err := c.run(ctx, []string{eepromCmd})
	var e showTransceiverEEPROM
	if err == nil {
		err = decodeInto(out[0], eepromCmd, &e)
	}
	if err != nil {
		c.noteOnce(fmt.Sprintf("arista: the modules' EEPROM codes were not read (%v): FEC on auto goes by the media type alone", err))
		return nil
	}
	return eepromCodes(e)
}

// noteOnce logs a line the first time it is seen in this run.
func (c *Collector) noteOnce(line string) {
	c.mu.Lock()
	if c.noted == nil {
		c.noted = map[string]bool{}
	}
	seen := c.noted[line]
	c.noted[line] = true
	c.mu.Unlock()
	if !seen {
		c.logf("%s", line)
	}
}

// moduleMedium is the neutral medium of the module in a cage: by its
// EEPROM codes where they were read, else by EOS's media type for the two
// strings a capture of this switch shows (`100GBASE-CWDM4`,
// `100GBASE-CR4`: the latter names a copper assembly without its 25G
// class -- EOS prints it for a CA-25G-S breakout cable and a CA-25G-L
// one alike). Any other string is no opinion.
func moduleMedium(codes moduleCodes, mediaType string) fecpolicy.Medium {
	if codes.ok {
		switch {
		case codes.ethernet&0x80 != 0:
			if m := fecpolicy.FromSFF8024(codes.extended); m != fecpolicy.Unknown {
				return m
			}
		case codes.ethernet&0x08 != 0:
			return fecpolicy.Copper40G
		case codes.ethernet != 0:
			return fecpolicy.Other
		}
	}
	switch mediaType {
	case "100GBASE-CWDM4":
		return fecpolicy.CWDM4
	case "100GBASE-CR4":
		return fecpolicy.CopperCR4
	}
	return fecpolicy.Unknown
}

// interfaceTypes is each interface's media type as `show interfaces
// status` prints it (`100GBASE-CR4` for a copper assembly, which `show
// interfaces transceiver` leaves without one).
func interfaceTypes(ss showInterfacesStatusFull) map[string]string {
	out := make(map[string]string, len(ss.InterfaceStatuses))
	for name, st := range ss.InterfaceStatuses {
		out[name] = st.InterfaceType
	}
	return out
}

// fecInputs is what a plan needs to decide FEC, copied under c.mu.
type fecInputs struct {
	rows  map[string]errCorr
	types map[string]string // interface -> media type (interfaceTypes)
	codes map[int]moduleCodes
	// stale: the modules changed since the codes were read; the next
	// collect reads them again, and until then only the media type speaks.
	stale bool
}

// fecPlan is a port's FEC commands, per interface (a split cage's lanes
// each take their own: one lane's stale line is not written onto the
// others): for a pushed mode, the commands that leave exactly it
// configured; on auto (d.FEC nil), the standard's choice where the
// configuration and EOS's default do not already give it. cage is the
// front-panel cage the module sits in (the port's index, or a lane
// switch's cage). The auto writes are the driver's own, not the
// controller's: the controller-only plan leaves them out. want is the
// mode each written interface ends on.
func fecPlan(in fecInputs, p devicemodel.Port, d devicemodel.PortDesired, cage int, controllerOnly bool, note func(string, ...any)) (cmds map[string][]string, want map[string]devicemodel.FEC) {
	names := laneNames(p)
	put := func(n string, c []string, f devicemodel.FEC) {
		if len(c) == 0 {
			return
		}
		if cmds == nil {
			cmds, want = map[string][]string{}, map[string]devicemodel.FEC{}
		}
		cmds[n], want[n] = c, f
	}
	if d.FEC != nil {
		for _, n := range names {
			row, ok := in.rows[n]
			if !ok {
				put(n, fecCommands(p, d), *d.FEC) // no row: the single line the running config showed
				continue
			}
			if *d.FEC == devicemodel.FECDisabled && len(row.configured()) == 0 {
				continue // EOS's default, nothing configured to remove (as before)
			}
			put(n, fecSetCommands(row, *d.FEC), *d.FEC)
		}
		return cmds, want
	}
	if controllerOnly || p.Optic == nil || !p.Present {
		return nil, nil
	}
	speed := p.AdminSpeed
	if speed == 0 {
		speed = p.SpeedMbps
	}
	if speed == 0 && len(p.SpeedCaps) == 1 {
		speed = p.SpeedCaps[0] // a lane: the one speed its cage runs
	}
	if d.SpeedMbps > 0 && d.SpeedMbps != speed {
		return nil, nil // the speed is changing: what the interface offers is read at the new one, next cycle
	}
	codes := in.codes[cage]
	if in.stale {
		codes = moduleCodes{}
	}
	media := p.Optic.MediaType
	if media == "" {
		media = in.types[names[0]]
	}
	medium := moduleMedium(codes, media)
	if medium == fecpolicy.Unknown {
		note("port %d: the module %q is not one the FEC table knows; FEC left as it is (set it in the UI to choose one)", d.Index, media)
		return nil, nil
	}
	for _, n := range names {
		row, ok := in.rows[n]
		if !ok {
			continue
		}
		choice, ok, reason := fecpolicy.Choose(medium, speed, row.available())
		if !ok {
			if len(fecpolicy.Allowed(medium, speed)) == 0 {
				note("port %d: no FEC known for the module %q (%s) at %d Mb/s on %s; FEC left as it is (set it in the UI to choose one)", d.Index, media, medium, speed, n)
			} else {
				note("port %d: the module %q on %s: %s; FEC left as it is", d.Index, media, n, reason)
			}
			return nil, nil
		}
		running := fecFor(row.Status)
		switch {
		case row.only(choice):
			continue // configured as chosen
		case len(row.configured()) == 0 && running == choice:
			continue // EOS's own default is the choice: nothing to write
		}
		put(n, fecSetCommands(row, choice), choice)
	}
	return cmds, want
}

// opticSignature names the modules seated, to notice a swap between two
// reads of the EEPROM codes.
func opticSignature(ports []devicemodel.Port) string {
	var parts []string
	for _, p := range ports {
		if p.Optic != nil {
			parts = append(parts, strconv.Itoa(p.Index)+"="+p.Optic.MediaType+"/"+p.Optic.Serial)
		}
	}
	sort.Strings(parts)
	return fmt.Sprint(parts)
}
