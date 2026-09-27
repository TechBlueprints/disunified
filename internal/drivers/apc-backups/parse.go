package apcbackups

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

// The card's pages are fixed HTML tables (no scripts that matter, apart
// from the load figures, which are written by inline scripts); they are
// read with expressions anchored on the field names and labels the pages
// carry, and the fixtures hold the real pages so a change in the card's
// layout breaks a test rather than a poll.

var (
	tagRe    = regexp.MustCompile(`<[^>]+>`)
	scriptRe = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>`)
	spaceRe  = regexp.MustCompile(`\s+`)
)

// text flattens HTML to its visible text, one space between cells.
func text(s string) string {
	s = scriptRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, " ", " ")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

// --- ulabout.htm: identity and ratings ---

type about struct {
	Model, SKU, Serial, Firmware, Made string
	ApparentVA, RealW                  int
	BatterySerial, BatteryChemistry    string
}

var aboutRe = regexp.MustCompile(`Model : (.*?) SKU : (.*?) Serial Number : (\S+) Firmware Revision : (.*?) Manufacture Date : (\S+) Apparent Power Rating : (\d+) VA Real Power Rating : (\d+) W(?: Battery Pack Serial Number : (\S+))?(?: Battery Chemistry : (.*?) Knowledge Base)?`)

func parseAbout(page string) about {
	m := aboutRe.FindStringSubmatch(text(page))
	if m == nil {
		return about{}
	}
	va, _ := strconv.Atoi(m[6])
	w, _ := strconv.Atoi(m[7])
	return about{Model: m[1], SKU: m[2], Serial: m[3], Firmware: m[4], Made: m[5], ApparentVA: va, RealW: w, BatterySerial: m[8], BatteryChemistry: strings.TrimSpace(m[9])}
}

// --- ulinput.htm: rated output voltage ---

var ratedRe = regexp.MustCompile(`Rated Output Voltage : (\d+) VAC`)

func parseRatedVoltage(page string) int {
	m := ratedRe.FindStringSubmatch(text(page))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// --- home.htm: status, battery, input, the outlet loads ---

type home struct {
	ChargePct    float64
	RuntimeMin   int
	InputV       float64
	HasInput     bool
	AlarmClass   string   // alarmGood, alarmWarning, alarmCritical (the card's CSS class on the alarm line)
	AlarmText    string   // "No Alarms Present", or the alarm summary
	Status       []string // the status list under the alarm line: "UPS is online.", ...
	BatteryLife  string   // "Battery OK", or what the card says
	OutletLoadsW []float64
	OutletNames  []string
	OutletStates []bool
}

var (
	chargeRe     = regexp.MustCompile(`([\d.]+)(?:&nbsp;|\s)*%(?:&nbsp;|\s)*</td>`)
	runtimeRe    = regexp.MustCompile(`(\d+)\s*minutes`)
	inputRe      = regexp.MustCompile(`([\d.]+)\s*(?:&nbsp;|\s)*<span id="langVAC">VAC</span>`)
	alarmRe      = regexp.MustCompile(`class="(alarm\w+)">.*?/>\s*(?:&nbsp;)*\s*([^<]*)<`)
	statusLiRe   = regexp.MustCompile(`<li>([^<]*)</li>`)
	battLifeRe   = regexp.MustCompile(`Battery Life Status</span>:</td>\s*<td >([^<&]*)`)
	outletLoadRe = regexp.MustCompile(`parseFloat\(([\d.]+)\+""\)`)
	outletRowRe  = regexp.MustCompile(`<td class="dataName">([^<]*)</td>\s*<td>(On|Off)</td>`)
)

func parseHome(page string) home {
	var h home
	if m := chargeRe.FindStringSubmatch(page); m != nil {
		h.ChargePct, _ = strconv.ParseFloat(m[1], 64)
	}
	if m := runtimeRe.FindStringSubmatch(page); m != nil {
		h.RuntimeMin, _ = strconv.Atoi(m[1])
	}
	if m := inputRe.FindStringSubmatch(page); m != nil {
		h.InputV, _ = strconv.ParseFloat(m[1], 64)
		h.HasInput = h.InputV > 0
	}
	if m := alarmRe.FindStringSubmatch(page); m != nil {
		h.AlarmClass = m[1]
		h.AlarmText = strings.TrimSpace(html.UnescapeString(m[2]))
	}
	for _, m := range statusLiRe.FindAllStringSubmatch(page, -1) {
		if s := strings.TrimSpace(html.UnescapeString(m[1])); s != "" {
			h.Status = append(h.Status, s)
		}
	}
	if m := battLifeRe.FindStringSubmatch(page); m != nil {
		h.BatteryLife = strings.TrimSpace(m[1])
	}
	for _, m := range outletLoadRe.FindAllStringSubmatch(page, -1) {
		w, _ := strconv.ParseFloat(m[1], 64)
		h.OutletLoadsW = append(h.OutletLoadsW, w)
	}
	for _, m := range outletRowRe.FindAllStringSubmatch(page, -1) {
		h.OutletNames = append(h.OutletNames, html.UnescapeString(m[1]))
		h.OutletStates = append(h.OutletStates, m[2] == "On")
	}
	return h
}

// --- uloutcfg2.htm: the outlets, with their kind, state, load and backup ---

// outletRow is one row of the Outlet Settings page. Kind is "MOG" (main
// outlet group: always on with the UPS, not switchable) or "SOG" (switched
// outlet group: the card's On/Off/Reboot form addresses it by N).
type outletRow struct {
	Kind          string
	N             int
	Name          string
	On            bool
	LoadW         float64
	BatteryBackup bool // a SOG can be configured surge-only; a MOG is always backed
	Master        bool
	Watchdog      bool
}

var outletCfgRe = regexp.MustCompile(`(?s)name="(MOG|SOG)(\d)Name"[^>]*value="([^"]*)"[^>]*/>\s*</td>\s*<td>(On|Off)</td>\s*<td>([\d.]+)(?:&nbsp;)?\s*</td>\s*<td>(.*?)</td>\s*<td>(.*?)</td>\s*<td>(.*?)</td>`)

func parseOutlets(page string) []outletRow {
	var out []outletRow
	for _, m := range outletCfgRe.FindAllStringSubmatch(page, -1) {
		n, _ := strconv.Atoi(m[2])
		w, _ := strconv.ParseFloat(m[5], 64)
		r := outletRow{Kind: m[1], N: n, Name: html.UnescapeString(m[3]), On: m[4] == "On", LoadW: w}
		r.BatteryBackup = yesNo(m[6])
		r.Master = strings.Contains(text(m[7]), "Master") || yesNo(m[7])
		r.Watchdog = yesNo(m[8])
		out = append(out, r)
	}
	return out
}

// yesNo reads a Yes/No cell, whether it is plain text (a MOG) or a select
// whose selected option is No (value 01000000) or Yes (00000000).
func yesNo(cell string) bool {
	if strings.Contains(cell, "<select") {
		for _, opt := range regexp.MustCompile(`<option[^>]*>`).FindAllString(cell, -1) {
			if strings.Contains(opt, "selected") {
				return !strings.Contains(opt, "01000000")
			}
		}
		return true // no option marked selected: the first (Yes) is shown
	}
	return strings.HasPrefix(strings.TrimSpace(text(cell)), "Yes")
}

// --- config.ini ---

// parseINI reads the card's config.ini into section -> key -> value,
// keeping values as written (quotes and all) so a caller can compare
// them with what the card would write back.
func parseINI(body string) map[string]map[string]string {
	out := map[string]map[string]string{}
	section := ""
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, ";") || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = t[1 : len(t)-1]
			if _, ok := out[section]; !ok {
				out[section] = map[string]string{}
			}
			continue
		}
		if k, v, ok := strings.Cut(t, "="); ok && section != "" {
			out[section][strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
