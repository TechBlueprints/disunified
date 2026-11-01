package device

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The wire contract: what we send must have the shape of what a real UniFi
// switch sends. The references are real informs captured from this
// controller (docs/fixtures/controller-10.6.106/inform-*.json, and the ECS
// Aggregation's 10.6.109 inform printed on the switch: contractReferences;
// identifiers and secrets replaced by scripts/sanitize-controller.py); each driver's
// payload is built by the real driver from its real captures, in
// contract_<driver>_test.go, and checked by runContract. Every key a real
// switch reports must be present with the same JSON type, at the device
// level and on the uplink port entry, unless it is listed below with the
// reason it is deliberately absent. A key missing from this list fails the
// test — that is the point: the `uplink`-as-object bug (2026-09-20) would
// have failed here.
var contractOmissions = map[string]string{
	// Hardware/firmware features this switch class does not have.
	"ble_caps":        "no Bluetooth radio",
	"has_speaker":     "no speaker",
	"etherlight_mode": "Etherlighting is a UniFi hardware feature",
	"guest_kicks":     "guest portal enforcement is not implemented",
	"guest_token":     "guest portal enforcement is not implemented",
	// Firmware identity we do not fake.
	"bomrev":      "UniFi board revision string",
	"bomrev_id":   "UniFi board revision id",
	"sysid":       "UniFi hardware system id",
	"fw2_caps":    "unknown bit vocabulary",
	"fw3_caps":    "unknown bit vocabulary",
	"routing_mac": "no L3 routing MAC (switch-only)",
	"ipv6":        "the driver does not read the management interface's IPv6 addresses (sonic does, and drops this)",
	"service_mac": "reported only while the OOB port is unplugged (see serviceMAC)",
	// Controller-side or model-specific.
	"cfgversion_effective":               "USW-only",
	"cfgversion_saved":                   "USW-only",
	"igmp_snoop_table":                   "USW-only",
	"pathmon":                            "USW-only path monitor",
	"stats_inform_interval_avg":          "USW-only",
	"stp_last_topology_change_timestamp": "EOS 4.26 gives per-port change times, not a bridge-wide one",
}

// switchOmissions are gaps only a bridged switch has. hw_caps names hardware
// the device physically has -- screen, LED bar, LCM, RPS, speaker, outlets
// (unifi-emu's capability_bits.json; every real USP-PDU-Pro sends 136 = outlets
// + LCM). A power device claims its outlet bit, because without it the
// controller discards the outlet table; neither bridged switch has any of
// those parts, so they send nothing rather than a claim they cannot honour.
var switchOmissions = map[string]string{
	"hw_caps": "the switch has none of the hardware the bits name (screen, LED bar, LCM, RPS, speaker, outlets)",
}

var portOmissions = map[string]string{
	"port_caps":     "USW-only bitmask with an unknown vocabulary",
	"stp_bridge_id": "USW-only; which bridge id it names is not established",
}

// contractReferences are the real switch informs every switch payload is
// held to. The ECS Aggregation's 10.6.109 inform was printed by mca-dump on
// the switch itself (the first from a SONiC-based UniFi switch read there)
// and its uplink is an aggregate: an uplink that is one link is excused the
// aggregate's keys, which are held to it on an aggregate of our own
// (TestWireContractSonicAggregate).
var contractReferences = []struct {
	path       string
	uplinkOmit map[string]string
}{
	{"controller-10.6.106/inform-ecs-aggregation-uswf066.json", nil},
	{"controller-10.6.106/inform-usw-xg16-usxg.json", nil},
	{"controller-10.6.109/inform-ecs-aggregation-uswf066.json", aggregateUplinkOmissions},
}

var aggregateUplinkOmissions = map[string]string{
	"aggregate_num_ports": "the reference's uplink is an aggregate's head; ours is one link",
	"lacp_state":          "the reference's uplink is an aggregate's head; ours is one link",
	"lag_member":          "the reference's uplink is an aggregate member; ours is one link",
	"partner_system_id":   "the reference's uplink is an aggregate's head; ours is one link",
	"service_mac_table":   "sent on an aggregate's head only",
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func loadReference(t *testing.T, name string) map[string]any {
	t.Helper()
	return loadFixtureJSON(t, filepath.Join("controller-10.6.106", name))
}

// loadFixtureJSON reads a JSON fixture by its path under docs/fixtures.
func loadFixtureJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", path))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func uplinkPort(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	for _, e := range m["port_table"].([]any) {
		p := e.(map[string]any)
		if up, _ := p["is_uplink"].(bool); up {
			return p
		}
	}
	t.Fatal("no is_uplink port")
	return nil
}

// downPort returns the first down port; when optical is set, the first down
// port with an optic slot (real switches send sfp_found only for those), so
// an SFP-only real switch is compared with one of our cages, not a copper port.
func downPort(t *testing.T, m map[string]any, optical bool) map[string]any {
	t.Helper()
	for _, e := range m["port_table"].([]any) {
		p := e.(map[string]any)
		up, _ := p["up"].(bool)
		_, slot := p["sfp_found"]
		if !up && (!optical || slot) {
			return p
		}
	}
	t.Fatal("no matching down port")
	return nil
}

func compareKeys(t *testing.T, what string, real, ours map[string]any, omit map[string]string) {
	t.Helper()
	var missing, typed []string
	for k, rv := range real {
		if len(k) > 0 && k[0] == '_' {
			continue // transport fields the controller adds
		}
		ov, ok := ours[k]
		if !ok {
			if _, allowed := omit[k]; !allowed {
				missing = append(missing, k)
			}
			continue
		}
		if rt, ot := jsonType(rv), jsonType(ov); rt != ot && rt != "null" && ot != "null" {
			typed = append(typed, fmt.Sprintf("%s (real %s, ours %s)", k, rt, ot))
		}
	}
	sort.Strings(missing)
	sort.Strings(typed)
	if len(missing) > 0 {
		t.Errorf("%s: keys a real switch sends that we do not (add them or list them in the omissions with a reason): %v", what, missing)
	}
	if len(typed) > 0 {
		t.Errorf("%s: keys with a different JSON type than a real switch: %v", what, typed)
	}
	for k := range omit {
		if _, ok := ours[k]; ok {
			t.Errorf("%s: %q is listed as omitted but we send it — drop it from the list", what, k)
		}
	}
}

func mergeOmissions(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// runContract checks one driver's payload against every reference inform:
// the device level, the uplink port and a down port, plus the keys that
// hid for a day. extraOmissions are the driver's own gaps, with reasons.
func runContract(t *testing.T, driver string, ours map[string]any, extraOmissions map[string]string) {
	t.Helper()
	runContractPorts(t, driver, ours, extraOmissions, nil)
}

// runContractPorts is runContract with per-driver port-level omissions
// too (a key a real switch's port carries that this device's port cannot,
// with the reason).
func runContractPorts(t *testing.T, driver string, ours map[string]any, extraOmissions, extraPortOmissions map[string]string) {
	t.Helper()
	portOmit := map[string]string{}
	for k, v := range portOmissions {
		portOmit[k] = v
	}
	for k, v := range extraPortOmissions {
		portOmit[k] = v
	}
	omit := map[string]string{}
	for k, v := range contractOmissions {
		omit[k] = v
	}
	for k, v := range switchOmissions {
		omit[k] = v
	}
	for k, v := range extraOmissions {
		omit[k] = v
	}
	for _, ref := range contractReferences {
		real := loadFixtureJSON(t, ref.path)
		compareKeys(t, driver+" "+ref.path+" device", real, ours, omit)
		compareKeys(t, driver+" "+ref.path+" uplink port", uplinkPort(t, real), uplinkPort(t, ours), mergeOmissions(portOmit, ref.uplinkOmit))
		rd := downPort(t, real, false)
		_, optical := rd["sfp_found"]
		compareKeys(t, driver+" "+ref.path+" down port", rd, downPort(t, ours, optical), portOmit)
	}
	// The one that hid for a day: uplink is the NAME of the if_table interface.
	if u, ok := ours["uplink"].(string); !ok || u == "" {
		t.Errorf("%s: uplink must be the management interface name (a string), got %v", driver, ours["uplink"])
	}
}
