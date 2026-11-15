package informloop

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// UniFi's client Block, replayed on the DX010 (docs/drivers/sonic.md
// §11.33): its on-switch reply log while a test client was blocked beside
// the site's one and then unblocked (controller-11.0.99/
// replies-dx010-block.ndjson, scrubbed with the address seeds of
// replies-dx010.ndjson, so it is the same network as collect-net11.txt;
// the two clients seeded to 02:00:00:00:b1:01, the test client, and
// 02:00:00:00:b1:02, in their sorted order). The pushes in that window also
// carry another session's 802.1X edits on port 31, which this test leaves
// to the 802.1X replays.
//
//   - 3: the first setparam with both clients (31 s after the Block): both
//     MB_ rules, then the kernel chain;
//   - 4: the block-sta cmd, after the setparam: nothing more;
//   - 8: the setparam after the Unblock (6 s after it): the test client's
//     rule goes, the chain follows;
//   - 9: the unblock-sta cmd: nothing more.
func TestReplaySonicDX010Block(t *testing.T) {
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7", "collect-net11.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fr.OrchL2, fr.OrchL2MCFix = "1", "1"
	fr.CurrentL2d()
	fr.UseSection("mac_block", "")
	var drv strings.Builder
	coll := sonic.NewCollector(fr)
	coll.Log = log.New(&drv, "", 0)
	coll.L2 = true
	coll.IGMP = true
	coll.Dial = "192.0.2.1"
	coll.Redial = fr.Dial
	coll.StateDir = t.TempDir()
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen, last := 0, ""
	written := func() string {
		c := fr.Recorded()
		last = strings.Join(c[seen:], "\n")
		seen = len(c)
		return last
	}
	got := map[int]string{}
	runReplay(t, replayDriver{
		Name: "sonic-dx010-block", Replies: "controller-11.0.99/replies-dx010-block.ndjson", Model: "USWF07D",
		Collector: coll, Control: coll, Snapshot: snap,
		Start:     &device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", UseAESGCM: true},
		GatewayIP: "192.0.2.5",
		Written:   written,
		Loop: func(c *Config) {
			c.AddressController = coll
			c.ControlIGMP, c.ControlNTP, c.ControlSyslog, c.ControlHostname = true, true, true, true
			c.AllowInitialChanges = true
			c.DefaultPortNames = map[int][]string{}
			for _, p := range snap.Ports {
				c.DefaultPortNames[p.Index] = append([]string{fmt.Sprintf("Port %d", p.Index), fmt.Sprintf("QSFP28 %d", p.Index)}, coll.DefaultPortNames(p)...)
			}
		},
		Check: func(t *testing.T, i int, rec replayRecord, sess *device.Session, l *Loop, logText string) {
			got[i] = last
			if testing.Verbose() {
				for _, w := range strings.Split(last, "\n") {
					if strings.Contains(w, "acl") || strings.Contains(w, "portchannel") || strings.Contains(w, "mac-block") {
						if len(w) > 160 {
							w = w[:160]
						}
						t.Logf("reply %d: %s", i, w)
					}
				}
			}
		},
	})
	rule := func(mac string) string {
		return "sudo sonic-db-cli CONFIG_DB hset 'ACL_RULE|DUI_MAC_ACL|MB_" + mac + "' PACKET_ACTION 'DROP' PRIORITY '1023' SRC_MAC '" + mac + "'"
	}
	const kernel = "sudo bash -s -- disunified-mac-block"
	want := map[int][]string{
		3: {rule("02:00:00:00:b1:01"), rule("02:00:00:00:b1:02"), kernel},
		8: {"sudo sonic-db-cli CONFIG_DB del 'ACL_RULE|DUI_MAC_ACL|MB_02:00:00:00:b1:01'", kernel},
	}
	for i := 0; i < len(got); i++ {
		w := got[i]
		for _, line := range want[i] {
			if !strings.Contains("\n"+w+"\n", "\n"+line+"\n") {
				t.Errorf("reply %d: no %q in\n%s", i, line, w)
			}
		}
		if want[i] == nil && (strings.Contains(w, "MB_") || strings.Contains(w, kernel)) {
			t.Errorf("reply %d wrote the Block again:\n%s", i, w)
		}
	}
	if strings.Contains(drv.String(), "on the switch's management path") || strings.Contains(drv.String(), "blocked clients (") {
		t.Errorf("a block was held:\n%s", drv.String())
	}
}
