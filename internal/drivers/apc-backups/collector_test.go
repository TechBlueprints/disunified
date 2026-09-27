package apcbackups

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

const fixtureDir = "../../../docs/fixtures/apc-backups-05.3"

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func startFixture(t *testing.T) (*Collector, *FixtureRunner, *devicemodel.Snapshot) {
	t.Helper()
	fr, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	c.Addr = "192.0.2.30"
	c.MAC = "02:00:00:00:00:01"
	c.Netmask = "255.255.255.0"
	c.GatewayMAC = "02:00:00:00:00:fe"
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, fr, snap
}

// The About UPS page: model, SKU, serial, firmware, ratings, battery.
func TestParseAbout(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	ab := parseAbout(fr.Pages_["ulabout"])
	if ab.Model != "Back-UPS Pro 500" || ab.SKU != "BG500" || ab.Serial != "SSJ00000000" || ab.RealW != 300 || ab.ApparentVA != 500 {
		t.Errorf("about = %+v", ab)
	}
	if !strings.HasPrefix(ab.Firmware, "UPS 05.3") || ab.BatteryChemistry == "" {
		t.Errorf("firmware %q battery %q", ab.Firmware, ab.BatteryChemistry)
	}
	if v := parseRatedVoltage(fr.Pages_["ulinput"]); v != 120 {
		t.Errorf("rated output voltage = %d, want 120", v)
	}
}

// The home page: battery, runtime, input, the status list, per-outlet loads.
func TestParseHome(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	h := parseHome(fr.Pages_["home"])
	if h.ChargePct != 100 || h.RuntimeMin != 15 || h.InputV != 117.9 || !h.HasInput {
		t.Errorf("home = %+v", h)
	}
	if h.AlarmClass != "alarmGood" || h.AlarmText != "No Alarms Present" || len(h.Status) != 1 || h.Status[0] != "UPS is online." {
		t.Errorf("alarm %q %q status %v", h.AlarmClass, h.AlarmText, h.Status)
	}
	if h.BatteryLife != "Battery OK" {
		t.Errorf("battery life %q", h.BatteryLife)
	}
	if len(h.OutletLoadsW) != 4 || h.OutletLoadsW[0] != 95.35 || len(h.OutletNames) != 4 {
		t.Errorf("outlet loads %v names %v", h.OutletLoadsW, h.OutletNames)
	}
}

// The Outlet Settings page: four rows, two main groups then two switched
// groups, with state, load and whether the switched ones are battery-backed.
func TestParseOutlets(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	rows := parseOutlets(fr.Pages_["uloutcfg2"])
	if len(rows) != 4 {
		t.Fatalf("rows = %+v", rows)
	}
	want := []outletRow{
		{Kind: "MOG", N: 1, Name: "outlet-1", On: true, LoadW: 111.10, BatteryBackup: true, Master: true},
		{Kind: "MOG", N: 2, Name: "unplugged", On: true, LoadW: 0, BatteryBackup: true},
		{Kind: "SOG", N: 1, Name: "unplugged", On: true, LoadW: 0, BatteryBackup: false},
		{Kind: "SOG", N: 2, Name: "unplugged", On: true, LoadW: 0, BatteryBackup: false},
	}
	for i, w := range want {
		if rows[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], w)
		}
	}
}

// The snapshot: identity, address, battery from the pages, four outlets
// with per-outlet metering, the switched groups alone switchable.
func TestSnapshotFromThePages(t *testing.T) {
	c, _, snap := startFixture(t)
	sys := snap.System
	if sys.Vendor != "APC" || sys.Model != "Back-UPS Pro 500" || sys.Serial != "SSJ00000000" || sys.Version != "05.3" || sys.MAC != "02:00:00:00:00:01" {
		t.Errorf("system = %+v", sys)
	}
	if !sys.DHCP {
		t.Errorf("the capture's card is on DHCP Only; DHCP must be reported")
	}
	if len(sys.Addresses) != 1 || sys.Addresses[0].IP != "192.0.2.30" || sys.Addresses[0].PrefixLen != 24 || sys.GatewayMAC != "02:00:00:00:00:fe" {
		t.Errorf("addresses = %v gateway_mac %q", sys.Addresses, sys.GatewayMAC)
	}
	b := sys.Battery
	if b == nil {
		t.Fatal("no battery")
	}
	if b.ChargePct != 100 || b.Runtime != 15*time.Minute || b.OnBattery || b.LowBattery || b.Fault || b.Charging {
		t.Errorf("battery = %+v", b)
	}
	if b.RealPowerRatingW != 300 || b.ApparentRatingVA != 500 || b.RealPowerW != 111.10 || b.LoadPct < 37.0 || b.LoadPct > 37.1 || !b.HasInput || b.InputVoltageV != 117.9 {
		t.Errorf("ratings/load = rating %v/%v real %v load%% %v input %v", b.RealPowerRatingW, b.ApparentRatingVA, b.RealPowerW, b.LoadPct, b.InputVoltageV)
	}
	if !sys.HasPowerDraw || sys.PowerDrawW != 111.10 || sys.PowerBudgetW != 300 {
		t.Errorf("power draw = %v of %v", sys.PowerDrawW, sys.PowerBudgetW)
	}
	if len(snap.Ports) != 1 || snap.Ports[0].IfName != "eth0" || snap.UplinkHint != 1 {
		t.Errorf("ports = %+v hint %d", snap.Ports, snap.UplinkHint)
	}
	if len(snap.Outlets) != 4 {
		t.Fatalf("outlets = %+v", snap.Outlets)
	}
	for i, o := range snap.Outlets {
		if o.Index != i+1 || !o.On || !o.HasMetering || o.VoltageV != 120 {
			t.Errorf("outlet %d = %+v", i+1, o)
		}
		if (i >= 2) != o.Switchable {
			t.Errorf("outlet %d switchable = %v; only the switched groups (3, 4) are", i+1, o.Switchable)
		}
	}
	if o := snap.Outlets[0]; o.PowerW != 111.10 || o.CurrentA < 0.92 || o.CurrentA > 0.93 {
		t.Errorf("metering on outlet 1 = %v W %v A", o.PowerW, o.CurrentA)
	}
	if c.names[0] != "outlet-1" {
		t.Errorf("names = %v", c.names)
	}
}

// Start fails on a card that is not a Back-UPS Pro network model (no
// About UPS page with a rating).
func TestStartNeedsTheAboutPage(t *testing.T) {
	fr, err := NewFixtureRunner(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	fr.Pages_["ulabout"] = "<html><body>APC | Log On</body></html>"
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	if _, err := c.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "About UPS") {
		t.Errorf("Start = %v, want a refusal naming the About page", err)
	}
}

func TestOpenNeedsURLAndLogin(t *testing.T) {
	if _, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{}); err == nil {
		t.Error("no url accepted")
	}
	if _, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{URL: "192.0.2.30"}); err == nil {
		t.Error("no login accepted")
	}
	d, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{URL: "192.0.2.30", Username: "apc", Password: "x", Options: map[string]string{"mac": "02:00:00:00:00:01"}})
	if err != nil {
		t.Fatal(err)
	}
	c := d.(*Collector)
	if c.Addr != "192.0.2.30" || c.r.(*Web).Base != "http://192.0.2.30" {
		t.Errorf("Open: addr %q base %q", c.Addr, c.r.(*Web).Base)
	}
}
