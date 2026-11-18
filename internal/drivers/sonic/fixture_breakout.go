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
	// sonic-yang validates the whole CONFIG_DB before the change (§11.8):
	// §10.6's MGMT_INTERFACE|eth0 without a MGMT_PORT row is a dangling
	// leafref, and the breakout fails whatever the cage.
	if mg := f.table("cfg_mgmt"); mg["MGMT_PORT|eth0"] == nil {
		for k := range mg {
			if strings.HasPrefix(k, "MGMT_INTERFACE|eth0|") {
				return fmt.Errorf(`libyang[0]: Leafref "/sonic-mgmt_port:sonic-mgmt_port/sonic-mgmt_port:MGMT_PORT/sonic-mgmt_port:MGMT_PORT_LIST/sonic-mgmt_port:name" of value "eth0" points to a non-existing leaf. (path: /sonic-mgmt_interface:sonic-mgmt_interface/MGMT_INTERFACE/MGMT_INTERFACE_LIST[name='eth0'][ip_prefix='%s']/name)`, strings.TrimPrefix(k, "MGMT_INTERFACE|eth0|"))
			}
		}
	}
	// ... and every MGMT_INTERFACE row needs a gateway of its own family;
	// `config interface ip add eth0 <ip>` without one writes NULL (§11.8).
	for k, fv := range f.table("cfg_mgmt") {
		if pfx, ok := strings.CutPrefix(k, "MGMT_INTERFACE|eth0|"); ok {
			if gw := fv["gwaddr"]; !(strings.Contains(pfx, ":") && strings.Contains(gw, ":")) && !(strings.Contains(pfx, ".") && strings.Contains(gw, ".")) {
				return fmt.Errorf(`libyang[0]: Must condition "(contains(current(), ':') and contains(../gwaddr, ':')) or (contains(current(), '.') and contains(../gwaddr, '.'))" not satisfied. (path: /sonic-mgmt_interface:sonic-mgmt_interface/MGMT_INTERFACE/MGMT_INTERFACE_LIST[name='eth0'][ip_prefix='%s']/ip_prefix)`, pfx)
			}
		}
	}
	// ... and sonic-ntp.yang has no VLAN among src_intf's types (the branch
	// is commented out), which the driver itself wrote before §11.8's re-run.
	if v := f.table("cfg_ntp_global")["NTP|global"]["src_intf"]; strings.HasPrefix(v, "Vlan") {
		return fmt.Errorf(`libyang[0]: Invalid value "%s" in "src_intf" element. (path: /sonic-ntp:sonic-ntp/NTP/global/src_intf[.='%s'])`, v, v)
	}
	// ... and an ACL table's ports are leafrefs to PORT or PORTCHANNEL: a
	// PortChannel deleted while a table bound it (`config portchannel del`
	// leaves the name) refuses the breakout (the DX010, 2026-11-15,
	// portchannel-del-acl-20261115.txt).
	{
		cports, lags := f.table("cfg_port"), f.table("cfg_lag")
		keys := make([]string, 0)
		for k := range f.table("cfg_acl_table") {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			for _, p := range strings.Split(f.table("cfg_acl_table")[k]["ports@"], ",") {
				if p != "" && cports["PORT|"+p] == nil && lags["PORTCHANNEL|"+p] == nil {
					name := strings.TrimPrefix(k, "ACL_TABLE|")
					return fmt.Errorf(`libyang[0]: Invalid value "%s" in "ports" element. (path: /sonic-acl:sonic-acl/ACL_TABLE/ACL_TABLE_LIST[ACL_TABLE_NAME='%s']/ports[.='%s'])`+"\nFailed to break out Port. Error: Failed to load the config. Error: ConfigMgmtDPB Class creation failed", p, name, p)
				}
			}
		}
	}
	// ... and a field the YANG model lacks leaves its whole entry
	// untranslated (sonic_yang_ext _xlateList: KeyError in the leaf lookup),
	// so everything that references the port dangles: learn_mode is in
	// neither sonic-port.yang nor sonic-portchannel.yang on 202511. Modelled
	// from that source, not from an observed refusal (§11.12).
	// ... and sonic-scheduler.yang's pir needs a cir > 0 ("pir can't be
	// configured without cir."), which the driver's egress shapers never
	// carry (the SAI refuses a port's minimum rate): live on the DX010,
	// 2026-11-17, the breakout refused every cycle while one shaper existed
	// (docs/drivers/sonic.md 11.38).
	for k, fv := range f.table("cfg_qos") {
		if strings.HasPrefix(k, "SCHEDULER|") && fv["pir"] != "" && atoi(fv["cir"]) <= 0 {
			return fmt.Errorf("Data Loading Failed\npir can't be configured without cir.\nConfigMgmt Class creation failed")
		}
	}
	for k, fv := range f.table("cfg_port") {
		if fv["learn_mode"] != "" {
			return fmt.Errorf("sonic_yang: %s: learn_mode is not in the YANG model; the entry is not translated", k)
		}
	}
	for k, fv := range f.table("cfg_lag") {
		if fv["learn_mode"] != "" {
			return fmt.Errorf("sonic_yang: %s: learn_mode is not in the YANG model; the entry is not translated", k)
		}
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

	// Dependencies: everything that names a port going away. Not the L2MC
	// tables: on the image kit their interface fields are YANG strings
	// (sonic-buildimage 0010), so their rows stay, naming the old ports
	// (live on the DX010 2026-11-15, breakout-l2mc-20261115.txt).
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
		// An ACL table's ports@ list names ports (a YANG leafref): -f
		// takes the port out of the list (below), the table stays.
		"cfg_acl_table": refs("cfg_acl_table", func(_ string, v map[string]string) bool {
			for _, p := range strings.Split(v["ports@"], ",") {
				if gone[p] {
					return true
				}
			}
			return false
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
				if sec == "cfg_acl_table" {
					var keep []string
					for _, p := range strings.Split(t[k]["ports@"], ",") {
						if !gone[p] {
							keep = append(keep, p)
						}
					}
					t[k]["ports@"] = strings.Join(keep, ",")
					continue
				}
				delete(t, k)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if len(deps["cfg_acl_table"]) > 0 {
		defer f.orchACL()
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
	var added []string
	for _, a := range adds {
		added = append(added, a.name)
	}
	f.lldpForget(gone, added)
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
	// STATE_DB PORT_TABLE: the old interfaces' rows go (their
	// supported_fecs spoke for the old speed). On a capture whose ports
	// list supported_fecs (the DX010's), the new ones get the SAI's answer
	// for their layout as read from that switch (docs/fec-by-standard.md
	// §3; collect-split-dac.txt for the lanes): a whole cage and a 2x50G
	// half none,rs, a 25G lane none,fc.
	if err := f.update("state_port", func(t table) error {
		listed := false
		for n := range gone {
			listed = listed || t["PORT_TABLE|"+n]["supported_fecs"] != ""
			delete(t, "PORT_TABLE|"+n)
		}
		for _, a := range adds {
			delete(t, "PORT_TABLE|"+a.name)
			if listed {
				fecs := "none,rs"
				if len(adds) == 4 {
					fecs = "none,fc"
				}
				t["PORT_TABLE|"+a.name] = map[string]string{"supported_fecs": fecs}
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
	// portconfig's set_speed (202511): the port's STATE_DB supported_speeds,
	// when the SAI has filled it, and nothing else -- not the breakout mode.
	if sup := f.table("state_port")["PORT_TABLE|"+port]["supported_speeds"]; sup != "" {
		if !containsInt(speedList(sup), atoi(speed)) {
			return fmt.Errorf("Invalid speed specified: %s\nValid speeds:%s", speed, sup)
		}
		return nil
	}
	// Right after a breakout the capture has no row yet: the mode's speeds
	// stand in for what the SAI answers a moment later.
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

// rsyslogConfig: systemctl restart rsyslog-config renders rsyslog.conf
// again from the Linux hostname (rsyslog-config.sh's `hostname=$(hostname)`),
// so every SONiC template in the capture's rsyslog_tmpl section takes the
// hostname section's name.
func (f *FixtureRunner) rsyslogConfig() error {
	name := strings.TrimSpace(f.section("hostname"))
	body := f.section("rsyslog_tmpl")
	old := rsyslogHosts(body)
	if name == "" || len(old) == 0 {
		return nil
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "$template SONiC") {
			for _, h := range old {
				line = strings.Replace(line, " "+h+" ", " "+name+" ", 1)
			}
			lines[i] = line
		}
	}
	f.setSection("rsyslog_tmpl", strings.Join(lines, "\n"))
	return nil
}
