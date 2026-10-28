package informloop

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// The ECS Core's recorded replies (replies-proxmox.ndjson: the controller
// adopting a USWF07D-shaped node, then pushing port config) drive the sonic
// driver read-only against the real VS capture: the device must adopt,
// connect and report every applied cfgversion, and the switch must receive
// nothing -- without a Controller nothing is written. The SONiC lab
// session's own replies, with control on, are TestReplaySonicLabSession
// (replies-sonic.ndjson).
func TestReplayControllerRepliesSonic(t *testing.T) {
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238524-ff3c0c9f6", "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	coll := sonic.NewCollector(fr)
	coll.Log = log.New(os.Stderr, "", 0)
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	written := func() string {
		s := strings.Join(fr.Commands[seen:], "\n")
		seen = len(fr.Commands)
		return s
	}
	nothing := replayExpect{mustNot: []string{"config ", "sudo", "redis"}}
	runReplay(t, replayDriver{
		Name: "sonic", Replies: "replies-proxmox.ndjson", Model: "USWF07D",
		Collector: coll, Control: nil, Snapshot: snap,
		GatewayIP: "192.0.2.2",
		Written:   written,
		Expect:    map[int]replayExpect{0: nothing, 1: nothing, 2: nothing, 3: nothing, 4: nothing, 5: nothing},
		Check: func(t *testing.T, i int, rec replayRecord, sess *device.Session, l *Loop, logText string) {
			switch i {
			case 0:
				if sess.Adopted() || l.state != StatePending {
					t.Errorf("reply 0: the device must still be pending")
				}
			case 1:
				if !sess.Adopted() || l.state != StateAdopting {
					t.Errorf("after the adopt reply: adopted=%v state=%v", sess.Adopted(), l.state)
				}
			case 2:
				if l.state != StateConnected {
					t.Errorf("after the first system_cfg: state=%v", l.state)
				}
			}
			if len(fr.Commands) != 0 {
				t.Errorf("reply %d: the read-only driver ran %v", i, fr.Commands)
			}
		},
	})
}
