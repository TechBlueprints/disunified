package informloop

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The DX010's 802.1X pushes of 2026-11-15 (Network 11.0.99,
// docs/drivers/sonic.md 11.25), replayed on the capture from the collector
// that reads 802.1X: the switch with 802.1X off, then 802.1X Control on for
// the device (every port force-authorised), each method on an empty cage
// in turn (31 Credentials, 30 Multiple Credentials, 29 MAC Address, 28
// Always Unauthorized), External RADIUS, and 802.1X off again.

func read11(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newDot1XRig(t *testing.T) (*breakoutRig, *sonic.Collector) {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7", "collect-dot1x.txt"))
	if err != nil {
		t.Fatal(err)
	}
	buf := &strings.Builder{}
	logger := log.New(buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	coll.L2 = true
	coll.IGMP = true
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !coll.Capabilities().Dot1X {
		t.Fatal("the capture does not claim 802.1X")
	}
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = coll.DefaultPortNames(p)
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: "fcc061a99a32e653", SystemCfg: read11(t, "system_cfg-dx010-dot1x-8-off.txt")}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, DefaultPortNames: defaults, ControlIGMP: true})
	if err != nil {
		t.Fatal(err)
	}
	return &breakoutRig{t: t, fr: fr, l: l, sess: sess, log: buf}, coll
}

func (r *breakoutRig) push11(file, ver string) []string {
	r.t.Helper()
	body, _ := json.Marshal(map[string]string{"_type": "setparam", "system_cfg": read11(r.t, file), "mgmt_cfg": "cfgversion=" + ver + "\n"})
	r.sess.Apply(time.Now(), body)
	if !r.l.applyPending(context.Background()) {
		r.t.Fatalf("%s was not consumed", file)
	}
	return r.writes()
}

func TestReplaySonicDX010Dot1X(t *testing.T) {
	r, coll := newDot1XRig(t)
	r.l.collect(context.Background())
	r.l.reconcile(context.Background())
	r.writes() // the capture's own drift under the off push is not this test's
	r.cycle("802.1X off")

	conf := func() map[string]any {
		var c map[string]any
		if err := json.Unmarshal([]byte(r.fr.L2Files["/etc/sonic/disunified/l2.json"]), &c); err != nil {
			t.Fatalf("l2.json: %v", err)
		}
		d, _ := c["dot1x"].(map[string]any)
		return d
	}

	// 802.1X Control on, every port force-authorised: nothing closes, and
	// l2d hears of every port as force_authorized (nothing to run); the
	// gateway's RADIUS server and its secret are the pushed ones.
	got := r.push11("system_cfg-dx010-dot1x-1-enabled.txt", "5a4ff4412e1e20d4")
	if j := strings.Join(got, "\n"); strings.Contains(j, "learn_mode") || strings.Contains(j, "hostapd") {
		t.Errorf("force-authorised ports were closed or hostapd built:\n%s", j)
	}
	d := conf()
	if d == nil || len(d["ports"].(map[string]any)) != 32 || d["ports"].(map[string]any)["Ethernet120"] != "force_authorized" {
		t.Errorf("l2d's 802.1X section = %v", d)
	}
	if s := r.fr.L2Files["/etc/sonic/disunified/dot1x-radius"]; s != "<secret>\n" {
		t.Errorf("the secrets file = %q", s)
	}
	r.cycle("802.1X on")

	// Each method on its cage: the port closed (Always Unauthorized too),
	// hostapd built once, l2d told the mode.
	for _, tc := range []struct {
		file, ver, iface, mode string
	}{
		{"system_cfg-dx010-dot1x-2-port31-credentials.txt", "87d0f163631f307e", "Ethernet120", "auto"},
		{"system_cfg-dx010-dot1x-3-port30-multi.txt", "e147a3ee0c7cba56", "Ethernet116", "multi_host"},
		{"system_cfg-dx010-dot1x-4-port29-mac.txt", "f8352498002f3411", "Ethernet112", "mac_based"},
		{"system_cfg-dx010-dot1x-5-port28-unauth.txt", "e8c1c6f4ab48b72d", "Ethernet108", "force_unauthorized"},
	} {
		got := r.push11(tc.file, tc.ver)
		if !containsInOrder(got, "sudo sonic-db-cli CONFIG_DB hset 'PORT|"+tc.iface+"' learn_mode drop") {
			t.Errorf("%s: the port was not closed: %q", tc.file, got)
		}
		if m := conf()["ports"].(map[string]any)[tc.iface]; m != tc.mode {
			t.Errorf("%s: l2d has %s as %v", tc.file, tc.iface, m)
		}
		r.cycle(tc.file)
	}
	if r.fr.Builds != 1 {
		t.Errorf("hostapd built %d times, want once", r.fr.Builds)
	}

	// External RADIUS: the profile's server in l2d's config.
	r.push11("system_cfg-dx010-dot1x-7-external.txt", "88400aa3336f2d5f")
	if s := conf()["servers"].([]any); len(s) != 1 || s[0].(map[string]any)["ip"] != "192.0.2.5" {
		t.Errorf("external servers = %v", s)
	}
	r.cycle("external")

	// 802.1X off: every controlled port opens, l2d's section goes.
	got = r.push11("system_cfg-dx010-dot1x-8-off.txt", "fcc061a99a32e653")
	for _, iface := range []string{"Ethernet108", "Ethernet112", "Ethernet116", "Ethernet120"} {
		if !containsInOrder(got, "sudo sonic-db-cli CONFIG_DB hset 'PORT|"+iface+"' learn_mode hardware", "sudo sonic-db-cli CONFIG_DB hdel 'PORT|"+iface+"' learn_mode") {
			t.Errorf("off: %s was not opened: %q", iface, got)
		}
	}
	if d := conf(); d != nil {
		t.Errorf("off: l2d keeps %v", d)
	}
	r.cycle("off")

	snap, err := coll.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports {
		if p.Dot1X != "" {
			t.Errorf("port %d reads back 802.1X %q after off", p.Index, p.Dot1X)
		}
	}
	s := r.log.String()
	if strings.Contains(s, "UNSUPPORTED") {
		t.Errorf("802.1X logged as unsupported:\n%s", s)
	}
	if strings.Contains(s, "<secret>") {
		t.Errorf("the RADIUS secret reached the log:\n%s", s)
	}
}
