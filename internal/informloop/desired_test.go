package informloop

import (
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

func TestDesiredPortsTreatsControllerDefaultsAsNoDescription(t *testing.T) {
	l := &Loop{cfg: Config{DefaultPortNames: map[int][]string{
		1:  {"Port 1", "SFP28 1"},
		2:  {"Port 2", "SFP28 2"},
		49: {"Port 49", "QSFP28 1"},
	}}}
	cfg := "switch.port.1.name=SFP28 1\nswitch.port.2.name=Julie desk\nswitch.port.2.status=disabled\nswitch.port.49.name=Port 49\nswitch.port.49.status=enabled\n"
	got := l.desiredPorts(cfg)
	want := []devicemodel.PortDesired{
		{Index: 1, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true},
		{Index: 2, Enabled: false, Description: "Julie desk", VLANSet: true, NativeVLAN: 1, TaggedAll: true},
		{Index: 49, Enabled: true, VLANSet: true, NativeVLAN: 1, TaggedAll: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestDesiredPortsHonoursAllowlist(t *testing.T) {
	l := &Loop{cfg: Config{ControlPorts: map[int]bool{2: true}}}
	got := l.desiredPorts("switch.port.1.status=disabled\nswitch.port.2.status=disabled\n")
	if len(got) != 1 || got[0].Index != 2 || got[0].Enabled {
		t.Errorf("got %+v", got)
	}
}

func TestDesiredPortsSpeedAndVLANs(t *testing.T) {
	l := &Loop{cfg: Config{}}
	cfg := "switch.vlan.1.id=1\nswitch.vlan.2.id=2\nswitch.vlan.5.id=10\n" +
		"switch.port.2.autoneg=disabled\nswitch.port.2.speed=10000\nswitch.port.2.duplex=enabled\n" +
		"switch.vlan.1.port.2.mode=untagged\nswitch.vlan.2.port.2.mode=tagged\nswitch.vlan.5.port.2.mode=tagged\n" +
		"switch.vlan.2.port.3.mode=untagged\n"
	got := l.desiredPorts(cfg)
	want := []devicemodel.PortDesired{
		{Index: 2, Enabled: true, SpeedMbps: 10000, VLANSet: true, NativeVLAN: 1, TaggedVLANs: []int{2, 10}},
		{Index: 3, Enabled: true, VLANSet: true, NativeVLAN: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

// The push's time zone always reaches the driver; its hostname
// (resolv.host.1.name) only with control.hostname.
func TestDesiredDeviceTimezoneAndHostname(t *testing.T) {
	cfg := readSystemCfg(t, "system_cfg-pdu-outlets.txt") // a recorded push carrying both
	off := (&Loop{cfg: Config{}}).desiredDevice(cfg)
	if off.Timezone != "MST7MDT,M3.2.0,M11.1.0" || off.Hostname != "" {
		t.Errorf("control.hostname off: timezone %q hostname %q", off.Timezone, off.Hostname)
	}
	on := (&Loop{cfg: Config{ControlHostname: true}}).desiredDevice(cfg)
	if on.Hostname != "APCAP7931" {
		t.Errorf("control.hostname on: hostname %q", on.Hostname)
	}
}

// Rate mode (packets/s) has no equivalent in the drivers, which take a share
// of the port's bandwidth: the port's storm control is left off and the loop
// says so, once per setting (docs/drivers/sonic.md 11.8: it was dropped in
// silence). The captured push is docs/fixtures/controller-10.6.109/
// system_cfg-dx010-storm-rate.txt.
func TestDesiredPortsStormRateIsNotedOnce(t *testing.T) {
	var buf strings.Builder
	l := &Loop{cfg: Config{Logger: log.New(&buf, "", 0)}}
	rate := "switch.port.3.stormctrl.status=enabled\nswitch.port.3.stormctrl.type=rate\nswitch.port.3.stormctrl.bcast=100\nswitch.port.3.stormctrl.mcast=100\n"
	if got := l.desiredPorts(rate); len(got) != 1 || got[0].StormCtrl != nil {
		t.Fatalf("got %+v", got)
	}
	l.desiredPorts(rate)
	if n := strings.Count(buf.String(), "NOTE: port 3 storm control in Rate mode (packets/s)"); n != 1 {
		t.Errorf("noted %d times, want once:\n%s", n, buf.String())
	}
	got := l.desiredPorts(strings.Replace(rate, "rate", "level", 1))
	if sc := got[0].StormCtrl; sc == nil || *sc.BroadcastPct != 100 || *sc.MulticastPct != 100 || sc.UnknownUnicastPct != nil {
		t.Errorf("level mode: %+v", sc)
	}
}
