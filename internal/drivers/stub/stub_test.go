package stub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

const fixtures = "../../../docs/fixtures/stub"

func open(t *testing.T, path string, opts map[string]string) *Device {
	t.Helper()
	o := map[string]string{"snapshot": path, "mac": "02:00:00:00:00:A5", "netmask": "255.255.0.0", "gateway_mac": "02:00:00:00:00:FE"}
	for k, v := range opts {
		o[k] = v
	}
	d, err := Driver{}.Open(context.Background(), devicemodel.DriverConfig{URL: "192.0.2.4", Options: o})
	if err != nil {
		t.Fatal(err)
	}
	return d.(*Device)
}

// The reference file: four 25G SFP28 lanes and a 100G QSFP28 uplink, the
// identity from the options as apc-ups takes them, the file's claims.
func TestFlexXGFile(t *testing.T) {
	d := open(t, filepath.Join(fixtures, "flexxg-4x25g.yaml"), nil)
	snap, err := d.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sys := snap.System
	if sys.MAC != "02:00:00:00:00:a5" || sys.GatewayMAC != "02:00:00:00:00:fe" || sys.Hostname != "lane-switch" || sys.Serial != "SSJ00000000" || sys.Version != "1.0.0" {
		t.Errorf("system = %+v", sys)
	}
	if len(sys.Addresses) != 1 || sys.Addresses[0].IP != "192.0.2.4" || sys.Addresses[0].PrefixLen != 16 {
		t.Errorf("addresses = %+v, want the url's 192.0.2.4/16", sys.Addresses)
	}
	if len(snap.Ports) != 5 || snap.UplinkPort() != 5 {
		t.Fatalf("%d ports, uplink %d; want 5 and port 5", len(snap.Ports), snap.UplinkPort())
	}
	for _, p := range snap.Ports[:4] {
		if !p.Up || p.SpeedMbps != 25000 || p.Media != devicemodel.MediaSFP28 || p.FEC != devicemodel.FECRS || !p.FECCapable || !p.Present || len(p.SpeedCaps) != 2 || p.MTU != 1500 {
			t.Errorf("lane %d = %+v", p.Index, p)
		}
	}
	if up := snap.Ports[4]; up.SpeedMbps != 100000 || up.Media != devicemodel.MediaQSFP28 || up.FEC != "" {
		t.Errorf("uplink = %+v", up)
	}
	if c := d.Capabilities(); !c.LACP || !c.FEC || c.AggregateSessions != 8 || c.STP {
		t.Errorf("capabilities = %+v", c)
	}
}

// The 2x50G file: two lanes, two empty ports (not present, down, no speed);
// the 75000 file: port 1 reports the summed speed as it is.
func TestOtherFiles(t *testing.T) {
	snap, err := open(t, filepath.Join(fixtures, "flexxg-2x50g.yaml"), nil).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports[2:4] {
		if p.Present || p.Up || p.SpeedMbps != 0 {
			t.Errorf("spare port %d = %+v, want empty", p.Index, p)
		}
	}
	if snap.Ports[0].SpeedMbps != 50000 || snap.Ports[1].SpeedMbps != 50000 {
		t.Errorf("lanes = %d, %d Mb/s", snap.Ports[0].SpeedMbps, snap.Ports[1].SpeedMbps)
	}
	snap, err = open(t, filepath.Join(fixtures, "flexxg-75000.yaml"), nil).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Ports[0].SpeedMbps != 75000 {
		t.Errorf("port 1 = %d Mb/s, want 75000", snap.Ports[0].SpeedMbps)
	}
}

// The file is re-read every cycle: an edit shows on the next collect.
func TestFileIsReReadEveryCycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.yaml")
	b, err := os.ReadFile(filepath.Join(fixtures, "flexxg-4x25g.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, b, 0o644)
	d := open(t, path, nil)
	if _, err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte(strings.Replace(string(b), "speed: 25000", "speed: 75000", 1)), 0o644)
	snap, err := d.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Ports[0].SpeedMbps != 75000 {
		t.Errorf("after the edit port 1 = %d Mb/s", snap.Ports[0].SpeedMbps)
	}
}

// Refused: no file, no MAC, a bad file.
func TestOpenRefuses(t *testing.T) {
	ref := filepath.Join(fixtures, "flexxg-4x25g.yaml")
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(bad, []byte("ports:\n  - {index: 1, media: SFP99}\n"), 0o644)
	twoUplinks := filepath.Join(t.TempDir(), "two.yaml")
	os.WriteFile(twoUplinks, []byte("ports:\n  - {index: 1, is_uplink: true}\n  - {index: 2, is_uplink: true}\n"), 0o644)
	for name, opts := range map[string]map[string]string{
		"no snapshot":  {"mac": "02:00:00:00:00:a5"},
		"no mac":       {"snapshot": ref},
		"bad media":    {"snapshot": bad, "mac": "02:00:00:00:00:a5"},
		"two uplinks":  {"snapshot": twoUplinks, "mac": "02:00:00:00:00:a5"},
		"missing file": {"snapshot": filepath.Join(t.TempDir(), "none.yaml"), "mac": "02:00:00:00:00:a5"},
	} {
		if _, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{URL: "192.0.2.4", Options: opts}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The stub writes nothing: it is not a Controller of any kind.
func TestStubWritesNothing(t *testing.T) {
	var d any = open(t, filepath.Join(fixtures, "flexxg-4x25g.yaml"), nil)
	if _, ok := d.(devicemodel.Controller); ok {
		t.Error("stub is a Controller")
	}
	if _, ok := d.(devicemodel.DeviceController); ok {
		t.Error("stub is a DeviceController")
	}
	if _, ok := d.(devicemodel.AddressController); ok {
		t.Error("stub is an AddressController")
	}
}
