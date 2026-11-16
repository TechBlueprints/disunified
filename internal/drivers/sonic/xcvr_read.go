package sonic

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// A module xcvrd has not read (docs/drivers/sonic.md §11.36). xcvrd on the
// DX010 misses a module inserted after boot: the platform says the cage is
// present and `sfputil show eeprom` reads it, but STATE_DB has no
// TRANSCEIVER_INFO, so the driver saw an empty cage -- no optic reported,
// and none of the module rules (the FEC and the layout a module calls for,
// §10.3, §10.9) could run. The driver reads such a cage through sfputil and
// uses the record exactly as it uses xcvrd's, which wins as soon as it
// exists. Read-only.

// sfpReadCommand runs sfpReadScript: the presence table, then the EEPROM of
// each candidate cage the table says is present.
const sfpReadCommand = "sudo bash -s -- disunified-sfp-read"

// The read's pace: once a minute while a cage is present and unread (its
// module may go, or change), every five minutes while none is (a module
// may arrive), never while xcvrd has a record for every cage.
const (
	sfpReadBusy = time.Minute
	sfpReadIdle = 5 * time.Minute
)

func sfpReadScript(ifaces []string) string {
	return `out=$(sfputil show presence 2>&1) || { echo "$out"; exit 1; }
echo "@@@ presence"
echo "$out"
for p in ` + strings.Join(ifaces, " ") + `; do
  if echo "$out" | grep -Eq "^$p +Present"; then
    echo "@@@ eeprom $p"
    sfputil show eeprom -p "$p" 2>&1
  fi
done
echo "@@@ end"
`
}

// sfpState is what the driver keeps of its own module reads.
type sfpState struct {
	next  time.Time                    // when the next read is due; zero = now
	reads map[string]map[string]string // first interface -> TRANSCEIVER_INFO-shaped fields
}

// unreadCages lists, per cage with no TRANSCEIVER_INFO under any of its
// interfaces, the cage's first interface (how SONiC keys a cage's module)
// and its port index.
func unreadCages(cfg, xcvr table) (ifaces []string, index map[string]int) {
	byIndex := map[int][]string{}
	for name, f := range cfg.byName("|") {
		if idx := atoi(f["index"]); idx > 0 {
			byIndex[idx] = append(byIndex[idx], name)
		}
	}
	have := xcvr.byName("|")
	index = map[string]int{}
	for idx, names := range byIndex {
		sort.Slice(names, func(i, j int) bool { return ifNum(names[i]) < ifNum(names[j]) })
		read := false
		for _, n := range names {
			if len(have[n]) > 0 {
				read = true
			}
		}
		if !read {
			ifaces = append(ifaces, names[0])
			index[names[0]] = idx
		}
	}
	sort.Slice(ifaces, func(i, j int) bool { return ifNum(ifaces[i]) < ifNum(ifaces[j]) })
	return ifaces, index
}

// sfpRefresh reads, when it is due, the cages xcvrd has no record for
// (out is the collect script's output of this cycle), and keeps what it
// read for build to merge.
func (c *Collector) sfpRefresh(ctx context.Context, out string, now time.Time) {
	sec := sections(out)
	cfg, err1 := parseTable("cfg_port", sec["cfg_port"])
	xcvr, err2 := parseTable("xcvr", sec["xcvr"])
	if err1 != nil || err2 != nil {
		return // build reports it
	}
	ifaces, index := unreadCages(cfg, xcvr)
	c.mu.Lock()
	for name := range c.sfp.reads {
		if _, unread := index[name]; !unread {
			delete(c.sfp.reads, name) // xcvrd has it now (or the cage is gone): its record wins
		}
	}
	due := !now.Before(c.sfp.next)
	c.mu.Unlock()
	if len(ifaces) == 0 || !due {
		return
	}
	text, err := c.run(ctx, sfpReadCommand, sfpReadScript(ifaces))
	psec := sections(text)
	if _, ok := psec["end"]; err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("the read did not run to completion: %s", firstLine(text))
		}
		c.warnOnce("sfp read", "sfputil could not be run (%v): a module xcvrd has not read stays unread (docs/drivers/sonic.md §11.36)", err)
		c.mu.Lock()
		c.sfp.next = now.Add(sfpReadIdle)
		c.mu.Unlock()
		return
	}
	reads := map[string]map[string]string{}
	for name, body := range psec {
		port, ok := strings.CutPrefix(name, "eeprom ")
		if !ok {
			continue
		}
		port = strings.TrimSpace(port)
		f, ok := parseSfputilEEPROM(body)
		if !ok {
			c.warnOnce("sfp unread "+port, "port %d (%s): module present but not read by xcvrd, and sfputil did not read it either (%s)", index[port], port, firstLine(body))
			continue
		}
		reads[port] = f
		if complianceName(f["specification_compliance"]) == "" {
			c.warnOnce("sfp vague "+port, "port %d (%s): module present but not read by xcvrd; read through sfputil, which names no compliance code for it, so the module rules (FEC, layout) cannot run", index[port], port)
		} else {
			c.warnOnce("sfp read "+port, "port %d (%s): module present but not read by xcvrd; read through sfputil (%s %s, %s)", index[port], port, strings.TrimSpace(f["manufacturer"]), strings.TrimSpace(f["model"]), complianceName(f["specification_compliance"]))
		}
	}
	c.mu.Lock()
	c.sfp.reads = reads
	if len(reads) > 0 {
		c.sfp.next = now.Add(sfpReadBusy)
	} else {
		c.sfp.next = now.Add(sfpReadIdle)
	}
	// A module that went (or that xcvrd reads later) may come back unread:
	// its line is then worth saying again.
	for key := range c.warned {
		if port, ok := strings.CutPrefix(key, "sfp read "); ok && reads[port] == nil {
			delete(c.warned, key)
		}
	}
	c.mu.Unlock()
}

// sfpMerge adds the driver's own module reads to xcvrd's table for the
// cages xcvrd has no record of. Called with c.mu held.
func (c *Collector) sfpMerge(xcvr table) {
	have := xcvr.byName("|")
	for name, f := range c.sfp.reads {
		if len(have[name]) == 0 {
			xcvr["TRANSCEIVER_INFO|"+name] = f
		}
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// sfputilFields maps `sfputil show eeprom`'s labels (sonic-utilities
// sfputil/main.py QSFP_DATA_MAP: the label is the human name of xcvrd's own
// field) to TRANSCEIVER_INFO's field names.
var sfputilFields = map[string]string{
	"Application Advertisement":        "application_advertisement",
	"Connector":                        "connector",
	"Encoding":                         "encoding",
	"Extended Identifier":              "ext_identifier",
	"Extended RateSelect Compliance":   "ext_rateselect_compliance",
	"Identifier":                       "type",
	"Nominal Bit Rate(100Mbs)":         "nominal_bit_rate",
	"Vendor Date Code(YYYY-MM-DD Lot)": "vendor_date",
	"Vendor Name":                      "manufacturer",
	"Vendor OUI":                       "vendor_oui",
	"Vendor PN":                        "model",
	"Vendor Rev":                       "vendor_rev",
	"Vendor SN":                        "serial",
}

// parseSfputilEEPROM reads one `sfputil show eeprom -p <port>` answer into
// TRANSCEIVER_INFO's fields: "<port>: SFP EEPROM detected", then one
// "Label: value" per line (values keep their padding, as xcvrd's do), the
// compliance codes as an indented block under "Specification compliance:",
// which becomes the dict text xcvrd stores (complianceName reads it).
// False when the EEPROM was not read ("SFP EEPROM not detected").
func parseSfputilEEPROM(body string) (map[string]string, bool) {
	lines := strings.Split(body, "\n")
	if len(lines) == 0 || !strings.HasSuffix(strings.TrimSpace(lines[0]), "SFP EEPROM detected") {
		return nil, false
	}
	f := map[string]string{}
	var codes []string
	inCodes, base := false, -1
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if base < 0 {
			base = indent
		}
		label, value, ok := strings.Cut(strings.TrimLeft(line, " \t"), ": ")
		if !ok {
			label, value = strings.TrimSuffix(strings.TrimSpace(line), ":"), ""
		}
		switch {
		case indent > base && inCodes:
			codes = append(codes, pyQuote(label)+": "+pyQuote(value))
		case label == "Specification compliance":
			inCodes = true
			if strings.TrimSpace(value) != "" { // an SFP's is one value, not a block
				f["specification_compliance"] = value
				inCodes = false
			}
		default:
			inCodes = false
			if strings.HasPrefix(label, "Length ") {
				f["cable_type"], f["cable_length"] = label, value
			} else if k, ok := sfputilFields[label]; ok {
				f[k] = value
			}
		}
	}
	if len(codes) > 0 {
		f["specification_compliance"] = "{" + strings.Join(codes, ", ") + "}"
	}
	return f, true
}

// pyQuote writes s as Python's repr of a str would, for parsePyDict.
func pyQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
