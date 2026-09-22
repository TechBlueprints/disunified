package unificfg

import (
	"strings"
	"testing"
)

// Outlet state reaches a power device as system_cfg lines, not as a command:
// switching an outlet is configuration, the way a port's state is.
func TestParseOutletRelayState(t *testing.T) {
	c := Parse(strings.Join([]string{
		"outlet.1.relay_state=enabled",
		"outlet.2.relay_state=disabled",
		"outlet.3.name=NAS",
		"mgmt_cfg.something=ignored",
	}, "\n"))

	if got := c.OutletIndexes(); len(got) != 3 {
		t.Fatalf("outlet indexes = %v, want 3", got)
	}
	if !c.Outlets[1].RelayOn {
		t.Error("outlet 1 should be on")
	}
	if c.Outlets[2].RelayOn {
		t.Error("outlet 2 should be off")
	}
	if c.Outlets[3].Name != "NAS" {
		t.Errorf("outlet 3 name = %q, want NAS", c.Outlets[3].Name)
	}
	// An outlet the controller only named must not be switched off by the
	// absence of a relay_state line.
	if !c.Outlets[3].RelayOn {
		t.Error("an outlet with no relay_state line must default to on")
	}
}

func TestParseOutletKeysAreAlsoKeptRaw(t *testing.T) {
	c := Parse("outlet.4.relay_state=disabled")
	if c.Raw["outlet.4.relay_state"] != "disabled" {
		t.Errorf("raw = %q, want the verbatim value", c.Raw["outlet.4.relay_state"])
	}
}

func TestConfigWithoutOutletsHasNone(t *testing.T) {
	c := Parse("switch.port.1.status=enabled")
	if len(c.Outlets) != 0 {
		t.Errorf("a switch config grew %d outlets", len(c.Outlets))
	}
}
