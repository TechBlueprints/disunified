package informloop

import (
	"context"
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The DX010's own push (system_cfg-dx010-held.txt, which carries no
// snooping keys: the controller withholds them from a device that does not
// claim the feature) with the ECS's networks and their snooping keys and
// switch.multicast.* (system_cfg-ecs-aggregation.txt; port lines left
// out), through the loop into the driver over the DX010 captured during
// the hardware test (docs/drivers/sonic.md §8.14). The loop hands the
// per-VLAN settings over; the driver writes VLAN 1's row on Vlan4094 and
// VLANs 2 and 69, drops the test VLAN (its static group before the port
// batch takes Ethernet4 out of it, back into Vlan4094), gives it the reserved groups after
// the port batch, and a reconcile writes nothing.
func TestReplaySonicIGMPSnooping(t *testing.T) {
	keep := regexp.MustCompile(`^switch\.vlan\.(\d+)\.(id|mode|status|igmp_\w+|forward_unknown_multicast)=|^switch\.multicast\.`)
	var extra []string
	for _, line := range strings.Split(readECSPush(t, "system_cfg-ecs-aggregation.txt"), "\n") {
		m := keep.FindStringSubmatch(line)
		if m == nil || m[1] == "1" && (m[2] == "id" || m[2] == "mode" || m[2] == "status") {
			continue // the DX010's push has VLAN 1 already
		}
		extra = append(extra, line)
	}
	text := strings.TrimRight(readECSPush(t, "system_cfg-dx010-held.txt"), "\n") + "\n" + strings.Join(extra, "\n") + "\n"

	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261102-e53777446", "collect-snooping.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	coll.L2 = false // spanning tree is TestReplaySonicSpanningTree's
	coll.IGMP = true
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps := coll.Capabilities(); !caps.IGMPSnooping || !caps.IGMPSnoopingAdvanced {
		t.Fatalf("the controller pushes these keys only to a device that claims them: %+v", caps)
	}
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = coll.DefaultPortNames(p)
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: text}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, AllowInitialChanges: true, DefaultPortNames: defaults, ControlIGMP: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if !l.applyPending(ctx) {
		t.Fatal("the push was not consumed")
	}
	if len(fr.L2MCScripts) != 2 {
		t.Fatalf("%d IGMP scripts, want the push's and the one after the port batch:\n%s", len(fr.L2MCScripts), buf.String())
	}
	if !strings.Contains(fr.L2MCScripts[0], `L2MC|Vlan4094\",\"fields\":{\"enabled\":\"true\",\"fast-leave\":\"false\",\"last-member-query-interval\":\"1000\",\"querier\":\"true\",\"query-interval\":\"125\",\"query-max-response-time\":\"10\",\"version\":\"3\"}`) {
		t.Errorf("VLAN 1's row:\n%s", fr.L2MCScripts[0])
	}
	// The push names no VLAN 3999: its rows go in the push's own script,
	// before the port batch takes Ethernet4 out of the VLAN.
	if !strings.Contains(fr.L2MCScripts[0], `{\"key\":\"L2MC_STATIC_MEMBER|Vlan3999|239.1.2.3|Ethernet4\"}`) {
		t.Errorf("the test VLAN's static group not removed:\n%s", fr.L2MCScripts[0])
	}
	cmds := fr.Recorded()
	script, member := -1, -1
	for i, c := range cmds {
		switch {
		case c == "sudo python3 - disunified-l2mc" && script < 0:
			script = i
		case c == "sudo config vlan member del 3999 Ethernet4":
			member = i
		}
	}
	if script < 0 || member < script {
		t.Errorf("Ethernet4 left Vlan3999 before its static group:\n%s", strings.Join(cmds, "\n"))
	}
	if !strings.Contains(fr.L2MCScripts[1], "L2MC_STATIC_MEMBER|Vlan4094|239.255.255.250|Ethernet4") {
		t.Errorf("Ethernet4 not given the reserved groups after the batch:\n%s", fr.L2MCScripts[1])
	}
	logText := buf.String()
	for _, want := range []string{"querier address 192.0.2.1 has no l2mcd field", "(VLANs 69) is refused by this switch's SAI"} {
		if !strings.Contains(logText, want) {
			t.Errorf("log lacks %q:\n%s", want, logText)
		}
	}
	if snap, err = coll.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if got := snap.System.IGMPSnooping; len(got) != 3 || !got[1] || !got[2] || !got[69] {
		t.Errorf("reported snooping %v", got)
	}
	n := len(fr.Recorded())
	l.reconcile(ctx)
	if got := fr.Recorded()[n:]; len(got) != 0 {
		t.Errorf("reconcile wrote %q", got)
	}
}
