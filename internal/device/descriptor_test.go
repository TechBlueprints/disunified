package device

import "testing"

// The serial a device reports: the operator's -serial while it fits the
// controller's varchar(12), else the MAC's hex digits as a real UniFi device
// reports (every UniFi device record on the console, 2026-11-12). main
// never passes the device's own serial; the long shapes are the two that
// halted the migration: a DMI UUID and a 22-character chassis serial.
func TestSerialFor(t *testing.T) {
	const mac = "02:00:00:0a:0b:0c"
	for _, c := range []struct{ serial, want string }{
		{"SSJ00000000", "SSJ00000000"},                           // the Arista's shape, 11
		{"SSJ000000000", "SSJ000000000"},                         // exactly 12
		{"", "0200000A0B0C"},                                     // none
		{"SSJ0000000000000000000", "0200000A0B0C"},               // 22
		{"00000000-0000-4000-8000-000000000000", "0200000A0B0C"}, // 36
	} {
		if got := SerialFor(c.serial, mac); got != c.want {
			t.Errorf("SerialFor(%q) = %q, want %q", c.serial, got, c.want)
		}
	}
}

func TestDescriptorSerialFallsBackToMAC(t *testing.T) {
	d, err := DescriptorFor("UDC48X6", nil, Identity{MAC: "02:00:00:0a:0b:0c", Serial: "00000000-0000-4000-8000-000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Serial != "0200000A0B0C" {
		t.Errorf("Serial = %q, want the MAC's 0200000A0B0C", d.Serial)
	}
}
