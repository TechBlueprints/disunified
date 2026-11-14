package aristaeos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

func readACLFixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(aclFixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// A collect on the switch carrying the driver's two lists (captured live)
// reads their state and counters in one batch after the poll, finds them
// active, and reports each test port's guard count (0: nothing was sent
// on either) with every other port unreported.
func TestCollectReadsTheDriversLists(t *testing.T) {
	c, ft := duiACLCollector(t)
	var batch []string
	for _, r := range ft.Runs {
		if len(r) > 1 && r[1] == "show ip access-lists summary" {
			batch = r
		}
	}
	want := "enable / show ip access-lists summary / show ip access-lists DUI_IP_ACL_Ethernet2 / show ip access-lists DUI_IP_ACL_Ethernet54-1"
	if strings.Join(batch, " / ") != want {
		t.Errorf("read-back batch %v", batch)
	}
	if c.aclStatus != "" {
		t.Errorf("status %q: both lists are active where they are bound in the capture", c.aclStatus)
	}
	for _, p := range c.last.Ports {
		if p.Health.RogueDHCPDrops != 0 {
			t.Errorf("port %d: %d", p.Index, p.Health.RogueDHCPDrops)
		}
	}
}

// The captured switch without lists runs no read-back at all.
func TestCollectReadsNoListsWhenThereAreNone(t *testing.T) {
	_, ft := startedCollector(t)
	for _, r := range ft.runs {
		for _, cmd := range r {
			if strings.HasPrefix(cmd, "show ip access-lists") {
				t.Errorf("ran %q with no list of the driver's", cmd)
			}
		}
	}
}

// The counter field as 4.26.14M fills it, from the capture's own counted
// list (default-control-plane-acl, `counters per-entry`, real matches);
// an entry nothing matched has an empty counterData, a remark none.
func TestACLCountersFromTheCapture(t *testing.T) {
	var l showIPACLs
	readACLFixture(t, "show-ip-access-lists.json", &l)
	cp := l.ACLList[0]
	if cp.Name != "default-control-plane-acl" || !cp.CountersEnabled {
		t.Fatalf("first list %q counters %v", cp.Name, cp.CountersEnabled)
	}
	if cp.Sequence[0].CounterData.PacketCount != 1201 || cp.Sequence[1].CounterData.PacketCount != 118098 || cp.Sequence[2].CounterData.PacketCount != 0 {
		t.Errorf("counts %+v", cp.Sequence[:3])
	}
	if _, ok := guardDrops(l, "default-control-plane-acl"); ok {
		t.Error("a list without the driver's guard remark has no guard count")
	}

	var g showIPACLs
	readACLFixture(t, "show-ip-access-lists-DUI_IP_ACL_Ethernet2.json", &g)
	if n, ok := guardDrops(g, "DUI_IP_ACL_Ethernet2"); !ok || n != 0 {
		t.Errorf("guard drops %d, %v", n, ok)
	}
	s := g.ACLList[0].Sequence
	if len(s) != 6 || s[3].Action != "deny" || s[3].Text != "deny udp any eq bootps any eq bootpc" || s[0].Action != "" {
		t.Errorf("sequence %+v", s)
	}
}

// Counts reach their ports: a split cage's lanes are summed into it, an
// aggregate's list goes to its first member, a list bound twice to nobody.
func TestApplyGuardDrops(t *testing.T) {
	ports := []devicemodel.Port{
		{Index: 2, IfName: "Ethernet2"},
		{Index: 50, IfName: "Ethernet50/1", Interfaces: []string{"Ethernet50/1", "Ethernet50/2", "Ethernet50/3", "Ethernet50/4"}},
		{Index: 7, IfName: "Ethernet7", LAGID: 3},
		{Index: 5, IfName: "Ethernet5", LAGID: 3},
		{Index: 9, IfName: "Ethernet9"},
		{Index: 10, IfName: "Ethernet10"},
	}
	st := aclState{bound: map[string]string{
		"Ethernet2": aclListName("Ethernet2"), "Ethernet50/1": aclListName("Ethernet50/1"), "Ethernet50/3": aclListName("Ethernet50/3"),
		"Port-Channel3": aclListName("Port-Channel3"), "Ethernet9": "DUI_IP_ACL_shared", "Ethernet10": "DUI_IP_ACL_shared",
	}}
	applyGuardDrops(ports, st, map[string]uint64{
		aclListName("Ethernet2"): 4, aclListName("Ethernet50/1"): 1, aclListName("Ethernet50/3"): 2,
		aclListName("Port-Channel3"): 8, "DUI_IP_ACL_shared": 16,
	})
	got := map[int]uint64{}
	for _, p := range ports {
		got[p.Index] = p.Health.RogueDHCPDrops
	}
	want := map[int]uint64{2: 4, 50: 3, 5: 8, 7: 0, 9: 0, 10: 0}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("port %d: %d, want %d (%v)", k, got[k], v, got)
		}
	}
}

// What EOS 4.26.14M makes of a VLAN qualifier on this 7160 (captured
// 2026-11-14, DUI_PROBE on the test ports): the rule is stored with mask 0,
// any VLAN, and the summary flags the list as not fully supported by the
// hardware -- which the read-back reports.
func TestVLANQualifierIsDroppedOnThe7160(t *testing.T) {
	var raw struct {
		ACLList []struct {
			Name     string `json:"name"`
			Sequence []struct {
				Text       string `json:"text"`
				RuleFilter struct {
					VLAN struct {
						ID   int `json:"id"`
						Mask int `json:"mask"`
					} `json:"vlan"`
				} `json:"ruleFilter"`
			} `json:"sequence"`
		} `json:"aclList"`
	}
	readACLFixture(t, "show-ip-access-lists-DUI_PROBE.json", &raw)
	r := raw.ACLList[0].Sequence[2] // written as `deny vlan 1 0xfff udp any eq 67 any eq 68`
	if r.RuleFilter.VLAN.ID != 1 || r.RuleFilter.VLAN.Mask != 0 || strings.Contains(r.Text, "vlan") {
		t.Errorf("rule %+v", r)
	}
	var sum showIPACLSummary
	readACLFixture(t, "show-ip-access-lists-summary-vlan-qualifier.json", &sum)
	st := aclState{lists: map[string]aclList{"DUI_PROBE": {}}}
	if s := aclStatus(sum, st); !strings.Contains(s, "DUI_PROBE has rules the hardware does not fully support") {
		t.Errorf("status %q", s)
	}
}
