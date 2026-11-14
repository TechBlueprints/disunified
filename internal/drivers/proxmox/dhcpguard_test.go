package proxmox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// ecsGuards are the trusted DHCP servers of a captured push (the ECS's,
// with Rogue DHCP Server Detection on: VLAN 1, 2 and 69).
func ecsGuards(t *testing.T) []devicemodel.DHCPGuard {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-ecs-dhcp-snoop.txt"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := unificfg.Parse(string(b))
	if cfg.DHCPSnoop == nil || !*cfg.DHCPSnoop {
		t.Fatal("capture has no switch.dhcp_snoop.status=enabled")
	}
	var out []devicemodel.DHCPGuard
	for _, g := range cfg.DHCPGuards {
		out = append(out, devicemodel.DHCPGuard{VLAN: g.VLAN, Server: g.Server})
	}
	return out
}

// withGuardListing puts the driver's table, as nft listed it on the node
// after the netns test's frames (nft-dhcpguard.json), into the capture's
// empty dhcpguard section.
func withGuardListing(t *testing.T, r *FixtureRunner) {
	t.Helper()
	const empty = "@@@ dhcpguard\n@@@ end"
	if !strings.Contains(r.Fixture, empty) {
		t.Fatal("capture has no empty dhcpguard section")
	}
	r.Fixture = strings.Replace(r.Fixture, empty, "@@@ dhcpguard\n"+strings.TrimSpace(loadFixtureRaw(t, "nft-dhcpguard.json"))+"\n@@@ end", 1)
}

func on(v bool) *bool { return &v }

func TestDHCPGuardClaimed(t *testing.T) {
	for fixture, want := range map[string]bool{"collect-node2-nft.txt": true, "collect-node2.txt": false} {
		c, _ := newTestCollector(t, fixture)
		if _, err := c.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := c.Capabilities().DHCPSnooping; got != want {
			t.Errorf("%s: DHCPSnooping claimed = %v, want %v (nft present and a VLAN-aware bridge)", fixture, got, want)
		}
	}
}

// TestDHCPGuardWrite: the node has no table; guarding on writes the whole
// table in one nft transaction, exactly the rule set the netns test loaded
// on a node, and a second apply writes nothing.
func TestDHCPGuardWrite(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2-nft.txt")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := devicemodel.DeviceDesired{DHCPSnooping: on(true), DHCPGuards: ecsGuards(t)}
	n, err := c.ApplyDevice(context.Background(), d)
	if err != nil || n != 1 {
		t.Fatalf("ApplyDevice = %d, %v; want 1 change", n, err)
	}
	cmds := commandsWith(r, "nft")
	if len(cmds) != 1 || cmds[0] != "nft -f -" {
		t.Fatalf("commands %q, want one nft -f -", cmds)
	}
	stdin := r.Stdins[len(r.Stdins)-1]
	script := loadFixtureRaw(t, "dhcpguard-netns-test.sh")
	const open = "cat > /tmp/dui-guard-ruleset.nft <<'EOF'\n"
	i := strings.Index(script, open)
	j := strings.Index(script[i+len(open):], "EOF\n")
	if i < 0 || j < 0 {
		t.Fatal("netns test has no rule set")
	}
	if tested := script[i+len(open) : i+len(open)+j]; stdin != tested {
		t.Errorf("rendered write differs from the rule set tested on the node:\n%s\n--- tested ---\n%s", stdin, tested)
	}
	for _, want := range []string{
		"type filter hook prerouting priority filter; policy accept;",
		`iifname { "tap120i0", "fwpr120p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 20"`,
		`iifname { "tap100i0", "fwpr100p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 1"`,
		// a stopped guest's NIC is guarded too, under the names it will get
		`iifname { "tap112i0", "fwpr112p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 13"`,
	} {
		if !strings.Contains(stdin, want) {
			t.Errorf("write lacks %q", want)
		}
	}
	if got := strings.Count(stdin, "counter drop"); got != 30 {
		t.Errorf("%d rules, want 30 (10 guest NICs x VLAN 1, 2, 69)", got)
	}
	before := len(r.Commands)
	if n, _ := c.ApplyDevice(context.Background(), d); n != 0 || len(r.Commands) != before {
		t.Errorf("second apply: %d changes, commands %q", n, r.Commands[before:])
	}
}

// TestDHCPGuardReadBack: the node holds the table the driver wrote; each
// port's drops are summed from its rules' counters, the same push writes
// nothing, guarding off deletes the table, and a push without the key
// leaves it.
func TestDHCPGuardReadBack(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2-nft.txt")
	withGuardListing(t, r)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	drops := map[int]uint64{}
	for _, p := range snap.Ports {
		if p.Health.RogueDHCPDrops > 0 {
			drops[p.Index] = p.Health.RogueDHCPDrops
		}
	}
	// The netns test dropped two replies entering through tap100i0 (port 1)
	// and two through fwpr120p0 (port 20, the firewalled guest).
	if len(drops) != 2 || drops[1] != 2 || drops[20] != 2 {
		t.Errorf("RogueDHCPDrops by port = %v, want map[1:2 20:2]", drops)
	}
	ctx := context.Background()
	if n, _ := c.ApplyDevice(ctx, devicemodel.DeviceDesired{DHCPSnooping: on(true), DHCPGuards: ecsGuards(t)}); n != 0 || len(commandsWith(r, "nft")) != 0 {
		t.Errorf("same guards on a node holding them: %d changes, %q", n, commandsWith(r, "nft"))
	}
	if n, _ := c.ApplyDevice(ctx, devicemodel.DeviceDesired{}); n != 0 || len(commandsWith(r, "nft")) != 0 {
		t.Errorf("no dhcp_snoop in the push: %d changes, %q", n, commandsWith(r, "nft"))
	}
	if n, _ := c.ApplyDevice(ctx, devicemodel.DeviceDesired{DHCPSnooping: on(false)}); n != 1 {
		t.Errorf("guarding off: %d changes, want 1", n)
	}
	if got := commandsWith(r, "nft"); len(got) != 1 || got[0] != "nft delete table bridge disunified" {
		t.Errorf("guarding off: %q, want the table deleted", got)
	}
	if n, _ := c.ApplyDevice(ctx, devicemodel.DeviceDesired{DHCPSnooping: on(false)}); n != 0 {
		t.Errorf("guarding off again: %d changes, want 0", n)
	}
}

func TestDHCPGuardUnclaimedWritesNothing(t *testing.T) {
	c, r := newTestCollector(t, "collect-node2.txt")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{DHCPSnooping: on(true), DHCPGuards: ecsGuards(t)}); n != 0 || len(commandsWith(r, "nft")) != 0 {
		t.Errorf("unclaimed: %d changes, %q", n, commandsWith(r, "nft"))
	}
}

// TestDHCPGuardRulesFollowPortVLANs: on proxmox-1's capture the two NICs of
// the guest with tag=8 and tag=9 are access ports on VLANs the ECS's push
// does not guard, so they get no rule; every other local NIC is a trunk
// (VLAN 1 untagged, the rest tagged) and gets one rule per guarded VLAN.
func TestDHCPGuardRulesFollowPortVLANs(t *testing.T) {
	c, _ := newTestCollector(t, "collect-node1.txt")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	rules := guardRules(c.guardPorts, guardServers(ecsGuards(t), t.Logf))
	trunks := 0
	for _, p := range c.guardPorts {
		hasAccess := p.VLAN.Mode == "access"
		named := false
		for _, r := range rules {
			if strings.Contains(r, `"`+p.Members[0]+`"`) {
				named = true
			}
		}
		if hasAccess && named {
			t.Errorf("access port %d (%v, VLAN %d) got a rule", p.Index, p.Members, p.VLAN.NativeVLAN)
		}
		if !hasAccess {
			trunks++
			if !named {
				t.Errorf("trunk port %d (%v) got no rule", p.Index, p.Members)
			}
		}
	}
	if trunks == 0 || len(rules) != 3*trunks {
		t.Errorf("%d rules for %d trunk NICs, want 3 each", len(rules), trunks)
	}
	access := 0
	for _, p := range c.guardPorts {
		if p.VLAN.Mode == "access" {
			access++
		}
	}
	if access != 2 {
		t.Errorf("%d access NICs, want the tagged guest's 2", access)
	}
	// With VLAN 8 guarded, the tag=8 NIC is guarded on its untagged frames,
	// each trunk on its VLAN 8 tagged ones, and the tag=9 NIC not at all.
	rules = guardRules(c.guardPorts, map[int][]string{8: {"192.0.2.1"}})
	untagged, tagged := 0, 0
	for _, r := range rules {
		switch {
		case strings.Contains(r, `"tap119i0"`) && strings.Contains(r, "ether type ip"):
			untagged++
		case strings.Contains(r, "vlan id 8 ") && !strings.Contains(r, `"tap119i`):
			tagged++
		default:
			t.Errorf("VLAN 8 guarded: unexpected rule %q", r)
		}
	}
	if untagged != 1 || tagged != trunks {
		t.Errorf("VLAN 8 guarded: %d untagged rules (want 1), %d tagged (want %d)", untagged, tagged, trunks)
	}
}
