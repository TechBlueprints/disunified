package sonic

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// fixtureDot1X is the DX010 collected with the collector that reads
// 802.1X (2026-11-15, docs/drivers/sonic.md §11.26): disunified-l2d
// running with spanning tree on, docker present, no hostapd installed yet
// and no port controlled.
const fixtureDot1X = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/collect-dot1x.txt"

func dot1xRig(t *testing.T, edit func(*FixtureRunner)) *l2Rig {
	t.Helper()
	return newL2RigAt(t, fixtureDot1X, false, edit)
}

// dot1xDevice is the device push with 802.1X on, one RADIUS server.
func dot1xDevice(on bool) devicemodel.DeviceDesired {
	d := rstp
	d.Dot1X = &devicemodel.Dot1X{Enabled: on, Servers: []devicemodel.RADIUSServer{{Addr: "192.0.2.1", Port: 1812, Secret: "0123456789abcdef0123456789abcdef"}}}
	return d
}

// dot1xPortsOf sets cage 31 to auto, 30 to force-unauthorised and 29 to
// force-authorised.
func dot1xPortsOf(d *devicemodel.PortDesired) {
	switch d.Index {
	case 31:
		d.Dot1X = devicemodel.Dot1XAuto
	case 30:
		d.Dot1X = devicemodel.Dot1XForceUnauthorized
	case 29:
		d.Dot1X = devicemodel.Dot1XForceAuthorized
	}
}

// 802.1X is claimed where disunified-l2d can run hostapd -- installed, or
// docker to build it -- and the collector reports it; never on a capture
// from before it, with options.l2d off, or with neither hostapd nor docker.
func TestDot1XClaim(t *testing.T) {
	if !dot1xRig(t, nil).c.Capabilities().Dot1X {
		t.Error("a switch that can build hostapd claims 802.1X")
	}
	if newL2Rig(t, nil).c.Capabilities().Dot1X {
		t.Error("a capture from before 802.1X (no hostapd field) claims it")
	}
	r := dot1xRig(t, nil)
	r.c.L2 = false
	if r.c.Capabilities().Dot1X {
		t.Error("options.l2d off claims 802.1X")
	}
	none := dot1xRig(t, func(fr *FixtureRunner) {
		in := fr.l2Inst()
		in.Docker = false
		fr.setL2Inst(in)
	})
	if none.c.Capabilities().Dot1X {
		t.Error("no hostapd and no docker claims 802.1X")
	}
}

// The first push with 802.1X on: the controlled ports are closed with the
// MAC filter's learn mode (the force-authorised one is not), hostapd is
// built on the switch, l2d's config lists every controlled port and the
// servers without their secrets, and the secrets go to a root-only file of
// their own -- in no log line. A second push writes nothing; 802.1X off
// opens the ports and empties l2d's section.
func TestDot1XApply(t *testing.T) {
	r := dot1xRig(t, nil)
	if _, err := r.push(rstp, r.ports(nil)); err != nil {
		t.Fatal(err)
	}
	ds := r.ports(dot1xPortsOf)
	cmds, err := r.push(dot1xDevice(true), ds)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(cmds, "\n")
	for _, want := range []string{
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet120' learn_mode drop",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet116' learn_mode drop",
		hostapdBuildCommand,
		l2Command,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "'PORT|Ethernet112' learn_mode") {
		t.Errorf("the force-authorised port was closed:\n%s", all)
	}
	c := r.conf()
	if c.Dot1X == nil || c.Dot1X.Ports["Ethernet120"] != "auto" || c.Dot1X.Ports["Ethernet116"] != "force_unauthorized" ||
		c.Dot1X.Ports["Ethernet112"] != "force_authorized" || len(c.Dot1X.Ports) != 3 ||
		len(c.Dot1X.Servers) != 1 || c.Dot1X.Servers[0] != (l2RADIUS{IP: "192.0.2.1", Port: 1812}) {
		t.Errorf("l2d's 802.1X section = %+v", c.Dot1X)
	}
	if strings.Contains(r.fr.L2Files[l2ConfPath], "0123456789abcdef") {
		t.Errorf("l2.json (read by the login user) holds the secret:\n%s", r.fr.L2Files[l2ConfPath])
	}
	if got := r.fr.L2Files[dot1xSecretPath]; got != "0123456789abcdef0123456789abcdef\n" {
		t.Errorf("secrets file = %q", got)
	}
	s := r.fr.L2Scripts[len(r.fr.L2Scripts)-1]
	if !strings.Contains(s, "sudo install -D -m 0600 \"$t\" "+dot1xSecretPath) || !strings.Contains(s, "sudo systemctl reload disunified-l2d") {
		t.Errorf("the secrets are root's alone and l2d reloads:\n%s", s)
	}
	if strings.Contains(r.log.String(), "0123456789abcdef") {
		t.Errorf("the secret reached the log:\n%s", r.log.String())
	}
	if !strings.Contains(r.log.String(), "802.1X 3 ports, 1 RADIUS servers") || !strings.Contains(r.log.String(), "building hostapd 2.11") {
		t.Errorf("the log names what was done:\n%s", r.log.String())
	}
	if cmds, _ := r.push(dot1xDevice(true), ds); len(cmds) != 0 {
		t.Errorf("a second push wrote %q", cmds)
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := port(snap, 31); p.Dot1X != devicemodel.Dot1XAuto {
		t.Errorf("port 31 reads back 802.1X %q", p.Dot1X)
	}
	if p := port(snap, 29); p.Dot1X != devicemodel.Dot1XForceAuthorized {
		t.Errorf("port 29 reads back 802.1X %q", p.Dot1X)
	}

	// 802.1X off: the loop sends no port mode, the ports open.
	cmds, err = r.push(dot1xDevice(false), r.ports(nil))
	if err != nil {
		t.Fatal(err)
	}
	all = strings.Join(cmds, "\n")
	if !strings.Contains(all, "sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet120' learn_mode hardware") ||
		!strings.Contains(all, "sudo sonic-db-cli CONFIG_DB hdel 'PORT|Ethernet120' learn_mode") {
		t.Errorf("802.1X off opens the port:\n%s", all)
	}
	if c := r.conf(); c.Dot1X != nil {
		t.Errorf("802.1X off leaves l2d's section %+v", c.Dot1X)
	}
}

// A port in an aggregate is not controlled: it stays open and l2d never
// hears of it.
func TestDot1XLeavesAnAggregateOpen(t *testing.T) {
	r := dot1xRig(t, nil)
	ds := r.ports(func(d *devicemodel.PortDesired) {
		if d.Index == 30 || d.Index == 31 {
			d.LAG = 30
			d.Dot1X = devicemodel.Dot1XAuto
		}
	})
	cmds, err := r.push(dot1xDevice(true), ds)
	if err != nil {
		t.Fatal(err)
	}
	if all := strings.Join(cmds, "\n"); strings.Contains(all, "learn_mode drop") {
		t.Errorf("an aggregate's member was closed:\n%s", all)
	}
	if c := r.conf(); c.Dot1X != nil {
		t.Errorf("l2d was given an aggregate's member: %+v", c.Dot1X)
	}
}

// The hosts l2d admitted keep their FDB entries: the MAC filter's plan
// deletes every static entry it does not want, and l2d's are wanted while
// their port is controlled. The failed attempts reach Port.Health.
func TestDot1XKeepsAdmittedHosts(t *testing.T) {
	const row = `{
"DUI_DOT1X_TABLE|Ethernet120": {"authorized": "02:00:00:00:00:03", "failures": "4", "last_failed_mac": "02:00:00:00:00:01", "mode": "auto"}
}`
	r := dot1xRig(t, func(fr *FixtureRunner) {
		fr.setSection("l2_dot1x", row)
		fr.setSection("appl_fdb", `{
"FDB_TABLE:Vlan4094:02:00:00:00:00:03": {"port": "Ethernet120", "type": "static"}
}`)
	})
	ds := r.ports(dot1xPortsOf)
	cmds, err := r.push(dot1xDevice(true), ds)
	if err != nil {
		t.Fatal(err)
	}
	if all := strings.Join(cmds, "\n"); strings.Contains(all, "FDB_TABLE") {
		t.Errorf("an admitted host's entry was touched:\n%s", all)
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := port(snap, 31)
	if p.Health.Dot1XFailures != 4 || len(p.Dot1XAuthorized) != 1 || p.Dot1XAuthorized[0] != "02:00:00:00:00:03" {
		t.Errorf("port 31: failures %d, authorised %q", p.Health.Dot1XFailures, p.Dot1XAuthorized)
	}
	// The port no longer controlled: the entry goes with it.
	cmds, err = r.push(dot1xDevice(true), r.ports(nil))
	if err != nil {
		t.Fatal(err)
	}
	if all := strings.Join(cmds, "\n"); !strings.Contains(all, `"op":"del","table":"FDB_TABLE","key":"Vlan4094:02:00:00:00:00:03"`) {
		t.Errorf("an uncontrolled port's admission stays:\n%s", all)
	}
}
