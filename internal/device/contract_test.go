package device

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/drivers/aristaeos"
)

// The wire contract: what we send must have the shape of what a real UniFi
// switch sends. The references are real informs captured from this
// controller (docs/fixtures/controller-10.6.106/inform-*.json, identifiers
// and secrets replaced by scripts/sanitize-captures.py); our payload is
// built by the real driver from the real EOS captures. Every key a real
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
	"hw_caps":     "unknown bit vocabulary; claiming bits we cannot honour is worse than omitting",
	"fw2_caps":    "unknown bit vocabulary",
	"fw3_caps":    "unknown bit vocabulary",
	"routing_mac": "no L3 routing MAC (switch-only)",
	"service_mac": "reported only while the OOB port is unplugged (see serviceMAC)",
	"ipv6":        "no IPv6 management address on the reference switch",
	// Controller-side or model-specific.
	"cfgversion_effective":               "USW-only",
	"cfgversion_saved":                   "USW-only",
	"igmp_snoop_table":                   "USW-only",
	"pathmon":                            "USW-only path monitor",
	"stats_inform_interval_avg":          "USW-only",
	"stp_last_topology_change_timestamp": "EOS 4.26 gives per-port change times, not a bridge-wide one",
}

var portOmissions = map[string]string{
	"sfp_rev":       "EOS does not expose the optic's hardware revision",
	"port_caps":     "USW-only bitmask with an unknown vocabulary",
	"stp_bridge_id": "USW-only; which bridge id it names is not established",
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

func buildContractPayload(t *testing.T) map[string]any {
	t.Helper()
	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "eos-4.26.14M"))
	c := aristaeos.NewCollector(ft)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ip := "192.0.2.9"
	if len(snap.System.Addresses) > 0 {
		ip = snap.System.Addresses[0].IP // the switch's own address, so netmask resolves
	}
	desc, err := DescriptorFor("UDC48X6", snap, Identity{MAC: snap.System.MAC, Serial: snap.System.Serial, IP: ip, Hostname: "arista", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetSnapshot(snap)
	sess.SetGatewayIP("192.0.2.1")
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func loadReference(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106", name))
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

func TestWireContractAgainstRealInforms(t *testing.T) {
	ours := buildContractPayload(t)
	for _, ref := range []string{"inform-ecs-aggregation-uswf066.json", "inform-usw-xg16-usxg.json"} {
		real := loadReference(t, ref)
		compareKeys(t, ref+" device", real, ours, contractOmissions)
		compareKeys(t, ref+" uplink port", uplinkPort(t, real), uplinkPort(t, ours), portOmissions)
		rd := downPort(t, real, false)
		_, optical := rd["sfp_found"]
		compareKeys(t, ref+" down port", rd, downPort(t, ours, optical), portOmissions)
	}
	// The one that hid for a day: uplink is the NAME of the if_table interface.
	if u, ok := ours["uplink"].(string); !ok || u == "" {
		t.Errorf("uplink must be the management interface name (a string), got %v", ours["uplink"])
	}
}
