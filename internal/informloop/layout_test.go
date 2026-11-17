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
	"github.com/TechBlueprints/disunified/internal/drivers/proxmox"
)

// layoutRun is one run of a bridge on the Proxmox node captures: started
// from one capture, holding the recorded push that puts VM 999's port (25)
// on native VLAN 2 (reply 4 of replies-proxmox.ndjson) as already applied,
// with its state in dir. The captures are the same node on two days: in
// collect-node2-nft.txt VM 999 runs on another node, in collect-node2.txt
// it runs here behind tap999i0.
type layoutRun struct {
	t    *testing.T
	fr   *proxmox.FixtureRunner
	l    *Loop
	log  strings.Builder
	seen int
}

func startLayoutRun(t *testing.T, startCapture, dir string) *layoutRun {
	t.Helper()
	r := &layoutRun{t: t}
	var err error
	r.fr, err = proxmox.NewFixtureRunner(layoutCapture(startCapture))
	if err != nil {
		t.Fatal(err)
	}
	coll := proxmox.NewCollector(r.fr)
	coll.ManageLLDP = false
	coll.Log = log.New(os.Stderr, "", 0)
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		CfgVersion string `json:"cfgversion"`
		SystemCfg  string `json:"system_cfg"`
	}
	if err := json.Unmarshal(loadReplies(t, "replies-proxmox.ndjson")[4].Payload, &stored); err != nil || stored.SystemCfg == "" {
		t.Fatalf("reply 4 is not a system_cfg push: %v", err)
	}
	desc, err := device.DescriptorFor("UDC48X6", snap, device.Identity{MAC: snap.System.MAC, IP: "192.0.2.9", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	const url = "http://192.0.2.1:8080/inform"
	sess := device.NewSession(desc, url, device.State{
		InformURL: url, Adopted: true, Key: "0123456789abcdef0123456789abcdef", UseAESGCM: true,
		CfgVersion: stored.CfgVersion, SystemCfg: stored.SystemCfg,
	}, nil, time.Now())
	sess.SetSnapshot(snap)
	r.l, err = New(desc, sess, Config{
		Logger: log.New(&r.log, "", 0), Interval: time.Minute, CollectTimeout: 10 * time.Second,
		Collector: coll, Controller: coll, StateDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.seen = len(r.fr.Commands)
	return r
}

func layoutCapture(name string) string {
	return filepath.Join("..", "..", "docs", "fixtures", "proxmox-9.1.6", name)
}

// now is what every later collect of the run reads.
func (r *layoutRun) now(capture string) {
	r.t.Helper()
	b, err := os.ReadFile(layoutCapture(capture))
	if err != nil {
		r.t.Fatal(err)
	}
	r.fr.Fixture = string(b)
}

// cycle is the part of a cycle that does not need the controller: collect,
// then the reconcile of the stored push. It returns the NIC writes made.
func (r *layoutRun) cycle() string {
	ctx := context.Background()
	r.l.collect(ctx)
	r.l.reconcile(ctx)
	var out []string
	for _, c := range r.fr.Commands[r.seen:] {
		if strings.Contains(c, "--net") {
			out = append(out, c)
		}
	}
	r.seen = len(r.fr.Commands)
	return strings.Join(out, "\n")
}

const (
	guestAway = "collect-node2-nft.txt"
	guestHere = "collect-node2.txt"
)

// A start whose first collect equals its startup snapshot is as it was:
// nothing fresh, nothing logged about the layout, and the stored push is
// reconciled onto the guest it was applied to.
func TestLayoutSeedLeavesAnUnchangedStartAlone(t *testing.T) {
	for _, dir := range []string{"", t.TempDir()} {
		r := startLayoutRun(t, guestHere, dir)
		if w := r.cycle(); !strings.Contains(w, "qm set 999 --net0") {
			t.Errorf("state dir %q: the stored push must reach port 25, whose guest was there at the start; wrote:\n%s", dir, w)
		}
		if len(r.l.freshPorts) != 0 || strings.Contains(r.log.String(), "layout") || strings.Contains(r.log.String(), "is new since") {
			t.Errorf("state dir %q: fresh %v, log:\n%s", dir, r.l.freshPorts, r.log.String())
		}
	}
}

// A port withheld when the bridge stops is withheld when it starts again:
// the guest arrived during the first run and its port was fresh; the second
// run starts with the guest already there, so its own snapshots agree, and
// only the first run's record says the controller's config was never
// matched against it.
func TestFreshPortSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	r := startLayoutRun(t, guestAway, dir)
	r.now(guestHere)
	if w := r.cycle(); w != "" || r.l.freshPorts[25] == "" {
		t.Fatalf("first run: port 25 must be fresh and unwritten; fresh %v, wrote:\n%s", r.l.freshPorts, w)
	}

	r = startLayoutRun(t, guestHere, dir)
	if w := r.cycle(); w != "" {
		t.Errorf("after the restart the stored push was written to the port that was withheld:\n%s", w)
	}
	if r.l.freshPorts[25] == "" || !strings.Contains(r.log.String(), "port 25 (vm999-net0/tap999i0) is new since the controller's config") {
		t.Errorf("after the restart: fresh %v, log:\n%s", r.l.freshPorts, r.log.String())
	}
	if strings.Contains(r.log.String(), "port layout changed") {
		t.Errorf("nothing changed since the record; log:\n%s", r.log.String())
	}
}

// A guest that arrives while the bridge is stopped is fresh at the first
// collect of the next run, though that run's startup snapshot already has it.
func TestOccupantChangedWhileStoppedIsFresh(t *testing.T) {
	dir := t.TempDir()
	r := startLayoutRun(t, guestAway, dir)
	if w := r.cycle(); w != "" || len(r.l.freshPorts) != 0 {
		t.Fatalf("first run: nothing to write, nothing fresh; fresh %v, wrote:\n%s", r.l.freshPorts, w)
	}

	changed := 0
	r = startLayoutRun(t, guestHere, dir)
	r.l.cfg.OnLayoutChange = func(*devicemodel.Snapshot) { changed++ }
	if w := r.cycle(); w != "" {
		t.Errorf("the stored push was written to a guest that arrived while the bridge was stopped:\n%s", w)
	}
	if r.l.freshPorts[25] == "" || changed != 1 {
		t.Errorf("fresh %v, layout-change calls %d (the controller is seeded from it); log:\n%s", r.l.freshPorts, changed, r.log.String())
	}
}

// An unreadable record is logged and the run starts from its own snapshot.
func TestUnreadableLayoutRecordFallsBackToTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LayoutFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startLayoutRun(t, guestAway, dir)
	if !strings.Contains(r.log.String(), "not loaded") {
		t.Errorf("log:\n%s", r.log.String())
	}
	r.now(guestHere)
	if w := r.cycle(); w != "" || r.l.freshPorts[25] == "" {
		t.Errorf("fresh %v, wrote:\n%s", r.l.freshPorts, w)
	}
}
