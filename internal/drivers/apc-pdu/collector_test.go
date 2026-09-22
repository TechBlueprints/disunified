package apcpdu

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

const fixtureDir = "../../../docs/fixtures/apc-aos-3.9.2"

func newTestCollector(t *testing.T) (*Collector, *FixtureRunner) {
	t.Helper()
	r, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return NewCollector(r), r
}

func TestCollectReadsIdentityFromTheCard(t *testing.T) {
	c, _ := newTestCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sys := snap.System
	if sys.Vendor != "APC" {
		t.Errorf("vendor = %q, want APC", sys.Vendor)
	}
	if sys.Model != "AP7931" {
		t.Errorf("model = %q, want AP7931", sys.Model)
	}
	// The serial is the scrubbed one; what matters is that it is read at all.
	if sys.Serial == "" {
		t.Error("serial is empty")
	}
	// Reported without the vendor's leading "v": the UI shows a version column.
	if sys.Version != "3.9.2" {
		t.Errorf("version = %q, want 3.9.2", sys.Version)
	}
	if sys.MAC != "02:00:00:00:00:01" {
		t.Errorf("mac = %q, want the card's scrubbed interface MAC", sys.MAC)
	}
	if sys.Uptime <= 0 {
		t.Error("uptime not read from sysUpTime")
	}
}

func TestCollectReadsEveryOutlet(t *testing.T) {
	c, _ := newTestCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := len(snap.Outlets); got != 16 {
		t.Fatalf("outlets = %d, want the card's 16", got)
	}
	for i, o := range snap.Outlets {
		if o.Index != i+1 {
			t.Fatalf("outlet %d has index %d: the table must be sorted and 1-based", i, o.Index)
		}
		if !o.Switchable {
			t.Errorf("outlet %d not switchable; every outlet on this PDU is", o.Index)
		}
		if !o.On {
			t.Errorf("outlet %d reported off; the capture has them all on", o.Index)
		}
		if o.HasMetering {
			t.Errorf("outlet %d claims metering: this PDU does not meter per outlet", o.Index)
		}
	}
	if snap.Outlets[0].Name != "Outlet 1" {
		t.Errorf("outlet 1 name = %q, want the card's own label", snap.Outlets[0].Name)
	}
}

// The PDU's single network interface is the uplink, and it must be reported as
// one, because this card runs no LLDP for the loop to infer it from.
func TestSnapshotPresentsTheNetworkPortAsUplink(t *testing.T) {
	c, _ := newTestCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(snap.Ports) != 1 {
		t.Fatalf("ports = %d, want 1 (the card's network interface)", len(snap.Ports))
	}
	if snap.UplinkPort() != 1 {
		t.Errorf("UplinkPort() = %d, want 1", snap.UplinkPort())
	}
}

// A rack PDU is not a switch: claiming a switch feature would put a control in
// the UI that this driver cannot honour.
func TestCapabilitiesClaimNothingSwitchLike(t *testing.T) {
	c, _ := newTestCollector(t)
	if got := c.Capabilities(); got != (devicemodel.Capabilities{}) {
		t.Errorf("capabilities = %+v, want none claimed", got)
	}
}

func TestApplyOutletsSwitchesOnlyWhatChanged(t *testing.T) {
	c, r := newTestCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Every outlet is on in the capture. Ask for outlet 8 off and the rest on.
	var desired []devicemodel.OutletDesired
	for i := 1; i <= 16; i++ {
		desired = append(desired, devicemodel.OutletDesired{Index: i, On: i != 8})
	}
	changed, err := c.ApplyOutlets(context.Background(), desired)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1", changed)
	}
	want := "1.3.6.1.4.1.318.1.1.12.3.3.1.1.4.8=2" // outlet 8, immediateOff
	if len(r.Sets) != 1 || r.Sets[0] != want {
		t.Errorf("sets = %v, want exactly [%s]", r.Sets, want)
	}
}

// The loop re-applies the same intent on every cycle with no push. An outlet
// is real load, so the second pass must write nothing at all.
func TestApplyOutletsIsIdempotent(t *testing.T) {
	c, r := newTestCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	desired := []devicemodel.OutletDesired{{Index: 3, On: false}}
	if _, err := c.ApplyOutlets(context.Background(), desired); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(r.Sets) != 1 {
		t.Fatalf("first apply wrote %v, want one set", r.Sets)
	}
	// Re-collect so the driver sees the state it just wrote, then re-apply.
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	changed, err := c.ApplyOutlets(context.Background(), desired)
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if changed != 0 || len(r.Sets) != 1 {
		t.Errorf("re-apply changed=%d sets=%v, want no second write", changed, r.Sets)
	}
}

// Names cannot be written over SNMP on this card, so they go out as a partial
// config.ini, batched into one upload: the card applies a config
// asynchronously and drops a second upload that arrives while the first is
// still being applied.
func TestApplyOutletsBatchesNamesIntoOneConfigUpload(t *testing.T) {
	c, r := newTestCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	desired := []devicemodel.OutletDesired{
		{Index: 2, On: true, Name: "NAS"},
		{Index: 5, On: true, Name: "Rack Fan"},
		{Index: 9, On: true, Name: "Outlet 9"}, // unchanged: already the card's name
	}
	changed, err := c.ApplyOutlets(context.Background(), desired)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if changed != 2 {
		t.Errorf("changed = %d, want 2 renames", changed)
	}
	if len(r.Configs) != 1 {
		t.Fatalf("configs = %d, want exactly one upload", len(r.Configs))
	}
	got := r.Configs[0]
	for _, want := range []string{"[RackPDUOutlet]", "Name2=NAS", "Name5=Rack Fan"} {
		if !strings.Contains(got, want) {
			t.Errorf("config upload missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Name9") {
		t.Errorf("config upload rewrote an unchanged name:\n%s", got)
	}
	if !strings.Contains(got, "\r\n") {
		t.Error("config upload must use CRLF line endings")
	}
	if len(r.Sets) != 0 {
		t.Errorf("renaming switched an outlet: %v", r.Sets)
	}
}

// The card truncates at 23 characters. Sending a longer name would leave the
// device permanently disagreeing with the desired name, and the driver would
// rewrite it on every cycle forever.
func TestOutletNameIsTruncatedToTheCardsLimit(t *testing.T) {
	long := "a-name-far-longer-than-the-card-accepts"
	got := string(outletNameConfig(map[int]string{4: long}))
	if !strings.Contains(got, "Name4="+long[:23]+"\r\n") {
		t.Errorf("name not truncated to 23 chars:\n%s", got)
	}
}

// An index the card does not have must be ignored, not guessed at: writing a
// command to an unknown outlet index is how you find out it is unknown.
func TestApplyOutletsIgnoresAnOutletTheDeviceDoesNotHave(t *testing.T) {
	c, r := newTestCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	changed, err := c.ApplyOutlets(context.Background(), []devicemodel.OutletDesired{{Index: 24, On: false}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if changed != 0 || len(r.Sets) != 0 {
		t.Errorf("wrote to a nonexistent outlet: changed=%d sets=%v", changed, r.Sets)
	}
}

// A switched rack PDU meters the phase, not the outlet. The card measures
// current only; watts come from that current and the operator-configured line
// voltage and power factor, which is the same arithmetic the card's own
// display does.
func TestAggregatePowerIsReadAndComputed(t *testing.T) {
	c, _ := newTestCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sys := snap.System
	if !sys.HasPowerDraw {
		t.Fatal("no aggregate power read; the load subtree must be walked")
	}
	// The capture has the rack idle: 0 tenths of an amp, rated 12 A at 120 V.
	if sys.PowerCurrentA != 0 {
		t.Errorf("current = %v A, want 0 (the capture is idle)", sys.PowerCurrentA)
	}
	if sys.PowerDrawW != 0 {
		t.Errorf("draw = %v W, want 0", sys.PowerDrawW)
	}
	if sys.PowerBudgetW != 12*120 {
		t.Errorf("budget = %v W, want 1440 (12 A x 120 V)", sys.PowerBudgetW)
	}
}

// A device that does not measure must not report 0 W, which would read as a
// real measurement of an idle rack.
func TestNoLoadReadingMeansNoPowerClaim(t *testing.T) {
	r, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	delete(r.Values, "1.3.6.1.4.1.318.1.1.12.2.3.1.1.2.1")
	snap, err := NewCollector(r).Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.System.HasPowerDraw {
		t.Error("claimed a power measurement with no load reading")
	}
}

// The controller's per-outlet Power Cycle (relayctl) is the card's own
// immediate-reboot command: an off/on with the configured reboot duration
// between, done by the card, so the bridge issues exactly one SET.
func TestCycleOutletIssuesTheRebootCommand(t *testing.T) {
	c, r := newTestCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.CycleOutlet(context.Background(), 9); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	want := "1.3.6.1.4.1.318.1.1.12.3.3.1.1.4.9=3" // control table, outlet 9, immediateReboot
	if len(r.Sets) != 1 || r.Sets[0] != want {
		t.Errorf("sets = %v, want exactly [%s]", r.Sets, want)
	}
	if len(r.Configs) != 0 {
		t.Errorf("a power cycle uploaded a config: %v", r.Configs)
	}
}

// The reachability fields a real device reports come from the card's IP-MIB
// tables, which index their rows by address. The gateway's MAC is the ARP
// entry for the default route's next hop.
func TestReachabilityFromTheIPMIB(t *testing.T) {
	c, _ := newTestCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sys := snap.System
	if len(sys.Addresses) != 1 {
		t.Fatalf("addresses = %+v, want the card's one (loopback excluded)", sys.Addresses)
	}
	if sys.Addresses[0].PrefixLen != 16 {
		t.Errorf("prefix = %d, want 16 (255.255.0.0)", sys.Addresses[0].PrefixLen)
	}
	if len(sys.ARP) == 0 {
		t.Fatal("ARP cache empty")
	}
	if sys.GatewayMAC == "" {
		t.Error("gateway MAC not resolved from the default route's ARP entry")
	}
	for _, mac := range sys.ARP {
		if len(mac) != 17 {
			t.Errorf("ARP MAC %q not normalised to aa:bb:cc:dd:ee:ff", mac)
		}
	}
}
