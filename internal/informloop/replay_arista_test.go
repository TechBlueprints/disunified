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
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The Arista replay: the controller's replies to the DCS-7160
// (replies.ndjson, from the adoption handshake through port edits made in
// the UI to set-locate/unset-locate) drive the real driver against the
// real EOS 4.26.14M captures. What must come out is the exact EOS
// configuration each push produced live.
func TestReplayControllerRepliesArista(t *testing.T) {
	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
	coll := aristaeos.NewCollector(ft)
	coll.Log = log.New(os.Stderr, "", 0)
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	written := func() string {
		var batches []string
		for _, b := range ft.Configured[seen:] {
			batches = append(batches, strings.Join(b, " | "))
		}
		seen = len(ft.Configured)
		return strings.Join(batches, "\n")
	}
	var connected int
	locating := func(sess *device.Session) bool {
		var m map[string]any
		_ = json.Unmarshal(sess.BuildPayload(time.Now()), &m)
		v, _ := m["locating"].(bool)
		return v
	}
	runReplay(t, replayDriver{
		Name: "arista-eos", Replies: "replies.ndjson", Collector: coll, Control: coll, Snapshot: snap,
		GatewayIP: "192.0.2.1", // the gateway in the scrubbed EOS ARP fixture
		Written:   written,
		Loop: func(c *Config) {
			c.ControlIGMP, c.ControlNTP, c.ControlSyslog, c.ControlSNMP = true, true, true, true
			c.OnConnected = func(*devicemodel.Snapshot) { connected++ }
			// The recording's first push applied live (the driver could not
			// plan a push then). The hold has its own test below.
			c.AllowInitialChanges = true
		},
		// The fixture transport remembers what was written, so each reply
		// writes only what its push changes against the switch as the
		// replies before it left it, and every other reply -- a reconcile,
		// a cmd -- writes nothing, as the deployed bridge does ("0 of 54
		// ports changed").
		Quiet: true,
		Expect: map[int]replayExpect{
			// 0-1: HTTP 400 (the controller's cooldown after a forget), 2: 404 pending,
			// 3: adopt via mgmt_cfg, 4: first system_cfg after adoption: the
			// site's VLANs the capture lacks, IGMP snooping per VLAN, the NTP
			// pool server, every port's name.
			4: {must: []string{"| vlan 69,4000 |", "| ntp server 0.ubnt.pool.ntp.org |", "interface Ethernet1 | description SFP28 1 |"}},
			5: {exactly: "enable | configure | interface Ethernet2 | shutdown | end | write memory"}, // first full push of the day: port 2 disabled
			// 6: an empty setparam, 7: build-ssh-session.
			// 8: Port State: Disabled again, with the ports renamed: port 2 is
			// already shut, so only the names are written.
			8:  {must: []string{"interface Ethernet2 | description Ethernet2 |"}, mustNot: []string{"shutdown"}},
			9:  {exactly: "enable | configure | interface Ethernet2 | no shutdown | end | write memory"},                                                                                                            // Port State: Active
			10: {exactly: "enable | configure | interface Ethernet2 | switchport mode trunk | switchport trunk native vlan 2 | switchport trunk allowed vlan 1-2,8-10,69,1000,2000,3000,4000 | end | write memory"}, // native VLAN kids
			11: {exactly: "enable | configure | interface Ethernet2 | switchport mode trunk | no switchport trunk native vlan | switchport trunk allowed vlan all | end | write memory"},                            // native VLAN back to Default
			// 12: set-locate, 13: unset-locate
			// 14-15: Global Switch Settings -> Rogue DHCP Server Detection on, then off
			// (site-wide; pushed only because the device claimed DHCP snooping at the
			// time). EOS 4.26 cannot honour it, so neither push writes snooping;
			// 14 also carries the site's new first NTP server.
			14: {exactly: "enable | configure | ntp server 192.0.2.5 | no ntp server 192.0.2.2 | end | write memory"},
		},
		Check: func(t *testing.T, i int, rec replayRecord, sess *device.Session, l *Loop, logText string) {
			var p map[string]any
			_ = json.Unmarshal(rec.Payload, &p)
			typ, _ := p["_type"].(string)
			cmd, _ := p["cmd"].(string)
			if typ == "setparam" && p["system_cfg"] != nil && len(ft.Configured) == 0 {
				t.Errorf("reply %d: a system_cfg produced no switch configuration", i)
			}
			switch i {
			case 0, 1, 2:
				if sess.Adopted() || l.state != StatePending {
					t.Errorf("reply %d: the device must still be pending (adopted=%v state=%v)", i, sess.Adopted(), l.state)
				}
			case 3:
				if !sess.Adopted() || l.state != StateAdopting {
					t.Errorf("after the adopt reply: adopted=%v state=%v", sess.Adopted(), l.state)
				}
			case 4:
				if l.state != StateConnected || connected != 1 {
					t.Errorf("after the first system_cfg: state=%v, OnConnected calls=%d", l.state, connected)
				}
			case 12:
				if !locating(sess) {
					t.Errorf("set-locate must turn locating on in the next inform")
				}
			case 13:
				if locating(sess) {
					t.Errorf("unset-locate must turn locating off")
				}
			case 14:
				// Rogue DHCP detection on, as Network 10.6.106 pushed it on
				// 2026-09-20: the status and no guard group. Claimed since
				// 2026-11-14 (the per-port guard ACLs): taken, and with no
				// trusted server on any VLAN there is no guard to write.
				if strings.Contains(logText, "UNSUPPORTED by this switch: DHCP snooping") {
					t.Errorf("reply %d: DHCP snooping is claimed and must not be logged unsupported", i)
				}
				for _, b := range ft.Configured {
					if strings.Contains(strings.Join(b, "\n"), "access-list") {
						t.Errorf("reply %d: snooping without guards wrote an ACL: %v", i, b)
					}
				}
			}
			if i == 15 {
				// A reconcile of the last push, from the switch as the replay
				// left it, finds nothing to write.
				n := len(ft.Configured)
				l.reconcile(context.Background())
				if len(ft.Configured) != n {
					t.Errorf("a reconcile after the last push wrote %q", ft.Configured[n:])
				}
				if len(ft.Unmodelled) != 0 {
					t.Errorf("writes the fixture transport could not show: %q", ft.Unmodelled)
				}
			}
			if typ == "cmd" && cmd == "build-ssh-session" && !strings.Contains(logText, `UNHANDLED cmd "build-ssh-session"`) {
				t.Errorf("reply %d: build-ssh-session must be logged as unhandled", i)
			}
		},
	})
}

// On a fresh adoption the bridge seeds the controller's port config from
// the switch, and the loop holds the first push while it would still change
// a port, so an adoption never resets the switch. The Arista can be held
// since it plans a push (devicemodel.Planner): the recorded push that
// disables port 2, arriving as the first push of an adoption, is held and
// stays pending; the switch's own state planned back changes nothing.
func TestAristaFirstPushAfterAdoptionIsHeld(t *testing.T) {
	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
	coll := aristaeos.NewCollector(ft)
	coll.Log = log.New(os.Stderr, "", 0)
	if _, err := coll.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(loadReplies(t, "replies.ndjson")[5].Payload, &p); err != nil {
		t.Fatal(err)
	}
	push, _ := p["system_cfg"].(string)
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: push}
	desc := inform.Descriptor{MAC: "02:00:00:00:00:01", Model: "UDC48X6"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), Collector: coll, Controller: coll, ApplyTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	l.applyPending(context.Background())
	if len(ft.Configured) != 0 {
		t.Errorf("the first push wrote %v", ft.Configured)
	}
	if _, _, ok := sess.Pending(); !ok {
		t.Error("the held push must stay pending so the controller keeps re-sending")
	}
	if !strings.Contains(buf.String(), "HOLDING the first push") {
		t.Errorf("log = %q", buf.String())
	}
	// The plan is the apply's own diff: it names port 2, and once the push
	// is written there is nothing left to plan.
	desired := l.desiredPorts(push)
	if planned := coll.PlanPorts(desired); !containsInt(planned, 2) {
		t.Errorf("the push's plan %v does not name port 2", planned)
	}
	if _, err := coll.ApplyPorts(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	if planned := coll.PlanPorts(desired); len(planned) != 0 {
		t.Errorf("after the apply the same push still plans changes to %v", planned)
	}
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
