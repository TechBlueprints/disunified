package apcnmc

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// timeSection holds the card's clock: its fixed UTC offset (NTPTimeZone), its
// daylight saving choice (DSTConfiguration) and its NTP servers. Both cards
// on site spell it [SystemDate/Time] (AOS 3.9.2 and 6.0.1).
const timeSection = "SystemDate/Time"

// The DSTConfiguration values. Both cards' Daylight Saving form offers three
// choices (docs/fixtures/apc-aos-3.9.2/datentp.htm, apc-backups-05.3/):
// "Disable DST", "Traditional US DST (Second Sunday in March to First Sunday
// in November)" and a custom rule. config.ini does not use the form's radio
// values (None, Continental_United_States, Custom): both cards' files say
// Disabled, and an exported config.ini published on APC's community forum
// says Traditional US. The custom rule's config.ini keys are not captured,
// so no other rule is written.
const (
	dstDisabled    = "Disabled"
	dstTraditional = "Traditional US"
)

// cardZones are the offsets east of UTC, in minutes, that the card's Time
// Zone list offers -- the same 32 on both cards' Date/Time forms
// (dateman.htm). NTPTimeZone holds one of them; nothing else can be set.
var cardZones = []int{
	-720, -660, -600, -540, -480, -420, -360, -300, -240, -210, -180, -120, -60,
	0, 60, 120, 180, 210, 240, 270, 300, 330, 345, 360, 390, 420, 480, 540, 570, 600, 660, 720,
}

// TZRule is one end of a POSIX DST rule in its Mm.w.d form: month 1-12,
// week 1-5 (5 = the last), weekday 0-6 (0 = Sunday), and the local time of
// the change in seconds after midnight (02:00 when the rule names none).
type TZRule struct {
	Month, Week, Day int
	Time             int
}

// TZ is a POSIX TZ string taken apart. Offsets are POSIX's: seconds WEST of
// UTC, so MST7 is 25200 and the zone is UTC-07:00.
type TZ struct {
	Std, DST       string
	StdOff, DSTOff int
	HasDST         bool
	Start, End     TZRule
}

var (
	tzName   = regexp.MustCompile(`^(?:<[A-Za-z0-9+-]{3,}>|[A-Za-z]{3,})`)
	tzOffset = regexp.MustCompile(`^[+-]?\d{1,2}(?::\d{2}(?::\d{2})?)?`)
)

// ParseTZ reads a POSIX TZ string (std offset [dst [offset] ,start[/time],end[/time]]).
// Only the Mm.w.d rule form is read; a zone name (America/Denver), a Julian
// day rule (Jn, n) and a DST name without a rule -- whose rule POSIX leaves
// to the implementation -- are refused.
func ParseTZ(s string) (TZ, error) {
	var tz TZ
	rest := strings.TrimSpace(s)
	name := func() (string, bool) {
		m := tzName.FindString(rest)
		rest = rest[len(m):]
		return m, m != ""
	}
	offset := func() (int, bool) {
		m := tzOffset.FindString(rest)
		if m == "" {
			return 0, false
		}
		rest = rest[len(m):]
		v, ok := parseHMS(m)
		return v, ok && v >= -24*3600 && v <= 24*3600
	}
	var ok bool
	if tz.Std, ok = name(); !ok {
		return tz, fmt.Errorf("time zone %q is not a POSIX TZ string (std offset[dst[offset],rule])", s)
	}
	if tz.StdOff, ok = offset(); !ok {
		return tz, fmt.Errorf("time zone %q has no UTC offset after %q", s, tz.Std)
	}
	if rest == "" {
		return tz, nil
	}
	if tz.DST, ok = name(); !ok {
		return tz, fmt.Errorf("time zone %q: cannot read %q", s, rest)
	}
	tz.HasDST = true
	tz.DSTOff = tz.StdOff - 3600
	if rest != "" && rest[0] != ',' {
		if tz.DSTOff, ok = offset(); !ok {
			return tz, fmt.Errorf("time zone %q: cannot read the DST offset in %q", s, rest)
		}
	}
	if rest == "" {
		return tz, fmt.Errorf("time zone %q names daylight saving time but no rule for it", s)
	}
	parts := strings.Split(rest, ",")
	if len(parts) != 3 || parts[0] != "" {
		return tz, fmt.Errorf("time zone %q: the DST rule %q is not ,start,end", s, rest)
	}
	var err error
	if tz.Start, err = parseRule(parts[1]); err != nil {
		return tz, fmt.Errorf("time zone %q: %v", s, err)
	}
	if tz.End, err = parseRule(parts[2]); err != nil {
		return tz, fmt.Errorf("time zone %q: %v", s, err)
	}
	return tz, nil
}

func parseRule(r string) (TZRule, error) {
	date, at, hasTime := strings.Cut(r, "/")
	rule := TZRule{Time: 2 * 3600}
	if !strings.HasPrefix(date, "M") {
		return rule, fmt.Errorf("DST rule %q is not in the Mm.w.d form", r)
	}
	f := strings.Split(date[1:], ".")
	if len(f) != 3 {
		return rule, fmt.Errorf("DST rule %q is not in the Mm.w.d form", r)
	}
	var n [3]int
	for i, s := range f {
		v, err := strconv.Atoi(s)
		if err != nil {
			return rule, fmt.Errorf("DST rule %q is not in the Mm.w.d form", r)
		}
		n[i] = v
	}
	rule.Month, rule.Week, rule.Day = n[0], n[1], n[2]
	if rule.Month < 1 || rule.Month > 12 || rule.Week < 1 || rule.Week > 5 || rule.Day < 0 || rule.Day > 6 {
		return rule, fmt.Errorf("DST rule %q is out of range", r)
	}
	if hasTime {
		v, ok := parseHMS(at)
		if !ok || tzOffset.FindString(at) != at {
			return rule, fmt.Errorf("DST rule %q has no valid time", r)
		}
		rule.Time = v
	}
	return rule, nil
}

// parseHMS reads [+-]h[h][:mm[:ss]] as seconds.
func parseHMS(s string) (int, bool) {
	sign := 1
	switch {
	case strings.HasPrefix(s, "-"):
		sign, s = -1, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	f := strings.Split(s, ":")
	mult := []int{3600, 60, 1}
	v := 0
	for i, p := range f {
		if i > 2 || p == "" {
			return 0, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || (i > 0 && n > 59) {
			return 0, false
		}
		v += n * mult[i]
	}
	return sign * v, true
}

// usRule is the rule of the card's "Traditional US DST": the second Sunday in
// March to the first Sunday in November (the form's own words), the change at
// 02:00 local time as in the United States (the card's help: "as implemented
// in most areas of the United States").
var usStart, usEnd = TZRule{Month: 3, Week: 2, Day: 0, Time: 7200}, TZRule{Month: 11, Week: 1, Day: 0, Time: 7200}

// CardZone maps a POSIX TZ onto the card's two settings: NTPTimeZone (its
// fixed offset, "-07:00") and DSTConfiguration. Only what the card expresses
// exactly is mapped: an offset in its list, written as captured (negative
// offsets; the spelling of zero and positive ones is not captured), and
// either no DST or the US rule with a one-hour shift. Anything else is an
// error naming why.
func CardZone(tz TZ) (zone, dst string, err error) {
	if tz.StdOff%60 != 0 {
		return "", "", fmt.Errorf("the UTC offset of %s has seconds; the card's time zones are whole minutes", tz.Std)
	}
	east := -tz.StdOff / 60
	in := false
	for _, z := range cardZones {
		in = in || z == east
	}
	if !in {
		return "", "", fmt.Errorf("UTC%+03d:%02d (%s) is not in the card's time zone list", east/60, abs(east%60), tz.Std)
	}
	if east >= 0 {
		return "", "", fmt.Errorf("UTC+%02d:%02d (%s): how the card's config.ini spells a zero or positive offset is not captured", east/60, east%60, tz.Std)
	}
	zone = fmt.Sprintf("-%02d:%02d", -east/60, -east%60)
	if !tz.HasDST {
		return zone, dstDisabled, nil
	}
	if tz.StdOff-tz.DSTOff != 3600 {
		return "", "", fmt.Errorf("%s shifts the clock by %ds; the card shifts by one hour only", tz.DST, tz.StdOff-tz.DSTOff)
	}
	if tz.Start != usStart || tz.End != usEnd {
		return "", "", fmt.Errorf("the DST rule %s is not the card's Traditional US DST (M3.2.0/2 to M11.1.0/2); the card's custom DST keys are not captured", ruleString(tz))
	}
	return zone, dstTraditional, nil
}

func ruleString(tz TZ) string {
	r := func(x TZRule) string {
		return fmt.Sprintf("M%d.%d.%d/%02d:%02d", x.Month, x.Week, x.Day, x.Time/3600, x.Time%3600/60)
	}
	return r(tz.Start) + "," + r(tz.End)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// ntpHost is what the card's NTP server fields take: "the IP address or
// domain name" (its help page), at most 64 characters (the form's
// maxlength). A value goes into a config file line, so nothing else passes.
var ntpHost = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

func validNTPServer(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	return net.ParseIP(s) != nil || ntpHost.MatchString(s)
}

// Time is a card's [SystemDate/Time] as read from its config.ini.
type Time struct {
	Section string      // the name as the card spells it
	Keys    [][2]string // every key and value, in file order
}

// ParseTime finds the time section in a whole config.ini.
func ParseTime(cfg []byte) (Time, bool) {
	name, keys, ok := section(cfg, timeSection)
	return Time{Section: name, Keys: keys}, ok
}

func (t Time) get(k string) (string, bool) {
	for _, kv := range t.Keys {
		if strings.EqualFold(kv[0], k) {
			return kv[1], true
		}
	}
	return "", false
}

// String renders the section for a log line.
func (t Time) String() string {
	parts := make([]string, 0, len(t.Keys))
	for _, kv := range t.Keys {
		parts = append(parts, kv[0]+"="+kv[1])
	}
	return "[" + t.Section + "] " + strings.Join(parts, ", ")
}

// Want is what the card's section must hold for c: each key and value, only
// keys the card's own section has. notes says why a part of c is left alone
// (a zone the card cannot express, more NTP servers than it has fields, a
// key its file lacks); that part is then not in the result.
func (t Time) Want(c devicemodel.Clock) (want map[string]string, notes []string) {
	want = map[string]string{}
	need := func(part string, keys ...string) bool {
		for _, k := range keys {
			if _, ok := t.get(k); !ok {
				notes = append(notes, fmt.Sprintf("the card's [%s] has no %s key; its %s is left alone", t.Section, k, part))
				return false
			}
		}
		return true
	}
	if c.Timezone != "" {
		tz, err := ParseTZ(c.Timezone)
		var zone, dst string
		if err == nil {
			zone, dst, err = CardZone(tz)
		}
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("the controller's time zone %q: %v; the card's time zone and DST are left alone", c.Timezone, err))
		case need("time zone and DST", "NTPTimeZone", "DSTConfiguration"):
			want["NTPTimeZone"], want["DSTConfiguration"] = zone, dst
		}
	}
	if c.NTPServers != nil {
		var bad []string
		for _, s := range c.NTPServers {
			if !validNTPServer(s) {
				bad = append(bad, s)
			}
		}
		switch {
		case len(bad) > 0:
			notes = append(notes, fmt.Sprintf("NTP server %q is neither an IP address nor a host name; the card's NTP servers are left alone", bad))
		case len(c.NTPServers) > 2:
			notes = append(notes, fmt.Sprintf("the card has two NTP server fields, the controller pushed %d servers %v; the card's NTP servers are left alone", len(c.NTPServers), c.NTPServers))
		case len(c.NTPServers) == 0:
		case need("NTP servers", "NTPEnable", "NTPPrimaryServer", "NTPSecondaryServer"):
			want["NTPEnable"] = "enabled"
			want["NTPPrimaryServer"] = c.NTPServers[0]
			want["NTPSecondaryServer"] = "0.0.0.0"
			if len(c.NTPServers) == 2 {
				want["NTPSecondaryServer"] = c.NTPServers[1]
			}
			// AOS 6 takes NTP servers from DHCP unless this is on (the
			// form's "Override Manual NTP Settings"); AOS 3.9.2 has no key.
			if _, ok := t.get("NTPManualOverride"); ok {
				want["NTPManualOverride"] = "enabled"
			}
		}
	}
	return want, notes
}

// Diff is the keys of want whose value the card's section does not hold,
// sorted. Values are compared without regard to case: the card writes
// enabled/disabled in lower case.
func (t Time) Diff(want map[string]string) []string {
	var out []string
	for k, v := range want {
		if cur, _ := t.get(k); !strings.EqualFold(cur, v) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Render is the partial config.ini that sets keys to want's values, in the
// card's own spelling of each key and in file order.
func (t Time) Render(want map[string]string, keys []string) []byte {
	set := map[string]bool{}
	for _, k := range keys {
		set[strings.ToLower(k)] = true
	}
	var b strings.Builder
	b.WriteString("[" + t.Section + "]\r\n")
	for _, kv := range t.Keys {
		if set[strings.ToLower(kv[0])] {
			for k, v := range want {
				if strings.EqualFold(k, kv[0]) {
					b.WriteString(kv[0] + "=" + v + "\r\n")
				}
			}
		}
	}
	return []byte(b.String())
}

// Uploads keeps one card's config.ini uploads apart. The card drops an
// upload that arrives while it is still applying the last one, without a
// word, so the uploads that share a card -- the syslog and clock appliers'
// and the driver's own address upload -- take a turn here and wait out the
// settle time between theirs. Nil = no gate.
type Uploads struct {
	mu   sync.Mutex
	last time.Time
}

// Busy is why an upload Take refused was not made, for its caller's error.
const Busy = "the card is still applying a config.ini upload made under a minute ago and would drop another"

// Take reports whether an upload may go now, settle after the last one, and
// if so marks now as the last: the caller uploads straight after. A caller
// refused uploads nothing and tries again on a later cycle.
func (u *Uploads) Take(now time.Time, settle time.Duration) bool {
	if u == nil {
		return true
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.last.IsZero() && now.Sub(u.last) < settle {
		return false
	}
	u.last = now
	return true
}

// ClockApplier keeps one card's [SystemDate/Time] on the controller's time
// zone, DST rule and NTP servers, the way SyslogApplier keeps its syslog
// section: the section read once at Start (Load), a partial upload of only
// the keys that differ, and a read back of the whole file once the card has
// had time to apply it, so what it logs is what the card holds.
type ClockApplier struct {
	Get     func(context.Context) ([]byte, error) // the card's whole config.ini
	Put     func(context.Context, []byte) error   // a partial config.ini
	Logf    func(format string, args ...any)      // nil = silent
	Settle  time.Duration                         // 0 = DefaultSettle
	Now     func() time.Time                      // nil = time.Now
	Uploads *Uploads                              // shared with the card's other uploads

	mu       sync.Mutex
	cur      Time
	loaded   bool
	awaiting bool              // an upload has not been read back yet
	want     map[string]string // the values last uploaded
	at       time.Time
	misses   int
	refused  map[string]bool // refusals already logged
}

// Load takes the card's time section from its whole config.ini and logs it.
func (a *ClockApplier) Load(cfg []byte) bool {
	t, ok := ParseTime(cfg)
	a.mu.Lock()
	a.cur, a.loaded = t, ok
	a.mu.Unlock()
	if ok {
		a.logf("clock: the card's config.ini reads %s", t)
	} else {
		a.logf("clock: the card's config.ini has no [%s] section", timeSection)
	}
	return ok
}

// Apply makes the card's clock follow c. A diff against what the card was
// last read as; changed reports an upload.
func (a *ClockApplier) Apply(ctx context.Context, c devicemodel.Clock) (changed bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.loaded {
		a.refuse(fmt.Sprintf("the card's config.ini has no [%s] section; its clock is left alone", timeSection))
		return false, nil
	}
	if a.awaiting {
		if a.now().Sub(a.at) < a.settle() {
			return false, nil
		}
		b, err := a.Get(ctx)
		if err != nil {
			return false, fmt.Errorf("read config.ini back: %w", err)
		}
		if t, ok := ParseTime(b); ok {
			a.cur = t
		}
		a.awaiting = false
		if len(a.cur.Diff(a.want)) == 0 {
			a.misses = 0
			a.logf("clock: the card's config.ini now reads %s", a.cur)
		} else {
			a.misses++
			a.logf("clock: the card's config.ini still reads %s after the upload (%d of %d)", a.cur, a.misses, maxMisses)
		}
	}
	want, notes := a.cur.Want(c)
	for _, n := range notes {
		a.refuse(n)
	}
	keys := a.cur.Diff(want)
	if len(keys) == 0 {
		return false, nil
	}
	if !sameValues(want, a.want) {
		a.misses = 0
	}
	if a.misses >= maxMisses {
		a.refuse(fmt.Sprintf("the card did not take %v in %d uploads; not writing it again until the controller's settings change or the bridge restarts", want, maxMisses))
		return false, nil
	}
	if !a.Uploads.Take(a.now(), a.settle()) {
		return false, nil // another upload is still settling
	}
	body := a.cur.Render(want, keys)
	a.logf("clock: the card differs from the controller in %s; uploading %s", strings.Join(keys, ", "), strings.ReplaceAll(strings.TrimSpace(string(body)), "\r\n", " "))
	if err := a.Put(ctx, body); err != nil {
		return false, fmt.Errorf("upload config.ini: %w", err)
	}
	a.awaiting, a.want, a.at = true, want, a.now()
	return true, nil
}

func sameValues(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func (a *ClockApplier) refuse(msg string) {
	if a.refused == nil {
		a.refused = map[string]bool{}
	}
	if a.refused[msg] {
		return
	}
	a.refused[msg] = true
	a.logf("clock: %s", msg)
}

func (a *ClockApplier) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

func (a *ClockApplier) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *ClockApplier) settle() time.Duration {
	if a.Settle > 0 {
		return a.Settle
	}
	return DefaultSettle
}
