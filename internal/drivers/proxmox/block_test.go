package proxmox

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// UniFi's client Block on a node (block.go, docs/drivers/proxmox.md §4d):
// the two clients a bridged switch's setparam carried while a test client
// was blocked beside the site's one (controller-11.0.99/
// replies-arista-block.ndjson; every bridged switch gets the same list),
// against node 2's capture with nft.

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

func TestBlockedClientsOnANode(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2-nft.txt")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	macs := capturedBlocked(t)
	d := devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: macs}
	n, err := c.ApplyDevice(context.Background(), d)
	if err != nil || n != 1 {
		t.Fatalf("ApplyDevice = %d, %v; want 1 change", n, err)
	}
	cmds := commandsWith(r, "nft")
	if len(cmds) != 1 || cmds[0] != "nft -f -" {
		t.Fatalf("commands %q, want one nft -f -", cmds)
	}
	text, mark := blockRuleset(c.Bridge, macs)
	want := "table bridge disunified_block\ndelete table bridge disunified_block\n" + text
	if got := r.Stdins[len(r.Stdins)-1]; got != want {
		t.Errorf("nft input\n%s\nwant\n%s", got, want)
	}
	rule := `meta ibrname "` + c.Bridge + `" ether saddr { ` + strings.Join(macs, ", ") + ` } counter drop`
	if !strings.Contains(text, rule) || !strings.Contains(text, `comment "dui:`+mark+`"`) || !strings.Contains(text, "hook prerouting") {
		t.Errorf("ruleset:\n%s", text)
	}
	if n, err := c.ApplyDevice(context.Background(), d); n != 0 || err != nil {
		t.Errorf("second apply = %d, %v; want nothing", n, err)
	}
	// A collect whose script printed no macblock section (an older
	// capture) keeps what the driver wrote.
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.ApplyDevice(context.Background(), d); n != 0 {
		t.Errorf("after a collect: %d changes", n)
	}
	// One unblocked: the table is replaced; none: it goes.
	if n, _ := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: macs[1:]}); n != 1 {
		t.Errorf("one unblock: %d changes", n)
	}
	if n, _ := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{BlockedSet: true}); n != 1 {
		t.Errorf("unblock: %d changes", n)
	}
	cmds = commandsWith(r, "nft")
	if last := cmds[len(cmds)-1]; !strings.HasPrefix(last, "nft delete table bridge disunified_block") {
		t.Errorf("last command %q", last)
	}
	// Not said yet: nothing.
	if n, _ := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{}); n != 0 {
		t.Errorf("unknown list: %d changes", n)
	}
}

// The node's own bridge MAC is never blocked.
func TestBlockedNodeItselfIsHeld(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2-nft.txt")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: []string{c.bridgeMAC}}
	c.ApplyDevice(context.Background(), d)
	c.ApplyDevice(context.Background(), d)
	// The capture prints no macblock section, so the driver makes sure of
	// "no table" once; it never writes a rule for the node itself.
	if cmds := commandsWith(r, "nft"); len(cmds) != 1 || !strings.HasPrefix(cmds[0], "nft delete table") {
		t.Errorf("the node blocked itself, or the removal repeats: %q", cmds)
	}
}
