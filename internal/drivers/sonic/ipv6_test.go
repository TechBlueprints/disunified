package sonic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// The DX010 on igmp-202511.20261106-f2e25f54c, 2026-11-14 (docs/drivers/
// sonic.md §11.22), managed in-band on Vlan4094 with the gateway sending
// router advertisements for a /64 on the management network:
//
//   - collect-ipv6-off.txt: SONiC as it ships, accept_ra 1 everywhere,
//     only the link-local on Vlan4094;
//   - collect-ipv6-off-leftover-route.txt: the same after a hand teardown
//     that flushed the address and the RA default but left the RA's
//     on-link /64 (proto kernel, an expiry);
//   - collect-ipv6-slaac.txt: accept_ra 2 on Vlan4094 -- the SLAAC address,
//     the RA default and the on-link /64, no INTF_TABLE row for the address;
//   - collect-ipv6-on.txt: the same with the driver's INTF_TABLE row, which
//     gave the address its ip2me route (uploads to it at 11.4 MB/s, 0.9
//     without).
const (
	fixtureIPv6Off      = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-ipv6-off.txt"
	fixtureIPv6Leftover = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-ipv6-off-leftover-route.txt"
	fixtureIPv6SLAAC    = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-ipv6-slaac.txt"
	fixtureIPv6On       = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-ipv6-on.txt"
)

// ipv6 applies the management network's IPv6 setting and returns the
// writes; then it collects again and checks a second apply writes nothing.
func (r *controlRig) ipv6(on bool) []string {
	r.t.Helper()
	n := len(r.fr.Commands)
	if _, err := r.c.ApplyManagementIPv6(context.Background(), on); err != nil {
		r.t.Fatalf("ApplyManagementIPv6(%v): %v", on, err)
	}
	got := append([]string(nil), r.fr.Commands[n:]...)
	if _, err := r.c.Collect(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	m := len(r.fr.Commands)
	if _, err := r.c.ApplyManagementIPv6(context.Background(), on); err != nil {
		r.t.Fatalf("second ApplyManagementIPv6(%v): %v", on, err)
	}
	if again := r.fr.Commands[m:]; len(again) != 0 {
		r.t.Errorf("not idempotent: the second apply wrote %q", again)
	}
	return got
}

func applLine(t *testing.T, ops ...applOp) string {
	t.Helper()
	b, err := json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	return "sudo python3 -c " + strings.TrimPrefix(applCommand, "sudo python3 -c ") + " " + sshrun.ShellQuote(string(b))
}

func sameCommands(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("writes:\n got %q\nwant %q", got, want)
	}
}

// SONiC as it ships ignores the RAs on Vlan4094 (forwarding on, accept_ra
// 1). With IPv6 on the management network the driver sets accept_ra 2 on
// the interface it manages the switch at -- and only there: every other
// VLAN keeps 1, or the switch would take an address on every network with
// RAs.
func TestIPv6OnTakesTheManagementNetworksRouterAdvertisements(t *testing.T) {
	r := rig20261106(t, fixtureIPv6Off)
	sameCommands(t, r.ipv6(true), []string{"echo 2 | sudo tee /proc/sys/net/ipv6/conf/Vlan4094/accept_ra >/dev/null"})
	ra := r.c.state.v6.acceptRA
	if ra["Vlan4094"] != "2" || ra["Vlan2"] != "1" || ra["Ethernet0"] != "1" {
		t.Errorf("accept_ra read back: Vlan4094 %q, Vlan2 %q, Ethernet0 %q", ra["Vlan4094"], ra["Vlan2"], ra["Ethernet0"])
	}
}

// Once the RA has given Vlan4094 its SLAAC address, the address gets the
// INTF_TABLE row orchagent makes its ip2me route from.
func TestIPv6OnGivesTheSLAACAddressItsIP2MeRow(t *testing.T) {
	r := rig20261106(t, fixtureIPv6SLAAC)
	if got := r.c.state.v6.slaac["Vlan4094"]; len(got) != 1 || got[0] != "2001:db8::3/64" {
		t.Fatalf("SLAAC addresses read: %q", got)
	}
	sameCommands(t, r.ipv6(true), []string{applLine(t,
		applOp{Op: "set", Table: "INTF_TABLE", Key: "Vlan4094:2001:db8::3/64", FV: []string{"scope", "global", "family", "IPv6"}})})
	if !r.c.state.v6.rows["Vlan4094"]["2001:db8::3/64"] {
		t.Errorf("rows read back: %v", r.c.state.v6.rows)
	}
}

// The switch as it ran on, and as it ships with IPv6 off: nothing to do.
func TestIPv6ConvergedStatesWriteNothing(t *testing.T) {
	for _, tc := range []struct {
		path string
		on   bool
	}{{fixtureIPv6On, true}, {fixtureIPv6Off, false}} {
		r := rig20261106(t, tc.path)
		if got := r.ipv6(tc.on); len(got) != 0 {
			t.Errorf("%s, IPv6 %v: wrote %q", tc.path, tc.on, got)
		}
	}
}

// IPv6 switched off on the management network: the row first (the chip's
// ip2me route), then accept_ra back to SONiC's 1, the SLAAC address, the
// RA's default route and its on-link /64, which the address flush leaves.
// The read-back has none of them, and IPv4 is untouched.
func TestIPv6OffUndoesEverything(t *testing.T) {
	r := rig20261106(t, fixtureIPv6On)
	sameCommands(t, r.ipv6(false), []string{
		applLine(t, applOp{Op: "del", Table: "INTF_TABLE", Key: "Vlan4094:2001:db8::1/64"}),
		"echo 1 | sudo tee /proc/sys/net/ipv6/conf/Vlan4094/accept_ra >/dev/null",
		"sudo ip -6 addr flush dev Vlan4094 scope global dynamic",
		"sudo ip -6 route flush dev Vlan4094 proto ra",
		"sudo ip -6 route del 2001:db8::6/64 dev Vlan4094",
	})
	v := r.c.state.v6
	if v.acceptRA["Vlan4094"] != "1" || len(v.slaac["Vlan4094"]) != 0 || len(v.routes) != 0 || len(v.rows["Vlan4094"]) != 0 {
		t.Errorf("read back: accept_ra %q, SLAAC %q, routes %v, rows %v", v.acceptRA["Vlan4094"], v.slaac["Vlan4094"], v.routes, v.rows)
	}
	if _, ok := r.fr.table("appl_intf")["INTF_TABLE:Vlan4094:192.0.2.1/16"]; !ok {
		t.Error("the IPv4 address's row went too")
	}
	if !r.fr.hasKernelAddress("Vlan4094", "192.0.2.1", "16") {
		t.Error("the IPv4 address went too")
	}
}

// The on-link prefix an RA left behind (seen on the DX010 after a teardown
// by hand) goes too.
func TestIPv6OffRemovesALeftoverPrefixRoute(t *testing.T) {
	r := rig20261106(t, fixtureIPv6Leftover)
	sameCommands(t, r.ipv6(false), []string{"sudo ip -6 route del 2001:db8::5/64 dev Vlan4094"})
}

// A row whose address the kernel no longer has (the prefix moved, or the
// address expired) goes, with IPv6 on as with it off.
func TestIPv6OnDropsARowForAGoneAddress(t *testing.T) {
	r := rig20261106(t, fixtureIPv6On)
	if err := r.fr.flushDynamic6("Vlan4094"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	sameCommands(t, r.ipv6(true), []string{applLine(t, applOp{Op: "del", Table: "INTF_TABLE", Key: "Vlan4094:2001:db8::1/64"})})
}

// A capture from before the section (collect.txt, 2026-11-07): nothing is
// written, and the reason is logged once.
func TestIPv6WithoutTheSectionWritesNothing(t *testing.T) {
	r := rig20261106(t, fixture20261106)
	for _, on := range []bool{true, false} {
		if n, err := r.c.ApplyManagementIPv6(context.Background(), on); n != 0 || err != nil {
			t.Errorf("IPv6 %v: %d writes, %v", on, n, err)
		}
	}
	if len(r.fr.Commands) != 0 {
		t.Errorf("wrote %q", r.fr.Commands)
	}
	if got := strings.Count(r.log.String(), "no ipv6_ra section"); got != 1 {
		t.Errorf("logged %d times:\n%s", got, r.log.String())
	}
}
