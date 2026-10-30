package apcnmc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The rack PDU's config.ini (AOS 3.9.2), captured and scrubbed; its syslog
// section is enabled with one server in slot 1.
const pduConfig = "../../../docs/fixtures/apc-aos-3.9.2/config.ini"

// The Back-UPS card's (AOS 6.0.1) as committed: the four sections that
// driver reads, no syslog section.
const backupsConfig = "../../../docs/fixtures/apc-backups-05.3/config.ini"

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseSyslogReadsTheCardsServers(t *testing.T) {
	s, ok := ParseSyslog(readFixture(t, pduConfig))
	if !ok || s.Section != "NetworkSYSLOG" {
		t.Fatalf("ParseSyslog = %q, %v", s.Section, ok)
	}
	if got := s.Hosts(); len(got) != 1 || got[0] != "192.0.2.4" {
		t.Errorf("Hosts = %v, want [192.0.2.4]", got)
	}
	if s.slots() != 4 {
		t.Errorf("slots = %d, want 4", s.slots())
	}
	if _, ok := ParseSyslog(readFixture(t, backupsConfig)); ok {
		t.Error("the Back-UPS fixture has no syslog section, but one was found")
	}
}

func TestRenderWritesOnlyTheCardsOwnKeys(t *testing.T) {
	s, _ := ParseSyslog(readFixture(t, pduConfig))
	for _, tc := range []struct {
		hosts []string
		want  string
	}{
		{[]string{"192.0.2.40"}, "[NetworkSYSLOG]\r\nSyslog=enabled\r\nServer1=192.0.2.40\r\nPort1=514\r\n"},
		{[]string{"192.0.2.40:5514"}, "[NetworkSYSLOG]\r\nSyslog=enabled\r\nServer1=192.0.2.40\r\nPort1=5514\r\n"},
		{[]string{"192.0.2.40", "192.0.2.41"}, "[NetworkSYSLOG]\r\nSyslog=enabled\r\nServer1=192.0.2.40\r\nPort1=514\r\nServer2=192.0.2.41\r\nPort2=514\r\n"},
		{nil, "[NetworkSYSLOG]\r\nSyslog=disabled\r\n"},
	} {
		b, err := s.Render(tc.hosts)
		if err != nil || string(b) != tc.want {
			t.Errorf("Render(%v) = %q, %v; want %q", tc.hosts, b, err, tc.want)
		}
	}
	// A slot holding a server the controller does not list is emptied.
	two, _ := ParseSyslog(Merge(readFixture(t, pduConfig), []byte("[NetworkSYSLOG]\r\nServer2=192.0.2.5\r\n")))
	if b, _ := two.Render([]string{"192.0.2.40"}); !strings.Contains(string(b), "Server2=0.0.0.0\r\n") {
		t.Errorf("Render with an extra server = %q, want Server2 emptied", b)
	}
	for _, bad := range [][]string{
		{"192.0.2.40\r\nServer2=192.0.2.66"},
		{"logs.example.net"},
		{"192.0.2.40:0"},
		{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4", "192.0.2.5"},
	} {
		if b, err := s.Render(bad); err == nil {
			t.Errorf("Render(%q) = %q, want a refusal", bad, b)
		}
	}
}

// card is the fixture's config.ini behind a Get/Put pair: an upload is
// merged into the file when take is set, as the card does a little after
// the transfer.
type card struct {
	cfg        []byte
	take       bool
	gets, puts int
	uploads    []string
}

func (c *card) applier(now *time.Time, logs *[]string) *SyslogApplier {
	return &SyslogApplier{
		Get: func(context.Context) ([]byte, error) { c.gets++; return c.cfg, nil },
		Put: func(_ context.Context, b []byte) error {
			c.puts++
			c.uploads = append(c.uploads, string(b))
			if c.take {
				c.cfg = Merge(c.cfg, b)
			}
			return nil
		},
		Logf: func(f string, a ...any) { *logs = append(*logs, fmt.Sprintf(f, a...)) },
		Now:  func() time.Time { return *now },
	}
}

// One upload, nothing more while the card settles, one read back that
// confirms it, then nothing at all: the loop calls Apply every cycle.
func TestApplierUploadsOnceAndReadsBack(t *testing.T) {
	ctx := context.Background()
	c := &card{cfg: readFixture(t, pduConfig), take: true}
	now, logs := time.Unix(0, 0), []string{}
	a := c.applier(&now, &logs)
	if !a.Load(c.cfg) {
		t.Fatal("Load found no syslog section")
	}
	want := []string{"192.0.2.40"}
	if changed, err := a.Apply(ctx, want); !changed || err != nil {
		t.Fatalf("first Apply = %v, %v; want an upload", changed, err)
	}
	if c.puts != 1 || c.uploads[0] != "[NetworkSYSLOG]\r\nSyslog=enabled\r\nServer1=192.0.2.40\r\nPort1=514\r\n" {
		t.Fatalf("uploads = %q", c.uploads)
	}
	now = now.Add(5 * time.Second)
	if changed, _ := a.Apply(ctx, want); changed || c.gets != 0 || c.puts != 1 {
		t.Errorf("Apply while settling: changed=%v gets=%d puts=%d; want nothing", changed, c.gets, c.puts)
	}
	now = now.Add(DefaultSettle)
	if changed, err := a.Apply(ctx, want); changed || err != nil || c.gets != 1 || c.puts != 1 {
		t.Errorf("Apply after settling: changed=%v err=%v gets=%d puts=%d; want one read back, no upload", changed, err, c.gets, c.puts)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "now reads [NetworkSYSLOG] Syslog=enabled, Facility=User, Server1=192.0.2.40") {
		t.Errorf("no confirmation logged: %q", logs)
	}
	now = now.Add(time.Hour)
	if changed, _ := a.Apply(ctx, want); changed || c.gets != 1 || c.puts != 1 {
		t.Errorf("a converged card was touched: gets=%d puts=%d", c.gets, c.puts)
	}
	// Remote logging switched off in UniFi: syslog off, servers kept.
	if changed, _ := a.Apply(ctx, nil); !changed || c.uploads[1] != "[NetworkSYSLOG]\r\nSyslog=disabled\r\n" {
		t.Errorf("Apply(nil) uploads = %q", c.uploads)
	}
}

// A card that does not take the upload is written maxMisses times, then
// left alone (with one log line) until the controller's host changes.
func TestApplierGivesUpOnACardThatDoesNotTakeIt(t *testing.T) {
	ctx := context.Background()
	c := &card{cfg: readFixture(t, pduConfig)}
	now, logs := time.Unix(0, 0), []string{}
	a := c.applier(&now, &logs)
	a.Load(c.cfg)
	for i := 0; i < 10; i++ {
		a.Apply(ctx, []string{"192.0.2.40"})
		now = now.Add(DefaultSettle)
	}
	if c.puts != maxMisses {
		t.Errorf("uploads = %d, want %d", c.puts, maxMisses)
	}
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "did not take") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("give-up logged %d times, want once: %q", n, logs)
	}
	if changed, _ := a.Apply(ctx, []string{"192.0.2.41"}); !changed {
		t.Error("a new host from the controller was not tried")
	}
}

// The Back-UPS fixture has no syslog section: nothing is written, and the
// refusal is logged once however often the loop asks.
func TestApplierLeavesACardWithoutTheSectionAlone(t *testing.T) {
	ctx := context.Background()
	c := &card{cfg: readFixture(t, backupsConfig), take: true}
	now, logs := time.Unix(0, 0), []string{}
	a := c.applier(&now, &logs)
	if a.Load(c.cfg) {
		t.Fatal("Load found a syslog section")
	}
	for i := 0; i < 3; i++ {
		if changed, err := a.Apply(ctx, []string{"192.0.2.40"}); changed || err != nil {
			t.Fatalf("Apply = %v, %v", changed, err)
		}
	}
	if c.puts != 0 || len(logs) != 2 {
		t.Errorf("puts=%d logs=%q; want none and one refusal after the Load line", c.puts, logs)
	}
}

// Merge replaces exactly the named keys in the named section.
func TestMergeChangesOnlyTheNamedKeys(t *testing.T) {
	before := readFixture(t, pduConfig)
	after := Merge(before, []byte("[NetworkSYSLOG]\r\nServer1=192.0.2.40\r\nPort1=5514\r\nNoSuchKey=1\r\n"))
	b, a := strings.Split(string(before), "\n"), strings.Split(string(after), "\n")
	if len(a) != len(b) {
		t.Fatalf("line count %d -> %d", len(b), len(a))
	}
	var diff []string
	for i := range b {
		if a[i] != b[i] {
			diff = append(diff, a[i])
		}
	}
	if len(diff) != 2 || diff[0] != "Server1=192.0.2.40" || diff[1] != "Port1=5514" {
		t.Errorf("changed lines = %q", diff)
	}
}
