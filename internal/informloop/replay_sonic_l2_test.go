package informloop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// Spanning tree and loop protection end to end: the controller's real push
// to the DX010 (system_cfg-dx010-held.txt) with the spanning-tree and
// loop-protection keys the controller sent the ECS Aggregation
// (system_cfg-ecs-aggregation.txt: switch.stp.*, and port N's stp.* and
// keepalive keys onto port N), through the loop into the driver, against the
// DX010 collected before anything was installed (collect-l2.txt).

var ecsPortKey = regexp.MustCompile(`^switch\.port\.(\d+)\.(stp\.[a-z_]+|keepalive)=`)

func dx010WithECSSpanningTree(t *testing.T) (string, map[int]string) {
	t.Helper()
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	edges := map[int]string{}
	var add []string
	for _, line := range strings.Split(read("system_cfg-ecs-aggregation.txt"), "\n") {
		if strings.HasPrefix(line, "switch.stp.") {
			add = append(add, line)
			continue
		}
		if m := ecsPortKey.FindStringSubmatch(line); m != nil {
			var n int
			fmt.Sscan(m[1], &n)
			if n <= 32 {
				add = append(add, line)
				if m[2] == "stp.edge" {
					edges[n] = strings.SplitN(line, "=", 2)[1]
				}
			}
		}
	}
	var out []string
	for _, line := range strings.Split(read("system_cfg-dx010-held.txt"), "\n") {
		if strings.HasPrefix(line, "switch.stp.") || ecsPortKey.MatchString(line) {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(append(out, add...), "\n"), edges
}

func TestReplaySonicSpanningTree(t *testing.T) {
	text, edges := dx010WithECSSpanningTree(t)
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238526-ff3c0c9f6", "collect-l2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	vs, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238524-ff3c0c9f6", "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fr.Help, fr.Zones = vs.Help, vs.Zones
	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	coll.Redial = fr.Dial
	coll.L2 = true
	var zero time.Duration
	coll.L2Wait = &zero
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps := coll.Capabilities(); !caps.STP || !caps.STPAutoEdge || !caps.LoopProtection {
		t.Fatalf("the controller pushes these keys only to a device that claims them: %+v", caps)
	}
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = coll.DefaultPortNames(p)
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: text}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, AllowInitialChanges: true, DefaultPortNames: defaults})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if !l.applyPending(ctx) {
		t.Fatal("the push was not consumed")
	}
	if fr.Builds != 1 || len(fr.L2Scripts) != 0 {
		t.Fatalf("the push starts the mstpd build and installs nothing yet: builds %d, scripts %d; log:\n%s", fr.Builds, len(fr.L2Scripts), buf.String())
	}
	// The build has run; the next reconcile installs and enables.
	if _, err := coll.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	l.reconcile(ctx)
	if len(fr.L2Scripts) != 1 {
		t.Fatalf("%d l2 scripts after the build, want 1; log:\n%s", len(fr.L2Scripts), buf.String())
	}
	var conf struct {
		STP struct {
			Enabled  bool
			Version  string
			Priority int
			Ports    map[string]struct {
				Enabled   bool
				Edge      string
				BPDUGuard bool `json:"bpdu_guard"`
			}
		}
		LoopGuard struct{ Enabled bool } `json:"loop_guard"`
	}
	if err := json.Unmarshal([]byte(fr.L2Files["/etc/sonic/disunified/l2.json"]), &conf); err != nil {
		t.Fatal(err)
	}
	if !conf.STP.Enabled || conf.STP.Version != "rstp" || conf.STP.Priority != 8192 || conf.LoopGuard.Enabled {
		t.Errorf("config = %+v, want rstp at 8192 and no loop protection (every keepalive is disabled in the push)", conf)
	}
	disabled := 0
	for n := 1; n <= 32; n++ {
		ifname := fmt.Sprintf("Ethernet%d", (n-1)*4)
		p, ok := conf.STP.Ports[ifname]
		if !ok || !p.Enabled || p.BPDUGuard || p.Edge != edges[n] {
			t.Errorf("port %d (%s) = %+v, want STP on, edge %q", n, ifname, p, edges[n])
		}
		if edges[n] == "disabled" {
			disabled++
		}
	}
	if disabled == 0 {
		t.Error("the composed push lost the ECS's edge-disabled ports")
	}
	if !strings.Contains(buf.String(), "dead-man timer cancelled") {
		t.Errorf("the first enable must be confirmed over a fresh connection; log:\n%s", buf.String())
	}
	if _, err := coll.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	n := len(fr.Commands)
	l.reconcile(ctx)
	if again := fr.Commands[n:]; len(again) != 0 {
		t.Errorf("a reconcile after the enable wrote %q", again)
	}
}
