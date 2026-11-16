package apcpdu

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
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

// staticAddress is the static IP Settings in a captured push.
func staticAddress(t *testing.T, systemCfg string) devicemodel.AddressDesired {
	t.Helper()
	a := unificfg.Parse(systemCfg).Address
	if a == nil || a.DHCP {
		t.Fatal("the captured push carries no static IP Settings")
	}
	n, _ := net.IPMask(net.ParseIP(a.Netmask).To4()).Size()
	return devicemodel.AddressDesired{IP: a.IP, PrefixLen: n, Gateway: a.Gateway, DNS: a.DNS}
}

// The address upload takes its turn at the card's gate with the syslog and
// clock uploads: an IP Settings change that comes a moment after the syslog
// upload is not made (the card would drop it) and goes a settle time later,
// and the clock upload then waits for it in turn. A switch to DHCP inside
// the window is refused the same way. The syslog host and the clock are the
// power path's captured push (Network 10.6.109); the address is the PDU's
// own static IP Settings push, the last reply of its reply log (Network
// 10.6.106, the replay's reply 13), which differs from the card's capture.
func TestApplyAddressTakesItsTurnWithSyslogAndClock(t *testing.T) {
	b, err := os.ReadFile("../../../docs/fixtures/controller-10.6.109/system_cfg-pdu-syslog-remote.txt")
	if err != nil {
		t.Fatal(err)
	}
	push := unificfg.Parse(string(b))
	clk := devicemodel.Clock{Timezone: push.Timezone, NTPServers: push.NTPServers}
	b, err = os.ReadFile("../../../docs/fixtures/controller-10.6.106/replies-pdu.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var rec struct {
		Payload struct {
			SystemCfg string `json:"system_cfg"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatal(err)
	}
	addr := staticAddress(t, rec.Payload.SystemCfg)

	c, r := newTestCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	now := time.Unix(0, 0)
	c.syslog.Now = func() time.Time { return now }
	c.clock.Now = func() time.Time { return now }
	c.now = func() time.Time { return now }
	ctx := context.Background()

	if s, err := c.ApplySyslog(ctx, push.SyslogHosts); !s || err != nil {
		t.Fatalf("ApplySyslog = %v, %v; want the syslog upload", s, err)
	}
	changed, err := c.ApplyAddress(ctx, addr)
	if changed || err == nil || !strings.Contains(err.Error(), apcnmc.Busy) {
		t.Fatalf("the address a moment after the syslog upload: changed=%v err=%v; want it refused as busy", changed, err)
	}
	if len(r.Configs) != 1 {
		t.Fatalf("uploads = %q; the address must not follow the syslog one inside its settle time", r.Configs)
	}

	now = now.Add(apcnmc.DefaultSettle)
	if changed, err := c.ApplyAddress(ctx, addr); !changed || err != nil {
		t.Fatalf("the address a settle time later: changed=%v err=%v; want the upload", changed, err)
	}
	want := "[NetworkTCP/IP]\r\nOverride=02 00 00 00 00 01\r\nBootMode=Manual\r\nSystemIP=192.0.2.3\r\nSubnetMask=255.255.0.0\r\nDefaultGateway=192.0.2.4\r\n"
	if len(r.Configs) != 2 || r.Configs[1] != want {
		t.Fatalf("uploads = %q, want the address's %q second", r.Configs, want)
	}
	if k, err := c.ApplyClock(ctx, clk); k || err != nil {
		t.Fatalf("ApplyClock right after the address upload = %v, %v; want it to wait", k, err)
	}
	if changed, err := c.ApplyAddress(ctx, devicemodel.AddressDesired{DHCP: true}); changed || err == nil || !strings.Contains(err.Error(), "DHCP") {
		t.Errorf("DHCP right after the address upload: changed=%v err=%v; want it refused, naming DHCP", changed, err)
	}
	if len(r.Configs) != 2 {
		t.Fatalf("uploads = %q; nothing may follow the address upload inside its settle time", r.Configs)
	}

	now = now.Add(apcnmc.DefaultSettle)
	if k, err := c.ApplyClock(ctx, clk); !k || err != nil {
		t.Fatalf("ApplyClock a settle time after the address upload = %v, %v; want the clock upload", k, err)
	}
	if len(r.Configs) != 3 || !strings.HasPrefix(r.Configs[2], "[SystemDate/Time]\r\n") {
		t.Errorf("uploads = %q, want the clock's third", r.Configs)
	}
}

// The outlet names are a config.ini upload too, so they take their turn at
// the same gate: a rename a moment after the syslog upload is refused as
// busy and written nothing, and the same rename a settle time later goes as
// one upload.
func TestApplyOutletNamesTakeTheirTurnWithSyslog(t *testing.T) {
	b, err := os.ReadFile("../../../docs/fixtures/controller-10.6.109/system_cfg-pdu-syslog-remote.txt")
	if err != nil {
		t.Fatal(err)
	}
	push := unificfg.Parse(string(b))
	c, r := newTestCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	now := time.Unix(0, 0)
	c.syslog.Now = func() time.Time { return now }
	c.clock.Now = func() time.Time { return now }
	c.now = func() time.Time { return now }
	ctx := context.Background()

	if s, err := c.ApplySyslog(ctx, push.SyslogHosts); !s || err != nil {
		t.Fatalf("ApplySyslog = %v, %v; want the syslog upload", s, err)
	}
	desired := []devicemodel.OutletDesired{{Index: 2, On: true, Name: "NAS"}}
	changed, err := c.ApplyOutlets(ctx, desired)
	if changed != 0 || err == nil || !strings.Contains(err.Error(), apcnmc.Busy) {
		t.Fatalf("names a moment after the syslog upload: changed=%d err=%v; want them refused as busy", changed, err)
	}
	if len(r.Configs) != 1 {
		t.Fatalf("uploads = %q; the names must not follow the syslog one inside its settle time", r.Configs)
	}

	now = now.Add(apcnmc.DefaultSettle)
	if changed, err := c.ApplyOutlets(ctx, desired); changed != 1 || err != nil {
		t.Fatalf("names a settle time later: changed=%d err=%v; want the one rename", changed, err)
	}
	if len(r.Configs) != 2 || !strings.HasPrefix(r.Configs[1], "[RackPDUOutlet]\r\n") || !strings.Contains(r.Configs[1], "Name2=NAS\r\n") {
		t.Errorf("uploads = %q, want the names' second", r.Configs)
	}
	if len(r.Sets) != 0 {
		t.Errorf("renaming switched an outlet: %v", r.Sets)
	}
}
