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
	"github.com/TechBlueprints/disunified/internal/unificfg"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The DX010's 802.1X pushes of 2026-11-15 (Network 11.0.99,
// docs/drivers/sonic.md 11.26), replayed on the capture from the collector
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
	r, coll := newNet11Rig(t, "system_cfg-dx010-dot1x-8-off.txt", "fcc061a99a32e653")
	if !coll.Capabilities().Dot1X {
		t.Fatal("the capture does not claim 802.1X")
	}
	return r, coll
}

// newNet11Rig is the DX010 on its 2026-11-15 image (collect-dot1x.txt, the
// collector that reads 802.1X) under one of Network 11.0.99's pushes.
func newNet11Rig(t *testing.T, applied, ver string) (*breakoutRig, *sonic.Collector) {
	t.Helper()
	return newNet11RigFrom(t, "collect-dot1x.txt", applied, ver)
}

// newNet11RigFrom: the loop over a capture in the 20261115 fixture dir,
// with the Network 11.0.99 push in file applied (cfgversion ver) already.
func newNet11RigFrom(t *testing.T, collect, applied, ver string) (*breakoutRig, *sonic.Collector) {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7", collect))
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
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = coll.DefaultPortNames(p)
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: ver, SystemCfg: read11(t, applied)}
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
		// The MAC method's idle timeout reaches l2d for that port only.
		idle, _ := conf()["idle_timeout"].(map[string]any)
		if tc.mode == "mac_based" && (len(idle) != 1 || idle[tc.iface] != float64(300)) {
			t.Errorf("%s: l2d's idle timeouts = %v, want %s 300", tc.file, idle, tc.iface)
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
	// The RADIUS secret leaves the switch with it (live 2026-11-15 it stayed).
	if _, ok := r.fr.L2Files["/etc/sonic/disunified/dot1x-radius"]; ok {
		t.Errorf("off: the secrets file was not removed")
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

// pushRedacted11 is push11 from the reply log as the recorder writes it:
// the push's secrets redacted (unificfg.RedactJSON), the RADIUS secret
// among them.
func (r *breakoutRig) pushRedacted11(file, ver string) []string {
	r.t.Helper()
	body, _ := json.Marshal(map[string]string{"_type": "setparam", "system_cfg": read11(r.t, file), "mgmt_cfg": "cfgversion=" + ver + "\n"})
	red, secrets := unificfg.RedactJSON(body)
	if len(secrets) == 0 {
		r.t.Fatalf("%s: nothing redacted", file)
	}
	if strings.Contains(string(red), "<secret>") || strings.Contains(string(red), "\\u003csecret\\u003e") {
		r.t.Fatalf("%s: the RADIUS secret survived redaction", file)
	}
	r.sess.Apply(time.Now(), red)
	if !r.l.applyPending(context.Background()) {
		r.t.Fatalf("%s was not consumed", file)
	}
	return r.writes()
}

// A replay of the redacted 802.1X pushes drives the driver as the raw ones
// do, with the placeholder where the secret was, and the placeholder is
// never a change of its own: the same push again writes nothing.
func TestReplaySonicDX010Dot1XRedacted(t *testing.T) {
	raw, _ := newDot1XRig(t)
	red, _ := newDot1XRig(t)
	for _, r := range []*breakoutRig{raw, red} {
		r.l.collect(context.Background())
		r.l.reconcile(context.Background())
		r.writes()
	}
	steps := []struct{ file, ver string }{
		{"system_cfg-dx010-dot1x-1-enabled.txt", "5a4ff4412e1e20d4"},
		{"system_cfg-dx010-dot1x-2-port31-credentials.txt", "87d0f163631f307e"},
		{"system_cfg-dx010-dot1x-7-external.txt", "88400aa3336f2d5f"},
	}
	for _, s := range steps {
		want := raw.push11(s.file, s.ver)
		got := red.pushRedacted11(s.file, s.ver)
		wantWrites(t, s.file+" (redacted)", got, want)
		red.cycle(s.file + " (redacted)")
		if again := red.pushRedacted11(s.file, s.ver+"0"); len(again) != 0 {
			t.Errorf("%s pushed again, redacted, wrote %q", s.file, again)
		}
	}
	if s := red.fr.L2Files["/etc/sonic/disunified/dot1x-radius"]; s != unificfg.Redacted+"\n" {
		t.Errorf("the secrets file from the redacted log = %q", s)
	}
}
