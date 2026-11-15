package sonic

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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
	return f.applyL2MCOps(ops)
}

// applyL2MCOps applies the IGMP script's ops to cfg_l2mc, with the
// refusals the CLI would make.
func (f *FixtureRunner) applyL2MCOps(ops []l2mcOp) error {
	if _, ok := sections(f.Fixture)["cfg_l2mc"]; !ok {
		return fmt.Errorf("disunified-l2mc: the capture has no cfg_l2mc section")
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

// danglingL2MC are the L2MC rows naming an interface the switch does not
// have. The image kit's sonic-buildimage 0010 made every interface in
// sonic-igmp-snooping.yang a string, so `config interface breakout -f`
// leaves these rows behind (fixture_breakout.go takes nothing out of
// cfg_l2mc; live on the DX010 2026-11-15, breakout-l2mc-20261115.txt): the
// member and router rows keyed by a deleted port, and its name in a static
// group's member list.
func (f *FixtureRunner) danglingL2MC() []string {
	ports, lags := f.table("cfg_port"), f.table("cfg_lag")
	exists := func(n string) bool { return ports["PORT|"+n] != nil || lags["PORTCHANNEL|"+n] != nil }
	var out []string
	for k, v := range f.table("cfg_l2mc") {
		parts := strings.Split(k, "|")
		switch {
		case parts[0] == "L2MC_STATIC_MEMBER" && len(parts) == 4 && !exists(parts[3]),
			parts[0] == "L2MC_MROUTER" && len(parts) == 3 && !exists(parts[2]):
			out = append(out, k)
		case parts[0] == "L2MC_STATIC_GROUP":
			for _, m := range strings.Split(v["static-members@"], ",") {
				if m != "" && !exists(m) {
					out = append(out, k+" "+m)
				}
			}
		}
	}
	sort.Strings(out)
	return out
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

// l2mcDel is `sonic-db-cli CONFIG_DB del '<an L2MC key>'`. A static
// member's delete reaches orchagent later (l2mcmgrd, l2mcd, APPL_DB): until
// the batch waits for it, the port still has a group member there.
func (f *FixtureRunner) l2mcDel(key string) error {
	err := f.update("cfg_l2mc", func(t table) error {
		if t[key] == nil {
			return fmt.Errorf("unmodelled: %s absent", key)
		}
		delete(t, key)
		return nil
	})
	if parts := strings.Split(key, "|"); err == nil && parts[0] == "L2MC_STATIC_MEMBER" && len(parts) == 4 && f.orchRefuses() {
		if f.l2mcPending == nil {
			f.l2mcPending = map[string]bool{}
		}
		f.l2mcPending[parts[3]] = true
	}
	return err
}

// The bridge port model (OrchL2MCFix; docs/drivers/sonic.md §8.15), from
// the kit's sonic-swss 0001: PortsOrch::removeBridgePort sets the bridge
// port admin down, then returns false while the port's m_l2mc_count is not
// zero, and addBridgePort returns an existing bridge port as it is.

// orchRefuses: l2mcd runs and orchagent lacks sonic-swss 0004.
func (f *FixtureRunner) orchRefuses() bool {
	if f.OrchL2MCFix == "1" {
		return false
	}
	return f.table("cfg_feature")["FEATURE|l2mcd"]["state"] == "enabled"
}

// vlanLeft is a port's last VLAN membership gone: orchagent removes its
// bridge port, or refuses with it admin down.
func (f *FixtureRunner) vlanLeft(port string) {
	if !f.orchRefuses() || !(f.l2mcPending[port] || f.L2MCLeaked[port]) {
		f.setBridgePort(port, "")
		return
	}
	if f.bpRefused == nil {
		f.bpRefused = map[string]bool{}
	}
	f.bpRefused[port] = true
	f.setBridgePort(port, "false")
}

// vlanJoined is a VLAN added to a port: a refused bridge port is reused as
// it is (stranded); otherwise the port has one, up.
func (f *FixtureRunner) vlanJoined(port string) {
	if f.bpRefused[port] {
		f.Stranded = append(f.Stranded, port)
		return
	}
	f.setBridgePort(port, "true")
}

// bridgeWaited is the batch's bridgePortWait for port: done once l2mcd's
// deletes have reached orchagent, refused for good on a leaked count.
func (f *FixtureRunner) bridgeWaited(port string) error {
	if f.bpRefused[port] && f.L2MCLeaked[port] {
		return fmt.Errorf("exit status 1: %s: orchagent has not removed its bridge port in %d s", port, bridgePortWaitLimit)
	}
	delete(f.l2mcPending, port)
	if f.bpRefused[port] {
		delete(f.bpRefused, port)
		f.setBridgePort(port, "")
	}
	return nil
}

// batchEnded: l2mcd catches up after the batch; a refused removal nothing
// reused is retried and succeeds, unless the count leaked.
func (f *FixtureRunner) batchEnded() {
	f.l2mcPending = nil
	for port := range f.bpRefused {
		if !f.L2MCLeaked[port] {
			delete(f.bpRefused, port)
			f.setBridgePort(port, "")
		}
	}
}

// setBridgePort writes the capture's asic_bridge_port row of port: admin
// "true"/"false", "" for none. A capture without the section has no rows.
func (f *FixtureRunner) setBridgePort(port, admin string) {
	if _, ok := sections(f.Fixture)["asic_bridge_port"]; !ok {
		return
	}
	_ = f.update("asic_bridge_port", func(t table) error {
		if admin == "" {
			delete(t, port)
		} else {
			t[port] = map[string]string{"admin": admin}
		}
		return nil
	})
}

// bridgeWaitPort is the interface of a bridgePortWait line, "" for another.
func bridgeWaitPort(w []string) string {
	if len(w) == 6 && w[0] == "python3" && w[1] == "-c" && w[5] == "</dev/null" && strings.Contains(w[2], "SAI_OBJECT_TYPE_BRIDGE_PORT") && w[4] == strconv.Itoa(bridgePortWaitLimit) {
		return w[3]
	}
	return ""
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
