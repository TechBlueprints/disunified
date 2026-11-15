package sonic

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"
)

// The DX010 on igmp-202511.20261115-6c8eb1cf7, a running switch (captured
// with the bridge service up, not during a boot).
const fixtureStarted = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/collect.txt"

// No collect from a booting switch was ever captured (docs/drivers/sonic.md
// §6.4.2). The boot's states are derived from the running switch's capture
// by taking away what the boot's journal shows absent at each moment.

// withoutPortInit: the switch before portsyncd's port init -- APPL_DB
// PORT_TABLE without its PortInitDone marker.
func withoutPortInit(t *testing.T, fr *FixtureRunner) {
	t.Helper()
	ap := fr.table("appl_port")
	if _, ok := ap["PORT_TABLE:PortInitDone"]; !ok {
		t.Fatal("the capture has no PORT_TABLE:PortInitDone")
	}
	delete(ap, "PORT_TABLE:PortInitDone")
	fr.setTable("appl_port", ap)
}

// withoutVLANNetdevs: the kernel before vlanmgrd made the Vlan<id>
// netdevs -- `ip -j addr` without them (and so without their addresses).
func withoutVLANNetdevs(t *testing.T, fr *FixtureRunner) {
	t.Helper()
	var links []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(fr.section("addr"))), &links); err != nil {
		t.Fatal(err)
	}
	var kept []map[string]any
	for _, l := range links {
		if n, _ := l["ifname"].(string); !strings.HasPrefix(n, "Vlan") {
			kept = append(kept, l)
		}
	}
	if len(kept) == len(links) {
		t.Fatal("the capture has no Vlan netdev")
	}
	b, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	fr.setSection("addr", string(b))
}

// bootRunner serves the collects of a switch that is still starting, in
// order, then the started switch's.
type bootRunner struct {
	*FixtureRunner
	boot     []string
	collects int
}

func (b *bootRunner) Run(ctx context.Context, command, stdin string) (string, error) {
	if command == collectCommand {
		b.collects++
		if len(b.boot) > 0 {
			out := b.boot[0]
			b.boot = b.boot[1:]
			return out, nil
		}
	}
	return b.FixtureRunner.Run(ctx, command, stdin)
}

func bootCollector(t *testing.T, states ...func(*testing.T, *FixtureRunner)) (*Collector, *bootRunner, string) {
	t.Helper()
	fr, err := NewFixtureRunner(fixtureStarted)
	if err != nil {
		t.Fatal(err)
	}
	dial := ifaceAddr(t, fr.Fixture, "Vlan4094")
	br := &bootRunner{FixtureRunner: fr}
	started := fr.Fixture
	for _, st := range states {
		st(t, fr)
		br.boot = append(br.boot, fr.Fixture)
		fr.Fixture = started
	}
	c := NewCollector(br)
	c.Log = log.New(testWriter{t}, "", 0)
	c.Dial = dial
	c.StartPoll = time.Millisecond
	return c, br, dial
}

// The boot of 2026-11-14: the first collect before swss (no port init, no
// VLAN netdevs), the next with the ports up but Vlan4094 not yet made,
// then the started switch. Start's snapshot is the last one: it carries
// the dialled address, which the IP Settings apply looks for.
func TestStartWaitsForTheSwitchToFinishStarting(t *testing.T) {
	both := func(t *testing.T, fr *FixtureRunner) { withoutPortInit(t, fr); withoutVLANNetdevs(t, fr) }
	c, br, dial := bootCollector(t, both, withoutVLANNetdevs)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if br.collects != 3 {
		t.Errorf("Start collected %d times, want 3 (two while starting, one started)", br.collects)
	}
	if len(snap.System.Addresses) == 0 || snap.System.Addresses[0].IP != dial || snap.System.Addresses[0].Iface != "Vlan4094" {
		t.Errorf("Start's snapshot addresses %+v, want %s on Vlan4094 first", snap.System.Addresses, dial)
	}
}

// A switch that never finishes starting fails Start (main.go retries it)
// rather than handing the loop a half-started snapshot.
func TestStartFailsOnASwitchThatDoesNotFinishStarting(t *testing.T) {
	c, br, _ := bootCollector(t, withoutPortInit, withoutPortInit, withoutPortInit)
	c.StartWait = 0
	_, err := c.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "has not finished starting") || !strings.Contains(err.Error(), "PortInitDone") {
		t.Fatalf("Start: %v, want a not-finished-starting error naming PortInitDone", err)
	}
	if br.collects != 1 {
		t.Errorf("Start collected %d times with no wait allowed, want 1", br.collects)
	}
}

func TestStartingWhy(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureStarted)
	if err != nil {
		t.Fatal(err)
	}
	dial := ifaceAddr(t, fr.Fixture, "Vlan4094")
	why := func(capture string, dial ...string) string {
		t.Helper()
		sec := sections(capture)
		db := map[string]table{}
		for _, n := range []string{"appl_port", "cfg_mgmt"} {
			tb, err := parseTable(n, sec[n])
			if err != nil {
				t.Fatal(err)
			}
			db[n] = tb
		}
		var addrs []ipAddr
		if err := decodeJSON("addr", sec["addr"], &addrs); err != nil {
			t.Fatal(err)
		}
		return startingWhy(db, addrs, dial)
	}
	started := fr.Fixture
	if w := why(started, dial); w != "" {
		t.Errorf("the running switch: %q, want started", w)
	}
	withoutPortInit(t, fr)
	if w := why(fr.Fixture, dial); !strings.Contains(w, "PortInitDone") || strings.Contains(w, "kernel") {
		t.Errorf("without port init: %q, want PortInitDone only", w)
	}
	fr.Fixture = started
	withoutVLANNetdevs(t, fr)
	if w := why(fr.Fixture, dial); !strings.Contains(w, "puts "+dial+" on Vlan4094") || strings.Contains(w, "PortInitDone") {
		t.Errorf("without the VLAN netdevs: %q, want the dialled address on Vlan4094 only", w)
	}
	// An address CONFIG_DB does not hold says nothing about starting: a
	// dial target elsewhere, or none at all.
	if w := why(fr.Fixture, "192.0.2.254"); w != "" {
		t.Errorf("dialling an address CONFIG_DB does not hold: %q, want started", w)
	}
	if w := why(fr.Fixture); w != "" {
		t.Errorf("no dial target: %q, want started", w)
	}
}
