package informloop

import (
	"context"
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

// The SONiC control replay: two port pushes the controller really made
// (system_cfg-port2-name-speed-vlans.txt and system_cfg-port54-fec-storm-
// bpdu-caps.txt, recorded from the Arista's and the ECS Aggregation's
// sessions) go through the loop's own apply path into the real driver,
// against the VS capture, and the SONiC commands that come out are checked.
// A second pass through the loop's reconcile, after a collect, must write
// nothing.

type sonicRig struct {
	fr   *sonic.FixtureRunner
	coll *sonic.Collector
	loop *Loop
	sess *device.Session
	log  *strings.Builder
}

func newSonicRig(t *testing.T, systemCfg string) *sonicRig {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238524-ff3c0c9f6", "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The controller's generic port names count as unnamed, as main sets them.
	defaults := map[int][]string{}
	for i := 1; i <= 54; i++ {
		defaults[i] = []string{fmt.Sprintf("Port %d", i), fmt.Sprintf("SFP28 %d", i), fmt.Sprintf("QSFP28 %d", i)}
	}
	for _, p := range snap.Ports {
		defaults[p.Index] = append(defaults[p.Index], coll.DefaultPortNames(p)...)
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: systemCfg}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, AllowInitialChanges: true, DefaultPortNames: defaults})
	if err != nil {
		t.Fatal(err)
	}
	return &sonicRig{fr: fr, coll: coll, loop: l, sess: sess, log: &buf}
}

func readSystemCfg(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// applyAndReconcile runs the push, then a collect and a reconcile, which
// must write nothing.
func (r *sonicRig) applyAndReconcile(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	if !r.loop.applyPending(ctx) {
		t.Fatal("the push was not consumed")
	}
	if ver, _, _ := r.sess.Applied(); ver != "v1" {
		t.Fatalf("the push was not applied; log:\n%s", r.log.String())
	}
	written := append([]string(nil), r.fr.Commands...)
	if _, err := r.coll.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	n := len(r.fr.Commands)
	r.loop.reconcile(ctx)
	if again := r.fr.Commands[n:]; len(again) != 0 {
		t.Errorf("the reconcile after the push wrote %d commands: %q", len(again), again[:min(len(again), 10)])
	}
	return written
}

func contains(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}

func count(cmds []string, re *regexp.Regexp) int {
	n := 0
	for _, c := range cmds {
		if re.MatchString(c) {
			n++
		}
	}
	return n
}

// Port 2 renamed "stu capture", forced to 10G, native 1 + tagged 2 and 10;
// jumbo off; every other port on Allow All over the site VLANs 2..4000.
func TestReplaySonicPort2NameSpeedVLANs(t *testing.T) {
	r := newSonicRig(t, readSystemCfg(t, "system_cfg-port2-name-speed-vlans.txt"))
	cmds := r.applyAndReconcile(t)
	for _, id := range []int{2, 8, 9, 10, 69, 1000, 2000, 3000, 4000} {
		if !contains(cmds, fmt.Sprintf("sudo config vlan add %d", id)) {
			t.Errorf("VLAN %d not created", id)
		}
	}
	if contains(cmds, "sudo config vlan add 1") || contains(cmds, "sudo config vlan add 4094") {
		t.Error("VLAN 1 is Vlan4094, which exists; neither may be created")
	}
	if !contains(cmds, "sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet4' description 'stu capture'") {
		t.Error("port 2's name not written")
	}
	if n := count(cmds, regexp.MustCompile(`description`)); n != 1 {
		t.Errorf("%d descriptions written; only port 2 has a name of its own", n)
	}
	// 10G on a 4-lane 100G cage is a breakout: refused on the VS.
	if n := count(cmds, regexp.MustCompile(`config interface (speed|autoneg|breakout)`)); n != 0 {
		t.Errorf("%d speed writes; 10G needs a breakout the VS cannot do", n)
	}
	if !strings.Contains(r.log.String(), "needs a breakout") {
		t.Error("the breakout refusal must be logged")
	}
	// Jumbo off: every port to 1500.
	if n := count(cmds, regexp.MustCompile(`^sudo config interface mtu Ethernet\d+ 1500$`)); n != 32 {
		t.Errorf("%d ports set to MTU 1500, want 32", n)
	}
	// Allow All on the 31 other ports: tagged in the 9 site VLANs; port 2
	// in its two; nothing in 4094 tagged.
	if n := count(cmds, regexp.MustCompile(`^sudo config vlan member add \d+ Ethernet\d+$`)); n != 31*9+2 {
		t.Errorf("%d tagged memberships, want %d", n, 31*9+2)
	}
	if n := count(cmds, regexp.MustCompile(`vlan member (add|del) (-u )?4094 `)); n != 0 {
		t.Errorf("the native VLAN was touched %d times; every port is already untagged in it", n)
	}
	// Port 2's own profile: native 1, tagged 2 and 10, the rest excluded.
	if n := count(cmds, regexp.MustCompile(`^sudo config vlan member add \d+ Ethernet4$`)); n != 2 ||
		!contains(cmds, "sudo config vlan member add 2 Ethernet4") || !contains(cmds, "sudo config vlan member add 10 Ethernet4") {
		t.Errorf("port 2 tagged in %d VLANs, want exactly 2 and 10", n)
	}
	if cmds[len(cmds)-1] != "sudo config save -y" {
		t.Errorf("the batch must end with config save, ends with %q", cmds[len(cmds)-1])
	}
	if strings.Contains(r.log.String(), "jumbo frames off; this switch always forwards") {
		t.Error("SONiC can turn jumbo off; the always-on note is for other drivers")
	}
}

// The ECS Aggregation's port 54 push (100G, RS-FEC, storm control 5% bcast /
// 1% mcast, BPDU guard), replayed onto port 5 of the 32-port switch: the
// capture's port-5 keys are replaced by its port-54 keys.
func TestReplaySonicPortFECStormBPDU(t *testing.T) {
	text := readSystemCfg(t, "system_cfg-port54-fec-storm-bpdu-caps.txt")
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "switch.port.5."):
			continue
		case strings.HasPrefix(line, "switch.port.54.name="):
			line = "switch.port.5.name=QSFP28 5"
		case strings.HasPrefix(line, "switch.port.54."):
			line = "switch.port.5." + strings.TrimPrefix(line, "switch.port.54.")
		}
		lines = append(lines, line)
	}
	r := newSonicRig(t, strings.Join(lines, "\n"))
	cmds := r.applyAndReconcile(t)
	for _, want := range []string{
		"sudo config interface fec Ethernet16 rs",
		"sudo config interface storm-control add Ethernet16 broadcast 5000000",
		"sudo config interface storm-control add Ethernet16 unknown-multicast 1000000",
	} {
		if !contains(cmds, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := count(cmds, regexp.MustCompile(`Ethernet16`)); n != 3+1+9 { // fec, 2 storm, mtu, 9 tagged VLANs
		t.Errorf("%d commands for port 5, want 13", n)
	}
	if n := count(cmds, regexp.MustCompile(`spanning-tree|interface speed|autoneg|description`)); n != 0 {
		t.Errorf("%d STP/speed/name writes; BPDU guard is refused without the STP daemon, 100G is the cage's speed", n)
	}
	if !strings.Contains(r.log.String(), "no STP daemon") {
		t.Error("the BPDU-guard refusal must be logged")
	}
}
