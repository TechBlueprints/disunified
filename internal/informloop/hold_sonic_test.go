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

// The DX010's first push (docs/drivers/sonic.md §10.2), as the bridge got
// it on adoption day: system_cfg-dx010-held.txt is the seeded push from
// Network 10.6.109 -- jumbo off (switch.mtu 9216), every port named and at
// the controller's defaults -- which the loop held 37 times in a row,
// because the switch's 32 ports ran SONiC's 9100.

// newDX010HoldRig is the loop over the DX010's capture with the first-push
// hold on, nothing applied yet, and the switch put back to what the L2
// preset gave it on the day, with SONiC's own commands: 9100 on every port
// (the capture has the 1500 typed by hand to release the hold) and no RS
// on cage 1 (typed by hand to bring the uplink up). prep runs more commands
// on the switch first.
func newDX010HoldRig(t *testing.T, prep ...string) *sonicRig {
	t.Helper()
	return newDX010HoldRigWith(t, nil, prep...)
}

// newDX010HoldRigWith is newDX010HoldRig with edit run on the capture first.
func newDX010HoldRigWith(t *testing.T, edit func(*sonic.FixtureRunner), prep ...string) *sonicRig {
	t.Helper()
	dir := filepath.Join("..", "..", "docs", "fixtures")
	fr, err := sonic.NewFixtureRunner(filepath.Join(dir, "sonic-202505.1238526-ff3c0c9f6", "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	vs, err := sonic.NewFixtureRunner(filepath.Join(dir, "sonic-202505.1238524-ff3c0c9f6", "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fr.Help, fr.Zones = vs.Help, vs.Zones // the same commit
	if edit != nil {
		edit(fr)
	}
	var cmds []string
	for i := 0; i < 32; i++ {
		cmds = append(cmds, fmt.Sprintf("sudo config interface mtu Ethernet%d 9100", 4*i))
	}
	cmds = append(cmds, "sudo config interface fec Ethernet0 none")
	for _, c := range append(cmds, prep...) {
		if _, err := fr.Run(context.Background(), c, ""); err != nil {
			t.Fatalf("%s: %v", c, err)
		}
	}
	fr.Commands = nil
	b, err := os.ReadFile(filepath.Join(dir, "controller-10.6.109", "system_cfg-dx010-held.txt"))
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
	defaults := map[int][]string{}
	for _, p := range snap.Ports {
		defaults[p.Index] = append([]string{fmt.Sprintf("QSFP28 %d", p.Index)}, coll.DefaultPortNames(p)...)
	}
	desc := inform.Descriptor{MAC: snap.System.MAC}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: string(b)}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, DefaultPortNames: defaults})
	if err != nil {
		t.Fatal(err)
	}
	return &sonicRig{fr: fr, coll: coll, loop: l, sess: sess, log: &buf}
}

// Jumbo is device-wide and follows UniFi from the first push: the push
// that only moves the MTU goes through, 1500 on every port, and cage 1
// gets the RS its module needs; then nothing more.
func TestDX010FirstPushIsNotHeldForJumbo(t *testing.T) {
	r := newDX010HoldRig(t)
	cmds := r.applyAndReconcile(t)
	if strings.Contains(r.log.String(), "HOLDING") {
		t.Errorf("the push was held:\n%s", r.log.String())
	}
	if n := count(cmds, regexp.MustCompile(`^sudo config interface mtu Ethernet\d+ 1500$`)); n != 32 {
		t.Errorf("%d ports set to 1500, want 32", n)
	}
	if !contains(cmds, "sudo config interface fec Ethernet0 rs") {
		t.Error("cage 1's module did not get RS")
	}
	if n := count(cmds, regexp.MustCompile(`config (vlan|interface (startup|shutdown|speed))|description`)); n != 0 {
		t.Errorf("%d port-config writes; the push matches the switch apart from the MTU", n)
	}
}

// The hold still protects a port's own config: a port someone shut down on
// the switch holds the push -- that port alone, not the 32 the MTU moves
// -- and nothing is written, the MTU and FEC included.
func TestDX010FirstPushStillHeldForPortState(t *testing.T) {
	r := newDX010HoldRig(t, "sudo config interface shutdown Ethernet8")
	if !r.loop.applyPending(context.Background()) {
		t.Fatal("the push was not consumed")
	}
	if _, _, pending := r.sess.Pending(); !pending {
		t.Error("the held push must stay pending")
	}
	if !strings.Contains(r.log.String(), "HOLDING the first push") || !strings.Contains(r.log.String(), "it would change 1 ports [3]") {
		t.Errorf("log:\n%s", r.log.String())
	}
	if len(r.fr.Commands) != 0 {
		t.Errorf("a held push wrote %q", r.fr.Commands)
	}
}

// Speed "auto" follows the module (§10.9) through the loop: the same push
// -- no port speed in it, so every port is on auto -- with cage 1 forced
// to 40G on the switch (the command verified live on the DX010). The
// CWDM4 runs 100G only, so the first push puts it back, and the hold,
// which plans without the module's writes, does not wait on it.
func TestDX010FirstPushRestoresTheModulesSpeed(t *testing.T) {
	r := newDX010HoldRig(t, "sudo config interface speed Ethernet0 40000")
	cmds := r.applyAndReconcile(t)
	if strings.Contains(r.log.String(), "HOLDING") {
		t.Errorf("the push was held:\n%s", r.log.String())
	}
	for _, want := range []string{"sudo config interface speed Ethernet0 100000", "sudo config interface fec Ethernet0 rs"} {
		if !contains(cmds, want) {
			t.Errorf("missing %q", want)
		}
	}
	if n := count(cmds, regexp.MustCompile(`config interface (speed|breakout)`)); n != 1 {
		t.Errorf("%d speed/breakout writes; only cage 1's module asks for one", n)
	}
}

// NTP from the in-band interface (§10.6) through the loop: the same real
// push carries the site's NTP servers; with the image's NTP|global
// (src_intf eth0, ntp_global-init_cfg.txt read from the unit) in place,
// the push takes chrony off eth0 -- the bridge dials Vlan4094, which
// sonic-ntp.yang does not allow as src_intf, so the field is deleted and the
// kernel sources from the in-band interface (§11.8) -- and the reconcile
// writes nothing more.
func TestDX010FirstPushSourcesNTPInBand(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238526-ff3c0c9f6")
	b, err := os.ReadFile(filepath.Join(dir, "ntp_global-init_cfg.txt"))
	if err != nil {
		t.Fatal(err)
	}
	section := strings.TrimPrefix(strings.Split(string(b), "@@@ end")[0], "@@@ cfg_ntp_global\n")
	r := newDX010HoldRigWith(t, func(fr *sonic.FixtureRunner) { fr.UseSection("cfg_ntp_global", section) })
	r.loop.cfg.ControlNTP = true
	snap, err := r.coll.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.System.Addresses {
		if a.Iface == "Vlan4094" {
			r.coll.Dial = a.IP // as the bridge dials it
		}
	}
	cmds := r.applyAndReconcile(t)
	if !contains(cmds, "sudo sonic-db-cli CONFIG_DB hdel 'NTP|global' src_intf") {
		t.Errorf("chrony not taken off eth0; commands %q", cmds)
	}
}

// chrony left offline by its boot hook (§10.6) through the loop: every
// source offline (chrony-offline.txt, read from the unit) while Vlan4094 is
// up; the push that carries the site's NTP servers (unchanged, so no NTP
// write restarts chrony) nudges it with `sudo chronyc onoffline`, the
// sources come online (chrony-after-onoffline.txt, read from the unit right
// after), and the reconcile writes nothing more.
func TestDX010PushNudgesChronyAfterAReboot(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238526-ff3c0c9f6")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	offline := strings.SplitN(read("chrony-offline.txt"), "@@@ chrony_sources\n", 2)[1]
	srcs, act, _ := strings.Cut(offline, "@@@ chrony_activity\n")
	act, _, _ = strings.Cut(act, "@@@ end")
	r := newDX010HoldRigWith(t, func(fr *sonic.FixtureRunner) {
		fr.UseSection("chrony_sources", srcs)
		fr.UseSection("chrony_activity", act)
		fr.ChronyAfterOnoffline = read("chrony-after-onoffline.txt")
	})
	r.loop.cfg.ControlNTP = true
	snap, err := r.coll.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.System.Addresses {
		if a.Iface == "Vlan4094" {
			r.coll.Dial = a.IP // as the bridge dials it
		}
	}
	cmds := r.applyAndReconcile(t)
	if n := count(cmds, regexp.MustCompile(`^sudo chronyc onoffline$`)); n != 1 {
		t.Errorf("%d nudges, want 1; commands %q", n, cmds)
	}
	if strings.Contains(r.log.String(), "applied 1 switch-level settings") {
		t.Errorf("the nudge was reported as a setting:\n%s", r.log.String())
	}
}
