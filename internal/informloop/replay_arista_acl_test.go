package informloop

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The controller's captured IP rule pushes through the loop into the Arista
// driver over the 4.26.14M capture (docs/drivers/arista-eos.md §9.2, §9.4):
// the Arista's own recorded push (replies.ndjson reply 11, the one that
// matches the captured switch) with the switch.acl.ip groups of the ECS's
// push (three rules, 2026-11-01) and of the DX010's (the six shapes the ECS
// round left out). Each must reach the switch as one timed session: a list
// per interface (57 on the captured switch) with the rules in the
// controller's order and no `counters per-entry` (no guard is pushed, and
// nothing reads a rule's count), bound inbound, then the confirm. A
// reconcile from the same push writes no ACL again.
func TestReplayAristaIPRulePushes(t *testing.T) {
	var p map[string]any
	if err := json.Unmarshal(loadReplies(t, "replies.ndjson")[11].Payload, &p); err != nil {
		t.Fatal(err)
	}
	base, _ := p["system_cfg"].(string)
	for _, tc := range []struct {
		push  string
		rules []string
	}{
		{"system_cfg-ecs-acl.txt", []string{
			"permit ip 192.0.2.0/24 198.51.100.0/24",
			"deny tcp 203.0.113.0/24 192.0.2.0/24 eq 9",
			"deny tcp 203.0.113.0/24 192.0.2.0/24 eq 1001",
		}},
		{"system_cfg-dx010-acl.txt", []string{
			"deny udp 203.0.113.0/24 192.0.2.0/24 eq 9",
			"deny tcp 203.0.113.0/24 192.0.2.0/24 eq 9",
			"deny udp 203.0.113.0/24 192.0.2.0/24 eq 9",
			"deny ip any 198.51.100.0/24",
			"deny tcp 203.0.113.0/24 eq 9 192.0.2.0/24",
			"deny ip 192.0.2.0/24 198.51.100.0/24",
		}},
	} {
		t.Run(tc.push, func(t *testing.T) {
			var extra []string
			for _, line := range strings.Split(readECSPush(t, tc.push), "\n") {
				if strings.HasPrefix(line, "switch.acl.ip.") {
					extra = append(extra, line)
				}
			}
			text := strings.TrimRight(base, "\n") + "\n" + strings.Join(extra, "\n") + "\n"

			ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
			coll := aristaeos.NewCollector(ft)
			var buf strings.Builder
			logger := log.New(&buf, "", 0)
			coll.Log = logger
			snap, err := coll.Start(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if caps := coll.Capabilities(); caps.IPACLs != 32 || caps.MACACLs != 0 {
				t.Fatalf("the controller pushes IP rules only to a device that claims them: %+v", caps)
			}
			desc := inform.Descriptor{MAC: snap.System.MAC}
			st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: text}
			sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
			l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, AllowInitialChanges: true, ApplyTimeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if !l.applyPending(ctx) {
				t.Fatal("the push was not consumed")
			}
			var session []string
			confirmed := false
			for _, b := range ft.Configured {
				s := strings.Join(b, "\n")
				if strings.Contains(s, "configure session dui-acl-") && strings.Contains(s, "commit timer") {
					session = b
				}
				if strings.HasPrefix(strings.TrimPrefix(s, "enable\n"), "configure session dui-acl-") && strings.Contains(s, " commit\nwrite memory") {
					confirmed = true
				}
			}
			if session == nil || !confirmed {
				t.Fatalf("no confirmed ACL session; batches %q\nlog:\n%s", ft.Configured, buf.String())
			}
			s := strings.Join(session, "\n")
			if strings.Count(s, "\nip access-list DUI_IP_ACL_") != 57 || strings.Count(s, "\nip access-group DUI_IP_ACL_") != 57 {
				t.Errorf("want 57 lists, each bound once:\n%s", s)
			}
			if strings.Contains(s, "counters per-entry") {
				t.Errorf("a list without a guard is not counted:\n%s", s)
			}
			body := listBody(s, "DUI_IP_ACL_Ethernet2")
			for i, r := range tc.rules {
				if len(body) <= 1+i || body[1+i] != itoa(20+10*i)+" "+r {
					t.Errorf("Ethernet2's list rule %d: got %q, want %q", i+1, body, r)
					break
				}
			}
			if n := len(body); n != len(tc.rules)+3 || body[n-1] != itoa(10*n)+" permit ip any any" {
				t.Errorf("Ethernet2's list: %q", body)
			}
			if strings.Contains(buf.String(), "HELD") || strings.Contains(buf.String(), "left as the switch has them") {
				t.Errorf("nothing is held:\n%s", buf.String())
			}
			n := len(ft.Configured)
			l.reconcile(ctx)
			for _, b := range ft.Configured[n:] {
				if strings.Contains(strings.Join(b, "\n"), "access-") {
					t.Errorf("reconcile wrote an ACL again: %q", b)
				}
			}
		})
	}
}

// listBody is the numbered lines of one list in a session body.
func listBody(session, name string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(session, "\n") {
		switch {
		case line == "ip access-list "+name:
			in = true
		case in && line == "exit":
			return out
		case in && line != "" && line[0] >= '0' && line[0] <= '9':
			out = append(out, line)
		}
	}
	return out
}

// The live end-to-end run of 2026-11-14 on Network 11.0.99
// (docs/drivers/arista-eos.md §9.7): one Block rule scoped to the Arista
// alone, its push (controller-11.0.99/system_cfg-arista-acl.txt), then the
// push after the rule was deleted (system_cfg-arista-noacl.txt, the same
// lines without the switch.acl.ip group). The first writes the rule into a
// list on each of the 57 interfaces; the second removes every list. Live, the
// second change failed: the deployed driver named every session `dui-acl`,
// and EOS will not enter the name of its last completed session ("Cannot
// enter session dui-acl, it is already complete"). Each change must open a
// session of its own and confirm that one.
func TestReplayAristaIPRuleLiveRound(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
	coll := aristaeos.NewCollector(ft)
	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	coll.Log = logger
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
	var opened, confirmed []string
	for i, push := range []string{"system_cfg-arista-acl.txt", "system_cfg-arista-noacl.txt"} {
		st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v" + itoa(i+1), PendingSystemCfg: read(push)}
		sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
		l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, AllowInitialChanges: true, ApplyTimeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		n := len(ft.Configured)
		if !l.applyPending(context.Background()) {
			t.Fatalf("%s was not consumed", push)
		}
		var body string
		for _, b := range ft.Configured[n:] {
			if len(b) < 2 || !strings.HasPrefix(b[1], "configure session dui-acl-") {
				continue
			}
			name := strings.TrimPrefix(b[1], "configure session ")
			switch {
			case b[len(b)-1] == "commit timer 00:02:00":
				opened, body = append(opened, name), strings.Join(b, "\n")
			case strings.HasSuffix(name, " commit"):
				confirmed = append(confirmed, strings.TrimSuffix(name, " commit"))
			}
		}
		switch i {
		case 0:
			if strings.Count(body, "\nip access-group DUI_IP_ACL_") != 57 || strings.Count(body, "\n20 deny tcp 203.0.113.0/24 198.51.100.0/24 eq 9\n") != 57 {
				t.Errorf("the rule push wants the rule in 57 bound lists:\n%s", body)
			}
		case 1:
			if strings.Count(body, "\nno ip access-list DUI_IP_ACL_") != 57 {
				t.Errorf("the push without the rule removes all 57 lists:\n%s\nlog:\n%s", body, buf.String())
			}
		}
	}
	if len(opened) != 2 || opened[0] == opened[1] || strings.Join(confirmed, ",") != strings.Join(opened, ",") {
		t.Errorf("sessions opened %q, confirmed %q: want two, each its own name, each confirmed", opened, confirmed)
	}
}
