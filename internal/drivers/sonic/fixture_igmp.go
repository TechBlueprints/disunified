package sonic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The FixtureRunner's model of IGMP snooping's tables (igmp.go): the
// capture's cfg_l2mc section is CONFIG_DB, the IGMP script's rows land in
// it, and the CLI's refusals the driver must order around are enforced --
// a snooping row only on an existing VLAN with FEATURE|l2mcd, a static
// member only of a VLAN that snoops and on a port of that VLAN, and no
// `config vlan member del` of a port a static group names there (the
// series' sonic-utilities, config/vlan.py).

// applyL2MCScript runs a `python3 - disunified-l2mc` script.
func (f *FixtureRunner) applyL2MCScript(script string) error {
	if _, ok := sections(f.Fixture)["cfg_l2mc"]; !ok {
		return fmt.Errorf("disunified-l2mc: the capture has no cfg_l2mc section")
	}
	if !strings.HasPrefix(script, l2mcScriptHead) {
		return fmt.Errorf("disunified-l2mc: unexpected script")
	}
	line := strings.TrimSpace(strings.TrimPrefix(script, l2mcScriptHead))
	lit, ok := strings.CutPrefix(line, "run(json.loads(")
	if !ok || !strings.HasSuffix(lit, "))") {
		return fmt.Errorf("disunified-l2mc: no ops line")
	}
	var raw string
	if err := json.Unmarshal([]byte(strings.TrimSuffix(lit, "))")), &raw); err != nil {
		return fmt.Errorf("disunified-l2mc: %w", err)
	}
	var ops []l2mcOp
	if err := json.Unmarshal([]byte(raw), &ops); err != nil {
		return fmt.Errorf("disunified-l2mc: %w", err)
	}
	vlans, members := f.table("cfg_vlan"), f.table("cfg_vlan_member")
	_, feature := f.table("cfg_feature")["FEATURE|l2mcd"]
	return f.update("cfg_l2mc", func(t table) error {
		for _, o := range ops {
			if o.Fields == nil {
				delete(t, o.Key)
				continue
			}
			table, key, _ := strings.Cut(o.Key, "|")
			parts := strings.Split(key, "|")
			if _, ok := vlans["VLAN|"+parts[0]]; !ok {
				return fmt.Errorf("%s: %s does not exist", o.Key, parts[0])
			}
			switch table {
			case "L2MC":
				if !feature {
					return fmt.Errorf("%s: no l2mcd on this image", o.Key)
				}
			case "L2MC_STATIC_MEMBER":
				if len(parts) != 3 {
					return fmt.Errorf("%s: bad key", o.Key)
				}
				if t["L2MC|"+parts[0]] == nil {
					return fmt.Errorf("%s: IGMP snooping is not enabled on %s", o.Key, parts[0])
				}
				if _, ok := members["VLAN_MEMBER|"+parts[0]+"|"+parts[2]]; !ok {
					return fmt.Errorf("%s: %s is not a member of %s", o.Key, parts[2], parts[0])
				}
			}
			t[o.Key] = copyFields(o.Fields)
		}
		return nil
	})
}

// staticNames reports whether a static group names port in Vlan<vid>.
func (f *FixtureRunner) staticNames(vid, port string) bool {
	pre := "L2MC_STATIC_MEMBER|Vlan" + vid + "|"
	for k := range f.table("cfg_l2mc") {
		if rest, ok := strings.CutPrefix(k, pre); ok && strings.HasSuffix(rest, "|"+port) {
			return true
		}
	}
	return false
}

// l2mcDel is `sonic-db-cli CONFIG_DB del '<an L2MC key>'`.
func (f *FixtureRunner) l2mcDel(key string) error {
	return f.update("cfg_l2mc", func(t table) error {
		if t[key] == nil {
			return fmt.Errorf("unmodelled: %s absent", key)
		}
		delete(t, key)
		return nil
	})
}

// featureState is `config feature state <name> enabled|disabled`.
func (f *FixtureRunner) featureState(name, state string) error {
	if state != "enabled" && state != "disabled" {
		return fmt.Errorf("invalid state %q", state)
	}
	return f.update("cfg_feature", func(t table) error {
		k := "FEATURE|" + name
		if t[k] == nil {
			return fmt.Errorf("feature %s does not exist", name)
		}
		t[k]["state"] = state
		return nil
	})
}
