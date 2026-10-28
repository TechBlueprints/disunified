package sonic

import (
	"fmt"
	"sort"
	"strings"
)

// The FixtureRunner's breakout, time zone and hostname writes, modelled on
// sonic-utilities 202505 (config/main.py `breakout`, `clock timezone`,
// `hostname`) and sonic-config-engine's BreakoutCfg: they change the
// capture's CONFIG_DB (and APPL_DB PORT_TABLE, which portmgrd keeps in
// step) so the next Collect reads them back, and they refuse what SONiC
// refuses.

// breakout: config interface breakout [-f] [-l] [-y] [-v] <parent> <mode>.
func (f *FixtureRunner) breakout(args []string) error {
	force, load, yes := false, false, false
	var pos []string
	for _, a := range args {
		switch a {
		case "-f", "--force-remove-dependencies":
			force = true
		case "-l", "--load-predefined-config":
			load = true
		case "-y", "--yes":
			yes = true
		case "-v", "--verbose":
		case "</dev/null":
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("no such option: %s", a)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return fmt.Errorf("usage: config interface breakout [OPTIONS] <interface_name> MODE")
	}
	if !yes {
		return fmt.Errorf("Aborted!") // the confirmation prompt, with no answer on stdin
	}
	parent, mode := pos[0], pos[1]
	present, plat := parsePlatform(f.section("platform_json"))
	if !present || plat == nil {
		return fmt.Errorf("[ERROR] Breakout feature is not available without platform.json file")
	}
	brk := f.table("cfg_breakout")
	if len(brk) == 0 {
		return fmt.Errorf("[ERROR] BREAKOUT_CFG table is NOT present in CONFIG DB")
	}
	cur, ok := brk["BREAKOUT_CFG|"+parent]
	if !ok {
		return fmt.Errorf("[ERROR] %s interface is NOT present in BREAKOUT_CFG table of CONFIG DB", parent)
	}
	pp, ok := plat[parent]
	if !ok {
		return fmt.Errorf("[ERROR] %s is not a Parent port. So, Breakout Mode is not available on this port", parent)
	}
	if _, ok := pp.BreakoutModes[mode]; !ok {
		return fmt.Errorf("[ERROR] Target mode %s is not available for the port %s", mode, parent)
	}
	if cur["brkout_mode"] == mode {
		return nil // "[WARNING] No action will be taken as current and desired Breakout Mode are same."
	}
	if load {
		// Probed on the VS: no SONiC build generates this file.
		return fmt.Errorf("Failed to break out Port. Error: [Errno 2] No such file or directory: '/etc/sonic/port_breakout_config_db.json'")
	}
	curMode, ok1 := parseMode(cur["brkout_mode"])
	newMode, ok2 := parseMode(mode)
	if !ok1 || !ok2 {
		return fmt.Errorf("Breakout mode validation failed")
	}
	dels, err := pp.children(parent, curMode)
	if err != nil {
		return err
	}
	adds, err := pp.children(parent, newMode)
	if err != nil {
		return err
	}
	ports := f.table("cfg_port")
	gone := map[string]bool{}
	for _, d := range dels {
		if _, ok := ports["PORT|"+d.name]; !ok {
			return fmt.Errorf("[ERROR] Interface name %s is invalid", d.name)
		}
		gone[d.name] = true
	}

	// Dependencies: everything that names a port going away.
	refs := func(sec string, match func(key string, v map[string]string) bool) []string {
		var out []string
		for k, v := range f.table(sec) {
			if match(k, v) {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	}
	last := func(k string) string { return k[strings.LastIndex(k, "|")+1:] }
	byLast := func(k string, _ map[string]string) bool { return gone[last(k)] }
	deps := map[string][]string{
		"cfg_vlan_member": refs("cfg_vlan_member", byLast),
		"cfg_lag_member":  refs("cfg_lag_member", byLast),
		"cfg_stp":         refs("cfg_stp", func(k string, _ map[string]string) bool { return strings.HasPrefix(k, "STP_PORT|") && gone[last(k)] }),
		"cfg_storm": refs("cfg_storm", func(k string, _ map[string]string) bool {
			parts := strings.Split(k, "|")
			return len(parts) == 3 && gone[parts[1]]
		}),
		"cfg_mirror": refs("cfg_mirror", func(_ string, v map[string]string) bool {
			if gone[v["dst_port"]] {
				return true
			}
			for _, s := range strings.Split(v["src_port"], ",") {
				if gone[strings.TrimSpace(s)] {
					return true
				}
			}
			return false
		}),
	}
	var all []string
	for _, ks := range deps {
		all = append(all, ks...)
	}
	if len(all) > 0 && !force {
		sort.Strings(all)
		return fmt.Errorf("Dependecies Exist. No further action will be taken: %s", strings.Join(all, ", "))
	}
	for sec, ks := range deps {
		if len(ks) == 0 {
			continue
		}
		if err := f.update(sec, func(t table) error {
			for _, k := range ks {
				delete(t, k)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := f.update("cfg_port", func(t table) error {
		for n := range gone {
			delete(t, "PORT|"+n)
		}
		for _, a := range adds {
			t["PORT|"+a.name] = copyFields(a.fields)
		}
		return nil
	}); err != nil {
		return err
	}
	// portmgrd: APPL_DB PORT_TABLE follows, every new port down at SONiC's
	// default MTU until configured.
	if err := f.update("appl_port", func(t table) error {
		for n := range gone {
			delete(t, "PORT_TABLE:"+n)
		}
		for _, a := range adds {
			t["PORT_TABLE:"+a.name] = map[string]string{
				"admin_status": "down", "alias": a.fields["alias"], "description": "", "index": a.fields["index"],
				"lanes": a.fields["lanes"], "mtu": "9100", "oper_status": "down", "speed": a.fields["speed"],
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return f.update("cfg_breakout", func(t table) error {
		t["BREAKOUT_CFG|"+parent] = map[string]string{"brkout_mode": mode}
		return nil
	})
}

// laneSpeedOK: on a platform with breakout modes, a port runs the speeds its
// cage's current mode allows (4x25G[10G]: 25G or 10G per lane).
func (f *FixtureRunner) laneSpeedOK(port, speed string) error {
	_, plat := parsePlatform(f.section("platform_json"))
	if plat == nil {
		return nil
	}
	idx := atoi(f.table("cfg_port")["PORT|"+port]["index"])
	for parent, pp := range plat {
		if pp.cageIndex() != idx {
			continue
		}
		m, ok := parseMode(f.table("cfg_breakout")["BREAKOUT_CFG|"+parent]["brkout_mode"])
		if ok && !containsInt(m.speeds, atoi(speed)) {
			return fmt.Errorf("Invalid speed specified: %s (mode %s runs %v)", speed, m.name, m.speeds)
		}
	}
	return nil
}

// timezone: config clock timezone <name>, a name `timedatectl
// list-timezones` prints.
func (f *FixtureRunner) timezone(name string) error {
	if f.Zones == "" {
		return fmt.Errorf("timedatectl: no zones")
	}
	found := false
	for _, z := range parseZones(f.Zones) {
		if z.name == name {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("Timezone %s does not conform format", name)
	}
	return f.update("metadata", func(t table) error {
		t["DEVICE_METADATA|localhost"]["timezone"] = name
		return nil
	})
}

// hostname: config hostname <name>; YANG stypes:hostname is 1..63
// characters. hostcfgd then sets the Linux hostname.
func (f *FixtureRunner) hostname(name string) error {
	if name == "" || len(name) > 63 {
		return fmt.Errorf("Failed to write new hostname to ConfigDB. Error: hostname length")
	}
	if err := f.update("metadata", func(t table) error {
		t["DEVICE_METADATA|localhost"]["hostname"] = name
		return nil
	}); err != nil {
		return err
	}
	f.setSection("hostname", name)
	return nil
}
