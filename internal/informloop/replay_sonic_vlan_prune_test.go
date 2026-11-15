package informloop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// A VLAN a tagged set adds and then drops, live on the DX010 on Network
// 11.0.99 (docs/drivers/sonic.md §11.29, 2026-11-15 21:33-21:40 UTC, a
// test deb of the delete): the switch as captured right after the run
// (sonic-igmp-202511.20261115-6c8eb1cf7/collect-vlan-prune.txt: Vlan4094
// only, PortChannel1 the uplink), and the bridge's own reply log of the
// run (controller-11.0.99/replies-dx010-vlan-prune.ndjson, scrubbed with
// scripts/sanitize-controller.py, its addresses seeded with the capture's
// scrub, the switch's name aligned with it, host-1):
//
//   - 0, 1: reconciles under the push in force before the run (whose
//     system_cfg is byte for byte record 4's; only the cfgversion differs);
//   - 2: port 20 (Ethernet76, an empty cage) set to a custom tagged set
//     with one network, VLAN 4010, through the controller's REST as the UI
//     saves it: the push lists 4010 with port 20 tagged and 31 exclude;
//   - 3: a reconcile (live, the new VLAN's kernel MTU followed here: the
//     capture has no such netdev to read);
//   - 4: port 20 back to its pre-test override: the push lists VLAN 1 only.
func TestReplaySonicDX010VLANPrune(t *testing.T) {
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7", "collect-vlan-prune.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fr.OrchL2, fr.OrchL2MCFix = "1", "1" // the image's orchagent, as in TestReplaySonicDX010Network11
	fr.CurrentL2d()
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
	recs := loadReplies(t, "controller-11.0.99/replies-dx010-vlan-prune.ndjson")
	if len(recs) != 5 {
		t.Fatalf("%d records in replies-dx010-vlan-prune.ndjson, want 5", len(recs))
	}
	var last struct {
		SystemCfg string `json:"system_cfg"`
	}
	if err := json.Unmarshal(recs[4].Payload, &last); err != nil || last.SystemCfg == "" {
		t.Fatalf("record 4 carries no system_cfg: %v", err)
	}
	seen, cur := 0, ""
	written := func() string {
		c := fr.Recorded()
		cur = strings.Join(c[seen:], "\n")
		seen = len(c)
		return cur
	}
	got := map[int]string{}
	runReplay(t, replayDriver{
		Name: "sonic-dx010-vlan-prune", Replies: "controller-11.0.99/replies-dx010-vlan-prune.ndjson", Model: "USWF07D",
		Collector: coll, Control: coll, Snapshot: snap,
		Start:     &device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", UseAESGCM: true, CfgVersion: "34a7a710933a053b", SystemCfg: last.SystemCfg},
		GatewayIP: "192.0.2.5",
		Written:   written,
		Loop: func(c *Config) {
			c.AddressController = coll
			c.ControlIGMP, c.ControlNTP, c.ControlSyslog, c.ControlHostname = true, true, true, true
			c.ControlReboot, c.ControlSNMP = true, true
			c.DefaultPortNames = map[int][]string{}
			for _, p := range snap.Ports {
				c.DefaultPortNames[p.Index] = append([]string{fmt.Sprintf("Port %d", p.Index), fmt.Sprintf("QSFP28 %d", p.Index)}, coll.DefaultPortNames(p)...)
			}
		},
		Check: func(t *testing.T, i int, rec replayRecord, sess *device.Session, l *Loop, logText string) {
			got[i] = cur
			if l.state != StateConnected {
				t.Errorf("reply %d: state %v, want CONNECTED throughout", i, l.state)
			}
		},
	})
	save := "sudo config save -y"
	want := map[int][]string{
		// As live: the VLAN made (at the ports' MTU), then port 20 tagged.
		2: {"sudo config vlan add 4010", "sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan4010' mtu 1500", save,
			"sudo config vlan member add 4010 Ethernet76", save},
		// As live: the member out, then the VLAN no push names.
		4: {"sudo config vlan member del 4010 Ethernet76", save, "sudo config vlan del 4010", save},
	}
	for i := range recs {
		w, ok := want[i]
		switch {
		case ok && got[i] != strings.Join(w, "\n"):
			t.Errorf("record %d: commands\n  got  %q\n  want %q", i, strings.Split(got[i], "\n"), w)
		case !ok && got[i] != "":
			t.Errorf("record %d (%s) wrote %q; a reconcile must write nothing", i, replyKind(recs[i]), got[i])
		}
	}
	end, err := coll.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(end.VLANs) != "[1]" {
		t.Errorf("VLANs after the run: %v, want only VLAN 1 (Vlan4094)", end.VLANs)
	}
}
