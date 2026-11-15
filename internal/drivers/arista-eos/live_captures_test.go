package aristaeos

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The live runs of 2026-11-14 on the Arista (docs/drivers/arista-eos.md §8,
// §9.6, §9.7): each capture below was read from the switch during the run.

// Under Quiet (`environment fan-speed override 30`, applied by the deployed
// bridge): `show system environment cooling` reports coolingMode manual and
// overrideFanSpeed 30. The driver reads that back as Quiet, so Quiet again
// writes nothing and Default writes `no environment fan-speed`, the batch
// the deployed bridge ran in the round trip.
func TestFanModeQuietFromOverrideCapture(t *testing.T) {
	ft := NewFixtureTransport(aclFixtureDir)
	ft.Overrides = map[string]string{"show system environment cooling": "show-system-environment-cooling-override30.json"}
	c := NewCollector(ft)
	ctx := context.Background()
	snap, err := c.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.System.FanMode != devicemodel.FanModeQuiet {
		t.Fatalf("fan mode = %q, want quiet", snap.System.FanMode)
	}
	if err := c.ApplyFanMode(ctx, devicemodel.FanModeQuiet); err != nil || len(ft.Configured) != 0 {
		t.Fatalf("quiet on a quiet switch: err %v, wrote %q", err, ft.Configured)
	}
	if err := c.ApplyFanMode(ctx, devicemodel.FanModeDefault); err != nil {
		t.Fatal(err)
	}
	want := []string{"enable", "configure", "no environment fan-speed", "end", "write memory"}
	if len(ft.Configured) != 1 || !reflect.DeepEqual(ft.Configured[0], want) {
		t.Fatalf("default wrote %q, want %q", ft.Configured, want)
	}

	// What the round trip read back after Default: automatic, no override.
	var after struct {
		CoolingMode      string `json:"coolingMode"`
		OverrideFanSpeed int    `json:"overrideFanSpeed"`
	}
	b, err := os.ReadFile(filepath.Join(aclFixtureDir, "show-system-environment-cooling-default-20261114-after-quiet.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &after); err != nil || after.CoolingMode != "automatic" || after.OverrideFanSpeed != 0 {
		t.Errorf("after Default: %+v %v", after, err)
	}
}

// A storm caught live: unknown-unicast storm control at 0.01 % on the
// uplink and test frames to an address only the switch did not know; the
// drop count is the frames above the level, and it lands in the port's
// unicast storm counter (anomalies bit 4, which raised the controller's
// "Unicast Storm Detected" alert through the deployed bridge).
func TestStormControlDropsCapturedLive(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(aclFixtureDir, "storm-control-drops", "show-storm-control.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sc showStormControl
	if err := json.Unmarshal(b, &sc); err != nil {
		t.Fatal(err)
	}
	ports := []devicemodel.Port{{Index: 1}}
	applyStormControl(ports, sc, parseEthName)
	h, s := ports[0].Health, ports[0].StormCtrl
	if h.StormDropsUnicast != 2230 || h.StormDropsBroadcast != 0 || h.StormDropsMulticast != 0 {
		t.Errorf("drops = %d/%d/%d, want 0/0/2230", h.StormDropsBroadcast, h.StormDropsMulticast, h.StormDropsUnicast)
	}
	if s == nil || !pctEqual(s.UnknownUnicastPct, f64(0.01)) || s.BroadcastPct != nil || s.MulticastPct != nil {
		t.Errorf("storm = %+v", s)
	}
}

// The guard on the uplink with real frames: five replies from the trusted
// server and seven from an untrusted address, sent to the switch's own
// address. The trusted entry counted 5 (all five reached the CPU), the
// guard's drop counted 7 (none did): the drop is the port's rogue count.
func TestGuardDropsCapturedLive(t *testing.T) {
	var l showIPACLs
	readACLFixture(t, "show-ip-access-lists-DUI_IP_ACL_Ethernet1-guard-drops.json", &l)
	if n, ok := guardDrops(l, "DUI_IP_ACL_Ethernet1"); !ok || n != 7 {
		t.Errorf("guard drops %d, %v; want 7", n, ok)
	}
	s := l.ACLList[0].Sequence
	if len(s) < 3 || s[1].Action != "permit" || s[1].CounterData.PacketCount != 5 || !strings.Contains(s[1].Text, "eq bootps any eq bootpc") {
		t.Errorf("trusted entry %+v", s)
	}

	// The same run's summary: the driver's list on each of 57 interfaces,
	// every one active where it is configured, counted in full.
	var sum showIPACLSummary
	readACLFixture(t, "show-ip-access-lists-summary-guard57.json", &sum)
	st := aclState{lists: map[string]aclList{}}
	for _, a := range sum.ACLList {
		if strings.HasPrefix(a.Name, aclPrefix) {
			st.lists[a.Name] = aclList{}
		}
	}
	if len(st.lists) != 57 {
		t.Fatalf("%d lists of the driver's, want 57", len(st.lists))
	}
	if s := aclStatus(sum, st); s != "" {
		t.Errorf("status %q", s)
	}
}
