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
			// plan a push then); the fixture transport is stateless, so a
			// held push could never converge here. The hold has its own
			// test below.
			c.AllowInitialChanges = true
		},
		// What each recorded push must do to the switch, as a diff against
		// the captured switch state (the fixture transport is stateless, so
		// a push that matches the captured state produces no port-2 lines —
		// which is itself the assertion for the "back to normal" pushes).
		Expect: map[int]replayExpect{
			// 0-1: HTTP 400 (the controller's cooldown after a forget), 2: 404 pending,
			// 3: adopt via mgmt_cfg, 4: first system_cfg after adoption
			5:  {must: []string{"interface Ethernet2 | shutdown"}},                                                                       // first full push of the day: port 2 disabled
			8:  {must: []string{"interface Ethernet2 | shutdown"}},                                                                       // Port State: Disabled
			9:  {mustNot: []string{"interface Ethernet2 | shutdown"}},                                                                    // Port State: Active
			10: {must: []string{"interface Ethernet2 | description Ethernet2 | switchport mode trunk | switchport trunk native vlan 2"}}, // native VLAN kids
			11: {mustNot: []string{"switchport trunk native vlan 2"}},                                                                    // native VLAN back to Default
			// 12: set-locate, 13: unset-locate
			// 14-15: Global Switch Settings -> Rogue DHCP Server Detection on, then off
			// (site-wide; pushed only because the device claimed DHCP snooping at the
			// time). EOS 4.26 cannot honour it, so neither push writes snooping.
			14: {mustNot: []string{"ip dhcp snooping"}},
			15: {mustNot: []string{"ip dhcp snooping"}},
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
				if !strings.Contains(logText, "UNSUPPORTED by this switch: DHCP snooping") {
					t.Errorf("reply %d: enabling rogue DHCP detection must be logged as unsupported", i)
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
