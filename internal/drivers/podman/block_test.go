package podman

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// UniFi's client Block on a Podman host (block.go, docs/drivers/podman.md
// §9): the two clients a bridged switch's setparam carried while a test
// client was blocked beside the site's one (controller-11.0.99/
// replies-arista-block.ndjson; every bridged switch gets the same list),
// against the host's capture of 2026-11-17, the first whose script lists
// nft and the driver's table (absent there: nothing has been written on
// the host).

const blockFixture = "../../../docs/fixtures/podman-5.8.2/collect-block.txt"

func capturedBlocked(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "docs", "fixtures", "controller-11.0.99", "replies-arista-block.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var rec struct {
			Payload struct {
				BlockedSTA *string `json:"blocked_sta"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Payload.BlockedSTA != nil {
			if macs := device.ParseBlockedSTA(*rec.Payload.BlockedSTA); len(macs) == 2 {
				return macs
			}
		}
	}
	t.Fatal("capture changed: no setparam with two blocked clients")
	return nil
}

func startBlockFixture(t *testing.T, path string) (*Collector, *FixtureRunner, *devicemodel.Snapshot) {
	t.Helper()
	fr, err := NewFixtureRunner(path)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, fr, snap
}

func nftCommands(fr *FixtureRunner) (cmds, stdins []string) {
	for i, c := range fr.Commands {
		if strings.HasPrefix(c, "nft ") {
			cmds = append(cmds, c)
			stdins = append(stdins, fr.Stdins[i])
		}
	}
	return
}

func TestBlockedClientsOnTheUplink(t *testing.T) {
	ctx := context.Background()
	c, fr, _ := startBlockFixture(t, blockFixture)
	uplink := strings.TrimSpace(fr.Section("uplink"))
	if uplink == "" || !c.nft || !c.block.Known || c.block.Present {
		t.Fatalf("capture: uplink %q, nft %v, table %+v; want an uplink, nft, the table listed and absent", uplink, c.nft, c.block)
	}
	macs := capturedBlocked(t)
	d := devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: macs}
	if n, err := c.ApplyDevice(ctx, d); err != nil || n != 1 {
		t.Fatalf("ApplyDevice = %d, %v; want 1 change", n, err)
	}
	cmds, stdins := nftCommands(fr)
	if len(cmds) != 1 || cmds[0] != "nft -f -" {
		t.Fatalf("commands %q, want one nft -f -", cmds)
	}
	text, mark := blockRuleset(uplink, macs)
	if want := "table netdev disunified_block\ndelete table netdev disunified_block\n" + text; stdins[0] != want {
		t.Errorf("nft input\n%s\nwant\n%s", stdins[0], want)
	}
	rule := "ether saddr { " + strings.Join(macs, ", ") + " } counter drop"
	for _, want := range []string{
		`type filter hook ingress device "` + uplink + `" priority filter; policy accept;`,
		`type filter hook egress device "` + uplink + `" priority filter; policy accept;`,
		`comment "dui:` + mark + `"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ruleset lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, rule) != 2 {
		t.Errorf("want the drop in both chains:\n%s", text)
	}
	// Idempotent while the driver's own write stands.
	if n, err := c.ApplyDevice(ctx, d); n != 0 || err != nil {
		t.Errorf("second apply = %d, %v; want nothing", n, err)
	}
	// One unblocked: the table is replaced; none: it goes, once.
	if n, _ := c.ApplyDevice(ctx, devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: macs[1:]}); n != 1 {
		t.Errorf("one unblock: %d changes", n)
	}
	if n, _ := c.ApplyDevice(ctx, devicemodel.DeviceDesired{BlockedSet: true}); n != 1 {
		t.Errorf("unblock: %d changes", n)
	}
	if n, _ := c.ApplyDevice(ctx, devicemodel.DeviceDesired{BlockedSet: true}); n != 0 {
		t.Errorf("unblock again: %d changes", n)
	}
	cmds, _ = nftCommands(fr)
	if len(cmds) != 3 || !strings.HasPrefix(cmds[2], "nft delete table netdev disunified_block") {
		t.Errorf("commands %q, want two writes and one delete", cmds)
	}
	// Not said yet: nothing.
	if n, _ := c.ApplyDevice(ctx, devicemodel.DeviceDesired{}); n != 0 {
		t.Errorf("unknown list: %d changes", n)
	}
}

// nft tables do not outlive a reboot. The capture lists the table as
// absent (the fixture runner does not model the write: no listing of a
// present table has been captured), which is what a host says after one:
// the next apply writes it again. With none blocked and none listed,
// nothing is written.
func TestBlockTableLostIsWrittenAgain(t *testing.T) {
	ctx := context.Background()
	c, fr, _ := startBlockFixture(t, blockFixture)
	if n, _ := c.ApplyDevice(ctx, devicemodel.DeviceDesired{BlockedSet: true}); n != 0 {
		t.Errorf("none blocked, no table: %d changes", n)
	}
	d := devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: capturedBlocked(t)}
	for i := 0; i < 2; i++ {
		if n, err := c.ApplyDevice(ctx, d); n != 1 || err != nil {
			t.Fatalf("apply %d = %d, %v; want 1", i, n, err)
		}
		if _, err := c.Collect(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if cmds, _ := nftCommands(fr); len(cmds) != 2 {
		t.Errorf("commands %q, want the table written twice", cmds)
	}
}

// The host's own uplink MAC is never blocked: on the uplink's egress it is
// the source of everything the host sends.
func TestBlockedHostItselfIsHeld(t *testing.T) {
	ctx := context.Background()
	c, fr, snap := startBlockFixture(t, blockFixture)
	d := devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: []string{strings.ToUpper(snap.System.MAC), "not a mac"}}
	for i := 0; i < 2; i++ {
		if n, err := c.ApplyDevice(ctx, d); n != 0 || err != nil {
			t.Errorf("apply = %d, %v; want nothing", n, err)
		}
	}
	if cmds, _ := nftCommands(fr); len(cmds) != 0 {
		t.Errorf("the host blocked itself: %q", cmds)
	}
}

// A host whose collect reports no nft (here a capture from before the
// script asked) is left alone.
func TestBlockNeedsNft(t *testing.T) {
	c, fr, _ := startBlockFixture(t, fixture)
	d := devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: capturedBlocked(t)}
	if n, err := c.ApplyDevice(context.Background(), d); n != 0 || err != nil {
		t.Errorf("apply = %d, %v; want nothing", n, err)
	}
	if cmds, _ := nftCommands(fr); len(cmds) != 0 {
		t.Errorf("commands %q", cmds)
	}
}

// The new capture still reads as the host: the driver's other tests' facts
// hold on it (one uplink, the LAN container a client).
func TestBlockCaptureReadsAsTheHost(t *testing.T) {
	_, fr, snap := startBlockFixture(t, blockFixture)
	if snap.UplinkHint != topSlot || snap.System.MAC == "" {
		t.Errorf("uplink at %d, MAC %q", snap.UplinkHint, snap.System.MAC)
	}
	if len(snap.MACTable) == 0 || !strings.Contains(fr.Section("networks"), `"driver": "macvlan"`) {
		t.Errorf("want a macvlan network with a client; MAC table %v", snap.MACTable)
	}
}
