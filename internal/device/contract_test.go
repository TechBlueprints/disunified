package device

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	apcbackups "github.com/TechBlueprints/disunified/internal/drivers/apc-backups"
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

// comparePortTypes holds every key that appears on any row of a table
// (port_table) in the reference and on any row of ours to the reference's
// JSON types for it. compareKeys sees two ports; a key that only a port with
// a seated optic carries (the sfp_* readings, sent as numbers until
// 2026-11-12) needs every port.
func comparePortTypes(t *testing.T, what string, real, ours map[string]any) {
	t.Helper()
	compareRowTypes(t, what, real, ours, "port_table")
}

func compareRowTypes(t *testing.T, what string, real, ours map[string]any, table string) {
	t.Helper()
	rowTypes := func(m map[string]any) map[string]map[string]bool {
		out := map[string]map[string]bool{}
		rows, _ := m[table].([]any)
		for _, r := range rows {
			row, _ := r.(map[string]any)
			for k, v := range row {
				if jt := jsonType(v); jt != "null" {
					if out[k] == nil {
						out[k] = map[string]bool{}
					}
					out[k][jt] = true
				}
			}
		}
		return out
	}
	rt, ot := rowTypes(real), rowTypes(ours)
	if len(rt) == 0 || len(ot) == 0 {
		t.Fatalf("%s: no %s rows to compare (real %d keys, ours %d)", what, table, len(rt), len(ot))
	}
	var typed []string
	for k, types := range ot {
		want, shared := rt[k]
		if !shared {
			continue // a key no real row carries is compareKeys' business
		}
		for jt := range types {
			if !want[jt] {
				typed = append(typed, fmt.Sprintf("%s (real %s, ours %s)", k, strings.Join(sortedKeys(want), "/"), jt))
			}
		}
	}
	sort.Strings(typed)
	if len(typed) > 0 {
		t.Errorf("%s: %s keys with a JSON type no real row has, on some row: %v", what, table, typed)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
// hid for a day. The types on every port are TestWireContractEveryPort's. extraOmissions are the driver's own gaps, with reasons.
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
	checkSerial(t, driver, ours)
}

// checkSerial holds a payload's serial to a real UniFi device's: the MAC's
// 12 hex digits, upper case (MaxSerialLen; a longer one halts Network 11's
// migration).
func checkSerial(t *testing.T, driver string, ours map[string]any) {
	t.Helper()
	mac, _ := ours["mac"].(string)
	if want := strings.ToUpper(strings.ReplaceAll(mac, ":", "")); mac == "" || ours["serial"] != want {
		t.Errorf("%s: serial %q, want the MAC's %s", driver, ours["serial"], want)
	}
	// hash_id is 16 hex digits on every real device; the controller kept
	// none of ours while it carried a UUID's dashes (hashID).
	if h, _ := ours["hash_id"].(string); !hashIDForm.MatchString(h) {
		t.Errorf("%s: hash_id %q, want 16 hex digits", driver, h)
	}
}

var hashIDForm = regexp.MustCompile(`^[0-9a-f]{16}$`)

// contractPayload is one driver's contract payload and the class of real
// device it is held to.
type contractPayload struct {
	driver string
	power  bool // a power device: held to the USP-PDU-Pro, not the switches
	build  func(t *testing.T) map[string]any
}

// contractPayloads are every driver's contract payloads, built by the real
// drivers from their real captures (the contract_<driver>_test.go helpers).
var contractPayloads = []contractPayload{
	{"arista-eos", false, buildContractPayload},
	{"proxmox", false, buildProxmoxContractPayload},
	{"podman", false, buildPodmanContractPayload},
	{"sonic", false, buildSonicContractPayload},
	{"sonic-hw", false, func(t *testing.T) map[string]any {
		m, _ := sonicCapturePayload(t, "sonic-202505.1238526-ff3c0c9f6/collect-hwtables.txt", "USWF07D")
		return m
	}},
	{"sonic-l2", false, func(t *testing.T) map[string]any {
		m, _ := sonicCapturePayload(t, "sonic-202505.1238526-ff3c0c9f6/collect-l2-running.txt", "USWF07D")
		return m
	}},
	{"apc-pdu", true, buildPDUContractPayload},
	{"apc-ups", true, buildUPSContractPayload},
	{"apc-backups", true, buildBackUPSContractPayload},
}

// buildBackUPSContractPayload is TestWireContractBackUPS's payload: the
// Back-UPS Pro driver over the card's real pages (docs/fixtures/apc-backups-05.3).
func buildBackUPSContractPayload(t *testing.T) map[string]any {
	t.Helper()
	fr, err := apcbackups.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "apc-backups-05.3"))
	if err != nil {
		t.Fatal(err)
	}
	c := apcbackups.NewCollector(fr)
	c.Addr, c.MAC, c.Netmask, c.GatewayMAC = "192.0.2.30", "02:00:00:00:00:01", "255.255.255.0", "02:00:00:00:00:fe"
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := DescriptorFor("USPDA2B", snap, Identity{MAC: snap.System.MAC, IP: snap.System.Addresses[0].IP, Hostname: "ups-2", UDAPIVersion: "1.0.0"})
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

// powerReferences are the real power-device informs a power payload is held to.
var powerReferences = []string{"controller-10.6.106/inform-usp-pdu-pro-usppdup.json"}

// TestWireContractEveryPort: for every key on any port of a reference
// inform and on any port of a driver's payload, the JSON types agree (and
// the same for a power device's outlet rows). The uplink-and-one-down-port
// comparison of runContract missed the optic readings for two months: only
// a port with a seated, DOM-capable optic carries them.
func TestWireContractEveryPort(t *testing.T) {
	for _, d := range contractPayloads {
		t.Run(d.driver, func(t *testing.T) {
			ours := d.build(t)
			refs := powerReferences
			if !d.power {
				refs = nil
				for _, r := range contractReferences {
					refs = append(refs, r.path)
				}
			}
			for _, ref := range refs {
				real := loadFixtureJSON(t, ref)
				comparePortTypes(t, d.driver+" "+ref+" every port", real, ours)
				if d.power {
					compareRowTypes(t, d.driver+" "+ref+" every outlet", real, ours, "outlet_table")
				}
			}
		})
	}
}
