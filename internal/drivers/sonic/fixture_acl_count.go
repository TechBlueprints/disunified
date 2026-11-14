package sonic

import (
	"fmt"
	"strconv"
	"strings"
)

// The FixtureRunner's half of the guard's per-port count (acl_count.go,
// docs/drivers/sonic.md §11.7.9), modelled on sonic-swss 202511's aclorch
// and what the DX010 showed on 2026-11-14: a table of a CONFIG_DB type
// (ACL_TABLE_TYPE) takes the rule fields the type's MATCHES name; a rule's
// IN_PORTS must name ports orchagent knows (STATE_DB PORT_TABLE), else it is
// Inactive and never retried; every Active rule has a counter in
// ACL_COUNTER_RULE_MAP, whose packets COUNTERS_DB holds only while
// FLEX_COUNTER_TABLE|ACL is enabled. collect.py's acl_counters section is the
// counted (_IN_) rules' packets; ACLHits stands in for frames hitting a rule.
// A capture from before 2026-11-14 lacks cfg_acl_type, cfg_flex_acl and
// acl_counters: ensureSection adds them as the writes need them.

// aclCountWrite: sonic-db-cli CONFIG_DB hset|del 'ACL_TABLE_TYPE|<t>' ...,
// and hset 'FLEX_COUNTER_TABLE|ACL' FLEX_COUNTER_STATUS <v> (what
// `counterpoll acl enable|disable` writes).
func (f *FixtureRunner) aclCountWrite(w []string) error {
	key := w[3]
	if key == flexACLKey {
		if w[2] != "hset" || len(w) != 6 || w[4] != "FLEX_COUNTER_STATUS" || (w[5] != "enable" && w[5] != "disable") {
			return fmt.Errorf("unmodelled %s write", flexACLKey)
		}
		f.ensureSection("cfg_flex_acl")
		if err := f.update("cfg_flex_acl", func(t table) error {
			if t[key] == nil {
				t[key] = map[string]string{"POLL_INTERVAL": "10000"}
			}
			t[key]["FLEX_COUNTER_STATUS"] = w[5]
			return nil
		}); err != nil {
			return err
		}
		return f.orchACL()
	}
	if !strings.HasPrefix(key, "ACL_TABLE_TYPE|DUI_") {
		return fmt.Errorf("unexpected ACL table type key %q", key)
	}
	f.ensureSection("cfg_acl_type")
	err := f.update("cfg_acl_type", func(t table) error {
		switch {
		case w[2] == "del" && len(w) == 4:
			delete(t, key)
		case w[2] == "hset" && len(w) >= 6 && len(w)%2 == 0:
			if t[key] == nil {
				t[key] = map[string]string{}
			}
			for i := 4; i < len(w); i += 2 {
				t[key][w[i]] = w[i+1]
			}
		default:
			return fmt.Errorf("unmodelled ACL_TABLE_TYPE write")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return f.orchACL()
}

// customType is a CONFIG_DB table type's matches, nil if the type is not
// there (orchagent then leaves the table pending).
func (f *FixtureRunner) customType(name string) map[string]bool {
	row, ok := f.table("cfg_acl_type")["ACL_TABLE_TYPE|"+name]
	if !ok || row["MATCHES@"] == "" {
		return nil
	}
	m := map[string]bool{}
	for _, x := range strings.Split(row["MATCHES@"], ",") {
		m[strings.TrimSpace(x)] = true
	}
	return m
}

// validCustomRule is validL3Rule for a table of a CONFIG_DB type: only the
// type's matches, and an IN_PORTS of ports orchagent has.
func (f *FixtureRunner) validCustomRule(v map[string]string, matches map[string]bool) bool {
	rest := map[string]string{}
	for k, val := range v {
		switch {
		case k == "PRIORITY" || k == "PACKET_ACTION":
		case !matches[k]:
			return false // "Match ... is not supported by table type"
		case k == "IN_PORTS":
			known := f.table("state_port")
			for _, p := range strings.Split(val, ",") {
				if _, ok := known["PORT_TABLE|"+p]; !ok || !f.hasPort(p) {
					return false // "Failed to locate port"
				}
			}
			continue
		}
		rest[k] = val
	}
	return validL3Rule(rest)
}

// aclCounters recomputes the acl_counters section after orchACL: one entry
// per Active counted rule of the driver's tables, keeping its packets; a
// new one reads 0 while polling is on and no value while it is off.
func (f *FixtureRunner) aclCounters(status table) {
	if _, ok := sections(f.Fixture)["acl_counters"]; !ok && len(f.table("cfg_acl_type")) == 0 {
		return // a capture from before the count, with nothing written for it
	}
	f.ensureSection("acl_counters")
	old := f.table("acl_counters")
	on := f.table("cfg_flex_acl")[flexACLKey]["FLEX_COUNTER_STATUS"] == "enable"
	out := table{}
	n := 0
	for k, st := range status {
		name, ok := strings.CutPrefix(k, "ACL_RULE_TABLE|")
		if !ok || st["status"] != "Active" || !strings.Contains(name, "_IN_") {
			continue
		}
		id := strings.Replace(name, "|", ":", 1)
		e := old[id]
		if e == nil {
			n++
			e = map[string]string{"oid": fmt.Sprintf("oid:0x90000000008%02x", n), "SAI_ACL_COUNTER_ATTR_PACKETS": ""}
		}
		if on && e["SAI_ACL_COUNTER_ATTR_PACKETS"] == "" {
			e["SAI_ACL_COUNTER_ATTR_PACKETS"] = "0"
		}
		out[id] = e
	}
	f.setTable("acl_counters", out)
}

// ACLHits counts n more frames on a counted rule ("<table>:<rule>"), as the
// chip would for frames it matched; an error if the rule has no counter or
// polling is off (no value to add to).
func (f *FixtureRunner) ACLHits(rule string, n uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.update("acl_counters", func(t table) error {
		e := t[rule]
		if e == nil {
			return fmt.Errorf("no counter for %s", rule)
		}
		cur, err := strconv.ParseUint(e["SAI_ACL_COUNTER_ATTR_PACKETS"], 10, 64)
		if err != nil {
			return fmt.Errorf("%s has no packet count (polling off?)", rule)
		}
		e["SAI_ACL_COUNTER_ATTR_PACKETS"] = strconv.FormatUint(cur+n, 10)
		return nil
	})
}
