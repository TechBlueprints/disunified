package apcpdu

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/apcnmc"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// The rack PDU (AOS 3.9.2) given the power path's captured push
// (Network 10.6.109): its syslog section and its time section each differ,
// so each is uploaded -- the clock one a settle time after the syslog one,
// since the card drops an upload that lands while it is applying the last
// -- each read back, and then nothing more is written.
func TestApplyClockAndSyslogTakeTurnsOnTheCard(t *testing.T) {
	b, err := os.ReadFile("../../../docs/fixtures/controller-10.6.109/system_cfg-pdu-syslog-remote.txt")
	if err != nil {
		t.Fatal(err)
	}
	push := unificfg.Parse(string(b))
	clk := devicemodel.Clock{Timezone: push.Timezone, NTPServers: push.NTPServers}
	c, r := newTestCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	puts := func() []string { return r.Configs }
	cfg := func() []byte { return r.Config }
	now := time.Unix(0, 0)
	c.syslog.Now = func() time.Time { return now }
	c.clock.Now = func() time.Time { return now }
	ctx := context.Background()
	cycle := func() (bool, bool) {
		s, err := c.ApplySyslog(ctx, push.SyslogHosts)
		if err != nil {
			t.Fatal(err)
		}
		k, err := c.ApplyClock(ctx, clk)
		if err != nil {
			t.Fatal(err)
		}
		return s, k
	}
	if s, k := cycle(); !s || k {
		t.Fatalf("first cycle: syslog %v clock %v; want the syslog upload alone", s, k)
	}
	now = now.Add(apcnmc.DefaultSettle)
	if s, k := cycle(); s || !k {
		t.Fatalf("a settle time later: syslog %v clock %v; want the clock upload", s, k)
	}
	want := "[SystemDate/Time]\r\nDSTConfiguration=Traditional US\r\nNTPPrimaryServer=192.0.2.4\r\nNTPSecondaryServer=0.ubnt.pool.ntp.org\r\n"
	if p := puts(); len(p) != 2 || p[1] != want {
		t.Fatalf("uploads = %q, want the clock's %q second", p, want)
	}
	for _, step := range []time.Duration{0, apcnmc.DefaultSettle, time.Hour} {
		now = now.Add(step)
		if s, k := cycle(); s || k {
			t.Errorf("after %v: syslog %v clock %v; want nothing", step, s, k)
		}
	}
	if len(puts()) != 2 {
		t.Errorf("uploads = %d, want 2", len(puts()))
	}
	tm, _ := apcnmc.ParseTime(cfg())
	if got := tm.Diff(map[string]string{"NTPTimeZone": "-07:00", "DSTConfiguration": "Traditional US", "NTPEnable": "enabled", "NTPPrimaryServer": "192.0.2.4", "NTPSecondaryServer": "0.ubnt.pool.ntp.org"}); len(got) != 0 {
		t.Errorf("the fixture card's time section differs in %v: %s", got, tm)
	}
}
