package sonic

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// l2Fixture is the DX010 collected with the collector that reads
// disunified-l2d's state, before anything was installed: no mstpd, no
// daemon, docker present, the kernel bridge's stp_state 0 (§11.5).
const l2Fixture = "../../../docs/fixtures/sonic-202505.1238526-ff3c0c9f6/collect-l2.txt"

type l2Rig struct {
	t   *testing.T
	c   *Collector
	fr  *FixtureRunner
	log *strings.Builder
}

func newL2Rig(t *testing.T, edit func(*FixtureRunner)) *l2Rig {
	t.Helper()
	fr, err := NewFixtureRunner(l2Fixture)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fr.Help, fr.Zones = vs.Help, vs.Zones
	if edit != nil {
		edit(fr)
	}
	var buf strings.Builder
	c := NewCollector(fr)
	c.Log = log.New(&buf, "", 0)
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	c.Redial = fr.Dial
	c.L2 = true
	var zero time.Duration
	c.L2Wait = &zero
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &l2Rig{t: t, c: c, fr: fr, log: &buf}
}

// ports is the controller's port push for the switch as it is, with fn
// applied to each port.
func (r *l2Rig) ports(fn func(*devicemodel.PortDesired)) []devicemodel.PortDesired {
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	var ds []devicemodel.PortDesired
	for _, p := range snap.Ports {
		d := devicemodel.PortDesired{Index: p.Index, Enabled: p.Enabled, Description: p.Description, VLANSet: true, NativeVLAN: 1, STPEdgeMode: "auto"}
		if fn != nil {
			fn(&d)
		}
		ds = append(ds, d)
	}
	return ds
}

// push runs the loop's order -- ApplyDevice, then ApplyPorts -- and returns
// the commands written and ApplyPorts' error.
func (r *l2Rig) push(dev devicemodel.DeviceDesired, ports []devicemodel.PortDesired) ([]string, error) {
	r.t.Helper()
	n := len(r.fr.Commands)
	if _, err := r.c.ApplyDevice(context.Background(), dev); err != nil {
		r.t.Fatalf("ApplyDevice: %v", err)
	}
	_, err := r.c.ApplyPorts(context.Background(), ports)
	return append([]string(nil), r.fr.Commands[n:]...), err
}

func (r *l2Rig) conf() l2Conf {
	r.t.Helper()
	var c l2Conf
	if err := json.Unmarshal([]byte(r.fr.L2Files[l2ConfPath]), &c); err != nil {
		r.t.Fatalf("l2.json: %v", err)
	}
	return c
}

func (r *l2Rig) inst() l2Inst { return r.fr.l2Inst() }

var rstp = devicemodel.DeviceDesired{STPSet: true, STPEnabled: true, STPMode: "rstp", STPPriority: 32768}

func TestL2CapabilitiesFollowTheSwitch(t *testing.T) {
	r := newL2Rig(t, nil)
	r.c.L2 = false
	if caps := r.c.Capabilities(); caps.STP || caps.LoopProtection {
		t.Errorf("with options.l2d off nothing is claimed: %+v", caps)
	}
	if cmds, _ := r.push(rstp, r.ports(nil)); len(cmds) != 0 {
		t.Errorf("with options.l2d off nothing is written: %q", cmds)
	}
	r = newL2Rig(t, nil)
	caps := r.c.Capabilities()
	if !caps.STP || !caps.BPDUGuard || !caps.STPAutoEdge || !caps.STPPortCost || !caps.LoopProtection {
		t.Errorf("a switch that can build mstpd claims RSTP, BPDU guard, auto edge, port cost and loop protection: %+v", caps)
	}
	none := newL2Rig(t, func(fr *FixtureRunner) {
		in := fr.l2Inst()
		in.Docker = false
		fr.setL2Inst(in)
	})
	if caps := none.c.Capabilities(); caps.STP || caps.BPDUGuard || caps.STPAutoEdge || caps.LoopProtection {
		t.Errorf("no mstpd and no docker: nothing to claim, got %+v", caps)
	}
	if _, err := none.push(rstp, none.ports(nil)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none.log.String(), "no mstpd and no docker to build it") {
		t.Errorf("the refusal must say why: %s", none.log.String())
	}
}

// The first push with spanning tree on: mstpd is built on the switch, then
// disunified-l2d is installed and started under the dead-man timer, which a
// fresh connection cancels once mstpd runs; a second push writes nothing.
func TestL2FirstEnable(t *testing.T) {
	r := newL2Rig(t, nil)
	if _, err := r.c.ApplyDevice(context.Background(), rstp); err != nil {
		t.Fatal(err)
	}
	if r.fr.Builds != 0 || len(r.fr.L2Scripts) != 0 {
		t.Fatal("nothing is written before the ports' settings are known (ApplyPorts follows ApplyDevice)")
	}
	cmds, err := r.push(rstp, r.ports(nil))
	if err != nil {
		t.Fatal(err)
	}
	if r.fr.Builds != 1 || len(r.fr.L2Scripts) != 0 || !contains(cmds, mstpdBuildCommand) {
		t.Fatalf("first push: build %d, l2 scripts %d (%q); want the build only", r.fr.Builds, len(r.fr.L2Scripts), cmds)
	}
	if r.inst().Mstpd != "0.2.0-" {
		t.Fatal("the fixture's build did not install mstpd")
	}
	cmds, err = r.push(rstp, r.ports(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.fr.L2Scripts) != 1 {
		t.Fatalf("second push: %d l2 scripts, want 1 (%q)", len(r.fr.L2Scripts), cmds)
	}
	s := r.fr.L2Scripts[0]
	for _, want := range []string{
		"sudo install -D -m 0755 \"$t\" " + l2dPath,
		"sudo install -D -m 0755 \"$t\" " + bridgeSTP,
		"sudo install -D -m 0644 \"$t\" " + l2dUnitPath,
		"sudo install -D -m 0644 \"$t\" " + l2ConfPath,
		"sudo systemctl daemon-reload",
		"sudo systemd-run --quiet --unit=disunified-l2d-deadman --on-active=180 /usr/bin/python3 " + l2dPath + " --deadman",
		"sudo systemctl restart disunified-l2d",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the install script lacks %q", want)
		}
	}
	if strings.Index(s, "systemd-run") > strings.Index(s, "systemctl restart") {
		t.Error("the dead-man timer must be armed before the daemon starts")
	}
	if r.fr.L2Files[l2dPath] != l2dScript || r.fr.L2Files[bridgeSTP] != bridgeSTPScript || r.fr.L2Files[l2dUnitPath] != l2dUnit {
		t.Error("the installed files differ from the embedded ones")
	}
	c := r.conf()
	if !c.STP.Enabled || c.STP.Version != "rstp" || c.STP.Priority != 32768 || c.LoopGuard.Enabled {
		t.Errorf("config = %+v", c)
	}
	if p := c.STP.Ports["Ethernet0"]; !p.Enabled || p.Edge != "auto" || p.BPDUGuard {
		t.Errorf("Ethernet0 = %+v, want STP on, edge auto, no BPDU guard", p)
	}
	if len(c.STP.Ports) != 32 {
		t.Errorf("%d ports in the config, want 32", len(c.STP.Ports))
	}
	if !contains(cmds, l2DeadmanCancel) || r.inst().Deadman != "inactive" {
		t.Errorf("the dead-man timer must be cancelled through a fresh connection: %q", cmds)
	}
	if len(r.fr.Dials) == 0 {
		t.Error("the confirmation must dial the switch afresh")
	}
	if cmds, err := r.push(rstp, r.ports(nil)); err != nil || len(cmds) != 0 {
		t.Errorf("a third push wrote %q (%v)", cmds, err)
	}
}

// Port settings change the config and reload the running daemon; no
// dead-man timer once spanning tree runs.
func TestL2PortSettings(t *testing.T) {
	r := enabled(t)
	ds := r.ports(func(d *devicemodel.PortDesired) {
		switch d.Index {
		case 2:
			d.BPDUGuard = true
		case 3:
			d.STPEdge = true // "STP disabled" on the port
		case 4:
			d.STPEdgeMode = "disabled"
		case 5:
			d.STPEdgeMode = "enabled"
			d.LoopProtect = true
		}
	})
	cmds, err := r.push(rstp, ds)
	if err != nil {
		t.Fatal(err)
	}
	s := r.fr.L2Scripts[len(r.fr.L2Scripts)-1]
	if !contains(cmds, l2Command) || !strings.Contains(s, "sudo systemctl reload disunified-l2d") || strings.Contains(s, "systemd-run") {
		t.Errorf("a settings change is a config write and a reload, no dead-man: %q", s)
	}
	c := r.conf()
	want := map[string]l2Port{
		"Ethernet4":  {Enabled: true, Edge: "auto", BPDUGuard: true},
		"Ethernet8":  {Enabled: false, Edge: "auto"},
		"Ethernet12": {Enabled: true, Edge: "disabled"},
		"Ethernet16": {Enabled: true, Edge: "enabled"},
	}
	for n, p := range want {
		if c.STP.Ports[n] != p {
			t.Errorf("%s = %+v, want %+v", n, c.STP.Ports[n], p)
		}
	}
	g := c.LoopGuard
	if !g.Enabled || len(g.Ports) != 1 || g.Ports[0] != "Ethernet16" || g.Threshold != 10 || g.Interval != 5 || g.Hold != 150 {
		t.Errorf("loop protection = %+v, want Ethernet16 at 10 moves / 5 s, 150 s down", g)
	}
	if cmds, _ := r.push(rstp, ds); len(cmds) != 0 {
		t.Errorf("a second push wrote %q", cmds)
	}
	// STP off: the config says so and the daemon tears it down itself.
	off := devicemodel.DeviceDesired{STPSet: true, STPMode: "rstp", STPPriority: 32768}
	if _, err := r.push(off, ds); err != nil {
		t.Fatal(err)
	}
	if c := r.conf(); c.STP.Enabled || !c.LoopGuard.Enabled {
		t.Errorf("STP off keeps loop protection running: %+v", c)
	}
}

// A fresh connection that fails after the enable leaves the dead-man timer
// to undo it; the switch marks its config, and the driver does not enable
// again until the controller has turned STP off and on.
func TestL2DeadmanPath(t *testing.T) {
	r := newL2Rig(t, func(fr *FixtureRunner) {
		in := fr.l2Inst()
		in.Mstpd = mstpdVersion + "-"
		fr.setL2Inst(in)
		fr.STPCutsUplink = true
	})
	_, err := r.push(rstp, r.ports(nil))
	if err == nil || !strings.Contains(err.Error(), "undoes spanning tree by itself") {
		t.Fatalf("an unconfirmed enable must fail loudly, got %v", err)
	}
	if r.inst().Deadman != "active" {
		t.Fatal("the dead-man timer must stay armed")
	}
	// The timer fired: l2d --deadman tore STP down and marked the config.
	in := r.inst()
	fired := *in.Conf
	fired.STP.Enabled, fired.Deadman = false, "2026-11-01T04:00:00-0600"
	in.Conf, in.Deadman = &fired, "inactive"
	r.fr.setL2Inst(in)
	r.fr.STPCutsUplink = false
	n := len(r.fr.L2Scripts)
	if _, err := r.push(rstp, r.ports(nil)); err != nil {
		t.Fatal(err)
	}
	if len(r.fr.L2Scripts) != n || !strings.Contains(r.log.String(), "dead-man timer undid spanning tree") {
		t.Fatalf("after the dead-man the driver must not enable again by itself: %d scripts, log %s", len(r.fr.L2Scripts)-n, r.log.String())
	}
	off := devicemodel.DeviceDesired{STPSet: true, STPMode: "rstp", STPPriority: 32768}
	if _, err := r.push(off, r.ports(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.push(rstp, r.ports(nil)); err != nil {
		t.Fatal(err)
	}
	c := r.conf()
	if !c.STP.Enabled || c.Deadman != "" || r.inst().Deadman != "inactive" {
		t.Errorf("off then on re-arms the first enable: %+v, dead-man %s", c, r.inst().Deadman)
	}
}

func TestL2BuildFailureIsRetriedLater(t *testing.T) {
	r := newL2Rig(t, func(fr *FixtureRunner) { fr.BuildFails = true })
	for i := 0; i < 3; i++ {
		if _, err := r.push(rstp, r.ports(nil)); err != nil {
			t.Fatal(err)
		}
	}
	if r.fr.Builds != 1 || len(r.fr.L2Scripts) != 0 {
		t.Errorf("builds %d, l2 scripts %d: a failed build is retried after 10 minutes, nothing installed meanwhile", r.fr.Builds, len(r.fr.L2Scripts))
	}
	if !strings.Contains(r.log.String(), "failed: wget") {
		t.Errorf("the build's failure must be logged: %s", r.log.String())
	}
}

// enabled is a rig whose switch runs disunified-l2d with spanning tree on.
func enabled(t *testing.T) *l2Rig {
	t.Helper()
	r := newL2Rig(t, func(fr *FixtureRunner) {
		in := fr.l2Inst()
		in.Mstpd = mstpdVersion + "-"
		fr.setL2Inst(in)
	})
	if _, err := r.push(rstp, r.ports(nil)); err != nil {
		t.Fatal(err)
	}
	if !r.conf().STP.Enabled || r.inst().Service != "active" {
		t.Fatal("setup: spanning tree is not on")
	}
	return r
}

func TestBridgeID(t *testing.T) {
	// mstpd's BR_ID_FMT (ctl_main.c): priority nibble, system id extension, MAC.
	prio, mac, ok := bridgeID("2.000.02:00:00:00:00:C1")
	if !ok || prio != 8192 || mac != "02:00:00:00:00:c1" {
		t.Errorf("bridgeID = %d %q %v", prio, mac, ok)
	}
	if _, _, ok := bridgeID("garbage"); ok {
		t.Error("garbage parsed")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// RSTP through disunified-l2d is the default since the driver's own path was
// verified live (2026-11-01); options.l2d: off is the escape.
func TestL2Option(t *testing.T) {
	for _, tc := range []struct {
		option string
		want   bool
	}{{"", true}, {"on", true}, {"off", false}, {"no", false}} {
		dev, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{SSH: "admin@192.0.2.9", Options: map[string]string{"l2d": tc.option}})
		if err != nil {
			t.Fatalf("Open(l2d=%q): %v", tc.option, err)
		}
		if got := dev.(*Collector).L2; got != tc.want {
			t.Errorf("l2d=%q: L2 = %v, want %v", tc.option, got, tc.want)
		}
	}
	if _, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{SSH: "admin@192.0.2.9", Options: map[string]string{"l2d": "maybe"}}); err == nil {
		t.Error("l2d=maybe must be refused")
	}
}
