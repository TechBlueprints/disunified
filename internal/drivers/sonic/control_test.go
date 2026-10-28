package sonic

import (
	"context"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The control side against the in-band capture: the FixtureRunner applies
// every write to the capture's CONFIG_DB and refuses what SONiC refuses, so
// each sequence below is checked for its exact commands, for being accepted
// in that order, for reading back, and for a second apply writing nothing.

type controlRig struct {
	t   *testing.T
	c   *Collector
	fr  *FixtureRunner
	log *strings.Builder
}

func newRig(t *testing.T, edit func(*FixtureRunner)) *controlRig {
	t.Helper()
	fr, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(fr)
	}
	var buf strings.Builder
	c := NewCollector(fr)
	c.Log = log.New(&buf, "", 0)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &controlRig{t: t, c: c, fr: fr, log: &buf}
}

// baseline is the switch's state as the controller would push it: every
// port as it is, native VLAN 1 (Vlan4094 untagged), nothing tagged.
func (r *controlRig) baseline() []devicemodel.PortDesired {
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	var ds []devicemodel.PortDesired
	for _, p := range snap.Ports {
		ds = append(ds, devicemodel.PortDesired{Index: p.Index, Enabled: p.Enabled, Description: p.Description, VLANSet: true, NativeVLAN: 1})
	}
	return ds
}

func edit(ds []devicemodel.PortDesired, idx int, fn func(*devicemodel.PortDesired)) []devicemodel.PortDesired {
	out := append([]devicemodel.PortDesired(nil), ds...)
	for i := range out {
		if out[i].Index == idx {
			fn(&out[i])
		}
	}
	return out
}

// apply runs ApplyPorts and returns the commands it wrote.
func (r *controlRig) apply(ds []devicemodel.PortDesired) ([]string, int) {
	r.t.Helper()
	n := len(r.fr.Commands)
	changed, err := r.c.ApplyPorts(context.Background(), ds)
	if err != nil {
		r.t.Fatalf("ApplyPorts: %v", err)
	}
	return append([]string(nil), r.fr.Commands[n:]...), changed
}

// expect applies ds, checks the exact commands, then collects (reading the
// writes back from the capture) and checks that a second apply writes
// nothing.
func (r *controlRig) expect(name string, ds []devicemodel.PortDesired, want []string) *devicemodel.Snapshot {
	r.t.Helper()
	got, _ := r.apply(ds)
	if !reflect.DeepEqual(got, want) {
		r.t.Errorf("%s: commands\n  got  %q\n  want %q", name, got, want)
	}
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	if again, changed := r.apply(ds); len(again) != 0 || changed != 0 {
		r.t.Errorf("%s: a second apply wrote %q (%d ports)", name, again, changed)
	}
	return snap
}

func port(snap *devicemodel.Snapshot, idx int) devicemodel.Port {
	for _, p := range snap.Ports {
		if p.Index == idx {
			return p
		}
	}
	return devicemodel.Port{}
}

const save = "sudo config save -y"

func TestBaselineWritesNothing(t *testing.T) {
	r := newRig(t, nil)
	if got, changed := r.apply(r.baseline()); len(got) != 0 || changed != 0 {
		t.Errorf("the switch as it is must need no write: %q", got)
	}
}

// (a) state and name.
func TestApplyStateAndName(t *testing.T) {
	r := newRig(t, nil)
	base := r.baseline()
	ds := edit(base, 2, func(d *devicemodel.PortDesired) { d.Enabled, d.Description = false, "lab" })
	snap := r.expect("disable+rename", ds, []string{
		"sudo config interface shutdown Ethernet4",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet4' description 'lab'",
		save,
	})
	if p := port(snap, 2); p.Enabled || p.Description != "lab" || p.Name != "lab" {
		t.Errorf("read back: enabled %v description %q name %q", p.Enabled, p.Description, p.Name)
	}
	r.expect("restore", base, []string{
		"sudo config interface startup Ethernet4",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet4' description ''",
		"sudo sonic-db-cli CONFIG_DB hdel 'PORT|Ethernet4' description",
		save,
	})
	if got := sanitizeDescription("it's \"a\"\nname\\" + strings.Repeat("x", 80)); strings.ContainsAny(got, "'\"\n\\") || len(got) > 64 {
		t.Errorf("sanitizeDescription = %q", got)
	}
}

// (b) VLANs with the native mapping: UniFi's 1 is Vlan4094 untagged. Several
// VLANs go in one command (-m), "all but the native" with -e.
func TestApplyVLANs(t *testing.T) {
	r := newRig(t, nil)
	n := len(r.fr.Commands)
	created, err := r.c.EnsureVLANs(context.Background(), []int{1, 2, 8, 69})
	if err != nil || created != 3 {
		t.Fatalf("EnsureVLANs = %d, %v", created, err)
	}
	if got, want := r.fr.Commands[n:], []string{"sudo config vlan add -m 2,8,69", save}; !reflect.DeepEqual(got, want) {
		t.Errorf("EnsureVLANs wrote %q, want %q", got, want)
	}
	if created, _ := r.c.EnsureVLANs(context.Background(), []int{1, 2, 8, 69}); created != 0 {
		t.Error("a second EnsureVLANs must create nothing")
	}
	base := r.baseline()
	if snap, _ := r.c.Collect(context.Background()); !reflect.DeepEqual(snap.VLANs, []int{1, 2, 8, 69}) {
		t.Errorf("VLANs = %v, want [1 2 8 69]", snap.VLANs)
	}
	tagged := edit(base, 3, func(d *devicemodel.PortDesired) { d.TaggedVLANs = []int{2, 69} })
	snap := r.expect("native 1 + tagged 2,69", tagged, []string{
		"sudo config vlan member add -m 2,69 Ethernet8",
		save,
	})
	if v := port(snap, 3).VLAN; v.Mode != "trunk" || v.NativeVLAN != 1 || !reflect.DeepEqual(v.Allowed, []int{1, 2, 69}) {
		t.Errorf("read back VLAN = %+v", v)
	}
	r.expect("tagged 8,69", edit(base, 3, func(d *devicemodel.PortDesired) { d.TaggedVLANs = []int{8, 69} }), []string{
		"sudo config vlan member del 2 Ethernet8",
		"sudo config vlan member add 8 Ethernet8",
		save,
	})
	r.expect("back to Block All", base, []string{
		"sudo config vlan member del -m 8,69 Ethernet8",
		save,
	})
	// Allow All: every VLAN on the switch but the native, in one command.
	r.expect("Allow All", edit(base, 3, func(d *devicemodel.PortDesired) { d.TaggedAll = true }), []string{
		"sudo config vlan member add -e -m 4094 Ethernet8",
		save,
	})
	// A native change is del, then add -u (SONiC allows one untagged VLAN);
	// dropping every tagged membership is del -e.
	snap = r.expect("native 2", edit(base, 3, func(d *devicemodel.PortDesired) { d.NativeVLAN = 2 }), []string{
		"sudo config vlan member del -e -m 4094 Ethernet8",
		"sudo config vlan member del 4094 Ethernet8",
		"sudo config vlan member add -u 2 Ethernet8",
		save,
	})
	if v := port(snap, 3).VLAN; v.Mode != "access" || v.NativeVLAN != 2 {
		t.Errorf("read back VLAN = %+v", v)
	}
	// Allow All on native 2: -e excludes the native and the port's own untagged VLAN.
	r.expect("native 2, Allow All", edit(base, 3, func(d *devicemodel.PortDesired) { d.NativeVLAN, d.TaggedAll = 2, true }), []string{
		"sudo config vlan member add -e -m 2,4094 Ethernet8",
		save,
	})
}

// A VLAN on the switch that the controller does not list is not tagged by
// Allow All: -e would take it, so the explicit list goes instead.
func TestAllowAllWithAForeignVLAN(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	if _, err := r.fr.Run(ctx, "sudo config vlan add 77", ""); err != nil { // made on the switch by hand
		t.Fatal(err)
	}
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.EnsureVLANs(ctx, []int{1, 2, 8}); err != nil {
		t.Fatal(err)
	}
	base := r.baseline()
	r.expect("Allow All beside Vlan77", edit(base, 3, func(d *devicemodel.PortDesired) { d.TaggedAll = true }), []string{
		"sudo config vlan member add -m 2,8 Ethernet8",
		save,
	})
}

// A 31-port Allow-All first push with VLANs 2,8,9,10,69 is one command per
// port, well inside the loop's 20 s apply budget at ~250 ms a command.
func TestAllowAllFirstPushIsOneCommandPerPort(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	n := len(r.fr.Commands)
	if _, err := r.c.EnsureVLANs(ctx, []int{1, 2, 8, 9, 10, 69}); err != nil {
		t.Fatal(err)
	}
	ensure := append([]string(nil), r.fr.Commands[n:]...)
	if want := []string{"sudo config vlan add -m 2,8,9,10,69", save}; !reflect.DeepEqual(ensure, want) {
		t.Errorf("EnsureVLANs wrote %q", ensure)
	}
	ds := r.baseline()
	for i := range ds {
		if ds[i].Index >= 2 {
			ds[i].TaggedAll = true
		}
	}
	got, changed := r.apply(ds)
	if changed != 31 || len(got) != 32 || got[0] != "sudo config vlan member add -e -m 4094 Ethernet4" || got[31] != save {
		t.Errorf("Allow-All push: %d ports, %d commands: %q", changed, len(got), got)
	}
	t.Logf("ApplyPorts: %d commands, first three %q", len(got), got[:3])
}

// The native-id collision (§5.2a): a controller network on native_vlan is
// refused whole by EnsureVLANs, and a port naming it is not written.
func TestNativeVLANCollisionRefused(t *testing.T) {
	r := newRig(t, nil)
	n := len(r.fr.Commands)
	if _, err := r.c.EnsureVLANs(context.Background(), []int{1, 2, 4094}); err == nil || !strings.Contains(err.Error(), "native_vlan") {
		t.Errorf("EnsureVLANs with 4094 = %v; want a native_vlan error", err)
	}
	if len(r.fr.Commands) != n {
		t.Errorf("the refused EnsureVLANs wrote %q", r.fr.Commands[n:])
	}
	base := r.baseline()
	ds := edit(base, 3, func(d *devicemodel.PortDesired) { d.TaggedVLANs = []int{4094} })
	ds = edit(ds, 3, func(d *devicemodel.PortDesired) { d.Enabled = false })
	if got, _ := r.apply(ds); len(got) != 0 {
		t.Errorf("a port tagging native_vlan must not be written at all: %q", got)
	}
	if !strings.Contains(r.log.String(), "VLAN 4094 is this switch's native_vlan") {
		t.Errorf("log = %q", r.log.String())
	}
	if got := r.c.PlanPorts(ds); len(got) != 0 {
		t.Errorf("PlanPorts = %v", got)
	}
}

// (c) a LAG: the members leave their VLANs before joining (SONiC refuses
// otherwise), the PortChannel carries the VLAN intent, and leaving restores
// the ports' own and deletes the emptied PortChannel.
func TestApplyLAG(t *testing.T) {
	r := newRig(t, nil)
	base := r.baseline()
	ds := edit(base, 3, func(d *devicemodel.PortDesired) { d.LAG = 3 })
	ds = edit(ds, 4, func(d *devicemodel.PortDesired) { d.LAG = 3 })
	if got := r.c.PlanPorts(ds); !reflect.DeepEqual(got, []int{3, 4}) {
		t.Errorf("PlanPorts = %v, want [3 4]", got)
	}
	n := len(r.fr.Commands)
	if _, changed := r.apply(ds); changed != 2 {
		t.Errorf("ApplyPorts changed %d ports, want 2 (as planned)", changed)
	}
	want := []string{
		"sudo config portchannel add PortChannel3",
		"sudo config vlan member del 4094 Ethernet8",
		"sudo config portchannel member add PortChannel3 Ethernet8",
		"sudo config vlan member del 4094 Ethernet12",
		"sudo config portchannel member add PortChannel3 Ethernet12",
		"sudo config vlan member add -u 4094 PortChannel3",
		save,
	}
	if got := r.fr.Commands[n:]; !reflect.DeepEqual(got, want) {
		t.Errorf("join: got %q\nwant %q", got, want)
	}
	snap, _ := r.c.Collect(context.Background())
	for _, idx := range []int{3, 4} {
		if p := port(snap, idx); p.LAG != "PortChannel3" || p.LAGID != 3 || p.VLAN.NativeVLAN != 1 {
			t.Errorf("port %d read back: lag %q id %d VLAN %+v (members report the PortChannel's VLANs)", idx, p.LAG, p.LAGID, p.VLAN)
		}
	}
	if again, _ := r.apply(ds); len(again) != 0 {
		t.Errorf("second apply wrote %q", again)
	}
	r.expect("leave", base, []string{
		"sudo config portchannel member del PortChannel3 Ethernet8",
		"sudo config portchannel member del PortChannel3 Ethernet12",
		"sudo config vlan member add -u 4094 Ethernet8",
		"sudo config vlan member add -u 4094 Ethernet12",
		"sudo config vlan member del 4094 PortChannel3",
		"sudo config portchannel del PortChannel3",
		save,
	})
}

// A PortChannel's MTU follows its members (seen live: SONiC refuses a member
// whose MTU differs, a new PortChannel is 9100, and `config interface mtu
// PortChannel<n>` is "Invalid port"): set in CONFIG_DB right after the
// PortChannel is made, and moved first when jumbo flips.
func TestLAGMTUFollowsTheMembers(t *testing.T) {
	r := newRig(t, nil)
	off, on := 1500, 9216 // jumbo off; jumbo on with the controller's switch.mtu=9216
	lag := func(mtu int) []devicemodel.PortDesired {
		var ds []devicemodel.PortDesired
		for _, d := range r.baseline() {
			if d.Index == 3 || d.Index == 4 {
				d.LAG, d.MTU = 3, mtu
				ds = append(ds, d)
			}
		}
		return ds
	}
	snap := r.expect("LAG with jumbo off", lag(off), []string{
		"sudo config interface mtu Ethernet8 1500",
		"sudo config interface mtu Ethernet12 1500",
		"sudo config portchannel add PortChannel3",
		"sudo sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel3' mtu 1500",
		"sudo config vlan member del 4094 Ethernet8",
		"sudo config portchannel member add PortChannel3 Ethernet8",
		"sudo config vlan member del 4094 Ethernet12",
		"sudo config portchannel member add PortChannel3 Ethernet12",
		"sudo config vlan member add -u 4094 PortChannel3",
		save,
	})
	for _, idx := range []int{3, 4} {
		if p := port(snap, idx); p.MTU != 1500 || p.LAGID != 3 {
			t.Errorf("port %d read back: MTU %d LAG %d", idx, p.MTU, p.LAGID)
		}
	}
	snap = r.expect("jumbo on with the LAG", lag(on), []string{
		"sudo sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel3' mtu 9216",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet8' mtu 9216",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet12' mtu 9216",
		save,
	})
	for _, idx := range []int{3, 4} {
		if p := port(snap, idx); p.MTU != 9216 {
			t.Errorf("port %d MTU after jumbo on = %d", idx, p.MTU)
		}
	}
	r.expect("jumbo off again", lag(off), []string{
		"sudo sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel3' mtu 1500",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet8' mtu 1500",
		"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet12' mtu 1500",
		save,
	})

	// Members already at the PortChannel's default 9100: no MTU write at all.
	r9100 := newRig(t, nil)
	var ds []devicemodel.PortDesired
	for _, d := range r9100.baseline() {
		if d.Index == 3 || d.Index == 4 {
			d.LAG = 3 // no jumbo key in the push: MTUs stay at SONiC's 9100
			ds = append(ds, d)
		}
	}
	got, _ := r9100.apply(ds)
	for _, c := range got {
		if strings.Contains(c, "mtu") {
			t.Errorf("9100 members wrote %q", c)
		}
	}
	// The runner refuses a member whose MTU differs, as SONiC did live.
	fr := r9100.fr
	ctx := context.Background()
	for _, c := range []string{"sudo config portchannel add PortChannel9", "sudo config interface mtu Ethernet28 1500", "sudo config vlan member del 4094 Ethernet28"} {
		if _, err := fr.Run(ctx, c, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fr.Run(ctx, "sudo config portchannel member add PortChannel9 Ethernet28", ""); err == nil || !strings.Contains(err.Error(), "is different than the PortChannel9 MTU size") {
		t.Errorf("an MTU mismatch must be refused: %v", err)
	}
	// SONiC refuses `config interface mtu` on a LAG member whatever the
	// value (probed on the guest); the member's own MTU moves by hset.
	if _, err := r.fr.Run(ctx, "sudo config interface mtu Ethernet8 1500", ""); err == nil || !strings.Contains(err.Error(), "'interface_name' is in portchannel!") {
		t.Errorf("config interface mtu on a LAG member must be refused: %v", err)
	}
	if _, err := r.fr.Run(ctx, "sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet8' mtu 1500", ""); err != nil {
		t.Errorf("the member's MTU by hset must be accepted: %v", err)
	}
	if _, err := fr.Run(ctx, "sudo config interface mtu PortChannel9 1500", ""); err == nil || !strings.Contains(err.Error(), "Invalid port") {
		t.Errorf("config interface mtu on a PortChannel must be refused: %v", err)
	}
}

// (d) a mirror: the destination is stripped of its VLANs before the session
// (SONiC refuses a destination with VLAN config) and gets them back after.
func TestApplyMirror(t *testing.T) {
	r := newRig(t, nil)
	base := r.baseline()
	ds := edit(base, 4, func(d *devicemodel.PortDesired) { d.MirrorSource = 3 })
	snap := r.expect("mirror 4 <- 3", ds, []string{
		"sudo config vlan member del 4094 Ethernet12",
		"sudo config mirror_session span add dui4 Ethernet12 Ethernet8 both",
		save,
	})
	if p := port(snap, 4); p.MirrorFrom != 3 || p.VLAN.NativeVLAN != 0 {
		t.Errorf("read back: mirror from %d, VLAN %+v", p.MirrorFrom, p.VLAN)
	}
	r.expect("mirror off", base, []string{
		"sudo config mirror_session remove dui4",
		"sudo config vlan member add -u 4094 Ethernet12",
		save,
	})
}

// (e) storm control: percent of the configured speed to kbps and back.
func TestApplyStormControl(t *testing.T) {
	r := newRig(t, nil)
	base := r.baseline()
	ten, five := 10.0, 5.0
	ds := edit(base, 2, func(d *devicemodel.PortDesired) {
		d.StormCtrl = &devicemodel.StormControlSpec{BroadcastPct: &ten, MulticastPct: &ten, UnknownUnicastPct: &ten}
	})
	snap := r.expect("10/10/10 at 100G", ds, []string{
		"sudo config interface storm-control add Ethernet4 broadcast 10000000",
		"sudo config interface storm-control add Ethernet4 unknown-multicast 10000000",
		"sudo config interface storm-control add Ethernet4 unknown-unicast 10000000",
		save,
	})
	sc := port(snap, 2).StormCtrl
	if sc == nil || *sc.BroadcastPct != 10 || *sc.MulticastPct != 10 || *sc.UnknownUnicastPct != 10 {
		t.Errorf("read back storm control = %+v", sc)
	}
	ds = edit(ds, 2, func(d *devicemodel.PortDesired) {
		d.StormCtrl = &devicemodel.StormControlSpec{BroadcastPct: &five, MulticastPct: &ten, UnknownUnicastPct: &ten}
	})
	r.expect("broadcast to 5%", ds, []string{
		"sudo config interface storm-control del Ethernet4 broadcast",
		"sudo config interface storm-control add Ethernet4 broadcast 5000000",
		save,
	})
	r.expect("off", base, []string{
		"sudo config interface storm-control del Ethernet4 broadcast",
		"sudo config interface storm-control del Ethernet4 unknown-multicast",
		"sudo config interface storm-control del Ethernet4 unknown-unicast",
		save,
	})
}

func TestStormArithmetic(t *testing.T) {
	for _, c := range []struct {
		pct   float64
		speed int
		kbps  int
	}{{10, 100000, 10000000}, {0.5, 25000, 125000}, {1, 10000, 100000}, {33.33, 100000, 33330000}} {
		if got := pctToKbps(c.pct, c.speed); got != c.kbps {
			t.Errorf("pctToKbps(%v, %d) = %d, want %d", c.pct, c.speed, got, c.kbps)
		}
		if got := kbpsToPct(c.kbps, c.speed); got != c.pct {
			t.Errorf("kbpsToPct(%d, %d) = %v, want %v", c.kbps, c.speed, got, c.pct)
		}
	}
	if got := kbpsToPct(3333333, 100000); got != 3.33 {
		t.Errorf("kbpsToPct rounds to two decimals: %v", got)
	}
}

func TestApplyFECAndJumbo(t *testing.T) {
	r := newRig(t, nil)
	base := r.baseline()
	rs, none := devicemodel.FECRS, devicemodel.FECDisabled
	snap := r.expect("fec rs", edit(base, 2, func(d *devicemodel.PortDesired) { d.FEC = &rs }), []string{
		"sudo config interface fec Ethernet4 rs", save,
	})
	if p := port(snap, 2); p.FECConfig != devicemodel.FECRS {
		t.Errorf("read back FEC config = %q", p.FECConfig)
	}
	r.expect("fec none", edit(base, 2, func(d *devicemodel.PortDesired) { d.FEC = &none }), []string{
		"sudo config interface fec Ethernet4 none", save,
	})
	off, on := 1500, 9216
	jumbo := func(v int) []devicemodel.PortDesired {
		ds := append([]devicemodel.PortDesired(nil), base[:3]...)
		for i := range ds {
			ds[i].MTU = v
		}
		return ds
	}
	snap = r.expect("jumbo off", jumbo(off), []string{
		"sudo config interface mtu Ethernet0 1500",
		"sudo config interface mtu Ethernet4 1500",
		"sudo config interface mtu Ethernet8 1500",
		save,
	})
	if p := port(snap, 1); p.MTU != 1500 {
		t.Errorf("read back MTU = %d", p.MTU)
	}
	r.expect("jumbo on", jumbo(on), []string{
		"sudo config interface mtu Ethernet0 9216",
		"sudo config interface mtu Ethernet4 9216",
		"sudo config interface mtu Ethernet8 9216",
		save,
	})
	if !r.c.SwitchesJumbo() {
		t.Error("SONiC switches jumbo by MTU")
	}
}

// A lane speed on an unsplit QSFP28 cage is a breakout: refused on the VS
// (no platform.json); a speed the port does not list is refused too; the
// cage's own speed writes nothing; "auto" never turns autoneg on there.
func TestSpeedRules(t *testing.T) {
	r := newRig(t, nil)
	base := r.baseline()
	for _, s := range []int{25000, 10000} {
		if got, _ := r.apply(edit(base, 2, func(d *devicemodel.PortDesired) { d.SpeedMbps = s })); len(got) != 0 {
			t.Errorf("speed %d wrote %q", s, got)
		}
	}
	if !strings.Contains(r.log.String(), "no platform.json") {
		t.Errorf("the breakout refusal must be logged: %q", r.log.String())
	}
	if got, _ := r.apply(edit(base, 2, func(d *devicemodel.PortDesired) { d.SpeedMbps = 40000 })); len(got) != 0 {
		t.Errorf("40G, not among the VS port's speeds, wrote %q", got)
	}
	if got, _ := r.apply(edit(base, 2, func(d *devicemodel.PortDesired) { d.SpeedMbps = 100000 })); len(got) != 0 {
		t.Errorf("the configured speed wrote %q", got)
	}
}

// STP port settings only with the daemon: refused with a log on the VS
// capture; with FEATURE|stp added to it, written and claimed.
func TestSTPPortSettingsAreFeatureGated(t *testing.T) {
	r := newRig(t, nil)
	base := r.baseline()
	ds := edit(base, 2, func(d *devicemodel.PortDesired) { d.BPDUGuard = true })
	if got, _ := r.apply(ds); len(got) != 0 {
		t.Errorf("BPDU guard without the STP daemon wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "no STP daemon") {
		t.Errorf("the STP refusal must be logged: %q", r.log.String())
	}
	if r.c.Capabilities().STP || r.c.Capabilities().BPDUGuard {
		t.Error("STP must not be claimed without FEATURE|stp")
	}

	withSTP := newRig(t, func(fr *FixtureRunner) {
		const head = "@@@ cfg_feature\n{\n"
		if !strings.Contains(fr.Fixture, head) {
			t.Fatal("capture changed: no cfg_feature section")
		}
		fr.Fixture = strings.Replace(fr.Fixture, head, head+`"FEATURE|stp": {"state": "enabled"},`+"\n", 1)
	})
	if caps := withSTP.c.Capabilities(); !caps.STP || !caps.BPDUGuard || !caps.STPPortCost {
		t.Errorf("capabilities with FEATURE|stp = %+v", caps)
	}
	if len(withSTP.fr.Helps) != len(commandChecks) {
		t.Errorf("with FEATURE|stp Start must check spanning-tree too: %v", withSTP.fr.Helps)
	}
	base = withSTP.baseline()
	snap := withSTP.expect("bpdu guard + STP off", edit(base, 2, func(d *devicemodel.PortDesired) { d.BPDUGuard, d.STPEdge = true, true }), []string{
		"sudo config spanning-tree interface bpdu_guard enable Ethernet4",
		"sudo config spanning-tree interface portfast enable Ethernet4",
		"sudo config spanning-tree interface disable Ethernet4",
		save,
	})
	if p := port(snap, 2); !p.BPDUGuard || !p.STPEdge {
		t.Errorf("read back: bpdu guard %v edge %v", p.BPDUGuard, p.STPEdge)
	}
}

// The FixtureRunner refuses what SONiC refuses, so an ordering mistake
// fails: a LAG member with VLAN config, a second untagged VLAN, a mirror
// destination with VLAN config.
func TestFixtureRunnerRefusesLikeSONiC(t *testing.T) {
	fr, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, bad := range []string{
		"sudo config vlan member add -u 2 Ethernet8",                         // Vlan2 does not exist
		"sudo config portchannel member add PortChannel3 Ethernet8",          // no PortChannel3
		"sudo config vlan add 4094",                                          // exists
		"sudo config mirror_session span add dui4 Ethernet12 Ethernet8 both", // destination has VLAN config
	} {
		if _, err := fr.Run(ctx, bad, ""); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := fr.Run(ctx, batchCommand, "set -e\nsudo config vlan add 2\nsudo config vlan member add -u 2 Ethernet8\nsudo config vlan add 3\n"); err == nil {
		t.Error("a second untagged VLAN on Ethernet8 must be refused")
	}
	if got := fr.Commands[len(fr.Commands)-1]; got != "sudo config vlan member add -u 2 Ethernet8" {
		t.Errorf("set -e: the batch must stop at the refusal, last command %q", got)
	}
	if _, err := fr.Run(ctx, "sudo config portchannel add PortChannel3", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fr.Run(ctx, "sudo config portchannel member add PortChannel3 Ethernet8", ""); err == nil || !strings.Contains(err.Error(), "VLAN_MEMBER") {
		t.Errorf("a member with VLAN config must be refused: %v", err)
	}
}

func TestStartChecksTheConfigCommands(t *testing.T) {
	fr, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fr.Help["config vlan member --help"] = strings.Replace(fr.Help["config vlan member --help"], "  del ", "  rm ", 1)
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	if _, err := c.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "config vlan member` lacks del") {
		t.Errorf("Start with `config vlan member del` missing = %v", err)
	}
	if got := helpSubcommands(fr.Help["config interface --help"]); !got["storm-control"] || !got["startup"] || got["Usage:"] {
		t.Errorf("helpSubcommands = %v", got)
	}
}

// Start also checks `config clock timezone` and `config hostname` (§7.1).
func TestStartChecksClockAndHostname(t *testing.T) {
	for _, c := range []struct{ section, from, to, want string }{
		{"config clock --help", "  timezone ", "  tz ", "`config clock` lacks timezone"},
		{"config --help", "  hostname ", "  hostnam ", "`config` lacks hostname"},
	} {
		fr, err := NewFixtureRunner(fixture)
		if err != nil {
			t.Fatal(err)
		}
		fr.Help[c.section] = strings.Replace(fr.Help[c.section], c.from, c.to, 1)
		col := NewCollector(fr)
		col.Log = log.New(testWriter{t}, "", 0)
		if _, err := col.Start(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Start = %v, want %q", c.section, err, c.want)
		}
	}
}

// A tagged membership of Vlan<native_vlan> reads as tagged VLAN 1 (the
// stand-in id never reaches the controller), and a push that tags VLAN 1
// on a port whose native VLAN is another is refused with a log: SONiC can
// carry UniFi's VLAN 1 only untagged.
func TestNativeVLANTaggedStandIn(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	for _, c := range []string{"sudo config vlan add 10", "sudo config vlan member del 4094 Ethernet8", "sudo config vlan member add -u 10 Ethernet8", "sudo config vlan member add 4094 Ethernet8"} {
		if _, err := r.fr.Run(ctx, c, ""); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := r.c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v := port(snap, 3).VLAN; v.Mode != "trunk" || v.NativeVLAN != 10 || !reflect.DeepEqual(v.Allowed, []int{1, 10}) {
		t.Errorf("port 3 VLAN = %+v, want trunk native 10 allowed [1 10]", v)
	}
	ds := edit(r.baseline(), 3, func(d *devicemodel.PortDesired) { d.NativeVLAN, d.TaggedVLANs = 10, []int{1} })
	ds = edit(ds, 4, func(d *devicemodel.PortDesired) { d.NativeVLAN, d.TaggedVLANs = 10, []int{1} })
	got, _ := r.apply(ds)
	for _, c := range got {
		if strings.Contains(c, "Ethernet12") || strings.Contains(c, "Ethernet8") {
			t.Errorf("a port tagging VLAN 1 was written: %q", c)
		}
	}
	if !strings.Contains(r.log.String(), "VLAN 1 tagged (native VLAN 10) cannot be expressed on SONiC") {
		t.Errorf("the refusal must be logged: %s", r.log.String())
	}
}
