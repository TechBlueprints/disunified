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

	"github.com/TechBlueprints/switch-to-unifi/internal/device"
	"github.com/TechBlueprints/switch-to-unifi/internal/drivers/arista-eos"
	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
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
			c.OnConnected = func(*switchmodel.Snapshot) { connected++ }
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
			}
			if typ == "cmd" && cmd == "build-ssh-session" && !strings.Contains(logText, `UNHANDLED cmd "build-ssh-session"`) {
				t.Errorf("reply %d: build-ssh-session must be logged as unhandled", i)
			}
		},
	})
}
