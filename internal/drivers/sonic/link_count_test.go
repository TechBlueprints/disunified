package sonic

import (
	"path/filepath"
	"testing"
)

// A split cage with several lanes up (the DX010, 2026-11-15: empty cage 32
// split 4x25G, lanes 1-3 in MAC loopback, lane 1 dropped and brought back
// once, lane 4 down; docs/drivers/sonic.md §5). portsorch's flap_count
// counts every oper change, so each lane's drops are half of it, rounded
// down, and the cage reports their sum: the old fold added the lanes'
// flap_counts and counted each up lane as half a drop.
func TestSplitCageLinkDrops(t *testing.T) {
	fr, err := NewFixtureRunner(filepath.Join(ucastFixtures, "collect-split-loop.txt"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := NewCollector(fr).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	appl := parseTableMust(t, sections(fr.Fixture), "appl_port")
	var drops, flaps uint64
	ups := 0
	for _, lane := range []string{"Ethernet124", "Ethernet125", "Ethernet126", "Ethernet127"} {
		f := appl["PORT_TABLE:"+lane]
		if f == nil {
			t.Fatalf("the capture has no %s: not split", lane)
		}
		n := atou(f["flap_count"])
		drops, flaps = drops+n/2, flaps+n
		if f["oper_status"] == "up" {
			ups++
		}
	}
	if ups < 2 || drops != 1 || flaps/2 == drops {
		t.Fatalf("the capture is not the run described: %d lanes up, %d drops, %d flaps", ups, drops, flaps)
	}
	for _, p := range snap.Ports {
		if p.Index != 32 {
			continue
		}
		if p.Lanes != 4 || !p.Up {
			t.Fatalf("cage 32: %d lanes, up %v", p.Lanes, p.Up)
		}
		if got := p.Health.LinkChanges / 2; got != drops || p.Health.LinkChanges%2 != 1 {
			t.Errorf("cage 32 LinkChanges %d (%d drops), want %d drops and up (odd)", p.Health.LinkChanges, got, drops)
		}
		return
	}
	t.Fatal("no port 32")
}
