package sonic

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// NTP from the in-band interface (docs/drivers/sonic.md §10.6). The image's
// NTP|global (init_cfg.json, read from the DX010: ntp_global-init_cfg.txt)
// names src_intf eth0, and chrony binds to it once eth0 has a configured
// address; the driver points it at the interface the bridge dials.

// hwRigAt is newHardwareRig over another DX010 capture.
func hwRigAt(t *testing.T, path string, edit func(*FixtureRunner)) *controlRig {
	t.Helper()
	fr, err := NewFixtureRunner(path)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fr.Help, fr.Zones = vs.Help, vs.Zones // the same commit
	if edit != nil {
		edit(fr)
	}
	var buf strings.Builder
	c := NewCollector(fr)
	c.Log = log.New(&buf, "", 0)
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &controlRig{t: t, c: c, fr: fr, log: &buf}
}

// imageNTPGlobal puts the image's NTP|global (src_intf eth0) in place.
func imageNTPGlobal(t *testing.T) func(*FixtureRunner) {
	return func(fr *FixtureRunner) {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(hwFixture), "ntp_global-init_cfg.txt"))
		if err != nil {
			t.Fatal(err)
		}
		fr.UseSection("cfg_ntp_global", sections(string(b))["cfg_ntp_global"])
	}
}

// ntpAsCaptured is the capture's own NTP servers, so only the source
// interface can differ.
func ntpAsCaptured(r *controlRig) devicemodel.DeviceDesired {
	return devicemodel.DeviceDesired{ManageNTP: true, NTPServers: r.system().NTPServers}
}

const ntpSrcVlan = "sudo sonic-db-cli CONFIG_DB hset 'NTP|global' src_intf Vlan4094"

func TestNTPSourcedFromTheDialledInterface(t *testing.T) {
	r := hwRigAt(t, hwFixture, imageNTPGlobal(t))
	r.device("image default", ntpAsCaptured(r), []string{ntpSrcVlan, save})
	if got := parseTableMust(t, sections(r.fr.Fixture), "cfg_ntp_global")["NTP|global"]["src_intf"]; got != "Vlan4094" {
		t.Errorf("read back src_intf %q", got)
	}
	// Without NTP management nothing is touched.
	r = hwRigAt(t, hwFixture, imageNTPGlobal(t))
	r.device("not managed", devicemodel.DeviceDesired{}, nil)
}

// The unit after the hand fix (collect-20261101.txt: src_intf Vlan4094,
// eth0 on 169.254.1.1/16 in MGMT_INTERFACE): nothing to write.
func TestNTPSourceAlreadyInBand(t *testing.T) {
	r := hwRigAt(t, filepath.Join(filepath.Dir(hwFixture), "collect-20261101.txt"), nil)
	r.device("already Vlan4094", ntpAsCaptured(r), nil)
}

// No NTP|global on the switch (a capture from before the section; an image
// without init_cfg.json's entry), or the bridge dialling eth0: left alone.
func TestNTPSourceLeftAlone(t *testing.T) {
	r := hwRigAt(t, hwFixture, nil)
	r.device("no NTP|global", ntpAsCaptured(r), nil)
	r = hwRigAt(t, filepath.Join(filepath.Dir(hwFixture), "collect-20261101.txt"), imageNTPGlobal(t))
	r.c.Dial = ifaceAddr(t, r.fr.Fixture, "eth0") // the link-local rescue address
	r.device("dialled on eth0", ntpAsCaptured(r), nil)
}
