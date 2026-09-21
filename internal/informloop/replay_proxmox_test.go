package informloop

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/proxmox"
)

// The Proxmox replay: the controller's recorded replies to a node
// (replies-proxmox.ndjson, cut from the bridge's own reply log on
// 2026-09-20 and scrubbed) drive the real driver against the real node
// capture. The pushes were made live through the controller's API: VM
// 999's port to native VLAN 2 with VLANs 10 and 69 tagged, then back to
// "All"; what must come out is the `qm set` each one produced on the node.
func TestReplayControllerRepliesProxmox(t *testing.T) {
	fr, err := proxmox.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "proxmox-9.1.6", "collect-node2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	coll := proxmox.NewCollector(fr)
	coll.ManageLLDP = false
	coll.Log = log.New(os.Stderr, "", 0)
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := len(fr.Commands) // the first poll's tag writes are not a push's doing
	written := func() string {
		s := strings.Join(fr.Commands[seen:], "\n")
		seen = len(fr.Commands)
		return s
	}
	runReplay(t, replayDriver{
		Name: "proxmox", Replies: "replies-proxmox.ndjson", Collector: coll, Control: coll, Snapshot: snap,
		GatewayIP: "192.0.2.2",
		Written:   written,
		// VM 999 is the last guest in the capture: its scrubbed MAC is in the
		// fixture's own config line, which is what the driver rewrites.
		Expect: map[int]replayExpect{
			// 0: 404 pending, 1: adopt via mgmt_cfg, 2-3: first pushes, every port at its default
			2: {mustNot: []string{"qm set"}},
			3: {mustNot: []string{"qm set"}},
			4: {must: []string{"qm set 999 --net0 'virtio=", ",bridge=vmbr0,tag=2,trunks=10;69'"}}, // native kids (2), tagged airstream (10) + visitor (69)
			// 5: back to All. The fixture is stateless for NIC options (every
			// collect re-reads the capture, where VM 999 is at its default), so
			// this push matches the captured state and must write nothing.
			5: {mustNot: []string{"qm set"}},
		},
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
		},
	})
}
