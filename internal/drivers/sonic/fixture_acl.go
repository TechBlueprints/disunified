package sonic

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// The FixtureRunner's ACL writes (acl.go), modelled on sonic-utilities
// 202505 (config/main.py `acl add table` / `acl remove table`,
// get_acl_bound_ports) and sonic-swss 202505's aclorch: the CONFIG_DB
// sections change, the CLI's refusals are errors, and after every write
// orchagent's verdict is recomputed into the state_acl section (STATE_DB
// ACL_TABLE_TABLE / ACL_RULE_TABLE) so the next Collect reads it back.

// aclL3Fields are the fields aclorch accepts on a rule in an L3 table: the
// table type's matches (TABLE_TYPE_L3), the port ranges Broadcom's ingress
// stage adds, the priority and the packet action.
var aclL3Fields = map[string]bool{
	"PRIORITY": true, "PACKET_ACTION": true,
	"ETHER_TYPE": true, "VLAN_ID": true, "IP_TYPE": true, "SRC_IP": true, "DST_IP": true,
	"ICMP_TYPE": true, "ICMP_CODE": true, "IP_PROTOCOL": true, "L4_SRC_PORT": true, "L4_DST_PORT": true,
	"TCP_FLAGS": true, "L4_SRC_PORT_RANGE": true, "L4_DST_PORT_RANGE": true,
}

// aclL2Fields are the fields aclorch takes on a rule in an L2 table (the
// kit's sonic-swss 0003: TABLE_TYPE_L2's matches), the priority and the
// packet action.
var aclL2Fields = map[string]bool{
	"PRIORITY": true, "PACKET_ACTION": true,
	"SRC_MAC": true, "DST_MAC": true, "ETHER_TYPE": true, "VLAN_ID": true,
}

// aclAddTable: config acl add table [-d desc] [-p ports] [-s stage] <name> <type>.
// The CLI writes the whole entry (set_entry), so a second add replaces
// the first; orchagent updates a table whose type and stage are unchanged.
func (f *FixtureRunner) aclAddTable(args []string) error {
	var pos []string
	ports, stage, desc := "", "ingress", ""
	portsSet := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-p", "--ports", "-s", "--stage", "-d", "--description":
			if i+1 >= len(args) {
				return fmt.Errorf("Error: Option '%s' requires an argument.", a)
			}
			i++
			switch a {
			case "-p", "--ports":
				ports, portsSet = args[i], true
			case "-s", "--stage":
				if args[i] != "ingress" && args[i] != "egress" {
					return fmt.Errorf("Error: Invalid value for '-s' / '--stage': '%s' is not one of 'ingress', 'egress'.", args[i])
				}
				stage = args[i]
			default:
				desc = args[i]
			}
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("Error: No such option: %s", a)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return fmt.Errorf("Usage: config acl add table [OPTIONS] <table_name> <table_type>")
	}
	if desc == "" {
		desc = pos[0]
	}
	if portsSet && ports == "" {
		return fmt.Errorf("Error: Failed to parse ACL table config: exception=Cannot bind empty list of ports")
	}
	valid := f.aclBindable()
	var list []string
	if portsSet {
		seen := map[string]bool{}
		for _, p := range strings.Split(ports, ",") {
			if !seen[p] {
				seen[p] = true
				list = append(list, p)
			}
		}
	} else {
		for p := range valid {
			list = append(list, p)
		}
	}
	for _, p := range list {
		if !valid[p] {
			return fmt.Errorf("Error: Failed to parse ACL table config: exception=Cannot bind ACL to specified port %s", p)
		}
	}
	sort.Strings(list) // set order on the switch; any order reads back the same
	if err := f.update("cfg_acl_table", func(t table) error {
		t["ACL_TABLE|"+pos[0]] = map[string]string{"policy_desc": desc, "ports@": strings.Join(list, ","), "stage": stage, "type": pos[1]}
		return nil
	}); err != nil {
		return err
	}
	return f.orchACL()
}

// aclBindable is get_acl_bound_ports: PortChannels with members, and the
// ports that are not members.
func (f *FixtureRunner) aclBindable() map[string]bool {
	out := map[string]bool{}
	members := map[string]bool{}
	for k := range f.table("cfg_lag_member") {
		if parts := strings.Split(k, "|"); len(parts) == 3 {
			out[parts[1]] = true
			members[parts[2]] = true
		}
	}
	for k := range f.table("cfg_port") {
		if p := strings.TrimPrefix(k, "PORT|"); !members[p] {
			out[p] = true
		}
	}
	return out
}

// aclTablesBinding are the ACL_TABLE keys whose ports@ list names p, in
// key order, as get_port_acl_binding reports them.
func (f *FixtureRunner) aclTablesBinding(p string) []string {
	var out []string
	for k, row := range f.table("cfg_acl_table") {
		for _, b := range strings.Split(row["ports@"], ",") {
			if b == p {
				out = append(out, k)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// aclRemoveTable: config acl remove table <name> (set_entry None: no check
// that it exists, and the table's rules stay in CONFIG_DB).
func (f *FixtureRunner) aclRemoveTable(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("Usage: config acl remove table [OPTIONS] <table_name>")
	}
	if err := f.update("cfg_acl_table", func(t table) error { delete(t, "ACL_TABLE|"+args[0]); return nil }); err != nil {
		return err
	}
	return f.orchACL()
}

// aclRuleWrite: sonic-db-cli CONFIG_DB hset 'ACL_RULE|<t>|<r>' f v ... or
// del 'ACL_RULE|<t>|<r>'. The driver writes only its own rules.
func (f *FixtureRunner) aclRuleWrite(w []string) error {
	key := w[3]
	if !strings.HasPrefix(key, "ACL_RULE|DUI_") || strings.Count(key, "|") != 2 {
		return fmt.Errorf("unexpected ACL key %q", key)
	}
	err := f.update("cfg_acl_rule", func(t table) error {
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
			return fmt.Errorf("unmodelled ACL_RULE write")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return f.orchACL()
}

// orchACL recomputes orchagent's verdict: a table of a known type whose
// ports all bind (no PortChannel member) is Active, else Inactive; a rule
// waits (no status) while its table is not Active, and is Active when
// every field is one the L3 table takes with a value aclorch parses, it
// has an action, and a priority -- else Inactive.
func (f *FixtureRunner) orchACL() error {
	if _, ok := sections(f.Fixture)["state_acl"]; !ok {
		return nil
	}
	members := map[string]bool{}
	for k := range f.table("cfg_lag_member") {
		if parts := strings.Split(k, "|"); len(parts) == 3 {
			members[parts[2]] = true
		}
	}
	out := table{}
	active := map[string]bool{}
	typ := map[string]string{}
	for k, v := range f.table("cfg_acl_table") {
		name := strings.TrimPrefix(k, "ACL_TABLE|")
		l2 := v["type"] == "L2" && strings.TrimSpace(f.OrchL2) != "" && f.OrchL2 != "0"
		ok := (v["type"] == "L3" || l2) && (v["stage"] == "ingress" || v["stage"] == "egress")
		for _, p := range strings.Split(v["ports@"], ",") {
			if members[p] {
				ok = false // "Bind table to LAG member ... is not allowed"
			}
		}
		status := "Inactive"
		if ok {
			status, active[name] = "Active", true
		}
		typ[name] = v["type"]
		out["ACL_TABLE_TABLE|"+name] = map[string]string{"status": status}
	}
	for k, v := range f.table("cfg_acl_rule") {
		name := strings.TrimPrefix(k, "ACL_RULE|")
		tbl, _, _ := strings.Cut(name, "|")
		if !active[tbl] {
			continue // "Wait for ACL table to be created"
		}
		status := "Inactive"
		if typ[tbl] == "L2" && validL2Rule(v) || typ[tbl] == "L3" && validL3Rule(v) {
			status = "Active"
		}
		out["ACL_RULE_TABLE|"+name] = map[string]string{"status": status}
	}
	f.setTable("state_acl", out)
	return nil
}

func validL3Rule(v map[string]string) bool {
	if v["PACKET_ACTION"] != "FORWARD" && v["PACKET_ACTION"] != "DROP" {
		return false
	}
	if _, err := strconv.ParseUint(v["PRIORITY"], 10, 32); err != nil {
		return false
	}
	for k, val := range v {
		if !aclL3Fields[k] {
			return false
		}
		switch k {
		case "VLAN_ID":
			if n, err := strconv.Atoi(val); err != nil || n < 1 || n > 4094 {
				return false
			}
		case "IP_PROTOCOL":
			if n, err := strconv.Atoi(val); err != nil || n < 0 || n > 255 {
				return false
			}
		case "L4_SRC_PORT", "L4_DST_PORT":
			if n, err := strconv.Atoi(val); err != nil || n < 0 || n > 65535 {
				return false
			}
		case "SRC_IP", "DST_IP":
			if ip, _, err := net.ParseCIDR(val); err != nil || ip.To4() == nil {
				return false
			}
		case "IP_TYPE":
			if val != "IPV4ANY" && val != "ANY" && val != "IP" && val != "NON_IP" && val != "IPV4" && val != "NON_IPV4" && val != "ARP" {
				return false
			}
		}
	}
	return true
}

// validL2Rule is validL3Rule for an L2 table: SRC_MAC/DST_MAC as <mac> or
// <mac>/<mask> (sonic-acl.yang's mac-addr-and-mask, 0003's parser).
func validL2Rule(v map[string]string) bool {
	if v["PACKET_ACTION"] != "FORWARD" && v["PACKET_ACTION"] != "DROP" {
		return false
	}
	if _, err := strconv.ParseUint(v["PRIORITY"], 10, 32); err != nil {
		return false
	}
	for k, val := range v {
		if !aclL2Fields[k] {
			return false
		}
		switch k {
		case "VLAN_ID":
			if n, err := strconv.Atoi(val); err != nil || n < 1 || n > 4094 {
				return false
			}
		case "SRC_MAC", "DST_MAC":
			parts := strings.Split(val, "/")
			if len(parts) > 2 {
				return false
			}
			for _, p := range parts {
				if hw, err := net.ParseMAC(p); err != nil || len(hw) != 6 {
					return false
				}
			}
		case "ETHER_TYPE":
			if n, err := strconv.ParseUint(val, 0, 16); err != nil || n < 0x600 {
				return false
			}
		}
	}
	return true
}
