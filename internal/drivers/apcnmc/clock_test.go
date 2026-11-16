package apcnmc

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// The two cards' Date/Time and Daylight Saving forms, captured read-only
// 2026-11-15 (the PDU's AOS 3.9.2, the Back-UPS card's AOS 6.0.1).
var forms = []string{"../../../docs/fixtures/apc-aos-3.9.2", "../../../docs/fixtures/apc-backups-05.3"}

// The power path's push with the site's time zone and NTP servers, captured
// from the rack PDU on Network 10.6.109.
const pduPush = "../../../docs/fixtures/controller-10.6.109/system_cfg-pdu-syslog-remote.txt"

// The ECS's push, which names its zone (America/Denver) where the power
// path's carries the POSIX string.
const ecsPush = "../../../docs/fixtures/controller-10.6.109/system_cfg-ecs-aggregation.txt"

func pushClock(t *testing.T, path string) devicemodel.Clock {
	t.Helper()
	c := unificfg.Parse(string(readFixture(t, path)))
	return devicemodel.Clock{Timezone: c.Timezone, NTPServers: c.NTPServers}
}

var zoneOption = regexp.MustCompile(`<option[^>]*>\s*(?:([+-]|\+/-| )?\s*(\d{1,2})(?::(\d{2}))? hours?)`)

// cardZones is the Time Zone list both cards' forms offer, offset for
// offset; the AOS 6 page renders a '+' as a space.
func TestCardZonesAreTheFormsList(t *testing.T) {
	for _, dir := range forms {
		page := string(readFixture(t, dir+"/dateman.htm"))
		sel := page[strings.Index(page, `name="timeZone"`):]
		sel = sel[:strings.Index(sel, "</select>")]
		var got []int
		for _, m := range zoneOption.FindAllStringSubmatch(sel, -1) {
			h, _ := strconv.Atoi(m[2])
			mm, _ := strconv.Atoi(m[3])
			v := h*60 + mm
			if m[1] == "-" {
				v = -v
			}
			got = append(got, v)
		}
		if fmt.Sprint(got) != fmt.Sprint(cardZones) {
			t.Errorf("%s: the form offers %v, cardZones is %v", dir, got, cardZones)
		}
	}
}

// Both forms offer exactly: no DST, the US rule (by its dates), and a custom
// rule that shifts by one hour.
func TestTheFormsDSTChoices(t *testing.T) {
	choice := regexp.MustCompile(`name="DSTSelectChoice" value="([^"]+)"`)
	for _, dir := range forms {
		page := string(readFixture(t, dir+"/datentp.htm"))
		var got []string
		for _, m := range choice.FindAllStringSubmatch(page, -1) {
			got = append(got, m[1])
		}
		if fmt.Sprint(got) != "[None Continental_United_States Custom]" {
			t.Errorf("%s: DST choices %v", dir, got)
		}
		for _, s := range []string{"Traditional US DST (Second Sunday in March to First Sunday in November)", "Adds 1 hour at start, subtracts 1 hour at end"} {
			if !strings.Contains(page, s) {
				t.Errorf("%s: the form does not say %q", dir, s)
			}
		}
	}
}

// The site's zone maps exactly onto the card: UTC-07:00 and the US rule.
func TestTheSitesZoneIsTheCardsMountainTimeWithUSDST(t *testing.T) {
	c := pushClock(t, pduPush)
	tz, err := ParseTZ(c.Timezone)
	if err != nil {
		t.Fatal(err)
	}
	if tz.Std != "MST" || tz.StdOff != 7*3600 || !tz.HasDST || tz.DST != "MDT" || tz.DSTOff != 6*3600 || tz.Start != usStart || tz.End != usEnd {
		t.Fatalf("ParseTZ(%q) = %+v", c.Timezone, tz)
	}
	zone, dst, err := CardZone(tz)
	if zone != "-07:00" || dst != "Traditional US" || err != nil {
		t.Errorf("CardZone = %q, %q, %v; want -07:00, Traditional US", zone, dst, err)
	}
	// The same zone with its times written out is the same rule.
	tz2, err := ParseTZ(c.Timezone[:strings.Index(c.Timezone, ",")] + ",M3.2.0/2,M11.1.0/02:00:00")
	if err != nil || tz2 != tz {
		t.Errorf("explicit times: %+v, %v", tz2, err)
	}
	// Without its DST part the zone is the card's Disabled.
	if zone, dst, err := CardZone(TZ{Std: "MST", StdOff: 7 * 3600}); zone != "-07:00" || dst != "Disabled" || err != nil {
		t.Errorf("no DST: %q, %q, %v", zone, dst, err)
	}
}

// What the card cannot say exactly is refused, never approximated: variants
// of the captured zone with another rule, shift or offset, and the zone name
// another push carries.
func TestZonesTheCardCannotExpressAreRefused(t *testing.T) {
	site := pushClock(t, pduPush).Timezone
	for _, tz := range []string{
		pushClock(t, ecsPush).Timezone,                   // a zone name
		strings.Replace(site, "M3.2.0", "M3.5.0", 1),     // another start
		strings.Replace(site, "M11.1.0", "M11.1.0/3", 1), // another end time
		strings.Replace(site, "MDT", "MDT5:30", 1),       // a 90-minute shift
		strings.Replace(site, "MST7", "MST7:20", 1),      // not in the card's list
		strings.Replace(site, "MST7", "MST-1", 1),        // positive: spelling not captured
		strings.Replace(site, ",M3.2.0,M11.1.0", "", 1),  // DST without a rule
		strings.Replace(site, "M3.2.0", "J60", 1),        // a Julian rule
		site + "\r\nNTPEnable=disabled",
	} {
		p, err := ParseTZ(tz)
		if err == nil {
			_, _, err = CardZone(p)
		}
		if err == nil {
			t.Errorf("%q: mapped, want a refusal", tz)
		}
	}
}

// The captured push against each card's own section: only the keys that
// differ, in the card's spelling and order. Both cards already hold
// UTC-07:00 and NTP on; the PDU has no NTPManualOverride key, the Back-UPS
// card's is already on.
func TestRenderWritesOnlyWhatDiffers(t *testing.T) {
	c := pushClock(t, pduPush)
	for _, tc := range []struct {
		cfg, want string
	}{
		{pduConfig, "[SystemDate/Time]\r\nDSTConfiguration=Traditional US\r\nNTPPrimaryServer=192.0.2.4\r\nNTPSecondaryServer=0.ubnt.pool.ntp.org\r\n"},
		{backupsConfig, "[SystemDate/Time]\r\nDSTConfiguration=Traditional US\r\nNTPPrimaryServer=192.0.2.4\r\nNTPSecondaryServer=0.ubnt.pool.ntp.org\r\n"},
	} {
		tm, ok := ParseTime(readFixture(t, tc.cfg))
		if !ok {
			t.Fatalf("%s: no time section", tc.cfg)
		}
		want, notes := tm.Want(c)
		if len(notes) != 0 {
			t.Errorf("%s: notes %v", tc.cfg, notes)
		}
		if got := string(tm.Render(want, tm.Diff(want))); got != tc.want {
			t.Errorf("%s: Render = %q, want %q", tc.cfg, got, tc.want)
		}
		// Once merged, nothing differs.
		after, _ := ParseTime(Merge(readFixture(t, tc.cfg), tm.Render(want, tm.Diff(want))))
		if d := after.Diff(want); len(d) != 0 {
			t.Errorf("%s: after the upload %v still differ", tc.cfg, d)
		}
	}
	// The Back-UPS card's override is switched on with the servers.
	off, _ := ParseTime(Merge(readFixture(t, backupsConfig), []byte("[SystemDate/Time]\r\nNTPManualOverride=disabled\r\n")))
	want, _ := off.Want(c)
	if got := off.Diff(want); !contains(got, "NTPManualOverride") {
		t.Errorf("override off: diff %v, want NTPManualOverride in it", got)
	}
	// One server empties the second field.
	pdu, _ := ParseTime(readFixture(t, pduConfig))
	one, _ := pdu.Want(devicemodel.Clock{NTPServers: c.NTPServers[:1]})
	if one["NTPSecondaryServer"] != "0.0.0.0" {
		t.Errorf("one server: %v", one)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// A part the card cannot take is left alone and said once; the other part
// still applies.
func TestWantLeavesARefusedPartAlone(t *testing.T) {
	c := pushClock(t, pduPush)
	pdu, _ := ParseTime(readFixture(t, pduConfig))
	three := devicemodel.Clock{Timezone: c.Timezone, NTPServers: append(append([]string(nil), c.NTPServers...), c.NTPServers[0])}
	want, notes := pdu.Want(three)
	if _, ok := want["NTPPrimaryServer"]; ok || len(notes) != 1 || want["DSTConfiguration"] != "Traditional US" {
		t.Errorf("three servers: want %v, notes %v", want, notes)
	}
	bad := devicemodel.Clock{Timezone: c.Timezone, NTPServers: []string{c.NTPServers[0] + "\r\nNTPEnable=disabled"}}
	if want, notes := pdu.Want(bad); want["NTPPrimaryServer"] != "" || len(notes) != 1 {
		t.Errorf("injected server: want %v, notes %v", want, notes)
	}
	named := devicemodel.Clock{Timezone: pushClock(t, ecsPush).Timezone, NTPServers: c.NTPServers}
	if want, notes := pdu.Want(named); want["NTPTimeZone"] != "" || want["NTPPrimaryServer"] == "" || len(notes) != 1 {
		t.Errorf("zone name: want %v, notes %v", want, notes)
	}
	// A card whose section lacks the key is not given it.
	noDST, _ := ParseTime([]byte(strings.Replace(string(readFixture(t, pduConfig)), "DSTConfiguration=Disabled", "", 1)))
	if want, notes := noDST.Want(c); want["NTPTimeZone"] != "" || len(notes) != 1 {
		t.Errorf("no DSTConfiguration key: want %v, notes %v", want, notes)
	}
}

func (c *card) clockApplier(now *time.Time, logs *[]string, up *Uploads) *ClockApplier {
	s := c.applier(now, logs)
	return &ClockApplier{Get: s.Get, Put: s.Put, Logf: s.Logf, Now: s.Now, Uploads: up}
}

// One upload, nothing while the card settles, one read back, then nothing:
// the loop calls Apply every cycle.
func TestClockApplierUploadsOnceAndReadsBack(t *testing.T) {
	ctx := context.Background()
	c := &card{cfg: readFixture(t, pduConfig), take: true}
	now, logs := time.Unix(0, 0), []string{}
	a := c.clockApplier(&now, &logs, nil)
	if !a.Load(c.cfg) {
		t.Fatal("Load found no time section")
	}
	clk := pushClock(t, pduPush)
	if changed, err := a.Apply(ctx, clk); !changed || err != nil {
		t.Fatalf("first Apply = %v, %v", changed, err)
	}
	for _, step := range []time.Duration{0, 30 * time.Second, DefaultSettle, time.Hour} {
		now = now.Add(step)
		if changed, err := a.Apply(ctx, clk); changed || err != nil {
			t.Errorf("Apply after %v = %v, %v; want nothing to do", step, changed, err)
		}
	}
	if c.puts != 1 || c.gets != 1 {
		t.Errorf("puts %d gets %d, want 1 and 1", c.puts, c.gets)
	}
	if last := logs[len(logs)-1]; !strings.Contains(last, "now reads") || !strings.Contains(last, "DSTConfiguration=Traditional US") {
		t.Errorf("last log %q", last)
	}
}

// A card that does not take the upload is written three times, then left
// alone until the controller's settings change.
func TestClockApplierStopsAfterThreeMisses(t *testing.T) {
	ctx := context.Background()
	c := &card{cfg: readFixture(t, pduConfig)}
	now, logs := time.Unix(0, 0), []string{}
	a := c.clockApplier(&now, &logs, nil)
	a.Load(c.cfg)
	clk := pushClock(t, pduPush)
	for i := 0; i < 10; i++ {
		a.Apply(ctx, clk)
		now = now.Add(DefaultSettle)
	}
	if c.puts != maxMisses {
		t.Errorf("puts = %d, want %d", c.puts, maxMisses)
	}
	clk.NTPServers = clk.NTPServers[:1]
	if changed, _ := a.Apply(ctx, clk); !changed {
		t.Error("new settings from the controller: want another upload")
	}
}

// The syslog and clock appliers of one card take turns: the second upload
// waits out the first's settle time, as the card drops one that lands
// while it is still applying the last.
func TestAppliersOnOneCardTakeTurns(t *testing.T) {
	ctx := context.Background()
	c := &card{cfg: readFixture(t, pduConfig), take: true}
	now, logs := time.Unix(0, 0), []string{}
	up := &Uploads{}
	s := c.applier(&now, &logs)
	s.Uploads = up
	k := c.clockApplier(&now, &logs, up)
	s.Load(c.cfg)
	k.Load(c.cfg)
	clk := pushClock(t, pduPush)
	if changed, _ := s.Apply(ctx, []string{"192.0.2.40"}); !changed {
		t.Fatal("syslog: want an upload")
	}
	now = now.Add(DefaultSettle - time.Second)
	if changed, _ := k.Apply(ctx, clk); changed {
		t.Error("clock uploaded inside the syslog upload's settle time")
	}
	now = now.Add(time.Second)
	if changed, _ := k.Apply(ctx, clk); !changed {
		t.Error("clock: want an upload once the syslog one settled")
	}
	now = now.Add(DefaultSettle)
	s.Apply(ctx, []string{"192.0.2.40"})
	k.Apply(ctx, clk)
	if c.puts != 2 {
		t.Errorf("puts = %d, want 2", c.puts)
	}
	tm, _ := ParseTime(c.cfg)
	sl, _ := ParseSyslog(c.cfg)
	if v, _ := tm.get("DSTConfiguration"); v != "Traditional US" || fmt.Sprint(sl.Hosts()) != "[192.0.2.40]" {
		t.Errorf("the card holds DST %q, syslog %v", v, sl.Hosts())
	}
}
