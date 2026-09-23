package unificfg

import (
	"os"
	"testing"
)

// The controller's own push for an adopted APC rack PDU (Network 10.6.106,
// 2026-09-21), scrubbed. Hand-written samples would prove only that the parser
// matches what the author imagined the controller sends.
const pduFixture = "../../docs/fixtures/controller-10.6.106/system_cfg-pdu-outlets.txt"

func parsePDUFixture(t *testing.T) *Config {
	t.Helper()
	b, err := os.ReadFile(pduFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return Parse(string(b))
}

// Outlet state reaches a power device as system_cfg lines, not as a command:
// switching an outlet is configuration, the way a port's state is.
func TestParseOutletsFromTheControllersOwnPush(t *testing.T) {
	c := parsePDUFixture(t)
	idx := c.OutletIndexes()
	if len(idx) != 16 {
		t.Fatalf("outlet indexes = %v, want the 16 the controller pushed", idx)
	}
	// The controller addresses the outlets at the indices the device reported,
	// which for a rack PDU claiming the USP-PDU-Pro are its AC positions 5..20.
	if idx[0] != 5 || idx[len(idx)-1] != 20 {
		t.Errorf("outlet indexes run %d..%d, want 5..20", idx[0], idx[len(idx)-1])
	}
	for _, i := range idx {
		if !c.Outlets[i].RelayOn {
			t.Errorf("outlet %d parsed as off; every outlet is enabled in this capture", i)
		}
	}
}

// The controller keeps outlet names in its own overrides and never sends them,
// so the bridge has no name to push down from the inform protocol.
func TestControllerPushesNoOutletNames(t *testing.T) {
	c := parsePDUFixture(t)
	for i, o := range c.Outlets {
		if o.Name != "" {
			t.Errorf("outlet %d carried a name %q; the controller does not send names", i, o.Name)
		}
	}
}

// Every key is still kept verbatim for diffing and logging.
func TestOutletKeysAreAlsoKeptRaw(t *testing.T) {
	c := parsePDUFixture(t)
	if c.Raw["outlet.5.relay_state"] != "enabled" {
		t.Errorf("raw = %q, want the verbatim value", c.Raw["outlet.5.relay_state"])
	}
	if c.Raw["outlet.status"] != "enabled" {
		t.Errorf("outlet.status = %q", c.Raw["outlet.status"])
	}
}

// A switch's push must not grow outlets.
func TestSwitchConfigHasNoOutlets(t *testing.T) {
	b, err := os.ReadFile("../../docs/fixtures/controller-10.6.106/system_cfg-port2-name-speed-vlans.txt")
	if err != nil {
		t.Skipf("switch fixture unavailable: %v", err)
	}
	if c := Parse(string(b)); len(c.Outlets) != 0 {
		t.Errorf("a switch config grew %d outlets", len(c.Outlets))
	}
}

// An outlet the controller mentions without a relay_state line must not be
// switched off by the absence of the key.
func TestOutletDefaultsToOnWhenOnlyNamed(t *testing.T) {
	c := Parse("outlet.7.some_future_key=x")
	if !c.Outlets[7].RelayOn {
		t.Error("an outlet with no relay_state line must default to on")
	}
}

// The controller's IP Settings for the device ride in the same push. The
// captured push has the default, "Using DHCP": netconf.1.ip=0.0.0.0 with the
// DHCP client enabled. A static setting is checked against its own capture
// once one exists; this one guards the form we have.
func TestParseAddressUsingDHCP(t *testing.T) {
	c := parsePDUFixture(t)
	if c.Address == nil {
		t.Fatal("the push carries netconf.1.* but Address is nil")
	}
	if !c.Address.DHCP {
		t.Error("dhcpc.1.status=enabled must parse as DHCP")
	}
	if c.Address.IP != "" {
		t.Errorf("netconf.1.ip=0.0.0.0 must not become an address, got %q", c.Address.IP)
	}
}

func TestParseAddressAbsent(t *testing.T) {
	if c := Parse("switch.port.1.status=enabled"); c.Address != nil {
		t.Errorf("a push with no netconf grew an Address: %+v", c.Address)
	}
}

// The static form, from the controller's own push after the PDU's IP
// Settings were set to a static address (2026-09-22): address and mask under
// netconf.1.*, the gateway under route.1.gateway (route.1.ip=0.0.0.0 marks
// the default route), DNS under resolv.nameserver.N.ip, and no dhcpc.1.*
// lines at all.
func TestParseAddressStatic(t *testing.T) {
	b, err := os.ReadFile("../../docs/fixtures/controller-10.6.106/system_cfg-pdu-static-ip.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	a := Parse(string(b)).Address
	if a == nil {
		t.Fatal("no Address parsed from the static push")
	}
	if a.DHCP {
		t.Error("a static push has no dhcpc.1.status line and must not parse as DHCP")
	}
	if a.IP != "192.0.2.1" || a.Netmask != "255.255.0.0" {
		t.Errorf("address = %s/%s, want 192.0.2.1/255.255.0.0", a.IP, a.Netmask)
	}
	if a.Gateway != "192.0.2.2" {
		t.Errorf("gateway = %q, want the default route's route.1.gateway", a.Gateway)
	}
	if len(a.DNS) != 1 || a.DNS[0] != "192.0.2.2" {
		t.Errorf("dns = %v, want the one resolv.nameserver", a.DNS)
	}
}
