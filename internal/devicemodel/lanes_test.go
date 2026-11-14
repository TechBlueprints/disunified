package devicemodel

import (
	"net"
	"testing"
)

func TestLaneSwitchMAC(t *testing.T) {
	// A multicast first octet (bit 0) must come out unicast, with the
	// locally administered bit (bit 1) set: built from bytes, not a sample.
	mcast := net.HardwareAddr{0x01, 0, 0, 0, 0, 0x01}.String()
	for _, c := range []struct {
		parent string
		cage   int
		want   string
	}{
		{"02:00:00:00:00:ff", 2, "02:00:00:00:00:02"}, // already local, unicast: only the cage changes
		{mcast, 9, "02:00:00:00:00:09"},
		{net.HardwareAddr{0, 0, 0, 0, 0, 0x01}.String(), 32, "02:00:00:00:00:20"},
	} {
		got, err := LaneSwitchMAC(c.parent, c.cage)
		if err != nil || got != c.want {
			t.Errorf("LaneSwitchMAC(%s, %d) = %s, %v; want %s", c.parent, c.cage, got, err, c.want)
		}
	}
	for _, bad := range []struct {
		mac  string
		cage int
	}{{"nope", 1}, {"aa:bb:cc:dd:ee:ff", 0}, {"aa:bb:cc:dd:ee:ff", 256}} {
		if _, err := LaneSwitchMAC(bad.mac, bad.cage); err == nil {
			t.Errorf("LaneSwitchMAC(%q, %d) accepted", bad.mac, bad.cage)
		}
	}
}

type fakeDevice struct{ Device }

func TestOpenedRegistry(t *testing.T) {
	a, b := &fakeDevice{}, &fakeDevice{}
	RegisterOpened("x", a)
	if d, ok := OpenedDevice("x"); !ok || d != a {
		t.Fatalf("OpenedDevice: %v %v", d, ok)
	}
	UnregisterOpened("x", b) // not the one registered: kept
	if _, ok := OpenedDevice("x"); !ok {
		t.Fatal("removed by another device")
	}
	UnregisterOpened("x", a)
	if _, ok := OpenedDevice("x"); ok {
		t.Fatal("still registered")
	}
}
