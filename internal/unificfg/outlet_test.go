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
