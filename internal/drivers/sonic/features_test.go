package sonic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The §7.1 features: time zone, hostname, locate and breakout.

// --- time zone ---

// The controller pushes a POSIX rule (every recorded push:
// system.timezone=MST7MDT,M3.2.0,M11.1.0); SONiC takes zone names only. The
// rule maps onto the switch's zone of that name, read back as the system's
// time zone; a zone the operator chose with the same rule is left alone; a
// zone name is taken as is; a rule no zone has is refused with a log.
func TestApplyTimezone(t *testing.T) {
	r := newRig(t, nil)
	const pushed = "MST7MDT,M3.2.0,M11.1.0"
	if got := r.system().Timezone; got != "UTC" {
		t.Fatalf("the capture's time zone = %q, want UTC", got)
	}
	r.device("posix rule", devicemodel.DeviceDesired{Timezone: pushed}, []string{"sudo config clock timezone MST7MDT", save})
	if got := r.system().Timezone; got != "MST7MDT" {
		t.Errorf("read back time zone = %q", got)
	}
	// The operator's own zone with the same rule stays.
	if _, err := r.fr.Run(context.Background(), "sudo config clock timezone America/Denver", ""); err != nil {
		t.Fatal(err)
	}
	r.system()
	r.device("same rule", devicemodel.DeviceDesired{Timezone: pushed}, nil)
	// A zone name the switch lists is written as is.
	r.device("zone name", devicemodel.DeviceDesired{Timezone: "Europe/Berlin"}, []string{"sudo config clock timezone Europe/Berlin", save})
	// The zone list is read once per run.
	if r.fr.ZoneReads != 1 {
		t.Errorf("zone list read %d times, want once", r.fr.ZoneReads)
	}
	// No zone has this rule: refused with a log, the rest of the push applies.
	r.device("unknown rule", devicemodel.DeviceDesired{Timezone: "XYZ9", ManageNTP: true, NTPServers: []string{"192.0.2.10"}},
		[]string{"sudo config ntp add 192.0.2.10", save})
	if !strings.Contains(r.log.String(), `time zone "XYZ9": no zone on this switch has the rule`) {
		t.Errorf("the refusal must be logged: %s", r.log.String())
	}
	// Not in the push: left alone.
	r.device("absent", devicemodel.DeviceDesired{}, nil)
}

func TestZoneFor(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(fixtureDir, "zones.txt")) // the VS's zone list, captured
	if err != nil {
		t.Fatal(err)
	}
	zs := parseZones(string(b))
	if len(zs) < 500 {
		t.Fatalf("%d zones in the capture", len(zs))
	}
	for _, c := range []struct{ pushed, cur, want string }{
		{"MST7MDT,M3.2.0,M11.1.0", "UTC", "MST7MDT"},
		{"MST7MDT,M3.2.0,M11.1.0", "America/Denver", "America/Denver"},
		{"MST7", "UTC", "MST"},
		{"UTC0", "Etc/UTC", "Etc/UTC"},
		{"America/Phoenix", "UTC", "America/Phoenix"},
	} {
		if got, ok, why := zoneFor(c.pushed, c.cur, zs); !ok || got != c.want {
			t.Errorf("zoneFor(%q, %q) = %q %v %s, want %q", c.pushed, c.cur, got, ok, why, c.want)
		}
	}
	for _, bad := range []string{"XYZ9", "Europe/Berlin; reboot", "$(reboot)"} {
		if got, ok, _ := zoneFor(bad, "UTC", zs); ok {
			t.Errorf("zoneFor(%q) = %q, want refused", bad, got)
		}
	}
}

// --- hostname ---

// control.hostname: the controller's resolv.host.1.name becomes the
// hostname (config hostname, then config save), read back on the next
// collect; a name that is not a DNS name, or longer than SONiC's 63
// characters, is refused with a log.
func TestApplyHostname(t *testing.T) {
	r := newRig(t, nil)
	r.device("rename", devicemodel.DeviceDesired{Hostname: "SONiCSeastone-DX010"}, []string{"sudo config hostname SONiCSeastone-DX010", save})
	if got := r.system().Hostname; got != "SONiCSeastone-DX010" {
		t.Errorf("read back hostname = %q", got)
	}
	r.device("same", devicemodel.DeviceDesired{Hostname: "SONiCSeastone-DX010"}, nil)
	for _, bad := range []string{"lab_switch", "-lab", "lab switch", "a..b", strings.Repeat("a", 64), "x$(reboot)"} {
		r.device("bad "+bad, devicemodel.DeviceDesired{Hostname: bad}, nil)
	}
	if n := strings.Count(r.log.String(), "from the controller"); n != 6 {
		t.Errorf("%d hostname refusals logged, want 6:\n%s", n, r.log.String())
	}
	r.device("dotted", devicemodel.DeviceDesired{Hostname: "lab-1.example.net"}, []string{"sudo config hostname lab-1.example.net", save})
	// Off (not in the desired state): left alone.
	r.device("off", devicemodel.DeviceDesired{}, nil)
}

// --- locate ---

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func ledCmds(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		if ledRe.MatchString(c) {
			out = append(out, ledRe.FindStringSubmatch(c)[1])
		}
	}
	return out
}

// On a platform whose API sets the status LED (the DX010, modelled):
// locate on writes off at once, then toggles green/off; locate off stops
// the toggling and leaves the LED green; a second locate on restarts the
// blink; Close stops it.
func TestLocateBlinksTheStatusLED(t *testing.T) {
	r := newRig(t, func(fr *FixtureRunner) { fr.LEDWorks = true })
	r.c.BlinkInterval = 5 * time.Millisecond
	ctx := context.Background()
	if err := r.c.SetLocate(ctx, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "four LED writes", func() bool { return len(ledCmds(r.fr.Recorded())) >= 4 })
	if err := r.c.SetLocate(ctx, false); err != nil {
		t.Fatal(err)
	}
	got := ledCmds(r.fr.Recorded())
	if got[0] != "off" || got[len(got)-1] != "green" {
		t.Errorf("LED writes %v: want off first, green last", got)
	}
	for i := 1; i < len(got)-1; i++ {
		if got[i] == got[i-1] {
			t.Errorf("LED writes %v do not alternate at %d", got, i)
		}
	}
	time.Sleep(30 * time.Millisecond)
	if after := ledCmds(r.fr.Recorded()); len(after) != len(got) || r.fr.LED != "green" {
		t.Errorf("the blink went on after locate off: %v (LED %s)", after[len(got):], r.fr.LED)
	}
	// A second locate on restarts the blink (one goroutine at a time); Close
	// stops it.
	if err := r.c.SetLocate(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := r.c.SetLocate(ctx, true); err != nil {
		t.Fatal(err)
	}
	n := len(r.fr.Recorded())
	waitFor(t, "the blink to resume", func() bool { return len(r.fr.Recorded()) > n+2 })
	if err := r.c.Close(); err != nil {
		t.Fatal(err)
	}
	n = len(r.fr.Recorded())
	time.Sleep(30 * time.Millisecond)
	if m := len(r.fr.Recorded()); m != n {
		t.Errorf("%d LED writes after Close", m-n)
	}
	if !strings.Contains(r.log.String(), "locate on: blinking the status LED") || !strings.Contains(r.log.String(), "locate off: status LED back to green") {
		t.Errorf("log: %s", r.log.String())
	}
}

// On the VS the platform API raises (no chassis metadata): the first write
// fails, that is logged once per run, and nothing more is tried.
func TestLocateWithoutLEDLogsOnce(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	for _, on := range []bool{true, false, true} {
		if err := r.c.SetLocate(ctx, on); err != nil {
			t.Fatalf("SetLocate(%v): %v; a platform without LED control is not an error", on, err)
		}
	}
	if got := ledCmds(r.fr.Recorded()); len(got) != 1 {
		t.Errorf("LED writes %v; want the one that found no LED", got)
	}
	if n := strings.Count(r.log.String(), "no controllable status LED"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, r.log.String())
	}
	if !strings.Contains(r.log.String(), "FileNotFoundError") {
		t.Errorf("the platform's own error should be in the log: %s", r.log.String())
	}
}

// --- breakout ---

const fixtureDir = "../../../docs/fixtures/sonic-202505.1238524-ff3c0c9f6"

// withDX010Platform turns the VS capture into the DX010's: the platform's
// platform.json interfaces block (the upstream file, sonic-buildimage
// device/celestica/x86_64-cel_seastone-r0/platform.json, kept beside the
// capture as dx010-platform.json) in the platform_json section as
// collect.py prints it, and BREAKOUT_CFG as SONiC initialises it from the
// SKU's hwsku.json (dx010-hwsku.json: every cage 1x100G[40G]). The VS uses
// the same SKU's port_config.ini, so ports, lanes and indexes agree.
func withDX010Platform(t *testing.T) func(*FixtureRunner) {
	return func(fr *FixtureRunner) {
		var pj struct {
			Interfaces map[string]platformPort `json:"interfaces"`
		}
		var hw struct {
			Interfaces map[string]struct {
				Mode string `json:"default_brkout_mode"`
			} `json:"interfaces"`
		}
		for name, v := range map[string]any{"dx010-platform.json": &pj, "dx010-hwsku.json": &hw} {
			b, err := os.ReadFile(filepath.Join(fixtureDir, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(b, v); err != nil {
				t.Fatal(err)
			}
		}
		names := make([]string, 0, len(pj.Interfaces))
		for n := range pj.Interfaces {
			names = append(names, n)
		}
		sort.Strings(names)
		var lines []string
		for _, n := range names {
			k, _ := json.Marshal(n)
			v, _ := json.Marshal(pj.Interfaces[n])
			lines = append(lines, string(k)+": "+string(v))
		}
		fr.setSection("platform_json", "{\n"+strings.Join(lines, ",\n")+"\n}")
		brk := table{}
		for n, v := range hw.Interfaces {
			brk["BREAKOUT_CFG|"+n] = map[string]string{"brkout_mode": v.Mode}
		}
		fr.setTable("cfg_breakout", brk)
	}
}

func speedOn(ds []devicemodel.PortDesired, idx, mbps int) []devicemodel.PortDesired {
	return edit(ds, idx, func(d *devicemodel.PortDesired) { d.SpeedMbps = mbps })
}

// The DX010's cages claim every speed of their breakout modes, and the
// baseline writes nothing.
func TestBreakoutCapsAndBaseline(t *testing.T) {
	r := newRig(t, withDX010Platform(t))
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := port(snap, 2)
	if !reflect.DeepEqual(p.SpeedCaps, []int{10000, 25000, 40000, 50000, 100000}) || p.Media != devicemodel.MediaQSFP28 || p.Lanes != 1 {
		t.Errorf("cage 2: caps %v media %v lanes %d", p.SpeedCaps, p.Media, p.Lanes)
	}
	if got, _ := r.apply(r.baseline()); len(got) != 0 {
		t.Errorf("baseline wrote %q", got)
	}
}

// Cage 2 (Ethernet4): split to 4x25G, lane speed 10G, join back to 100G,
// and 2x50G; each read back folded into one cage with its intent (enabled,
// native VLAN) re-applied on the children, and each idempotent.
func TestBreakoutSplitLaneSpeedJoin(t *testing.T) {
	r := newRig(t, withDX010Platform(t))
	base := r.baseline()
	base = edit(base, 2, func(d *devicemodel.PortDesired) { d.Description = "lab-2" })
	r.expect("name", base, []string{"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet4' description 'lab-2'", save})

	children := []string{"Ethernet4", "Ethernet5", "Ethernet6", "Ethernet7"}
	var split []string
	split = append(split, "sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null")
	for _, ch := range children {
		split = append(split, "sudo config interface startup "+ch, "sudo sonic-db-cli CONFIG_DB hset 'PORT|"+ch+"' description 'lab-2'")
	}
	for _, ch := range children {
		split = append(split, "sudo config vlan member add -u 4094 "+ch)
	}
	snap := r.expect("split 4x25G", speedOn(base, 2, 25000), append(split, save))
	p := port(snap, 2)
	if p.Lanes != 4 || !reflect.DeepEqual(p.Interfaces, children) || p.AdminSpeed != 25000 || p.LanesDiverge || !p.Enabled ||
		p.VLAN.NativeVLAN != 1 || p.Description != "lab-2" || p.Name != "lab-2" {
		t.Errorf("split cage: lanes %d interfaces %v speed %d diverge %v enabled %v native %d desc %q name %q",
			p.Lanes, p.Interfaces, p.AdminSpeed, p.LanesDiverge, p.Enabled, p.VLAN.NativeVLAN, p.Description, p.Name)
	}
	if !reflect.DeepEqual(p.SpeedCaps, []int{10000, 25000, 40000, 50000, 100000}) {
		t.Errorf("a split cage still claims the join: caps %v", p.SpeedCaps)
	}
	if got := r.c.PortName(devicemodel.Port{Index: 2}); got != "Eth2" {
		t.Errorf("the split cage's default name = %q, want the faceplate's Eth2", got)
	}
	// The neighbours are untouched.
	if q := port(snap, 3); q.Lanes != 1 || q.IfName != "Ethernet8" {
		t.Errorf("cage 3 changed: %+v", q.Interfaces)
	}

	var lane10 []string
	for _, ch := range children {
		lane10 = append(lane10, "sudo config interface speed "+ch+" 10000")
	}
	snap = r.expect("lanes to 10G", speedOn(base, 2, 10000), append(lane10, save))
	if p := port(snap, 2); p.AdminSpeed != 10000 || p.Lanes != 4 || p.LanesDiverge {
		t.Errorf("10G lanes: speed %d lanes %d diverge %v", p.AdminSpeed, p.Lanes, p.LanesDiverge)
	}

	snap = r.expect("join 1x100G", speedOn(base, 2, 100000), []string{
		"sudo config interface breakout -f -y Ethernet4 '1x100G[40G]' </dev/null",
		"sudo config interface startup Ethernet4",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet4' description 'lab-2'",
		"sudo config vlan member add -u 4094 Ethernet4",
		save,
	})
	if p := port(snap, 2); p.Lanes != 1 || !reflect.DeepEqual(p.Interfaces, []string{"Ethernet4"}) || p.AdminSpeed != 100000 || p.VLAN.NativeVLAN != 1 {
		t.Errorf("joined cage: lanes %d interfaces %v speed %d native %d", p.Lanes, p.Interfaces, p.AdminSpeed, p.VLAN.NativeVLAN)
	}

	// 40G is the 1x100G mode's other speed: a speed change, no breakout.
	r.expect("40G", speedOn(base, 2, 40000), []string{"sudo config interface speed Ethernet4 40000", save})

	snap = r.expect("split 2x50G", speedOn(base, 2, 50000), []string{
		"sudo config interface breakout -f -y Ethernet4 '2x50G' </dev/null",
		"sudo config interface startup Ethernet4",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet4' description 'lab-2'",
		"sudo config interface startup Ethernet6",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet6' description 'lab-2'",
		"sudo config vlan member add -u 4094 Ethernet4",
		"sudo config vlan member add -u 4094 Ethernet6",
		save,
	})
	if p := port(snap, 2); p.Lanes != 2 || !reflect.DeepEqual(p.Interfaces, []string{"Ethernet4", "Ethernet6"}) || p.AdminSpeed != 50000 {
		t.Errorf("2x50G: lanes %d interfaces %v speed %d", p.Lanes, p.Interfaces, p.AdminSpeed)
	}
}

// A split carries the whole intent over: tagged VLANs, jumbo MTU and a
// disabled port reach every child.
func TestBreakoutCarriesTheIntent(t *testing.T) {
	r := newRig(t, withDX010Platform(t))
	ds := edit(r.baseline(), 2, func(d *devicemodel.PortDesired) {
		d.SpeedMbps, d.Enabled, d.TaggedVLANs, d.MTU = 25000, false, []int{2, 69}, 9216
	})
	if _, err := r.c.EnsureVLANs(context.Background(), []int{1, 2, 69}); err != nil {
		t.Fatal(err)
	}
	got, _ := r.apply(ds)
	if got[0] != "sudo config interface breakout -f -y Ethernet4 '4x25G[10G]' </dev/null" {
		t.Fatalf("first write %q", got[0])
	}
	// 2 and 69 are every VLAN on the switch but the native one: the -e form.
	for _, ch := range []string{"Ethernet4", "Ethernet5", "Ethernet6", "Ethernet7"} {
		for _, want := range []string{"sudo config interface mtu " + ch + " 9216", "sudo config vlan member add -u 4094 " + ch, "sudo config vlan member add -e -m 4094 " + ch} {
			if !hasString(got, want) {
				t.Errorf("missing %q in %q", want, got)
			}
		}
		if hasString(got, "sudo config interface startup "+ch) || hasString(got, "sudo config interface shutdown "+ch) {
			t.Errorf("%s: a disabled cage's children are already down after a breakout; wrote %q", ch, got)
		}
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := port(snap, 2); p.Enabled || p.MTU != 9216 || !reflect.DeepEqual(p.VLAN.Allowed, []int{1, 2, 69}) || p.LanesDiverge {
		t.Errorf("split cage: enabled %v mtu %d allowed %v diverge %v", p.Enabled, p.MTU, p.VLAN.Allowed, p.LanesDiverge)
	}
	if again, _ := r.apply(ds); len(again) != 0 {
		t.Errorf("a second apply wrote %q", again)
	}
}

// Refusals: a speed no mode runs; a cage in an aggregate (-f would drop the
// membership the controller still wants); and the VS, which has no
// platform.json (TestSpeedRules). None of them is an error.
func TestBreakoutRefusals(t *testing.T) {
	r := newRig(t, withDX010Platform(t))
	base := r.baseline()
	if got, _ := r.apply(speedOn(base, 2, 1000)); len(got) != 0 {
		t.Errorf("1G wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "not among the cage's breakout speeds") {
		t.Errorf("log: %s", r.log.String())
	}
	lag := edit(edit(base, 2, func(d *devicemodel.PortDesired) { d.LAG = 2 }), 3, func(d *devicemodel.PortDesired) { d.LAG = 2 })
	if _, err := r.c.ApplyPorts(context.Background(), lag); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.apply(speedOn(lag, 2, 25000)); len(got) != 0 {
		t.Errorf("a breakout of an aggregated cage wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "is in an aggregate") {
		t.Errorf("log: %s", r.log.String())
	}
}

// The FixtureRunner refuses what SONiC's breakout refuses: no -y (the
// prompt aborts), dependencies without -f, -l (no
// port_breakout_config_db.json on any build), a mode the port lacks, and
// breakout at all without platform.json.
func TestFixtureRunnerBreakoutRefusals(t *testing.T) {
	r := newRig(t, withDX010Platform(t))
	ctx := context.Background()
	for _, c := range []struct{ cmd, err string }{
		{"sudo config interface breakout Ethernet4 '4x25G[10G]'", "Aborted"},
		{"sudo config interface breakout -y Ethernet4 '4x25G[10G]'", "Dependecies Exist"},
		{"sudo config interface breakout -f -l -y Ethernet4 '4x25G[10G]'", "port_breakout_config_db.json"},
		{"sudo config interface breakout -f -y Ethernet4 '8x10G'", "not available for the port"},
		{"sudo config interface breakout -f -y Ethernet5 '4x25G[10G]'", "NOT present in BREAKOUT_CFG"},
	} {
		if _, err := r.fr.Run(ctx, c.cmd, ""); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: err %v, want %q", c.cmd, err, c.err)
		}
	}
	vs := newRig(t, nil)
	if _, err := vs.fr.Run(ctx, "sudo config interface breakout -f -y Ethernet4 '4x25G[10G]'", ""); err == nil || !strings.Contains(err.Error(), "without platform.json") {
		t.Errorf("the VS must refuse breakout: %v", err)
	}
}

// The mode grammar and SONiC's child-port derivation (portconfig.py's
// BreakoutCfg.get_config) on the DX010's cage 2.
func TestBreakoutChildren(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(fixtureDir, "dx010-platform.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pj struct {
		Interfaces map[string]platformPort `json:"interfaces"`
	}
	if err := json.Unmarshal(b, &pj); err != nil {
		t.Fatal(err)
	}
	pp := pj.Interfaces["Ethernet4"]
	got := map[string][]string{}
	for _, m := range pp.modes() {
		ch, err := pp.children("Ethernet4", m)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range ch {
			got[m.name] = append(got[m.name], c.name+" "+c.fields["lanes"]+" "+c.fields["alias"]+" "+c.fields["speed"]+" idx"+c.fields["index"]+" sub"+c.fields["subport"]+" fec"+c.fields["fec"])
		}
	}
	want := map[string][]string{
		"1x100G[40G]": {"Ethernet4 69,70,71,72 Eth2 100000 idx2 sub0 fec"},
		"2x50G":       {"Ethernet4 69,70 Eth2/1 50000 idx2 sub1 fec", "Ethernet6 71,72 Eth2/2 50000 idx2 sub2 fec"},
		"4x25G[10G]":  {"Ethernet4 69 Eth2/1 25000 idx2 sub1 fec", "Ethernet5 70 Eth2/2 25000 idx2 sub2 fec", "Ethernet6 71 Eth2/3 25000 idx2 sub3 fec", "Ethernet7 72 Eth2/4 25000 idx2 sub4 fec"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("children\n got  %v\n want %v", got, want)
	}
	if m, ok := parseMode("2x200G(4)"); !ok || m.ports != 2 || m.def != 200000 || m.assigned != 4 {
		t.Errorf("parseMode(2x200G(4)) = %+v %v", m, ok)
	}
	if m, ok := parseMode("1x100000[40000]"); !ok || !reflect.DeepEqual(m.speeds, []int{100000, 40000}) {
		t.Errorf("bare Mb/s mode = %+v %v", m, ok)
	}
	if _, ok := parseMode("2x25G(2)+1x50G(2)"); ok {
		t.Error("combined modes are not offered")
	}
}
