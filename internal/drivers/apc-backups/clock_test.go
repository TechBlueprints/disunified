package apcbackups

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/apcnmc"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// The Back-UPS card (AOS 6.0.1, NTPManualOverride already on) given the power path's captured push
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
	c, fr, _ := startFixture(t)
	puts := func() []string { return fr.Puts }
	cfg := func() []byte { return []byte(fr.Config) }
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

// The address upload takes its turn at the card's gate with the syslog and
// clock uploads, as on the PDU: the power path's captured push (Network
// 10.6.109) carries a static address, a syslog host and a clock that all
// differ from this card's (on DHCP). The address a moment after the syslog
// upload is not made (the card would drop it) and goes a settle time later;
// the clock upload then waits for it, and so does a switch back to DHCP.
func TestApplyAddressTakesItsTurnWithSyslogAndClock(t *testing.T) {
	b, err := os.ReadFile("../../../docs/fixtures/controller-10.6.109/system_cfg-pdu-syslog-remote.txt")
	if err != nil {
		t.Fatal(err)
	}
	push := unificfg.Parse(string(b))
	clk := devicemodel.Clock{Timezone: push.Timezone, NTPServers: push.NTPServers}
	if push.Address == nil || push.Address.DHCP {
		t.Fatal("the captured push carries no static IP Settings")
	}
	n, _ := net.IPMask(net.ParseIP(push.Address.Netmask).To4()).Size()
	addr := devicemodel.AddressDesired{IP: push.Address.IP, PrefixLen: n, Gateway: push.Address.Gateway, DNS: push.Address.DNS}

	c, fr, _ := startFixture(t)
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
	if len(fr.Puts) != 1 {
		t.Fatalf("uploads = %q; the address must not follow the syslog one inside its settle time", fr.Puts)
	}

	now = now.Add(apcnmc.DefaultSettle)
	if changed, err := c.ApplyAddress(ctx, addr); !changed || err != nil {
		t.Fatalf("the address a settle time later: changed=%v err=%v; want the upload", changed, err)
	}
	want := "[NetworkTCP/IP]\r\nOverride=02 00 00 00 00 01\r\nBootMode=Manual\r\nSystemIP=192.0.2.1\r\nSubnetMask=255.255.0.0\r\nDefaultGateway=192.0.2.2\r\n"
	if len(fr.Puts) != 2 || fr.Puts[1] != want {
		t.Fatalf("uploads = %q, want the address's %q second", fr.Puts, want)
	}
	if k, err := c.ApplyClock(ctx, clk); k || err != nil {
		t.Fatalf("ApplyClock right after the address upload = %v, %v; want it to wait", k, err)
	}
	if changed, err := c.ApplyAddress(ctx, devicemodel.AddressDesired{DHCP: true}); changed || err == nil || !strings.Contains(err.Error(), "DHCP") {
		t.Errorf("DHCP right after the address upload: changed=%v err=%v; want it refused, naming DHCP", changed, err)
	}
	if len(fr.Puts) != 2 {
		t.Fatalf("uploads = %q; nothing may follow the address upload inside its settle time", fr.Puts)
	}

	now = now.Add(apcnmc.DefaultSettle)
	if k, err := c.ApplyClock(ctx, clk); !k || err != nil {
		t.Fatalf("ApplyClock a settle time after the address upload = %v, %v; want the clock upload", k, err)
	}
	if len(fr.Puts) != 3 || !strings.HasPrefix(fr.Puts[2], "[SystemDate/Time]\r\n") {
		t.Errorf("uploads = %q, want the clock's third", fr.Puts)
	}
}
