package apcups

import (
	"bufio"
	"errors"
	"net"

	"context"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const fixtureDir = "../../../docs/fixtures/apc-smtl-15.5"

// rawRegs reads the fixture independently of the runner, so the collector's
// decoding is checked against the captured words rather than against
// numbers typed into this file.
func rawRegs(t *testing.T) map[int]uint16 {
	t.Helper()
	f, err := os.Open(filepath.Join(fixtureDir, "registers.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[int]uint16{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		a, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok || strings.HasPrefix(a, "#") {
			continue
		}
		addr, _ := strconv.Atoi(a)
		val, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 16)
		out[addr] = uint16(val)
	}
	return out
}

func near(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func startFixture(t *testing.T) (*Collector, *FixtureRunner) {
	t.Helper()
	fr, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	c.Addr = "192.0.2.30:502"
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, fr
}

func TestCollectorReadsTheReferenceDriversFourBlocks(t *testing.T) {
	_, fr := startFixture(t)
	want := []string{"0,27", "128,44", "516,120", "1026,22"}
	if strings.Join(fr.Reads, " ") != strings.Join(want, " ") {
		t.Errorf("reads = %v, want exactly %v (the map is sparse; other blocks are refused)", fr.Reads, want)
	}
}

func TestCollectorDecodesIdentityAndRating(t *testing.T) {
	c, _ := startFixture(t)
	snap, _ := c.Collect(context.Background())
	sys := snap.System
	if sys.Vendor != "APC" || sys.Model != "Smart-UPS 1500" || sys.Version != "15.5" {
		t.Errorf("identity = %q %q %q", sys.Vendor, sys.Model, sys.Version)
	}
	if sys.Serial != "SSJ00000000" {
		t.Errorf("serial = %q, want the scrubbed documentation serial", sys.Serial)
	}
	b := sys.Battery
	if b == nil {
		t.Fatal("no Battery on a UPS snapshot")
	}
	if b.RealPowerRatingW != 1350 || b.ApparentRatingVA != 1440 {
		t.Errorf("rating = %.0f W / %.0f VA, want the SMTL1500's 1350 / 1440", b.RealPowerRatingW, b.ApparentRatingVA)
	}
	if len(sys.Addresses) != 1 || sys.Addresses[0].IP != "192.0.2.30" {
		t.Errorf("addresses = %v, want the dialled address", sys.Addresses)
	}
}

// Every measurement is held to the raw words with the reference driver's
// fixed-point scales.
func TestCollectorDecodesMeasurementsFromTheCapturedWords(t *testing.T) {
	c, _ := startFixture(t)
	snap, _ := c.Collect(context.Background())
	b, r := snap.System.Battery, rawRegs(t)
	checks := []struct {
		name      string
		got, want float64
	}{
		{"load %", b.LoadPct, float64(r[136]) / 256},
		{"real W", b.RealPowerW, float64(r[136]) / 256 / 100 * float64(r[589])},
		{"apparent VA", b.ApparentPowerVA, float64(r[138]) / 256 / 100 * float64(r[588])},
		{"charge %", float64(b.ChargePct), math.Round(float64(r[130]) / 512)},
		{"battery V", b.VoltageV, float64(int16(r[131])) / 32},
		{"battery C", b.TemperatureC, float64(int16(r[135])) / 128},
		{"output V", b.OutputVoltageV, float64(r[142]) / 64},
		{"output A", b.OutputCurrentA, float64(r[140]) / 32},
		{"output Hz", b.OutputFrequencyHz, float64(r[144]) / 128},
		{"input V", b.InputVoltageV, float64(r[151]) / 64},
		{"runtime s", b.Runtime.Seconds(), float64(uint32(r[128])<<16 | uint32(r[129]))},
		{"transfer high V", float64(b.TransferHighV), float64(r[1026])},
		{"transfer low V", float64(b.TransferLowV), float64(r[1027])},
	}
	for _, ck := range checks {
		if !near(ck.got, ck.want, 1e-9) {
			t.Errorf("%s = %v, want %v from the captured word", ck.name, ck.got, ck.want)
		}
	}
	if !b.HasOutput || !b.HasInput || !b.HasTemperature {
		t.Error("measured values not flagged as measured")
	}
	// Plausibility of the capture itself: a rack on a 1350 W unit.
	if b.LoadPct < 1 || b.LoadPct > 120 || b.OutputVoltageV < 100 || b.OutputVoltageV > 130 {
		t.Errorf("implausible capture: load %.1f%%, %.1f V", b.LoadPct, b.OutputVoltageV)
	}
}

// The unit was captured on mains, in ECO mode, not on battery: the status
// word's register order is what decides this, so it is pinned here.
func TestCollectorDecodesTheStatusWordInRegisterOrder(t *testing.T) {
	c, _ := startFixture(t)
	snap, _ := c.Collect(context.Background())
	b := snap.System.Battery
	if b.OnBattery || b.OutputOff || b.Bypass || b.LowBattery || b.Overload || b.Fault {
		t.Errorf("captured unit was online and healthy, got %+v", *b)
	}
	if !b.ECOMode {
		t.Error("ECO/high-efficiency mode not decoded (NUT reported vendor:apc:HE for this capture)")
	}
	if b.ChargePct == 100 && b.Charging {
		t.Error("a full pack on mains must not be reported as charging")
	}
	if b.TransferCause != "AcceptableInput" {
		t.Errorf("transfer cause = %q", b.TransferCause)
	}
}

func TestCollectorPresentsTheOutletGroupsAsOutlets(t *testing.T) {
	c, _ := startFixture(t)
	snap, _ := c.Collect(context.Background())
	r := rawRegs(t)
	status := make([]uint16, 27)
	static := make([]uint16, 120)
	for i := range status {
		status[i] = r[i]
	}
	for i := range static {
		static[i] = r[516+i]
	}
	groups := OutletGroups(status, static)
	if len(groups) != 2 || groups[0] != "0:Unswitched Group:on" || groups[1] != "1:Outlet Group 1:on" {
		t.Fatalf("groups = %v, want the SMTL1500's unswitched group and Outlet Group 1, both on", groups)
	}
	// One outlet per group, index = group + 1; the Main group is never switchable.
	if len(snap.Outlets) != 2 {
		t.Fatalf("outlets = %+v, want two (the two groups)", snap.Outlets)
	}
	main, g1 := snap.Outlets[0], snap.Outlets[1]
	if main.Index != 1 || !main.On || main.Switchable {
		t.Errorf("main group row = %+v, want index 1, on, unswitchable", main)
	}
	if g1.Index != 2 || !g1.On || !g1.Switchable {
		t.Errorf("group 1 row = %+v, want index 2, on, switchable", g1)
	}
	for _, o := range snap.Outlets {
		if o.Name != "" || o.HasMetering {
			t.Errorf("outlet %d reports a name or metering (%+v); names are controller-owned and the map has no per-group metering", o.Index, o)
		}
	}
	if len(snap.Ports) != 1 || snap.UplinkHint != 1 || !snap.Ports[0].Up {
		t.Errorf("ports = %+v hint %d, want one up SmartConnect port as the uplink", snap.Ports, snap.UplinkHint)
	}
	if c.Capabilities() != (devicemodel.Capabilities{}) {
		t.Error("a UPS claimed switch capabilities")
	}
}

// Start must fail loudly on a unit that answers Modbus without a Smart-UPS
// identity or rating, rather than adopt an empty device.
func TestStartFailsWithoutARating(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	fr.Regs[589] = 0
	c := NewCollector(fr)
	if _, err := c.Start(context.Background()); err == nil {
		t.Error("Start accepted a unit with no real-power rating")
	}
}

// A block the map does not define is refused the way the device refuses it.
func TestFixtureRefusesUnmappedRegistersLikeTheDevice(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fr.ReadRegisters(context.Background(), 100, 4); err == nil {
		t.Error("read of unmapped registers succeeded")
	}
}

// Modbus carries no IP configuration, so the network comes from the
// operator; it is what the controller places the device by.
func TestReachabilityComesFromTheOperator(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	c.Addr = "192.0.2.30:502"
	c.Netmask = "255.255.255.0"
	c.GatewayMAC = "02:00:00:00:00:FE"
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.System.Addresses[0].PrefixLen; got != 24 {
		t.Errorf("prefix = %d, want 24 from 255.255.255.0", got)
	}
	if snap.System.GatewayMAC != "02:00:00:00:00:fe" {
		t.Errorf("gateway MAC = %q, want lower-cased", snap.System.GatewayMAC)
	}
	if prefixLen("") != 0 || prefixLen("not-a-mask") != 0 {
		t.Error("an absent or bad mask must give no prefix, not a guess")
	}
}

// Without an operator-supplied MAC the device gets a stable locally
// administered one from its serial, so adoption never touches the port's own
// client record or reservation.
func TestSyntheticMACIsStableLocalAndUnicast(t *testing.T) {
	c, _ := startFixture(t)
	snap, _ := c.Collect(context.Background())
	mac := snap.System.MAC
	if mac != syntheticMAC("SSJ00000000") || mac != syntheticMAC(snap.System.Serial) {
		t.Errorf("MAC %q is not the serial-derived address", mac)
	}
	if !strings.HasPrefix(mac, "02:") || len(mac) != 17 {
		t.Errorf("MAC %q is not a locally-administered unicast address", mac)
	}
	if syntheticMAC("SSJ00000000") == syntheticMAC("another unit") {
		t.Error("different serials gave the same address")
	}
	c2 := NewCollector(&FixtureRunner{Regs: rawRegs(t)})
	c2.Addr, c2.MAC = "192.0.2.30:502", "02:00:00:00:00:02"
	snap2, _ := c2.Collect(context.Background())
	if snap2.System.MAC != "02:00:00:00:00:02" {
		t.Errorf("operator MAC not honoured: %q", snap2.System.MAC)
	}
}

// Dialled by a name, the reported address is what the name resolves to now;
// a name that stops resolving reports nothing rather than a stale address.
func TestDialledByNameReportsTheResolvedAddress(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	c.Addr = "ups.example.net:502"
	c.Netmask = "255.255.0.0"
	answer := "192.0.2.30"
	c.Resolve = func(_ context.Context, host string) ([]net.IP, error) {
		if host != "ups.example.net" {
			t.Errorf("resolved %q, want the dialled name", host)
		}
		if answer == "" {
			return nil, errors.New("NXDOMAIN")
		}
		return []net.IP{net.ParseIP(answer)}, nil
	}
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.System.Addresses) != 1 || snap.System.Addresses[0].IP != "192.0.2.30" || snap.System.Addresses[0].PrefixLen != 16 {
		t.Errorf("addresses = %+v, want the resolved 192.0.2.30/16", snap.System.Addresses)
	}
	answer = "192.0.2.31" // the lease moved
	snap, _ = c.Collect(context.Background())
	if snap.System.Addresses[0].IP != "192.0.2.31" {
		t.Errorf("after a lease move the reported address is %s, want 192.0.2.31", snap.System.Addresses[0].IP)
	}
	answer = ""
	snap, _ = c.Collect(context.Background())
	if len(snap.System.Addresses) != 0 {
		t.Errorf("a name that does not resolve reported %+v; want no address rather than a stale one", snap.System.Addresses)
	}
}

func TestDialledByLiteralReportsTheLiteral(t *testing.T) {
	c, _ := startFixture(t)
	snap, _ := c.Collect(context.Background())
	if snap.System.Addresses[0].IP != "192.0.2.30" {
		t.Errorf("addresses = %+v", snap.System.Addresses)
	}
}
