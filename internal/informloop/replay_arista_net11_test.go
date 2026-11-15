package informloop

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
)

// The Arista on Network 11, replayed: controller-11.0.99/replies-arista.ndjson
// is the deployed bridge's own reply log (scrubbed with
// scripts/sanitize-controller.py, the switch's address and gateway aligned
// with the EOS capture's scrub: 192.0.2.1 and 192.0.2.2). Record 0 is the
// last push from Network 10.6.109 (2026-11-12) and record 1 the reconcile
// after it; then every setparam since the console installed 11.0.97 by
// itself (2026-11-13), each with the noop that followed: the upgrade's
// fresh push, the interval-only replies, 11.0.99's pushes of 2026-11-14 --
// Rogue DHCP detection's status now pushed disabled, port 54 renamed
// Ethernet54/1-4 and back twice (its breakout probed live), a Block rule
// scoped to the Arista and its removal, a rule matching the switch's own
// address and its removal. The bridge was long adopted when the upgrade
// came, so the device starts adopted on the (test) site key.
//
// The fixture transport is stateless: every collect reads the 4.26.14M
// capture, which predates three of the site's VLANs and holds none of the
// driver's lists, so every apply and every reconcile writes those VLANs and
// the port batch again. That writing is the baseline (a reconcile of the
// 10.6.109 push); each reply must write exactly the baseline plus its own
// change. The upgrade's push changes nothing, as it did live (0 of 54 ports
// changed). A removed rule cannot show here (the next collect forgets the
// lists); TestReplayAristaIPRuleLiveRound replays that pair.
func TestReplayAristaNetwork11(t *testing.T) {
	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
	coll := aristaeos.NewCollector(ft)
	var drv strings.Builder
	coll.Log = log.New(&drv, "", 0)
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	var writes []string // per reply
	written := func() string {
		var batches []string
		for _, b := range ft.Configured[seen:] {
			batches = append(batches, strings.Join(b, " | "))
		}
		seen = len(ft.Configured)
		w := strings.Join(batches, "\n")
		writes = append(writes, w)
		return w
	}
	pushes := map[int]string{} // reply index -> cfgversion
	runReplay(t, replayDriver{
		Name: "arista-eos-11.0.99", Replies: "controller-11.0.99/replies-arista.ndjson", Collector: coll, Control: coll, Snapshot: snap,
		Start:     &device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", UseAESGCM: true},
		GatewayIP: "192.0.2.2", // the gateway in the scrubbed EOS capture (ip route 0.0.0.0/0)
		Written:   written,
		// The stateless transport can never converge a held push, and the
		// bridge had applied pushes for weeks before record 0.
		Loop: func(c *Config) { c.AllowInitialChanges = true },
		Check: func(t *testing.T, i int, rec replayRecord, sess *device.Session, l *Loop, logText string) {
			var p map[string]any
			_ = json.Unmarshal(rec.Payload, &p)
			if v, _ := p["cfgversion"].(string); v != "" && p["system_cfg"] != nil {
				pushes[i] = v
			}
			if l.state != StateConnected {
				t.Errorf("reply %d: state %v, want CONNECTED throughout", i, l.state)
			}
		},
	})
	if len(writes) != 43 {
		t.Fatalf("%d replies written, want 43", len(writes))
	}
	for i, v := range map[int]string{0: "c4f1ed8aeebdb80e", 2: "2de7e5df990bb7b2", 18: "5e0f70df3798599c", 27: "c7be91764c25b2dd", 35: "ce21bec74b0c6d61"} {
		if pushes[i] != v {
			t.Fatalf("reply %d is push %q, want %s: the fixture changed", i, pushes[i], v)
		}
	}

	// The baseline: the reconcile after the last 10.6.109 push, one VLAN
	// batch and one port batch, port 54 named as EOS names it.
	base := strings.Split(writes[1], "\n")
	if len(base) != 2 || !strings.Contains(base[0], "vlan ") || !strings.Contains(base[1], "interface Ethernet54/1 | description Ethernet54/1 |") {
		t.Fatalf("baseline (reply 1) is not one VLAN and one port batch:\n%s", writes[1])
	}
	renamed := strings.Replace(base[1], "description Ethernet54/1 |", "description Ethernet54/1-4 |", 1)
	if renamed == base[1] {
		t.Fatal("no port 54 description in the baseline")
	}
	rule := func(line string) func(string) []string {
		return func(w string) []string {
			var bad []string
			ls := strings.Split(w, "\n")
			if len(ls) != 4 || ls[0] != base[0] || ls[3] != base[1] {
				return []string{"want the baseline with one ACL session and its confirm between"}
			}
			if !strings.HasPrefix(ls[1], "enable | configure session dui-acl-") || !strings.HasSuffix(ls[1], "commit timer 00:02:00") {
				bad = append(bad, "the rule is not written in a timed session: "+ls[1][:80])
			}
			if n := strings.Count(ls[1], "| ip access-group DUI_IP_ACL_"); n != 57 {
				bad = append(bad, "bound lists: "+itoa(n)+", want 57")
			}
			if n := strings.Count(ls[1], "| 20 "+line+" |"); n != 57 {
				bad = append(bad, "rule "+line+" in "+itoa(n)+" lists, want 57")
			}
			if !strings.HasSuffix(ls[2], " commit | write memory") {
				bad = append(bad, "not confirmed: "+ls[2])
			}
			return bad
		}
	}
	same := func(what, want string) func(string) []string {
		return func(w string) []string {
			if w != want {
				return []string{"want exactly " + what}
			}
			return nil
		}
	}
	block := rule("deny tcp 203.0.113.0/24 198.51.100.0/24 eq 9")  // dui-test-arista-block-1
	match := rule("deny udp 198.51.100.0/24 host 192.0.2.1 eq 68") // dui-test-arista-match-1: the switch's own address
	plain := same("the baseline", writes[1])
	split := same("the baseline with port 54 described Ethernet54/1-4", base[0]+"\n"+renamed)
	want := map[int]func(string) []string{
		18: split, 19: plain, 20: plain, 21: split, 22: split, 23: plain, // 18 also pushes dhcp_snoop.status=disabled: nothing to write
		27: block, 28: block, // the rule, and the reconcile after it (the capture forgets it)
		35: match, 36: match,
	}
	for i, w := range writes {
		check := plain
		if f, ok := want[i]; ok {
			check = f
		}
		if bad := check(w); len(bad) > 0 {
			t.Errorf("reply %d (cfgversion %q): %s; wrote:\n%s", i, pushes[i], strings.Join(bad, "; "), w)
		}
	}
}
