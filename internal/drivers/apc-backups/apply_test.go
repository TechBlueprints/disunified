package apcbackups

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Switching a switched group: one two-step form post carrying the change,
// the fixture's page flips as the card's would, the next poll reads the new
// state back, and a push that matches writes nothing.
func TestApplyOutletsSwitchesASwitchedGroupOnce(t *testing.T) {
	c, fr, _ := startFixture(t)
	desired := []devicemodel.OutletDesired{{Index: 1, On: true}, {Index: 2, On: true}, {Index: 3, On: true}, {Index: 4, On: false}}
	if got := c.PlanOutlets(desired); len(got) != 1 || got[0] != 4 {
		t.Errorf("PlanOutlets = %v, want [4]", got)
	}
	n, err := c.ApplyOutlets(context.Background(), desired)
	if err != nil || n != 1 {
		t.Fatalf("ApplyOutlets = %d, %v; want 1 change", n, err)
	}
	if len(fr.Controls) != 1 || fr.Controls[0][2] != codeOff || fr.Controls[0][1] != "" {
		t.Errorf("controls = %v, want switched group 2 off and nothing else", fr.Controls)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Outlets[3].On || !snap.Outlets[2].On {
		t.Errorf("after the switch: outlet 4 on=%v, outlet 3 on=%v", snap.Outlets[3].On, snap.Outlets[2].On)
	}
	n, err = c.ApplyOutlets(context.Background(), desired)
	if err != nil || n != 0 || len(fr.Controls) != 1 {
		t.Errorf("a matching push must write nothing: %d, %v, controls %v", n, err, fr.Controls)
	}
	// Back on, with a second group changing in the same push: one post.
	desired[3].On, desired[2].On = true, false
	n, err = c.ApplyOutlets(context.Background(), desired)
	if err != nil || n != 2 || len(fr.Controls) != 2 || fr.Controls[1][1] != codeOff || fr.Controls[1][2] != codeOn {
		t.Errorf("two changes: %d, %v, controls %v", n, err, fr.Controls)
	}
}

// Main outlet groups have no relay: a push to switch one is refused with a
// log line, never posted; an outlet the UPS does not have is ignored.
func TestMainGroupsAreNeverCommanded(t *testing.T) {
	c, fr, _ := startFixture(t)
	n, err := c.ApplyOutlets(context.Background(), []devicemodel.OutletDesired{{Index: 1, On: false}, {Index: 2, On: false}, {Index: 7, On: false}})
	if err != nil || n != 0 || len(fr.Controls) != 0 {
		t.Errorf("main groups: %d, %v, controls %v", n, err, fr.Controls)
	}
	if err := c.CycleOutlet(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "main outlet group") {
		t.Errorf("cycling a main group: %v", err)
	}
	if err := c.CycleOutlet(context.Background(), 9); err == nil {
		t.Error("cycling an unknown outlet must fail")
	}
}

// A power cycle is the card's Reboot action on the switched group; the
// group reads back on afterwards.
func TestCycleOutletIsTheCardsReboot(t *testing.T) {
	c, fr, _ := startFixture(t)
	if err := c.CycleOutlet(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if len(fr.Controls) != 1 || fr.Controls[0][1] != codeReboot {
		t.Errorf("controls = %v", fr.Controls)
	}
	snap, _ := c.Collect(context.Background())
	if !snap.Outlets[2].On {
		t.Error("after a reboot the group reads back on")
	}
}

// A refused post leaves the driver's view alone.
func TestControlFailureChangesNothing(t *testing.T) {
	c, fr, _ := startFixture(t)
	fr.FailNext = errors.New("HTTP 500")
	if _, err := c.ApplyOutlets(context.Background(), []devicemodel.OutletDesired{{Index: 4, On: false}}); err == nil {
		t.Fatal("a failed post must be an error")
	}
	if snap, _ := c.Collect(context.Background()); !snap.Outlets[3].On {
		t.Error("the outlet must still read on")
	}
}

// The controller's IP Settings become a partial config.ini with the card's
// MAC in Override=, as on the PDU: static applied once, DHCP re-applied
// only when the card is not already on it, nothing without the MAC.
func TestApplyAddressWritesTheTCPIPSection(t *testing.T) {
	c, fr, _ := startFixture(t)
	changed, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.30", PrefixLen: 24, Gateway: "192.0.2.1"})
	if err != nil || !changed {
		t.Fatalf("static: %v, %v", changed, err)
	}
	if len(fr.Puts) != 1 || !strings.Contains(fr.Puts[0], "Override=02 00 00 00 00 01\r\n") || !strings.Contains(fr.Puts[0], "BootMode=Manual\r\n") || !strings.Contains(fr.Puts[0], "SystemIP=192.0.2.30\r\n") || !strings.Contains(fr.Puts[0], "SubnetMask=255.255.255.0\r\n") || !strings.Contains(fr.Puts[0], "DefaultGateway=192.0.2.1\r\n") {
		t.Errorf("config.ini = %q", fr.Puts)
	}
	if changed, _ := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.30", PrefixLen: 24, Gateway: "192.0.2.1"}); changed {
		t.Error("the same static address again must be a no-op")
	}
	// The controller's push for a usp device carries no route: the
	// operator's gateway option fills it, and a card holding a different
	// gateway is rewritten.
	c.Gateway = "192.0.2.254"
	changed, err = c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.30", PrefixLen: 24})
	if err != nil || !changed || !strings.Contains(fr.Puts[len(fr.Puts)-1], "DefaultGateway=192.0.2.254\r\n") {
		t.Errorf("gateway from the option: %v, %v, %q", changed, err, fr.Puts[len(fr.Puts)-1])
	}
	if changed, _ := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.30", PrefixLen: 24}); changed {
		t.Error("with the card holding the option's gateway, no rewrite")
	}
	changed, err = c.ApplyAddress(context.Background(), devicemodel.AddressDesired{DHCP: true})
	if err != nil || !changed || len(fr.Puts) != 3 || !strings.Contains(fr.Puts[2], "BootMode=DHCP Only\r\n") {
		t.Errorf("back to DHCP: %v, %v, puts %q", changed, err, fr.Puts)
	}
	if changed, _ := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{DHCP: true}); changed {
		t.Error("DHCP again must be a no-op")
	}
	c.MAC = ""
	if _, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.31", PrefixLen: 24}); err == nil {
		t.Error("without the card's MAC the section would be ignored; must be refused")
	}
}
