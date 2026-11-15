package informloop

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
)

// The Inter-VLAN transit leaving the Arista's push, replayed:
// controller-11.0.99/replies-arista-transit.ndjson is the deployed bridge's
// own reply log from 17:43 to 17:50 UTC on 2026-11-15, while the DX010 ran
// a routed network: the push of 17:43:34 names VLAN 4040 (the transit,
// every Arista port Allow All), the one of 17:46:34 no longer does, then
// two noops. It is scrubbed with the address mapping of the EOS capture it
// runs over (vlan-prune/driver: the switch after the driver's live run, with
// 4040 still on it and two spare VLANs a hand-made SVI and a trunk group
// hold). The first push writes no VLAN (4040 is there); the second
// deletes 4040 and the driver's IGMP line for it, and nothing else; the
// noops write nothing.
// docs/drivers/arista-eos.md §12.
func TestReplayAristaTransitVLAN(t *testing.T) {
	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M", "vlan-prune", "driver"))
	coll := aristaeos.NewCollector(ft)
	var drv strings.Builder
	coll.Log = log.New(&drv, "", 0)
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	var writes []string
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
	runReplay(t, replayDriver{
		Name: "arista-eos-transit", Replies: "controller-11.0.99/replies-arista-transit.ndjson", Collector: coll, Control: coll, Snapshot: snap,
		Start:     &device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", UseAESGCM: true},
		GatewayIP: "192.0.2.2",
		Written:   written,
		Loop:      func(c *Config) { c.AllowInitialChanges = true },
	})
	want := []string{"", "", "enable | configure | ip igmp snooping vlan 4040 | no vlan 4040 | end | write memory", "", ""}
	if len(writes) != len(want) {
		t.Fatalf("%d replies written, want %d: %q", len(writes), len(want), writes)
	}
	// The capture is of 2026-11-15 22:24 UTC, after the re-cabling of that
	// afternoon (ports 51-52 in Port-Channel1, the ports renamed): the
	// 17:43 push, older, puts the ports back as they were then, in one
	// port batch, and writes no VLAN (4040 is there).
	if b := strings.Split(writes[0], "\n"); len(b) != 1 || !strings.Contains(b[0], "| interface Ethernet51/1 | description Ethernet51/1 | no channel-group |") || strings.Contains(b[0], "vlan 4040") {
		t.Errorf("reply 0 is not one port batch without a VLAN write:\n%s", writes[0])
	}
	for i := 1; i < len(want); i++ {
		if writes[i] != want[i] {
			t.Errorf("reply %d wrote:\n%s\nwant:\n%s", i, writes[i], want[i])
		}
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("writes the fixture transport could not show: %q", ft.Unmodelled)
	}
	for _, n := range []string{"interface Vlan3977 exists", `"trunk group dui-prune-test"`} {
		if strings.Count(drv.String(), n) != 1 {
			t.Errorf("note %q logged %d times, want once", n, strings.Count(drv.String(), n))
		}
	}
}
