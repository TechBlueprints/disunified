package informloop

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/proxmox"
)

// A guest that arrives on the node between the bridge's start and its first
// cycle. The two captures are the same node on two days: in one VM 999 runs
// on another node of the cluster (its port 25 here is reserved by its tag
// and has no interface behind it), in the other it runs here, behind
// tap999i0. The bridge restarts holding the recorded push that puts port 25
// on native VLAN 2 (reply 4 of replies-proxmox.ndjson), so the first-push
// hold is off; it starts, and provisions the controller, from the capture
// without the guest, and its first collect is the one with it. The stored
// push was never matched against the guest that arrived, so the reconcile of
// that cycle must not write it; the recorded push back to "All" (reply 5)
// is what the guest has, and makes the port an ordinary one again.
func TestReplayProxmoxOccupantChangesBetweenStartAndFirstCollect(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "fixtures", "proxmox-9.1.6")
	fr, err := proxmox.NewFixtureRunner(filepath.Join(dir, "collect-node2-nft.txt"))
	if err != nil {
		t.Fatal(err)
	}
	arrived, err := os.ReadFile(filepath.Join(dir, "collect-node2.txt"))
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
	fr.Fixture = string(arrived) // every collect of the run finds the guest here
	seen := len(fr.Commands)
	written := func() string {
		s := strings.Join(fr.Commands[seen:], "\n")
		seen = len(fr.Commands)
		return s
	}

	// The push the bridge had applied before it stopped, as recorded.
	var stored struct {
		CfgVersion string `json:"cfgversion"`
		SystemCfg  string `json:"system_cfg"`
	}
	if err := json.Unmarshal(loadReplies(t, "replies-proxmox.ndjson")[4].Payload, &stored); err != nil || stored.SystemCfg == "" {
		t.Fatalf("reply 4 is not a system_cfg push: %v", err)
	}

	const withheld = "port 25 (vm999-net0/tap999i0) is new since the controller's config"
	runReplay(t, replayDriver{
		Name: "proxmox-seed", Replies: "replies-proxmox.ndjson", From: 5,
		Collector: coll, Control: coll, Snapshot: snap,
		Start: &device.State{
			Adopted: true, Key: "0123456789abcdef0123456789abcdef", UseAESGCM: true,
			CfgVersion: stored.CfgVersion, SystemCfg: stored.SystemCfg,
		},
		GatewayIP: "192.0.2.2",
		Written:   written,
		Before: func(t *testing.T, l *Loop, written func() string, logText func() string) {
			ctx := context.Background()
			l.collect(ctx)
			l.reconcile(ctx)
			if w := written(); strings.Contains(w, "qm set 999 --net0") {
				t.Errorf("the stored push was written to port 25, whose guest arrived after the start:\n%s", w)
			}
			if _, fresh := l.freshPorts[25]; !fresh {
				t.Errorf("port 25 is not fresh after the first collect; fresh: %v", l.freshPorts)
			}
			if s := logText(); !strings.Contains(s, "port layout changed") || !strings.Contains(s, withheld) {
				t.Errorf("the change and the withheld port are not logged:\n%s", s)
			}
		},
		Expect: map[int]replayExpect{
			0: {mustNot: []string{"qm set 999 --net0"}}, // back to All: what the port has
		},
		Check: func(t *testing.T, i int, rec replayRecord, sess *device.Session, l *Loop, logText string) {
			if _, fresh := l.freshPorts[25]; fresh {
				t.Errorf("port 25 is still fresh after the push that matches it")
			}
		},
	})
}
