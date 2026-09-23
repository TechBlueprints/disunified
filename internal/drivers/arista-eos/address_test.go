package aristaeos

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The fixture switch carries its address on Management1 (192.0.2.1/16), with
// a default route via 192.0.2.2 and that same host as its name server.
const fixtureHost = "192.0.2.1"

func addressCollector(t *testing.T, redial func(string) Transport) (*Collector, *FixtureTransport) {
	t.Helper()
	ft := NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
	c := NewCollector(ft)
	c.host, c.redial = fixtureHost, redial
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return c, ft
}

func TestAddressingIsReadFromTheRunningConfig(t *testing.T) {
	c, _ := addressCollector(t, nil)
	snap := c.last
	if snap.System.Gateway != "192.0.2.2" {
		t.Errorf("gateway = %q, want the default route's next hop", snap.System.Gateway)
	}
	if snap.System.DHCP {
		t.Error("the fixture's management interface has a static address; reported as DHCP")
	}
	if len(c.nameServers) != 1 || c.nameServers[0] != "192.0.2.2" {
		t.Errorf("name servers = %v", c.nameServers)
	}
	if iface, a := managementInterface(snap, fixtureHost); iface != "Management1" || a.PrefixLen != 16 {
		t.Errorf("management interface = %q %+v", iface, a)
	}
}

// Re-sending what the switch already has must write nothing: an address
// change is the one write that can sever the bridge's own connection.
func TestApplyAddressIsANoOpWhenTheSwitchAlreadyHasIt(t *testing.T) {
	c, ft := addressCollector(t, nil)
	before := len(ft.Configured)
	changed, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: fixtureHost, PrefixLen: 16, Gateway: "192.0.2.2", DNS: []string{"192.0.2.2"}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if changed || len(ft.Configured) != before {
		t.Errorf("changed=%v, config batches written: %v", changed, ft.Configured[before:])
	}
}

// The DHCP default is refused at the driver too, not only in the loop.
func TestApplyAddressNeverAppliesDHCP(t *testing.T) {
	c, ft := addressCollector(t, nil)
	before := len(ft.Configured)
	if changed, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{DHCP: true}); err != nil || changed || len(ft.Configured) != before {
		t.Errorf("DHCP: changed=%v err=%v batches=%v", changed, err, ft.Configured[before:])
	}
}

// A gateway change keeps the address, so the same transport confirms: one
// session batch with a commit timer, then the exec-level confirm and a
// write memory.
func TestApplyAddressChangesTheGatewayInATimedSession(t *testing.T) {
	c, ft := addressCollector(t, nil)
	before := len(ft.Configured)
	changed, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: fixtureHost, PrefixLen: 16, Gateway: "192.0.2.9"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got := ft.Configured[before:]
	if len(got) != 2 {
		t.Fatalf("batches = %d, want the session then the confirm: %v", len(got), got)
	}
	session := strings.Join(got[0], " / ")
	for _, want := range []string{"configure session dui-address", "interface Management1", "ip address 192.0.2.1/16", "no ip route 0.0.0.0/0 192.0.2.2", "ip route 0.0.0.0/0 192.0.2.9", "commit timer 00:02:00"} {
		if !strings.Contains(session, want) {
			t.Errorf("session batch lacks %q: %s", want, session)
		}
	}
	if confirm := strings.Join(got[1], " / "); confirm != "enable / configure session dui-address commit / write memory" {
		t.Errorf("confirm batch = %s", confirm)
	}
	if c.last.System.Gateway != "192.0.2.9" {
		t.Error("the snapshot's gateway was not updated after a confirmed change")
	}
}

// answering is a transport that stands in for the switch at its new address.
type answering struct {
	*FixtureTransport
	fail   bool
	closed bool
}

func (a *answering) Run(ctx context.Context, cmds []string) ([]json.RawMessage, error) {
	if a.fail {
		return nil, errors.New("dial tcp: connect: no route to host")
	}
	return a.FixtureTransport.Run(ctx, cmds)
}
func (a *answering) Close() error { a.closed = true; return nil }

// An address move is confirmed only through a fresh dial to the new address.
// When that answers, the confirm goes over it and the driver switches its
// own transport to it.
func TestApplyAddressMovesTheSwitchAndConfirmsAtTheNewAddress(t *testing.T) {
	newT := &answering{FixtureTransport: NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))}
	var dialled string
	c, oldT := addressCollector(t, func(h string) Transport { dialled = h; return newT })
	changed, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.40", PrefixLen: 16, Gateway: "192.0.2.2"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if dialled != "192.0.2.40" {
		t.Errorf("re-dialled %q, want the new address", dialled)
	}
	last := oldT.Configured[len(oldT.Configured)-1]
	if !strings.Contains(strings.Join(last, " / "), "commit timer 00:02:00") {
		t.Errorf("the old transport's last batch must be the timed session, got %v", last)
	}
	if n := len(newT.Configured); n != 1 || strings.Join(newT.Configured[0], " / ") != "enable / configure session dui-address commit / write memory" {
		t.Errorf("confirm over the new transport = %v", newT.Configured)
	}
	if c.t != newT || c.host != "192.0.2.40" {
		t.Error("the driver did not switch its transport to the new address")
	}
}

// When the new address does not answer, nothing is confirmed: the driver
// reports the failure and the switch's own timer reverts the change.
func TestApplyAddressLeavesTheTimerToRevertWhenTheNewAddressDoesNotAnswer(t *testing.T) {
	newT := &answering{FixtureTransport: NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M")), fail: true}
	c, oldT := addressCollector(t, func(string) Transport { return newT })
	changed, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.40", PrefixLen: 16})
	if err == nil || changed {
		t.Fatalf("expected a failure; changed=%v err=%v", changed, err)
	}
	if !strings.Contains(err.Error(), "revert") {
		t.Errorf("the error must say the timer will revert: %v", err)
	}
	if len(newT.Configured) != 0 {
		t.Errorf("confirmed over an unanswering transport: %v", newT.Configured)
	}
	if !newT.closed {
		t.Error("the failed dial was not closed")
	}
	if c.t != oldT || c.host != fixtureHost {
		t.Error("the driver switched transports despite the failure")
	}
}

// With no way to re-dial, an address move is not even attempted past the
// session: the error says so and the switch reverts.
func TestApplyAddressRefusesAMoveItCannotConfirm(t *testing.T) {
	c, _ := addressCollector(t, nil)
	if changed, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.40", PrefixLen: 16}); err == nil || changed || !strings.Contains(err.Error(), "cannot be re-dialled") {
		t.Errorf("changed=%v err=%v", changed, err)
	}
}
