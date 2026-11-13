package device

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
)

// The MAC table's two times (docs/mac-table-times.md): `uptime` is the
// seconds since the address was learned on the port, `age` since a frame
// from it was last seen, so uptime >= age. A shape check cannot see a
// value of the right type and range with the wrong meaning; these tests
// hold the relation.

// macTableEntries is every mac_table entry of an inform: the per-port
// tables and the top-level one, each with where it came from.
func macTableEntries(m map[string]any) (where []string, entries []map[string]any) {
	pt, _ := m["port_table"].([]any)
	for _, p := range pt {
		pe, _ := p.(map[string]any)
		mt, _ := pe["mac_table"].([]any)
		for _, e := range mt {
			where = append(where, fmt.Sprintf("port %v", pe["port_idx"]))
			entries = append(entries, e.(map[string]any))
		}
	}
	top, _ := m["mac_table"].([]any)
	for _, e := range top {
		where = append(where, "mac_table")
		entries = append(entries, e.(map[string]any))
	}
	return where, entries
}

// checkMACTimes holds every entry to a real switch's: whole seconds, never
// negative, uptime >= age; and a top-level entry reports the same pair as
// the same entry in its port's table.
func checkMACTimes(t *testing.T, what string, m map[string]any) int {
	t.Helper()
	where, entries := macTableEntries(m)
	perPort := map[string][2]float64{}
	pt, _ := m["port_table"].([]any)
	for _, p := range pt {
		pe, _ := p.(map[string]any)
		mt, _ := pe["mac_table"].([]any)
		for _, e := range mt {
			e := e.(map[string]any)
			perPort[fmt.Sprintf("%v/%v/%v", e["mac"], e["vlan"], pe["port_idx"])] = [2]float64{e["age"].(float64), e["uptime"].(float64)}
		}
	}
	for i, e := range entries {
		age, aok := e["age"].(float64)
		up, uok := e["uptime"].(float64)
		if !aok || !uok || age < 0 || up < 0 || age != float64(int64(age)) || up != float64(int64(up)) {
			t.Errorf("%s %s: %v: age %v, uptime %v; want whole non-negative seconds", what, where[i], e["mac"], e["age"], e["uptime"])
			continue
		}
		if up < age {
			t.Errorf("%s %s: %v: uptime %v < age %v", what, where[i], e["mac"], up, age)
		}
		if where[i] == "mac_table" {
			k := fmt.Sprintf("%v/%v/%v", e["mac"], e["vlan"], e["port_idx"])
			if pp, ok := perPort[k]; ok && (pp[0] != age || pp[1] != up) {
				t.Errorf("%s: %s: top-level age/uptime %v/%v, port table %v/%v", what, k, age, up, pp[0], pp[1])
			}
		}
	}
	return len(entries)
}

// Every entry of the real informs that carry a MAC table holds the
// relation (233 entries; six learned once and silent since, age = uptime).
func TestMACTimesRealInforms(t *testing.T) {
	total := 0
	for _, f := range []string{
		"controller-10.6.106/inform-ecs-aggregation-uswf066.json",
		"controller-10.6.106/inform-usw-xg16-usxg.json",
		"controller-10.6.109/inform-ecs-aggregation-uswf066.json",
	} {
		m := loadFixtureJSON(t, filepath.Join("..", "..", "docs", "fixtures", f))
		n := checkMACTimes(t, f, m)
		if n == 0 {
			t.Errorf("%s: no mac_table entries", f)
		}
		total += n
	}
	if total != 233 {
		t.Errorf("%d entries in the real informs, want 233", total)
	}
}

// Every driver's contract payload holds the same relation.
func TestMACTimesEveryDriver(t *testing.T) {
	for _, c := range contractPayloads {
		checkMACTimes(t, c.driver, c.build(t))
	}
}

// macPayload renders snap the way the contract tests do.
func macPayload(t *testing.T, model string, snap *devicemodel.Snapshot) map[string]any {
	t.Helper()
	desc, err := DescriptorFor(model, snap, Identity{MAC: snap.System.MAC, IP: "192.0.2.9", Hostname: "dut", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", State{Key: "0123456789abcdef0123456789abcdef", Adopted: true}, nil, time.Now())
	sess.SetSnapshot(snap)
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The Arista: uptime is the seconds since EOS's lastMove for the entry, age
// is 0 (EOS 4.26's table carries no last-seen time), from the capture.
func TestMACTimesArista(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M")
	var mt struct {
		UnicastTable struct {
			Entries []struct {
				MAC      string  `json:"macAddress"`
				VLAN     int     `json:"vlanId"`
				LastMove float64 `json:"lastMove"`
			} `json:"tableEntries"`
		} `json:"unicastTable"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "show-mac-address-table.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &mt); err != nil {
		t.Fatal(err)
	}
	lastMove := map[string]int64{}
	for _, e := range mt.UnicastTable.Entries {
		lastMove[fmt.Sprintf("%s/%d", strings.ToLower(e.MAC), e.VLAN)] = int64(e.LastMove)
	}
	snap, err := aristaeos.NewCollector(aristaeos.NewFixtureTransport(dir)).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := macPayload(t, "UDC48X6", snap)
	_, entries := macTableEntries(m)
	if len(entries) == 0 {
		t.Fatal("no mac_table entries")
	}
	for _, e := range entries {
		k := fmt.Sprintf("%v/%v", e["mac"], e["vlan"])
		lm, ok := lastMove[k]
		if !ok {
			t.Errorf("%s: not in the capture", k)
			continue
		}
		if want := snap.TakenAt.Unix() - lm; int64(e["uptime"].(float64)) != want {
			t.Errorf("%s: uptime %v, want %d (since lastMove)", k, e["uptime"], want)
		}
		if e["age"].(float64) != 0 {
			t.Errorf("%s: age %v, want 0", k, e["age"])
		}
	}
}
