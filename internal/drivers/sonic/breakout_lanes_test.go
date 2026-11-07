package sonic

import (
	"context"
	"log"
	"strings"
	"testing"
)

// The DX010 on igmp-202511.20261106-f2e25f54c, captured 2026-11-07 during
// the breakout run (docs/drivers/sonic.md 11.11): collect.txt with every cage
// whole and eth0's address row removed, collect-4x25g.txt with cage 2 split
// to 4x25G. Each lane's STATE_DB supported_speeds is the SAI's answer for it
// at that breakout: 1000,10000,25000 for a 25G lane, 40000,100000 for a
// whole cage.
const (
	fixture20261106      = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect.txt"
	fixture20261106Split = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-4x25g.txt"
)

func rig20261106(t *testing.T, path string) *controlRig {
	t.Helper()
	fr, err := NewFixtureRunner(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	c := NewCollector(fr)
	c.Log = log.New(&buf, "", 0)
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &controlRig{t: t, c: c, fr: fr, log: &buf}
}

// A split cage claims 1G: its lanes' supported_speeds say so, and the claim
// merges them. No breakout mode lists 1G (platform.json: 1x100G[40G], 2x50G,
// 4x25G[10G]), and the driver used to refuse the speed it claimed. A 1G push
// on the split cage is now a lane speed on every lane -- what `config
// interface speed` accepts, since portconfig checks supported_speeds alone --
// and back to 25G the same way; nothing is broken out for either.
func TestSplitCageRunsTheLanesOwnSpeeds(t *testing.T) {
	r := rig20261106(t, fixture20261106Split)
	base := r.baseline()
	if got, _ := r.apply(base); len(got) != 0 {
		t.Fatalf("the capture as it is needs no write: %q", got)
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := port(snap, 2); p.Lanes != 4 || !containsInt(p.SpeedCaps, 1000) {
		t.Fatalf("cage 2: %d lanes, caps %v (the split claims 1G)", p.Lanes, p.SpeedCaps)
	}
	snap = r.expect("1G", speedOn(base, 2, 1000), []string{
		"sudo config interface speed Ethernet4 1000", "sudo config interface speed Ethernet5 1000",
		"sudo config interface speed Ethernet6 1000", "sudo config interface speed Ethernet7 1000", save})
	if p := port(snap, 2); p.Lanes != 4 || p.AdminSpeed != 1000 {
		t.Errorf("read back: %d lanes at %d", p.Lanes, p.AdminSpeed)
	}
	r.expect("back to 25G", speedOn(base, 2, 25000), []string{
		"sudo config interface speed Ethernet4 25000", "sudo config interface speed Ethernet5 25000",
		"sudo config interface speed Ethernet6 25000", "sudo config interface speed Ethernet7 25000", save})
	if strings.Contains(r.log.String(), "not among the cage's breakout speeds") {
		t.Errorf("refused: %s", r.log.String())
	}
	// The CLI refuses what the lane does not run, as portconfig does.
	if _, err := r.fr.Run(context.Background(), batchCommand, "set -e\nsudo config interface speed Ethernet4 40000\n"); err == nil || !strings.Contains(err.Error(), "Invalid speed specified: 40000") {
		t.Errorf("40G on a 25G lane: %v", err)
	}
}

// A whole cage does not claim 1G (its supported_speeds are 40000,100000),
// and a 1G push on it is still refused with the note, nothing written.
func TestWholeCageRefusesASpeedNoModeOrLaneRuns(t *testing.T) {
	r := rig20261106(t, fixture20261106)
	base := r.baseline()
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := port(snap, 2); containsInt(p.SpeedCaps, 1000) {
		t.Fatalf("a whole cage claims 1G: %v", p.SpeedCaps)
	}
	if got, _ := r.apply(speedOn(base, 2, 1000)); len(got) != 0 {
		t.Fatalf("wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "port 2: 1000 Mb/s is not among the cage's breakout speeds") {
		t.Errorf("no note: %s", r.log.String())
	}
}

// On this image, with eth0's address row gone, a breakout validates: the
// split, the lanes at 10G, 2x50G and the join, as the DX010 ran them live.
func TestBreakoutModesOnTheCurrentImage(t *testing.T) {
	r := rig20261106(t, fixture20261106)
	base := r.baseline()
	got, _ := r.apply(speedOn(base, 2, 25000))
	if len(got) == 0 || got[0] != "sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null" {
		t.Fatalf("4x25G: %q\n%s", got, r.log.String())
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ = r.apply(speedOn(base, 2, 10000))
	if strings.Join(got, ";") != "sudo config interface speed Ethernet4 10000;sudo config interface speed Ethernet5 10000;sudo config interface speed Ethernet6 10000;sudo config interface speed Ethernet7 10000;"+save {
		t.Errorf("10G: %q", got)
	}
	got, _ = r.apply(speedOn(base, 2, 50000))
	if len(got) == 0 || got[0] != "sudo config interface breakout -f -y Ethernet4 '2x50G' </dev/null" {
		t.Errorf("2x50G: %q", got)
	}
	got, _ = r.apply(speedOn(base, 2, 100000))
	if len(got) == 0 || got[0] != "sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null" {
		t.Errorf("join: %q", got)
	}
}
