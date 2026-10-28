package sonic

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The device controller, the key installer, the rebooter and the address
// controller against the capture (§6.3, §6.4).

// device applies d and returns the commands written; then it collects and
// checks a second apply writes nothing.
func (r *controlRig) device(name string, d devicemodel.DeviceDesired, want []string) {
	r.t.Helper()
	n := len(r.fr.Commands)
	if _, err := r.c.ApplyDevice(context.Background(), d); err != nil {
		r.t.Fatalf("%s: ApplyDevice: %v", name, err)
	}
	if got := r.fr.Commands[n:]; !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
		r.t.Errorf("%s: commands\n  got  %q\n  want %q", name, got, want)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	n = len(r.fr.Commands)
	if changed, err := r.c.ApplyDevice(context.Background(), d); err != nil || changed != 0 || len(r.fr.Commands) != n {
		r.t.Errorf("%s: a second apply wrote %q (%v)", name, r.fr.Commands[n:], err)
	}
}

func (r *controlRig) system() devicemodel.System {
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return snap.System
}

// (b) NTP: the controller's servers are added; only the ones the bridge
// added are removed; the operator's own stay; ownership survives a restart.
func TestApplyNTPOwnership(t *testing.T) {
	r := newRig(t, nil)
	dir := t.TempDir()
	r.c.StateDir = dir
	r.device("two servers", devicemodel.DeviceDesired{ManageNTP: true, NTPServers: []string{"192.0.2.10", "192.0.2.11"}}, []string{
		"sudo config ntp add 192.0.2.10", "sudo config ntp add 192.0.2.11", save,
	})
	if got := r.system().NTPServers; !reflect.DeepEqual(got, []string{"192.0.2.10", "192.0.2.11"}) {
		t.Errorf("read back NTP = %v", got)
	}
	r.device("one server", devicemodel.DeviceDesired{ManageNTP: true, NTPServers: []string{"192.0.2.10"}}, []string{
		"sudo config ntp del 192.0.2.11", save,
	})
	// An operator's own server is never removed.
	if _, err := r.fr.Run(context.Background(), "sudo config ntp add 192.0.2.20", ""); err != nil {
		t.Fatal(err)
	}
	r.system()
	// A fresh collector on the same state dir knows what the bridge owns.
	c2 := NewCollector(r.fr)
	c2.Log = r.c.Log
	c2.StateDir = dir
	if err := c2.loadManaged(); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r2 := &controlRig{t: t, c: c2, fr: r.fr, log: r.log}
	r2.device("none", devicemodel.DeviceDesired{ManageNTP: true}, []string{"sudo config ntp del 192.0.2.10", save})
	if got := r2.system().NTPServers; !reflect.DeepEqual(got, []string{"192.0.2.20"}) {
		t.Errorf("the operator's server must stay: %v", got)
	}
	// A hostname is resolved by the bridge; one that does not resolve is logged.
	r2.c.Resolve = func(h string) (string, error) {
		if h == "ntp.example.net" {
			return "192.0.2.12", nil
		}
		return resolveIP(h)
	}
	r2.device("by name", devicemodel.DeviceDesired{ManageNTP: true, NTPServers: []string{"ntp.example.net"}}, []string{"sudo config ntp add 192.0.2.12", save})
}

// A pool hostname answers a different address on every lookup; the driver
// pins the first answer and keeps it while the hostname stays in the push
// and the address stays on the switch (live, re-resolving on every push
// churned `ntp add <new>; ntp del <old>` each cycle).
func TestNTPHostnamePinned(t *testing.T) {
	r := newRig(t, nil)
	dir := t.TempDir()
	r.c.StateDir = dir
	n := 0
	pool := func(h string) (string, error) {
		switch h {
		case "pool.example.net":
			n++
			return "192.0.2." + strconv.Itoa(100+n), nil
		case "nowhere.example.net":
			return "", fmt.Errorf("cannot resolve %q: no such host", h)
		}
		return h, nil
	}
	r.c.Resolve = pool
	push := devicemodel.DeviceDesired{ManageNTP: true, NTPServers: []string{"pool.example.net", "192.0.2.1", "nowhere.example.net"}}
	// device() applies twice: the second lookup answers .102, and nothing is written.
	r.device("pool + IP", push, []string{"sudo config ntp add 192.0.2.101", "sudo config ntp add 192.0.2.1", save})
	if n < 1 || strings.Count(r.log.String(), "nowhere.example.net") != 1 {
		t.Errorf("an unresolvable name is logged once and skipped: %q", r.log.String())
	}
	// Across a restart: the pin is in the state dir.
	c2 := NewCollector(r.fr)
	c2.Log, c2.StateDir, c2.Resolve = r.c.Log, dir, pool
	if err := c2.loadManaged(); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r2 := &controlRig{t: t, c: c2, fr: r.fr, log: r.log}
	r2.device("after a restart", push, nil)
	// Removed from the switch by hand: resolved again.
	if _, err := r.fr.Run(context.Background(), "sudo config ntp del 192.0.2.101", ""); err != nil {
		t.Fatal(err)
	}
	r2.system()
	before := n
	r2.device("pinned address gone", push, []string{"sudo config ntp add 192.0.2." + strconv.Itoa(101+before), save})
	// The hostname leaves (its address goes) and comes back (resolved again).
	r2.device("hostname left", devicemodel.DeviceDesired{ManageNTP: true, NTPServers: []string{"192.0.2.1"}}, []string{"sudo config ntp del 192.0.2." + strconv.Itoa(101+before), save})
	before = n
	r2.device("hostname back", push, []string{"sudo config ntp add 192.0.2." + strconv.Itoa(101+before), save})
}

func TestApplySyslog(t *testing.T) {
	r := newRig(t, nil)
	r.device("two hosts", devicemodel.DeviceDesired{ManageSyslog: true, SyslogHosts: []string{"192.0.2.30", "192.0.2.31:5514"}}, []string{
		"sudo config syslog add 192.0.2.30 --port 514", "sudo config syslog add 192.0.2.31 --port 5514", save,
	})
	if got := r.system().SyslogHosts; !reflect.DeepEqual(got, []string{"192.0.2.30", "192.0.2.31:5514"}) {
		t.Errorf("read back syslog = %v", got)
	}
	r.device("port change, one gone", devicemodel.DeviceDesired{ManageSyslog: true, SyslogHosts: []string{"192.0.2.31:6514"}}, []string{
		"sudo config syslog del 192.0.2.31", "sudo config syslog add 192.0.2.31 --port 6514", "sudo config syslog del 192.0.2.30", save,
	})
}

// (c) SNMP: the managed community is replaced; the image's own (the
// capture's community-1, `public` before the scrub) is left alone.
func TestApplySNMPCommunity(t *testing.T) {
	r := newRig(t, nil)
	r.device("set", devicemodel.DeviceDesired{SNMPSet: true, SNMPCommunity: "lab-ro"}, []string{
		"sudo config snmp community add 'lab-ro' RO", save,
	})
	r.device("change", devicemodel.DeviceDesired{SNMPSet: true, SNMPCommunity: "lab-ro2"}, []string{
		"sudo config snmp community add 'lab-ro2' RO", "sudo config snmp community del 'lab-ro'", save,
	})
	if got := r.system().SNMPCommunities; !reflect.DeepEqual(got, []string{"community-1", "lab-ro2"}) {
		t.Errorf("read back communities = %v", got)
	}
	r.device("off", devicemodel.DeviceDesired{SNMPSet: true}, []string{"sudo config snmp community del 'lab-ro2'", save})
	if got := r.system().SNMPCommunities; !reflect.DeepEqual(got, []string{"community-1"}) {
		t.Errorf("the image's community must stay: %v", got)
	}
	if strings.Contains(r.log.String(), "lab-ro") {
		t.Error("a community is a credential and must not be logged")
	}
}

// STP and the snooping features: refused with a log on a community image;
// with FEATURE|stp, PVST is written (UniFi stp/rstp -> pvst, mstp refused).
func TestApplySTPIsFeatureGated(t *testing.T) {
	r := newRig(t, nil)
	on := true
	r.device("refused", devicemodel.DeviceDesired{STPSet: true, STPEnabled: true, STPMode: "rstp", STPPriority: 4096,
		IGMPSnooping: map[int]bool{1: true}, DHCPSnooping: &on}, nil)
	for _, want := range []string{"no STP daemon", "IGMP snooping requested", "DHCP snooping requested"} {
		if !strings.Contains(r.log.String(), want) {
			t.Errorf("log lacks %q: %s", want, r.log.String())
		}
	}
	withSTP := newRig(t, func(fr *FixtureRunner) {
		fr.Fixture = strings.Replace(fr.Fixture, "@@@ cfg_feature\n{\n", "@@@ cfg_feature\n{\n"+`"FEATURE|stp": {"state": "enabled"},`+"\n", 1)
	})
	withSTP.device("rstp at 4096", devicemodel.DeviceDesired{STPSet: true, STPEnabled: true, STPMode: "rstp", STPPriority: 4096}, []string{
		"sudo config spanning-tree enable pvst", "sudo config spanning-tree priority 4096", save,
	})
	if sys := withSTP.system(); sys.STPMode != "stp" || sys.STPPriority != 4096 {
		t.Errorf("read back STP = %s/%d", sys.STPMode, sys.STPPriority)
	}
	withSTP.device("mstp refused", devicemodel.DeviceDesired{STPSet: true, STPEnabled: true, STPMode: "mstp", STPPriority: 4096}, nil)
	if !strings.Contains(withSTP.log.String(), "MSTP requested") {
		t.Error("the MSTP refusal must be logged")
	}
	withSTP.device("off", devicemodel.DeviceDesired{STPSet: true}, []string{"sudo config spanning-tree disable pvst", save})
}

// SSH keys: the bridge's marked block is rewritten whole, atomically, and
// compared by type and value. (Test key values are not real keys.)
func TestInstallSSHKeys(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	keys := []devicemodel.SSHKey{
		{Type: "ssh-ed25519", Value: "dGVzdC1rZXktb25l", Comment: "ops@example.net"},
		{Type: "ssh-rsa", Value: "dGVzdC1rZXktdHdv"},
		{Type: "ssh-rsa", Value: "<pubkey>"}, // not a key: skipped
	}
	n, err := r.c.InstallSSHKeys(ctx, keys)
	if err != nil || n != 2 || len(r.fr.KeyScripts) != 1 {
		t.Fatalf("InstallSSHKeys = %d, %v; scripts %d", n, err, len(r.fr.KeyScripts))
	}
	script := r.fr.KeyScripts[0]
	for _, want := range []string{keysBegin + "\nssh-ed25519 dGVzdC1rZXktb25l ops@example.net\nssh-rsa dGVzdC1rZXktdHdv\n" + keysEnd, `mktemp`, `mv -f "$tmp" "$f"`, `skip=1`} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.c.InstallSSHKeys(ctx, keys); n != 0 || len(r.fr.KeyScripts) != 1 {
		t.Error("the same keys again must write nothing")
	}
	// A comment change alone is not a change; a key gone is.
	keys[0].Comment = "renamed"
	if n, _ := r.c.InstallSSHKeys(ctx, keys); n != 0 {
		t.Error("compared by type and value only")
	}
	if n, _ := r.c.InstallSSHKeys(ctx, keys[1:]); n != 1 || len(r.fr.KeyScripts) != 2 {
		t.Error("removing a key must rewrite the block")
	}
	if n, _ := r.c.InstallSSHKeys(ctx, nil); n != 0 || len(r.fr.KeyScripts) != 3 || strings.Contains(r.fr.KeyScripts[2], "DISUNIFIED_KEYS") {
		t.Error("no keys: the block is removed and none written")
	}
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(r.fr.section("authorized_keys")) != "" {
		t.Errorf("block left behind: %q", r.fr.section("authorized_keys"))
	}
}

func TestReboot(t *testing.T) {
	r := newRig(t, nil)
	if err := r.c.Reboot(context.Background()); err != nil || r.fr.Commands[len(r.fr.Commands)-1] != "sudo reboot" {
		t.Errorf("Reboot: %v, %q", err, r.fr.Commands)
	}
}

// (d) the address: add the new one, prove it by dialling, then remove the
// old one and move over; a dial that fails takes the new address back off.
func TestApplyAddressSuccess(t *testing.T) {
	r := newRig(t, nil)
	r.c.Dial = ifaceAddr(t, r.fr.Fixture, "Vlan4094")
	r.c.Redial = r.fr.Dial
	dir := t.TempDir()
	r.c.StateDir = dir
	sys := r.system()
	old, gw := sys.Addresses[0], sys.Gateway
	if old.Iface != "Vlan4094" {
		t.Fatalf("dialled address = %+v", old)
	}
	n := len(r.fr.Commands)
	d := devicemodel.AddressDesired{IP: "192.0.2.200", PrefixLen: 16, Gateway: gw, DNS: []string{"192.0.2.53"}}
	changed, err := r.c.ApplyAddress(context.Background(), d)
	if err != nil || !changed {
		t.Fatalf("ApplyAddress = %v, %v", changed, err)
	}
	// The new address is a secondary in the old one's subnet: with SONiC's
	// promote_secondaries=0 removing the old one would take it too (live).
	want := []string{
		"sudo config interface ip add Vlan4094 192.0.2.200/16",
		"sudo config dns nameserver add 192.0.2.53",
		"echo 1 | sudo tee /proc/sys/net/ipv4/conf/Vlan4094/promote_secondaries >/dev/null",
		"sudo config interface ip remove Vlan4094 " + old.IP + "/16",
		"ip -4 -o addr show dev Vlan4094 | grep -q ' 192.0.2.200/16 '",
		save,
	}
	if got := r.fr.Commands[n:]; !reflect.DeepEqual(got, want) {
		t.Errorf("commands\n  got  %q\n  want %q", got, want)
	}
	if !reflect.DeepEqual(r.fr.Dials, []string{"192.0.2.200"}) || r.c.Dial != "192.0.2.200" {
		t.Errorf("dialled %v, now on %q", r.fr.Dials, r.c.Dial)
	}
	// The dial target is persisted for a restart.
	c2 := NewCollector(r.fr)
	c2.StateDir = dir
	if err := c2.loadManaged(); err != nil || c2.savedDial != "192.0.2.200" || c2.pendingDial != "" {
		t.Errorf("persisted dial = %q pending %q (%v)", c2.savedDial, c2.pendingDial, err)
	}
	sys = r.system()
	if sys.Addresses[0].IP != "192.0.2.200" || sys.Addresses[0].Iface != "Vlan4094" || sys.DHCP {
		t.Errorf("read back addresses %v (DHCP %v)", sys.Addresses, sys.DHCP)
	}
	for _, a := range sys.Addresses {
		if a.IP == old.IP {
			t.Errorf("the old address is still there: %v", sys.Addresses)
		}
	}
	n = len(r.fr.Commands)
	if changed, err := r.c.ApplyAddress(context.Background(), d); changed || err != nil || len(r.fr.Commands) != n {
		t.Errorf("a second apply: %v %v %q", changed, err, r.fr.Commands[n:])
	}
	// A gateway change on the same address: the new route, then the old one goes.
	n = len(r.fr.Commands)
	d.Gateway = "192.0.2.254"
	if changed, err := r.c.ApplyAddress(context.Background(), d); !changed || err != nil {
		t.Fatalf("gateway change: %v %v", changed, err)
	}
	if got, want := r.fr.Commands[n:], []string{"sudo config route add prefix 0.0.0.0/0 nexthop 192.0.2.254", "sudo config route del prefix 0.0.0.0/0 nexthop " + gw, save}; !reflect.DeepEqual(got, want) {
		t.Errorf("gateway change: %q", got)
	}
	if sys := r.system(); sys.Gateway != "192.0.2.254" {
		t.Errorf("read back gateway = %q", sys.Gateway)
	}
}

func TestApplyAddressDialFails(t *testing.T) {
	r := newRig(t, nil)
	dialled := ifaceAddr(t, r.fr.Fixture, "Vlan4094")
	r.c.Dial = dialled
	r.c.Redial = r.fr.Dial
	r.fr.DialFails = true
	gw := r.system().Gateway
	n := len(r.fr.Commands)
	_, err := r.c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.200", PrefixLen: 16, Gateway: "192.0.2.254"})
	if err == nil || !strings.Contains(err.Error(), "does not answer at 192.0.2.200") || !strings.Contains(err.Error(), "keeps its old one") {
		t.Fatalf("ApplyAddress with no answer = %v", err)
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
	sys := r.system()
	if r.c.Dial != dialled || sys.Addresses[0].IP != dialled || sys.Gateway != gw {
		t.Errorf("after a failed change: dial %q, addresses %v, gateway %q", r.c.Dial, sys.Addresses, sys.Gateway)
	}
}

// The live failure: promote_secondaries not in effect, so removing the old
// primary takes the new secondary with it. The verification stops the
// batch before save; the runner stays on the old target and the new address
// is left as a pending dial target.
func TestApplyAddressLosesTheNewAddress(t *testing.T) {
	r := newRig(t, nil)
	dialled := ifaceAddr(t, r.fr.Fixture, "Vlan4094")
	r.c.Dial = dialled
	r.c.Redial = r.fr.Dial
	r.c.StateDir = t.TempDir()
	r.fr.IgnorePromote = true
	gw := r.system().Gateway
	n := len(r.fr.Commands)
	_, err := r.c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.200", PrefixLen: 16, Gateway: gw})
	if err == nil || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("ApplyAddress = %v", err)
	}
	got := r.fr.Commands[n:]
	if last := got[len(got)-1]; last != "ip -4 -o addr show dev Vlan4094 | grep -q ' 192.0.2.200/16 '" {
		t.Errorf("the batch must stop at the verification, last command %q", last)
	}
	for _, c := range got {
		if c == save {
			t.Error("nothing may be saved when the new address did not survive")
		}
	}
	if r.c.Dial != dialled || r.c.pendingDial != "192.0.2.200" || r.c.savedDial != "" {
		t.Errorf("after the failure: dial %q pending %q saved %q", r.c.Dial, r.c.pendingDial, r.c.savedDial)
	}
}

// deadRunner is a switch address that no longer answers.
type deadRunner struct{ host string }

func (d deadRunner) Run(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("ssh dial %s:22: connect: host is down", d.host)
}
func (deadRunner) Close() error { return nil }

// After a move -- or a move cut short -- the old address is dead: the
// collector tries the other known address once, adopts it, and goes on.
func TestRestartReconnectsThroughTheNewAddress(t *testing.T) {
	fr, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, tc := range []struct{ name, state string }{
		{"adopted dial", `{"dial": "192.0.2.200"}`},
		{"pending dial", `{"pending_dial": "192.0.2.200"}`},
	} {
		if err := os.WriteFile(filepath.Join(dir, managedFile), []byte(tc.state), 0o600); err != nil {
			t.Fatal(err)
		}
		var buf strings.Builder
		c := NewCollector(deadRunner{"192.0.2.1"})
		c.Log = log.New(&buf, "", 0)
		c.Dial, c.configHost, c.StateDir, c.Redial = "192.0.2.1", "192.0.2.1", dir, fr.Dial
		if err := c.loadManaged(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Start(context.Background()); err != nil {
			t.Fatalf("%s: Start through the dead address: %v", tc.name, err)
		}
		if c.Dial != "192.0.2.200" || c.savedDial != "192.0.2.200" || c.pendingDial != "" || !strings.Contains(buf.String(), "answers at 192.0.2.200") {
			t.Errorf("%s: dial %q saved %q pending %q; log %q", tc.name, c.Dial, c.savedDial, c.pendingDial, buf.String())
		}
	}
	// No other address known: the failure stands.
	c := NewCollector(deadRunner{"192.0.2.1"})
	c.Log = log.New(&strings.Builder{}, "", 0)
	c.Dial, c.configHost, c.Redial = "192.0.2.1", "192.0.2.1", fr.Dial
	if _, err := c.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "host is down") {
		t.Errorf("without a candidate: %v", err)
	}
}

func TestApplyAddressDeclinesDHCP(t *testing.T) {
	r := newRig(t, nil)
	if _, err := r.c.ApplyAddress(context.Background(), devicemodel.AddressDesired{DHCP: true}); err == nil || !strings.Contains(err.Error(), "no DHCP client") {
		t.Errorf("DHCP = %v", err)
	}
}
