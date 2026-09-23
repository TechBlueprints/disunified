package informloop

import (
	"os"
	"testing"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/fixtures/controller-10.6.106/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// The two pushes on record: the default "Using DHCP" and a static setting.
func TestAddressIntentAppliesAStaticSetting(t *testing.T) {
	static := readFixture(t, "system_cfg-pdu-static-ip.txt")
	d, _, ok := addressIntent("", static)
	if !ok || d.DHCP || d.IP != "192.0.2.1" || d.PrefixLen != 16 || d.Gateway != "192.0.2.2" {
		t.Errorf("static push -> %+v ok=%v", d, ok)
	}
}

// The default DHCP push, with nothing static before it, applies nothing: it
// is what every freshly adopted device is told, and means nothing.
func TestAddressIntentIgnoresTheDHCPDefault(t *testing.T) {
	dhcp := readFixture(t, "system_cfg-pdu-outlets.txt")
	if _, _, ok := addressIntent("", dhcp); ok {
		t.Error("the DHCP default was turned into intent with no static push before it")
	}
	// On a reconcile there is no previous push to compare with either.
	if _, _, ok := addressIntent("", dhcp); ok {
		t.Error("reconcile applied DHCP")
	}
	// And a DHCP push following a DHCP push is still the default.
	if _, _, ok := addressIntent(dhcp, dhcp); ok {
		t.Error("DHCP after DHCP was treated as a change")
	}
}

// DHCP pushed right after a static setting can only mean someone changed
// it in the controller: that is applied.
func TestAddressIntentAppliesDHCPWhenItReplacesAStaticSetting(t *testing.T) {
	static := readFixture(t, "system_cfg-pdu-static-ip.txt")
	dhcp := readFixture(t, "system_cfg-pdu-outlets.txt")
	d, _, ok := addressIntent(static, dhcp)
	if !ok || !d.DHCP {
		t.Errorf("static -> DHCP must apply DHCP; got %+v ok=%v", d, ok)
	}
}

func TestAddressIntentRejectsAStaticSettingWithoutAnAddress(t *testing.T) {
	_, reason, ok := addressIntent("", "netconf.1.status=enabled\nnetconf.1.ip=192.0.2.9\n")
	if ok || reason == "" {
		t.Errorf("static without a mask: ok=%v reason=%q", ok, reason)
	}
}
