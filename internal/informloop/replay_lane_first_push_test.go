package informloop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/inform"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/breakout"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// The first push after a lane switch's adoption, from the recorded run on
// the DX010's cage 3 (docs/drivers/breakout.md §16.10): the controller's
// default for an unseeded port (no switch.vlan.N.port.M keys: Allow All)
// over ten site VLANs the parent had pruned (collect-split-dac.txt holds
// only the native stand-in, Vlan4094), then the seeded push that names each
// lane's VLANs as the lanes have them. Live, the hold planned the first
// push before EnsureVLANs had made those VLANs, saw no change, let it
// through, and every lane was tagged with all ten until the seeded push
// took them off again. The first push must be held, and no lane may gain a
// VLAN at any step.
func TestReplayLaneFirstPushHeldOverPrunedVLANs(t *testing.T) {
	pushes := laneSwitchPushes(t, "replies-lane-switch-dx010-cage3.ndjson")
	if len(pushes) < 2 {
		t.Fatalf("the recording has %d pushes with a system_cfg, want the first and the seeded one", len(pushes))
	}
	first, seeded := pushes[0], pushes[1]
	if strings.Contains(first.cfg, ".port.1.mode=") || !strings.Contains(seeded.cfg, "switch.vlan.2.port.1.mode=exclude") {
		t.Fatalf("the recording's first two pushes are not the unseeded and the seeded one")
	}

	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7", "collect-split-dac.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var buf syncBuf
	logger := log.New(&buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	coll.L2 = false
	if _, err := coll.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const name = "sw-cage3"
	if err := coll.AttachLaneSwitch(3, devicemodel.Attachment{MAC: laneSwitchMAC, Name: name}); err != nil {
		t.Fatal(err)
	}
	o, err := breakout.ParseOptions(map[string]string{"parent": "sw", "cage": "3", "mac": laneSwitchMAC, "name": name})
	if err != nil {
		t.Fatal(err)
	}
	lanes := breakout.New(o, coll, coll)
	lanes.Log = logger
	snap, err := lanes.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = append(lanes.DefaultPortNames(p), fmt.Sprintf("Port %d", p.Index))
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
	// Just adopted (the mgmt_cfg with the key): no push followed yet.
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef"}, nil, time.Now())
	sess.SetSnapshot(snap)
	l, err := New(desc, sess, Config{Logger: logger, Collector: lanes, Controller: lanes, DefaultPortNames: defaults})
	if err != nil {
		t.Fatal(err)
	}
	before := laneVLANs(t, lanes)
	seen := len(fr.Recorded())
	step := func(what string, p recordedPush) []string {
		t.Helper()
		l.collect(context.Background())
		sess.Apply(time.Now(), p.body)
		l.applyPending(context.Background())
		got := fr.Recorded()[seen:]
		seen = len(fr.Recorded())
		for _, c := range got {
			if strings.Contains(c, "config vlan member add") {
				t.Errorf("%s: a lane gained a VLAN: %q", what, c)
			}
		}
		if after := laneVLANs(t, lanes); !slices.Equal(after, before) {
			t.Errorf("%s: the lanes' VLANs went from %v to %v", what, before, after)
		}
		return got
	}

	if got := step("the first push", first); len(got) != 0 {
		t.Errorf("the held first push wrote %q", got)
	}
	if !strings.Contains(buf.String(), "HOLDING the first push after adoption") {
		t.Errorf("the first push was not held:\n%s", buf.String())
	}
	if v, _, ok := sess.Applied(); ok && v == first.ver {
		t.Errorf("the held first push was reported as applied")
	}

	step("the seeded push", seeded)
	if v, _, ok := sess.Applied(); !ok || v != seeded.ver {
		t.Errorf("the seeded push was not applied: applied %q (%v)\n%s", v, ok, buf.String())
	}
	// And the next cycle leaves the lanes as they are.
	l.collect(context.Background())
	l.reconcile(context.Background())
	if got := fr.Recorded()[seen:]; len(got) != 0 {
		t.Errorf("the reconcile after the seeded push wrote %q", got)
	}
}

type recordedPush struct {
	ver, cfg string
	body     []byte
}

// laneSwitchPushes are the setparam replies with a system_cfg in a
// recorded reply log, in order.
func laneSwitchPushes(t *testing.T, file string) []recordedPush {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", file))
	if err != nil {
		t.Fatal(err)
	}
	var out []recordedPush
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		var p struct {
			Type       string `json:"_type"`
			Cfg        string `json:"system_cfg"`
			Cfgversion string `json:"cfgversion"`
		}
		if len(rec.Payload) == 0 || json.Unmarshal(rec.Payload, &p) != nil || p.Type != "setparam" || p.Cfg == "" {
			continue
		}
		out = append(out, recordedPush{ver: p.Cfgversion, cfg: p.Cfg, body: rec.Payload})
	}
	return out
}

// laneVLANs is each lane's VLAN membership as the lane switch reads it.
func laneVLANs(t *testing.T, lanes *breakout.Device) []string {
	t.Helper()
	snap, err := lanes.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range snap.Ports {
		if p.Index > 4 {
			continue
		}
		out = append(out, fmt.Sprintf("%d:%s native %d all %v %v", p.Index, p.VLAN.Mode, p.VLAN.NativeVLAN, p.VLAN.AllowAll, p.VLAN.Allowed))
	}
	return out
}
