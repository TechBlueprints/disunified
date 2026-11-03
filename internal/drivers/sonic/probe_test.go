package sonic

import (
	"context"
	"errors"
	"net"
	osuser "os/user"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// The bridge on the switch itself (docs/drivers/sonic-on-switch.md §2-3):
// the local runner, and the TCP probe that stands in for a fresh dial.

// probeAt is where the tests' probe points: the controller's inform port.
const probeAt = "192.0.2.1:8080"

// probeCall is one call of a recordingProbe.
type probeCall struct{ addr, from string }

// recordingProbe answers err to every call and records it.
type recordingProbe struct {
	err   error
	calls []probeCall
}

func (p *recordingProbe) probe(_ context.Context, addr, fromIP string) error {
	p.calls = append(p.calls, probeCall{addr, fromIP})
	return p.err
}

func TestOpenTransport(t *testing.T) {
	me, err := osuser.Current()
	if err != nil {
		t.Fatal(err)
	}
	open := func(ssh string, opts map[string]string) (*Collector, error) {
		dev, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{SSH: ssh, Options: opts})
		if err != nil {
			return nil, err
		}
		return dev.(*Collector), nil
	}
	if _, err := open("admin@192.0.2.10", map[string]string{"transport": "telnet"}); err == nil || !strings.Contains(err.Error(), "transport must be ssh or local") {
		t.Errorf("transport telnet: %v", err)
	}
	other := "not-" + me.Username
	if _, err := open(other+"@192.0.2.10", map[string]string{"transport": "local", "probe": probeAt}); err == nil || !strings.Contains(err.Error(), "the service must run as the login user") {
		t.Errorf("another user: %v", err)
	}
	if _, err := open(me.Username+"@192.0.2.10", map[string]string{"transport": "local"}); err == nil || !strings.Contains(err.Error(), "set options.probe host:port") {
		t.Errorf("no probe: %v", err)
	}
	if _, err := open(me.Username+"@192.0.2.10", map[string]string{"transport": "local", "probe": "192.0.2.1"}); err == nil {
		t.Error("a probe without a port must be refused")
	}
	c, err := open(me.Username+"@192.0.2.10", map[string]string{"transport": "local", "probe": probeAt, "ssh_key": "/nonexistent.pub"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.runner().(*sshrun.Local); !ok {
		t.Errorf("runner = %T, want *sshrun.Local", c.runner())
	}
	if _, ok := c.Redial("192.0.2.200").(*sshrun.Local); !ok {
		t.Error("a fresh connection on the switch is a fresh local runner")
	}
	if c.CurrentAddress() != "192.0.2.10" || c.Probe == nil || c.ProbeAddr != probeAt {
		t.Errorf("local: dial %q, probe %v at %q", c.CurrentAddress(), c.Probe != nil, c.ProbeAddr)
	}
	// Over SSH the dial is the proof: the probe the bridge fills for
	// every driver is not used.
	c, err = open("admin@192.0.2.10:2222", map[string]string{"probe": probeAt})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.runner().(*sshrun.SSH); !ok || c.Probe != nil || c.ProbeAddr != "" || c.CurrentAddress() != "192.0.2.10" {
		t.Errorf("ssh: runner %T, probe %v %q, dial %q", c.runner(), c.Probe != nil, c.ProbeAddr, c.CurrentAddress())
	}
}

func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	ctx := context.Background()
	if err := tcpProbe(ctx, ln.Addr().String(), "127.0.0.1"); err != nil {
		t.Errorf("bound to 127.0.0.1: %v", err)
	}
	if err := tcpProbe(ctx, ln.Addr().String(), ""); err != nil {
		t.Errorf("unbound: %v", err)
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	closed.Close()
	if err := tcpProbe(ctx, addr, "127.0.0.1"); err == nil {
		t.Error("a closed port must fail")
	}
	// An address this host does not have cannot be a source.
	if err := tcpProbe(ctx, ln.Addr().String(), "192.0.2.123"); err == nil || !strings.Contains(err.Error(), "assign requested address") {
		t.Errorf("bound to an address the host lacks: %v", err)
	}
}

// probeRig is the address controller's rig with the local transport's
// proof: no Redial use, a recording probe.
func probeRig(t *testing.T, err error) (*controlRig, *recordingProbe, string) {
	r := newRig(t, nil)
	dialled := ifaceAddr(t, r.fr.Fixture, "Vlan4094")
	r.c.Dial = dialled
	r.c.Redial = r.fr.Dial
	p := &recordingProbe{err: err}
	r.c.Probe, r.c.ProbeAddr = p.probe, probeAt
	return r, p, dialled
}

// The probe replaces the dial: the same move as TestApplyAddressSuccess,
// proven by a connection from the new address, finished through the runner
// the bridge has.
func TestApplyAddressProbe(t *testing.T) {
	r, p, old := probeRig(t, nil)
	r.c.StateDir = t.TempDir()
	gw := r.system().Gateway
	n, closes := len(r.fr.Commands), r.fr.Closes
	changed, err := r.c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.200", PrefixLen: 16, Gateway: gw, DNS: []string{"192.0.2.53"}})
	if err != nil || !changed {
		t.Fatalf("ApplyAddress = %v, %v", changed, err)
	}
	want := []string{
		"sudo config interface ip add Vlan4094 192.0.2.200/16",
		"sudo config dns nameserver add 192.0.2.53",
		"echo 1 | sudo tee /proc/sys/net/ipv4/conf/Vlan4094/promote_secondaries >/dev/null",
		"sudo config interface ip remove Vlan4094 " + old + "/16",
		"ip -4 -o addr show dev Vlan4094 | grep -q ' 192.0.2.200/16 '",
		save,
	}
	if got := r.fr.Commands[n:]; !reflect.DeepEqual(got, want) {
		t.Errorf("commands\n  got  %q\n  want %q", got, want)
	}
	if len(r.fr.Dials) != 0 {
		t.Errorf("nothing is dialled on the switch itself: %v", r.fr.Dials)
	}
	if want := []probeCall{{probeAt, "192.0.2.200"}}; !reflect.DeepEqual(p.calls, want) {
		t.Errorf("probe calls %v, want %v (from the new address)", p.calls, want)
	}
	if r.c.CurrentAddress() != "192.0.2.200" || r.c.savedDial != "192.0.2.200" {
		t.Errorf("now on %q (saved %q)", r.c.CurrentAddress(), r.c.savedDial)
	}
	if r.fr.Closes != closes || r.c.runner() != sshrun.Runner(r.fr) {
		t.Errorf("the runner is kept, not closed: %d closes, runner %T", r.fr.Closes-closes, r.c.runner())
	}
}

// A probe that fails takes the new address back off, as a failed dial does.
func TestApplyAddressProbeFails(t *testing.T) {
	r, p, old := probeRig(t, errors.New("connect: network is unreachable"))
	gw := r.system().Gateway
	n := len(r.fr.Commands)
	_, err := r.c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.200", PrefixLen: 16, Gateway: "192.0.2.254"})
	if err == nil || !strings.Contains(err.Error(), "does not reach "+probeAt+" from 192.0.2.200") || !strings.Contains(err.Error(), "keeps its old one") {
		t.Fatalf("ApplyAddress with a failing probe = %v", err)
	}
	want := []string{
		"sudo config interface ip add Vlan4094 192.0.2.200/16",
		"sudo config route add prefix 0.0.0.0/0 nexthop 192.0.2.254",
		"sudo config interface ip remove Vlan4094 192.0.2.200/16",
		"sudo config route del prefix 0.0.0.0/0 nexthop 192.0.2.254",
	}
	if got := r.fr.Commands[n:]; !reflect.DeepEqual(got, want) {
		t.Errorf("commands\n  got  %q\n  want %q", got, want)
	}
	if len(p.calls) != 1 || len(r.fr.Dials) != 0 {
		t.Errorf("probe calls %v, dials %v", p.calls, r.fr.Dials)
	}
	sys := r.system()
	if r.c.CurrentAddress() != old || sys.Addresses[0].IP != old || sys.Gateway != gw {
		t.Errorf("after a failed change: dial %q, addresses %v, gateway %q", r.c.CurrentAddress(), sys.Addresses, sys.Gateway)
	}
}

// The live failure (TestApplyAddressLosesTheNewAddress) on the switch
// itself: the batch stops at the verification, nothing is saved, the new
// address is a pending target -- and the bridge's own runner stays open.
func TestApplyAddressProbeLosesTheNewAddress(t *testing.T) {
	r, _, old := probeRig(t, nil)
	r.c.StateDir = t.TempDir()
	r.fr.IgnorePromote = true
	gw := r.system().Gateway
	closes := r.fr.Closes
	_, err := r.c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.200", PrefixLen: 16, Gateway: gw})
	if err == nil || !strings.Contains(err.Error(), "nothing was saved") || !strings.Contains(err.Error(), "reaches "+probeAt+" from 192.0.2.200") {
		t.Fatalf("ApplyAddress = %v", err)
	}
	if r.c.CurrentAddress() != old || r.c.pendingDial != "192.0.2.200" || r.fr.Closes != closes {
		t.Errorf("dial %q pending %q, %d closes", r.c.CurrentAddress(), r.c.pendingDial, r.fr.Closes-closes)
	}
}

// The first STP enable on the switch itself: the probe stands between the
// check and the dead-man's cancel. Failing, the timer stays armed.
func TestL2DeadmanProbe(t *testing.T) {
	installed := func(fr *FixtureRunner) {
		in := fr.l2Inst()
		in.Mstpd = mstpdVersion + "-"
		fr.setL2Inst(in)
	}
	r := newL2Rig(t, installed)
	p := &recordingProbe{err: errors.New("connect: no route to host")}
	r.c.Probe, r.c.ProbeAddr = p.probe, probeAt
	_, err := r.push(rstp, r.ports(nil))
	if err == nil || !strings.Contains(err.Error(), "does not reach "+probeAt) || !strings.Contains(err.Error(), "undoes spanning tree by itself") {
		t.Fatalf("an unreachable probe must leave the enable unconfirmed, got %v", err)
	}
	if r.inst().Deadman != "active" {
		t.Error("the dead-man timer must stay armed")
	}
	for _, c := range r.fr.Commands {
		if c == l2DeadmanCancel {
			t.Error("the dead-man was cancelled without the probe")
		}
	}
	if len(p.calls) != 10 || p.calls[0] != (probeCall{probeAt, ""}) {
		t.Errorf("probe calls %v: want 10 tries, unbound", p.calls)
	}

	r = newL2Rig(t, installed)
	p = &recordingProbe{}
	r.c.Probe, r.c.ProbeAddr = p.probe, probeAt
	if _, err := r.push(rstp, r.ports(nil)); err != nil {
		t.Fatal(err)
	}
	if r.inst().Deadman != "inactive" || len(p.calls) != 1 || !strings.Contains(r.log.String(), "the switch reaches "+probeAt+"; dead-man timer cancelled") {
		t.Errorf("a reachable probe cancels the timer: dead-man %s, probe calls %v, log %s", r.inst().Deadman, p.calls, r.log.String())
	}
}

// On the switch itself collect.py reports no SSH connection; the far end
// an IP ACL set must not cut is then the probe's host.
func TestIPRulesNeverCutTheProbe(t *testing.T) {
	r := newACLRig(t, func(fr *FixtureRunner) {
		// What collect.py prints without SSH_CONNECTION.
		on := strings.Replace(fr.Fixture, "@@@ ssh\n192.0.2.6 192.0.2.1\n", "@@@ ssh\n\n", 1)
		if on == fr.Fixture {
			t.Fatal("capture changed: no ssh section to empty")
		}
		fr.Fixture = on
	})
	if r.c.sshFrom != "" {
		t.Fatalf("ssh %q", r.c.sshFrom)
	}
	r.c.Dial = "192.0.2.1"
	sw := devicemodel.IPACLEnd{CIDR: "192.0.2.0/16"}
	// Each case its own rule name: a hold is logged once per message.
	block := func(name, src string) []devicemodel.IPACL {
		return []devicemodel.IPACL{{Name: name, Src: devicemodel.IPACLEnd{CIDR: src}, Dst: sw}}
	}
	for _, tc := range []struct {
		name, probe, src string
		held             bool
	}{
		{"no probe: every source counts", "", "192.0.2.0/29", true},
		{"from the probe's host", "203.0.113.5:8080", "203.0.113.0/24", true},
		{"from elsewhere", "203.0.113.5:8080", "192.0.2.0/29", false},
	} {
		r.c.ProbeAddr = tc.probe
		r.log.Reset()
		got := r.deviceCmds(devicemodel.DeviceDesired{IPACLSet: true, IPACLs: block(tc.name, tc.src)})
		held := strings.Contains(r.log.String(), "would block")
		if held != tc.held || held && len(got) != 0 {
			t.Errorf("%s: held %v (want %v), wrote %q, log %s", tc.name, held, tc.held, got, r.log.String())
		}
		r.deviceCmds(devicemodel.DeviceDesired{IPACLSet: true})
		if _, err := r.c.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
