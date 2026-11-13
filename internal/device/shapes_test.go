package device

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// The shape test (docs/wire-shapes.md §4.3): every value a driver's
// contract payload sends is held to the shapes of the values the site's real
// `usw` devices held on the controller (docs/fixtures/controller-10.6.109/
// shapes-usw.json, written by scripts/wire-shapes.py capture): no type a real
// device never had, no string longer than the longest real one, no new
// character class, no number negative, fractional or past 32 bits where no
// real one was. The contract tests compare keys and types; a typed store
// (Network 11's PostgreSQL) punishes values, which is how a 36-character
// serial halted a migration. The path rules and the comparison are
// scripts/wire-shapes.py's, line for line; keep the two identical.

// shapeExcusal names one difference: a path and the kind the script prints
// (type, longer, charset, number).
type shapeExcusal struct{ path, kind string }

// shapeExcusalsAll are differences every payload has against the reference
// because of where the reference comes from or what the tests feed in, not
// because of a value a bridge sends. One that excuses nothing on any driver
// fails.
var shapeExcusalsAll = map[shapeExcusal]string{
	{"uplink", "type"}:       "the reference is the controller's view (stat/device), whose uplink is its own object; every real inform sends the interface name as a string (CLAUDE.md §7, the uplink fix)",
	{"inform_url", "longer"}: "test input: the tests' controller URL (http://192.0.2.1:8080/inform) is one character longer than the site's; the controller sets this URL, as on a real device",
}

// Per driver.
var (
	excuseOptics = map[shapeExcusal]string{
		{"port_table[].sfp_current", "longer"}: "a 100G module's laser bias is near 40 mA (\"39.508\"); every module on the site's real switches draws under 10 mA. The format is the ECS's (three decimals)",
		{"port_table[].sfp_part", "longer"}:    "SFF-8636's vendor part number is a 16-character field and this module fills it; the longest on the site's real switches is 15",
	}
	excuseSonicPSU = map[shapeExcusal]string{
		{"psu_table[].part_no", "longer"}:    "the DX010's supplies name their part, as the ECS's own inform does; the site's real switches report theirs blank (docs/wire-shapes.md §2)",
		{"psu_table[].part_no", "charset"}:   "the DX010's supplies name their part, as the ECS's own inform does; the site's real switches report theirs blank",
		{"psu_table[].serial_no", "longer"}:  "the DX010's supplies report a serial, as the ECS's own inform does; the site's real switches report theirs blank",
		{"psu_table[].serial_no", "charset"}: "the DX010's supplies report a serial, as the ECS's own inform does; the site's real switches report theirs blank",
	}
	excuseTestNetmask = map[shapeExcusal]string{
		{"netmask", "longer"}: "test input: the test's 255.255.255.0 (the driver's netmask option) is longer than the site's mask; any dotted mask is one a real device sends",
	}
)

// shapeExcusals are each driver's differences that stay, with the reason. A
// difference not listed fails, and so does a listed one that no longer
// differs.
var shapeExcusals = map[string]map[shapeExcusal]string{
	"arista-eos": mergeShapeExcusals(excuseOptics, map[shapeExcusal]string{
		{"lldp_table[].local_port_name", "longer"}: "the EOS name of a breakout lane (Ethernet49/1, 12 characters); the ECS sends its own vendor names here too (Ethernet12), the longest on site is 10",
		{"psu_table[].part_no", "longer"}:          "the 7160's supplies name their part, as the ECS's own inform does; the site's real switches report theirs blank (docs/wire-shapes.md §2)",
		{"psu_table[].part_no", "charset"}:         "the 7160's supplies name their part, as the ECS's own inform does; the site's real switches report theirs blank",
		{"sys_stats.mem_buffer", "number"}:         "memory in bytes (3.3 GB of buffers in the capture) passes 2^31; no real switch on the site has that much (docs/wire-shapes.md §2)",
	}),
	"proxmox": {},
	"podman-macvlan": {
		{"sys_stats.mem_buffer", "number"}: "memory in bytes (3.5 GB of buffers and page cache in the 2026-11-13 capture) passes 2^31, as the Arista's does; Buffers plus Cached stays (docs/wire-shapes.md §8)",
	},
	"sonic":       {},
	"sonic-hw":    mergeShapeExcusals(excuseOptics, excuseSonicPSU),
	"sonic-l2":    mergeShapeExcusals(excuseOptics, excuseSonicPSU),
	"apc-ups":     excuseTestNetmask,
	"apc-backups": excuseTestNetmask,
}

func mergeShapeExcusals(ms ...map[shapeExcusal]string) map[shapeExcusal]string {
	out := map[shapeExcusal]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

var (
	shapeKeyID   = regexp.MustCompile(`^(?:[0-9a-f]{24}|[0-9]+|([0-9a-f]{2}:){5}[0-9a-f]{2})$`)
	shapePortN   = regexp.MustCompile(`_[0-9]+-`)
	shapeDigits  = regexp.MustCompile(`^[0-9]+$`)
	shapeDotted  = regexp.MustCompile(`^-?[0-9.]+$`)
	shapeMAC     = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)
	shapeHex     = regexp.MustCompile(`^[0-9a-f]+$`)
	shapeHEX     = regexp.MustCompile(`^[0-9A-F]+$`)
	shapeASCII   = regexp.MustCompile(`^[\x21-\x7e]*$`)
	shapeASCIISp = regexp.MustCompile(`^[\x20-\x7e]*$`)
)

const shapeMaxDepth = 8

// shapeClass is wire-shapes.py's cls().
func shapeClass(s string) string {
	switch {
	case s == "":
		return "empty"
	case shapeDigits.MatchString(s):
		return "digits"
	case shapeDotted.MatchString(s):
		return "dotted-num"
	case shapeMAC.MatchString(s):
		return "mac"
	case shapeHex.MatchString(s):
		return "hex"
	case shapeHEX.MatchString(s):
		return "HEX"
	case shapeASCII.MatchString(s):
		return "ascii"
	case shapeASCIISp.MatchString(s):
		return "ascii-space"
	}
	return "non-ascii"
}

// shapeType is wire-shapes.py's typ().
func shapeType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "num"
	case string:
		return "string"
	case []any:
		return "array"
	}
	return "object"
}

// shapeKey is wire-shapes.py's norm_key(): an id, an index or a MAC is
// <key>; port_16-rx_bytes is port_<n>-rx_bytes.
func shapeKey(k string) string {
	if shapeKeyID.MatchString(k) {
		return "<key>"
	}
	return shapePortN.ReplaceAllStringFunc(k, func(string) string { return "_<n>-" })
}

type shape struct {
	Types    map[string]bool
	StrLen   int
	Charset  map[string]bool
	Neg      bool
	Frac     bool
	GtInt32  bool
	examples []string // ours only: what to print when it differs
}

func newShape() *shape { return &shape{Types: map[string]bool{}, Charset: map[string]bool{}} }

// shapeWalk is wire-shapes.py's walk().
func shapeWalk(v any, path string, out map[string]*shape, depth int) {
	e := out[path]
	if e == nil {
		e = newShape()
		out[path] = e
	}
	t := shapeType(v)
	e.Types[t] = true
	switch t {
	case "string":
		s := v.(string)
		if n := utf8.RuneCountInString(s); n > e.StrLen { // Python's len() counts code points
			e.StrLen = n
		}
		e.Charset[shapeClass(s)] = true
		if len(e.examples) < 3 {
			e.examples = append(e.examples, s)
		}
	case "num":
		f := v.(float64)
		e.Neg = e.Neg || f < 0
		e.Frac = e.Frac || f != math.Trunc(f)
		e.GtInt32 = e.GtInt32 || math.Abs(f) > math.MaxInt32
		if len(e.examples) < 3 {
			e.examples = append(e.examples, fmt.Sprint(f))
		}
	}
	if depth >= shapeMaxDepth {
		return
	}
	switch x := v.(type) {
	case []any:
		for _, el := range x {
			shapeWalk(el, path+"[]", out, depth+1)
		}
	case map[string]any:
		for k, el := range x {
			p := shapeKey(k)
			if path != "" {
				p = path + "." + p
			}
			shapeWalk(el, p, out, depth+1)
		}
	}
}

func shapesOf(v any) map[string]*shape {
	out := map[string]*shape{}
	shapeWalk(v, "", out, 0)
	delete(out, "")
	return out
}

type shapeDiff struct{ path, kind, note string }

func setMinus(a map[string]bool, bs ...string) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for _, b := range bs {
		delete(out, b)
	}
	return out
}

func setMinusSet(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

func joinSet(m map[string]bool) string { return strings.Join(sortedKeys(m), "/") }

// shapeDifferences is wire-shapes.py's differences(): every path of mine
// that real devices hold differently. Paths no real device has are the
// contract tests' business.
func shapeDifferences(mine, ref map[string]*shape) []shapeDiff {
	var out []shapeDiff
	paths := make([]string, 0, len(mine))
	for p := range mine {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		m, r := mine[p], ref[p]
		if r == nil {
			continue
		}
		if extra := setMinus(setMinusSet(m.Types, r.Types), "null"); len(extra) > 0 {
			out = append(out, shapeDiff{p, "type", fmt.Sprintf("%s (real: %s)", joinSet(extra), joinSet(r.Types))})
			continue
		}
		if m.Types["string"] && r.Types["string"] {
			if m.StrLen > r.StrLen {
				out = append(out, shapeDiff{p, "longer", fmt.Sprintf("%d > %d", m.StrLen, r.StrLen)})
			}
			newCls := setMinus(setMinusSet(m.Charset, r.Charset), "empty")
			// a narrower class inside a wider one real devices use is no difference
			if r.Charset["ascii-space"] || r.Charset["non-ascii"] {
				newCls = setMinus(newCls, "ascii", "digits", "dotted-num", "hex", "HEX", "mac")
			}
			if r.Charset["non-ascii"] {
				newCls = setMinus(newCls, "ascii-space")
			}
			if r.Charset["ascii"] {
				newCls = setMinus(newCls, "digits", "dotted-num", "hex", "HEX", "mac")
			}
			if r.Charset["dotted-num"] || r.Charset["hex"] || r.Charset["HEX"] {
				newCls = setMinus(newCls, "digits")
			}
			if len(newCls) > 0 {
				out = append(out, shapeDiff{p, "charset", fmt.Sprintf("%s (real: %s)", joinSet(newCls), joinSet(r.Charset))})
			}
		}
		if m.Types["num"] && r.Types["num"] {
			for _, f := range []struct {
				mine, real bool
				word       string
			}{{m.Neg, r.Neg, "negative"}, {m.Frac, r.Frac, "fractional"}, {m.GtInt32, r.GtInt32, "past 32 bits"}} {
				if f.mine && !f.real {
					out = append(out, shapeDiff{p, "number", f.word + ", never on a real device"})
				}
			}
		}
	}
	return out
}

// loadShapes reads a manifest written by wire-shapes.py capture.
func loadShapes(t *testing.T, path string) map[string]*shape {
	t.Helper()
	m := loadFixtureJSON(t, path)
	paths, ok := m["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatalf("%s: no paths", path)
	}
	set := func(v any) map[string]bool {
		out := map[string]bool{}
		for _, x := range v.([]any) {
			out[x.(string)] = true
		}
		return out
	}
	out := map[string]*shape{}
	for p, v := range paths {
		e := v.(map[string]any)
		out[p] = &shape{
			Types:   set(e["types"]),
			StrLen:  int(e["strlen"].(float64)),
			Charset: set(e["charset"]),
			Neg:     e["neg"].(bool),
			Frac:    e["frac"].(bool),
			GtInt32: e["gt_int32"].(bool),
		}
	}
	return out
}

const shapesReference = "controller-10.6.109/shapes-usw.json"

func TestWireShapes(t *testing.T) {
	ref := loadShapes(t, shapesReference)
	known := map[string]bool{}
	usedAll := map[shapeExcusal]bool{}
	for _, d := range contractPayloads {
		known[d.driver] = true
		t.Run(d.driver, func(t *testing.T) {
			mine := shapesOf(d.build(t))
			excused := shapeExcusals[d.driver]
			used := map[shapeExcusal]bool{}
			var failed []string
			for _, diff := range shapeDifferences(mine, ref) {
				k := shapeExcusal{diff.path, diff.kind}
				if _, ok := excused[k]; ok {
					used[k] = true
					continue
				}
				if _, ok := shapeExcusalsAll[k]; ok {
					usedAll[k] = true
					continue
				}
				failed = append(failed, fmt.Sprintf("%-8s %-48s %s; ours e.g. %q", diff.kind, diff.path, diff.note, mine[diff.path].examples))
			}
			if len(failed) > 0 {
				t.Errorf("%s: values no real UniFi switch sends (fix them, or excuse them in shapeExcusals with a reason):\n  %s",
					d.driver, strings.Join(failed, "\n  "))
			}
			for k, why := range excused {
				if !used[k] {
					t.Errorf("%s: shape excusal %s (%s) excuses nothing (%s): drop it", d.driver, k.path, k.kind, why)
				}
			}
		})
	}
	for k, why := range shapeExcusalsAll {
		if !usedAll[k] {
			t.Errorf("shape excusal %s (%s) for every driver excuses nothing on any (%s): drop it", k.path, k.kind, why)
		}
	}
	for drv := range shapeExcusals {
		if !known[drv] {
			t.Errorf("shapeExcusals names %q, which has no contract payload", drv)
		}
	}
}

// The rules themselves, on the cases wire-shapes.py's regular expressions
// decide (no protocol values: classes and key forms only).
func TestWireShapesRules(t *testing.T) {
	for in, want := range map[string]string{
		"": "empty", "0123": "digits", "-1.64": "dotted-num", "4.26.14": "dotted-num",
		"aa:bb:cc:dd:ee:ff": "mac", "deadbeef": "hex", "DEADBEEF": "HEX", "AaBb": "ascii",
		"eth0 up": "ascii-space", "café": "non-ascii", "AA:BB:CC:DD:EE:FF": "ascii",
	} {
		if got := shapeClass(in); got != want {
			t.Errorf("shapeClass(%q) = %s, want %s", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"16": "<key>", "aa:bb:cc:dd:ee:ff": "<key>", "0123456789abcdef01234567": "<key>",
		"port_16-rx_bytes": "port_<n>-rx_bytes", "port_16_rx": "port_16_rx", "a_1-_2-": "a_<n>-_<n>-",
		"x_1_2-": "x_1_<n>-", "sfp_found": "sfp_found",
	} {
		if got := shapeKey(in); got != want {
			t.Errorf("shapeKey(%q) = %s, want %s", in, got, want)
		}
	}
	s := shapesOf(map[string]any{"a": []any{map[string]any{"b": -1.5}}, "big": float64(1 << 32), "n": "café"})
	if e := s["a[].b"]; e == nil || !e.Neg || !e.Frac || e.GtInt32 {
		t.Errorf("a[].b = %+v", e)
	}
	if e := s["big"]; e == nil || !e.GtInt32 {
		t.Errorf("big = %+v", e)
	}
	if e := s["n"]; e == nil || e.StrLen != 4 {
		t.Errorf("n = %+v: a string's length is its code points, as in Python", e)
	}
}
