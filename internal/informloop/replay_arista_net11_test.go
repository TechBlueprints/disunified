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
// The fixture transport remembers what was written, so the replay runs as
// it ran live: the reconcile of the 10.6.109 push (record 0) writes the
// site's VLANs and the port settings the 4.26.14M capture predates, and
// from then on each push writes only its own change and every noop
// nothing ("0 of 54 ports changed", the deployed bridge's log). The
// upgrade's push changes nothing, as it did live. Each rule goes in as one
// timed session and its confirm, and each removal takes the 57 lists out
// again in a session of its own.
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
		// The bridge had applied pushes for weeks before record 0.
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

	// Record 0: one VLAN batch, one port batch, port 54 named as EOS
	// names it, and the delete of VLAN 3, which the 2026-09 capture's
	// switch had and no push names (docs/drivers/arista-eos.md §12).
	first := strings.Split(writes[0], "\n")
	if len(first) != 3 || first[0] != "enable | configure | vlan 69,4000,4010 | end | write memory" || !strings.Contains(first[1], "interface Ethernet54/1 | description Ethernet54/1 |") || first[2] != "enable | configure | no vlan 3 | end | write memory" {
		t.Fatalf("record 0 is not one VLAN batch, one port batch and the delete of VLAN 3:\n%s", writes[0])
	}
	port54 := func(desc string) string {
		return "enable | configure | interface Ethernet54/1 | description " + desc + " | end | write memory"
	}
	// session is an ACL change: the timed session with body's lines in
	// each of the 57 lists (or of their removal), then its confirm.
	session := func(lines ...string) func(string) []string {
		return func(w string) []string {
			ls := strings.Split(w, "\n")
			if len(ls) != 2 {
				return []string{"want one ACL session and its confirm"}
			}
			var bad []string
			name, _, _ := strings.Cut(strings.TrimPrefix(ls[0], "enable | configure session "), " |")
			if !strings.HasPrefix(ls[0], "enable | configure session dui-acl-") || !strings.HasSuffix(ls[0], "| commit timer 00:02:00") {
				bad = append(bad, "not a timed session: "+ls[0][:80])
			}
			if ls[1] != "enable | configure session "+name+" commit | write memory" {
				bad = append(bad, "not confirmed: "+ls[1])
			}
			for _, l := range lines {
				if n := strings.Count(ls[0], "| "+l); n != 57 {
					bad = append(bad, itoa(n)+" x "+l+", want 57")
				}
			}
			return bad
		}
	}
	exactly := func(want string) func(string) []string {
		return func(w string) []string {
			if w != want {
				return []string{"want " + want}
			}
			return nil
		}
	}
	added := func(rule string) func(string) []string {
		return session("ip access-list DUI_IP_ACL_", "20 "+rule+" |", "ip access-group DUI_IP_ACL_")
	}
	removed := session("no ip access-group DUI_IP_ACL_", "no ip access-list DUI_IP_ACL_")
	want := map[int]func(string) []string{
		0:  func(string) []string { return nil },
		18: exactly(port54("Ethernet54/1-4")), 19: exactly(port54("Ethernet54/1")), // 18 also pushes dhcp_snoop.status=disabled: nothing to write
		21: exactly(port54("Ethernet54/1-4")), 23: exactly(port54("Ethernet54/1")),
		27: added("deny tcp 203.0.113.0/24 198.51.100.0/24 eq 9"), // dui-test-arista-block-1
		29: removed,
		35: added("deny udp 198.51.100.0/24 host 192.0.2.1 eq 68"), // dui-test-arista-match-1: the switch's own address
		37: removed,
	}
	for i, w := range writes {
		check, ok := want[i]
		if !ok {
			check = exactly("")
		}
		if bad := check(w); len(bad) > 0 {
			t.Errorf("reply %d (cfgversion %q): %s; wrote:\n%s", i, pushes[i], strings.Join(bad, "; "), w)
		}
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("writes the fixture transport could not show: %q", ft.Unmodelled)
	}
	// While a rule was in, each collect read the 57 lists back from the
	// switch as the session left them (the transport serves the lists it
	// wrote, fixture_acl.go), with nothing missing and nothing out of place.
	lists := 0
	for _, r := range ft.Runs {
		if len(r) > 1 && r[1] == "show ip access-lists summary" {
			lists = max(lists, len(r)-2)
		}
	}
	if lists != 57 {
		t.Errorf("the ACL read-back read %d lists at most, want 57", lists)
	}
	if strings.Contains(drv.String(), "ACL read-back:") {
		t.Errorf("the ACL read-back logged a problem:\n%s", drv.String())
	}
}
