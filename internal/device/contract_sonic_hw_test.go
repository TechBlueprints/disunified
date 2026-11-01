package device

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// The DX010 against UniFi's own SONiC switch (docs/drivers/sonic.md §11):
// the hardware capture with collect.py's later sections
// (sonic-202505.1238526-ff3c0c9f6/collect-hwtables.txt), claimed as the
// ECS Core, against the ECS Aggregation's inform on Network 10.6.109
// (mca-dump on the switch, scrubbed: controller-10.6.109/
// inform-ecs-aggregation-uswf066.json). Its aggregates are checked apart,
// on the ECS's own SONiC tables (TestWireContractSonicAggregate): the
// DX010 has one cable.
var sonicHWOmissions = map[string]string{
	"root_switch":  "no spanning tree runs on the DX010 yet (§11.2: RSTP is its own work)",
	"stp_priority": "no spanning tree runs on the DX010 yet",
}

// sonicHWPSUOmissions: the one key of the ECS's supply row with no source.
var sonicHWPSUOmissions = map[string]string{
	"psu_type": "an unnamed vocabulary (131072 on the ECS's 550 W supply); nothing says what a DX010 supply would be",
}

const viaVS = "sonic-202505.1238524-ff3c0c9f6"

func loadReference109(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", name))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// sonicCapturePayload runs the sonic collector over a capture (with the
// VS capture's help texts and zones: both 202505 builds are commit
// ff3c0c9f6) and renders it claimed as model.
func sonicCapturePayload(t *testing.T, capture, model string) (map[string]any, *devicemodel.Snapshot) {
	t.Helper()
	c, snap := sonicCaptureSnapshot(t, capture)
	return sonicRender(t, c, snap, model), snap
}

func sonicCaptureSnapshot(t *testing.T, capture string) (*sonic.Collector, *devicemodel.Snapshot) {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", capture))
	if err != nil {
		t.Fatal(err)
	}
	vs, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", viaVS, "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fr.Help, fr.Zones = vs.Help, vs.Zones
	c := sonic.NewCollector(fr)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, snap
}

// sonicRender is the payload a session sends for snap, claimed as model.
func sonicRender(t *testing.T, c *sonic.Collector, snap *devicemodel.Snapshot, model string) map[string]any {
	t.Helper()
	ip := snap.System.Addresses[0].IP
	desc, err := DescriptorFor(model, snap, Identity{MAC: snap.System.MAC, Serial: snap.System.Serial, IP: ip, Hostname: snap.System.Hostname, UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	desc.FWCaps = FWCapsFor(c.Capabilities())
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetCapabilities(c.Capabilities())
	sess.SetSnapshot(snap)
	sess.SetGatewayIP("192.0.2.2")
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func row(t *testing.T, m map[string]any, table string, i int) map[string]any {
	t.Helper()
	rows, _ := m[table].([]any)
	if len(rows) <= i {
		t.Fatalf("%s has %d rows", table, len(rows))
	}
	return rows[i].(map[string]any)
}

func portEntry(t *testing.T, m map[string]any, idx int) map[string]any {
	t.Helper()
	for _, e := range m["port_table"].([]any) {
		p := e.(map[string]any)
		if n, _ := p["port_idx"].(float64); int(n) == idx {
			return p
		}
	}
	t.Fatalf("no port %d", idx)
	return nil
}

func TestWireContractSonicHardware(t *testing.T) {
	ours, _ := sonicCapturePayload(t, "sonic-202505.1238526-ff3c0c9f6/collect-hwtables.txt", "USWF07D")
	real := loadReference109(t, "inform-ecs-aggregation-uswf066.json")
	merge := mergeOmissions
	omit := merge(contractOmissions, switchOmissions, sonicHWOmissions)
	delete(omit, "ipv6")
	compareKeys(t, "sonic hardware device", real, ours, omit)
	compareKeys(t, "sonic hardware uplink port", uplinkPort(t, real), uplinkPort(t, ours), merge(portOmissions, aggregateUplinkOmissions))
	compareKeys(t, "sonic hardware down port", downPort(t, real, true), downPort(t, ours, true), merge(portOmissions, sonicDownPortOmissions))
	compareKeys(t, "sonic hardware psu row", row(t, real, "psu_table", 0), row(t, ours, "psu_table", 0), sonicHWPSUOmissions)
	compareKeys(t, "sonic hardware fan row", row(t, real, "fan_table", 0), row(t, ours, "fan_table", 0), nil)

	// The values behind those keys, from the capture.
	psu := row(t, ours, "psu_table", 0)
	if psu["power"] != 78.5 || psu["maxwatt"] != 900.0 || psu["psu_caps"] != 4.0 || psu["psu_eeprom_valid"] != true ||
		!reflect.DeepEqual(psu["fan"], map[string]any{"0": 5952.0}) || !reflect.DeepEqual(psu["temp"], map[string]any{"0": 28.0, "1": 48.0, "2": 38.0}) {
		t.Errorf("PSU 1 row %v: want the chip's input power 78.5 W, its fan RPM and three temperatures, 900 W, hot-swappable", psu)
	}
	if ours["total_max_power"] != 153.0 || ours["power_consumption"] != 153.0 {
		t.Errorf("total_max_power %v power_consumption %v, want the supplies' 78.5 + 74.5 W drawn, as the ECS sends", ours["total_max_power"], ours["power_consumption"])
	}
	if fans, _ := ours["fan_table"].([]any); len(fans) != 5 {
		t.Errorf("fan_table %v, want the five drawers", fans)
	}
	if f := row(t, ours, "fan_table", 0); f["label"] != "Fan1" || f["direction"] != "intake" || f["present"] != true {
		t.Errorf("fan row %v", f)
	}
	want := map[string]any{"capacity": 8196.0, "count": 152.0, "threshold_warning": 5409.0, "threshold_critical": 6474.0}
	if !reflect.DeepEqual(ours["mac_table_capability"], want) {
		t.Errorf("mac_table_capability %v, want CRM's %v", ours["mac_table_capability"], want)
	}
	if !reflect.DeepEqual(ours["ipv6"], []any{"fe80::2/64"}) {
		t.Errorf("ipv6 %v", ours["ipv6"])
	}
	if ours["displayable_version"] != "202505.1238526-ff3c0c9f6" {
		t.Errorf("displayable_version %v", ours["displayable_version"])
	}
	if p := portEntry(t, ours, 1); p["sfp_rev"] != "1" {
		t.Errorf("port 1 sfp_rev %v", p["sfp_rev"])
	}
	if p := portEntry(t, ours, 2); p["sfp_rev"] != "" {
		t.Errorf("empty cage sfp_rev %v, want \"\" as the ECS sends", p["sfp_rev"])
	}
}

// An aggregate on the DX010 as UniFi's own SONiC switch reports one. The
// DX010 has had one cable, so no capture holds a PortChannel: its own
// capture's first two cages go into PortChannel1 at the model level, as the
// planned core aggregate stands before its second cable -- cage 1 bundled
// with its LLDP neighbour (the core) as the LACP partner, cage 2 a member
// that is not -- and the payload is held to the ECS Aggregation's inform:
// the aggregate's head (its port 49) key for key, the aggregate keys on
// the member (its port 50), the lacp_state rows' own keys and types, and
// none of them on a port in no aggregate. How the driver reads teamd's
// records is hwtables_test.go's (upstream SONiC's own test data).
func TestWireContractSonicAggregate(t *testing.T) {
	c, snap := sonicCaptureSnapshot(t, "sonic-202505.1238526-ff3c0c9f6/collect-hwtables.txt")
	if snap.Ports[0].Neighbor == nil {
		t.Fatal("cage 1 has no LLDP neighbour in the capture")
	}
	partner := snap.Ports[0].Neighbor.ChassisID
	for i, bundled := range []bool{true, false} {
		p := &snap.Ports[i]
		p.LAG, p.LAGID = "PortChannel1", 1
		p.LACP = &devicemodel.LACP{Active: bundled}
		if bundled {
			p.LACP.PartnerSystem = partner
		}
	}
	ours := sonicRender(t, c, snap, "USWF07D")
	real := loadReference109(t, "inform-ecs-aggregation-uswf066.json")
	rh, rm := portEntry(t, real, 49), portEntry(t, real, 50)
	oh, om := portEntry(t, ours, 1), portEntry(t, ours, 2)

	compareKeys(t, "sonic aggregate head", rh, oh, portOmissions)
	aggKeys := []string{"lag_member", "aggregate_num_ports", "lacp_state", "partner_system_id", "service_mac_table"}
	for _, k := range aggKeys {
		if _, r := rm[k]; r != (om[k] != nil) {
			t.Errorf("aggregate member %s: the ECS sends it %v, ours %v", k, r, om[k] != nil)
		}
		if _, r := portEntry(t, real, 1)[k]; r {
			t.Errorf("the ECS's port 1 (no aggregate) sends %s", k)
		}
		if _, o := portEntry(t, ours, 3)[k]; o {
			t.Errorf("our port 3 (no aggregate) sends %s", k)
		}
	}
	rows, _ := oh["lacp_state"].([]any)
	refRow := rh["lacp_state"].([]any)[0].(map[string]any)
	for _, r := range rows {
		compareKeys(t, "sonic lacp_state row", refRow, r.(map[string]any), nil)
		compareKeys(t, "sonic lacp_state row (reverse)", r.(map[string]any), refRow, nil)
	}

	// The values, from the capture and the model's aggregate.
	wantRows := []any{
		map[string]any{"active": true, "member_port": 1.0, "speed": float64(snap.Ports[0].SpeedMbps)},
		map[string]any{"active": false, "member_port": 2.0, "speed": float64(snap.Ports[1].SpeedMbps)},
	}
	if !reflect.DeepEqual(oh["lacp_state"], wantRows) || oh["aggregate_num_ports"] != 2.0 || oh["partner_system_id"] != partner ||
		oh["lag_member"] != true || om["lag_member"] != true || !reflect.DeepEqual(oh["service_mac_table"], []any{}) {
		t.Errorf("head %v / member lag_member %v", map[string]any{"lacp_state": oh["lacp_state"], "aggregate_num_ports": oh["aggregate_num_ports"],
			"partner_system_id": oh["partner_system_id"], "lag_member": oh["lag_member"], "service_mac_table": oh["service_mac_table"]}, om["lag_member"])
	}
}
