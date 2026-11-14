package sonic

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The FixtureRunner's model of portfeat.go's writes (§11.12): APPL_DB's
// isolation group and static FDB entries, CONFIG_DB's shaper profiles and
// port QoS maps, and lldpd's per-port LLDP-MED TLV (the lldp_med section,
// one "<interface> on" line per port that sends it).

// portFeatWrite applies one write if it is one of these; ok is false for
// any other command.
func (f *FixtureRunner) portFeatWrite(w []string) (ok bool, err error) {
	switch {
	case len(w) == 5 && w[0] == "python3" && w[1] == "-c" && w[3] == "disunified-appl":
		var ops []applOp
		if err := json.Unmarshal([]byte(w[4]), &ops); err != nil {
			return true, err
		}
		for _, o := range ops {
			op := "hset"
			if o.Op == "del" {
				op = "del"
			}
			if err := f.applWrite(op, o.Table+":"+o.Key, o.FV); err != nil {
				return true, err
			}
		}
		return true, nil
	case len(w) >= 2 && w[0] == "sonic-db-cli" && w[1] == "APPL_DB":
		return true, fmt.Errorf("a plain APPL_DB write never reaches orchagent (no ProducerStateTable notify): %s", strings.Join(w, " "))
	case len(w) >= 4 && w[0] == "sonic-db-cli" && w[1] == "CONFIG_DB" && (strings.HasPrefix(w[3], "SCHEDULER|") || strings.HasPrefix(w[3], "PORT_QOS_MAP|")):
		return true, f.qosWrite(w[2], w[3], w[4:])
	case len(w) >= 6 && w[0] == "docker" && w[1] == "exec" && w[2] == "lldp" && w[3] == "lldpcli":
		return true, f.lldpcli(w[4:])
	}
	return false, nil
}

func (f *FixtureRunner) ensureSection(name string) {
	if !strings.Contains(f.Fixture, "@@@ "+name+"\n") {
		body := "{}"
		if name == "lldp_med" {
			body = ""
		}
		f.UseSection(name, body)
	}
}

func (f *FixtureRunner) applWrite(op, key string, fv []string) error {
	sec := ""
	switch {
	case strings.HasPrefix(key, "ISOLATION_GROUP_TABLE:"):
		sec = "appl_iso"
	case strings.HasPrefix(key, "INTF_TABLE:"):
		// An address row: orchagent takes it only for a router interface
		// it has (the interface's own row, "INTF_TABLE:<iface>").
		sec = "appl_intf"
		iface, _, _ := strings.Cut(strings.TrimPrefix(key, "INTF_TABLE:"), ":")
		if _, ok := f.table("appl_intf")["INTF_TABLE:"+iface]; op == "hset" && !ok {
			return fmt.Errorf("unmodelled: an address row for %s, which is no router interface", iface)
		}
	case strings.HasPrefix(key, "FDB_TABLE:"):
		sec = "appl_fdb"
		vlan, _, _ := strings.Cut(strings.TrimPrefix(key, "FDB_TABLE:"), ":")
		if op == "hset" && !f.vlanExists(vlan) {
			return fmt.Errorf("unmodelled: FDB entry in %s, which does not exist", vlan)
		}
	default:
		return fmt.Errorf("unmodelled APPL_DB key %q", key)
	}
	f.ensureSection(sec)
	return f.update(sec, func(t table) error {
		switch op {
		case "del":
			delete(t, key)
		case "hset":
			if len(fv) == 0 || len(fv)%2 != 0 {
				return fmt.Errorf("hset %s: odd field list", key)
			}
			if t[key] == nil {
				t[key] = map[string]string{}
			}
			for i := 0; i < len(fv); i += 2 {
				t[key][fv[i]] = fv[i+1]
			}
		default:
			return fmt.Errorf("unmodelled APPL_DB %s", op)
		}
		return nil
	})
}

func (f *FixtureRunner) vlanExists(name string) bool {
	_, ok := f.table("cfg_vlan")["VLAN|"+name]
	return ok
}

func (f *FixtureRunner) qosWrite(op, key string, fv []string) error {
	f.ensureSection("cfg_qos")
	if op == "hset" && strings.HasPrefix(key, "PORT_QOS_MAP|") {
		// qosorch resolves the reference; a missing profile leaves the
		// entry pending, which the driver never means to do.
		for i := 0; i+1 < len(fv); i += 2 {
			if fv[i] == "scheduler" {
				if _, ok := f.table("cfg_qos")["SCHEDULER|"+fv[i+1]]; !ok {
					return fmt.Errorf("unmodelled: %s names scheduler %s, which does not exist", key, fv[i+1])
				}
			}
		}
		if !f.hasPort(strings.TrimPrefix(key, "PORT_QOS_MAP|")) {
			return fmt.Errorf("unexpected key %q", key)
		}
	}
	return f.update("cfg_qos", func(t table) error {
		switch op {
		case "del":
			if n, ok := strings.CutPrefix(key, "SCHEDULER|"); ok {
				for k, v := range t {
					if strings.HasPrefix(k, "PORT_QOS_MAP|") && v["scheduler"] == n {
						return fmt.Errorf("scheduler %s is still referenced by %s", n, k)
					}
				}
			}
			delete(t, key)
		case "hset":
			if len(fv) == 0 || len(fv)%2 != 0 {
				return fmt.Errorf("hset %s: odd field list", key)
			}
			if t[key] == nil {
				t[key] = map[string]string{}
			}
			for i := 0; i < len(fv); i += 2 {
				t[key][fv[i]] = fv[i+1]
			}
		case "hdel":
			for _, fl := range fv {
				delete(t[key], fl)
			}
			if len(t[key]) == 0 {
				delete(t, key) // redis drops an empty hash
			}
		default:
			return fmt.Errorf("unmodelled CONFIG_DB %s", op)
		}
		return nil
	})
}

// lldpcli models `configure|unconfigure ports <p> lldp custom-tlv ... oui
// 00,12,bb subtype 1|2 ...`, the only lldpcli writes the driver makes, in
// the lldp_med section's "<port> <caps>|- <policy>|-" lines.
func (f *FixtureRunner) lldpcli(a []string) error {
	line := strings.Join(a, " ")
	if len(a) < 6 || a[1] != "ports" || a[3] != "lldp" || a[4] != "custom-tlv" || !strings.Contains(line, "oui 00,12,bb subtype ") {
		return fmt.Errorf("unmodelled lldpcli %s", line)
	}
	port := a[2]
	if !f.hasPort(port) {
		return fmt.Errorf("lldpcli: no such port %s", port) // lldpd 0001: an error for a missing port
	}
	sub, info := "", "-"
	for i := 0; i+1 < len(a); i++ {
		switch a[i] {
		case "subtype":
			sub = a[i+1]
		case "oui-info":
			info = a[i+1]
		}
	}
	f.ensureSection("lldp_med")
	type tlvs struct{ caps, pol string }
	ports := map[string]tlvs{}
	for _, l := range strings.Split(sections(f.Fixture)["lldp_med"], "\n") {
		fs := strings.Fields(l)
		if len(fs) < 2 {
			continue
		}
		t := tlvs{caps: "-", pol: "-"}
		switch fs[1] {
		case "on":
			t.caps = medCaps
		case "off", "-":
		default:
			t.caps = fs[1]
		}
		if len(fs) > 2 {
			t.pol = fs[2]
		}
		ports[fs[0]] = t
	}
	t, ok := ports[port]
	if !ok {
		t = tlvs{caps: "-", pol: "-"}
	}
	if a[0] == "unconfigure" {
		info = "-"
	} else if a[0] != "configure" {
		return fmt.Errorf("unmodelled lldpcli %s", a[0])
	}
	switch sub {
	case "1":
		t.caps = info
	case "2":
		t.pol = info
	default:
		return fmt.Errorf("unmodelled lldpcli subtype %s", sub)
	}
	ports[port] = t
	names := make([]string, 0, len(ports))
	for p := range ports {
		names = append(names, p)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, p := range names {
		b.WriteString(p + " " + ports[p].caps + " " + ports[p].pol + "\n")
	}
	f.UseSection("lldp_med", b.String())
	return nil
}
