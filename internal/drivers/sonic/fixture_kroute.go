package sonic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The FixtureRunner's model of kroute.go's writes (§6.4.1) on the capture's
// metadata, frr_route and route sections. zebra's answer to the re-install
// -- the default back in the kernel through an inline nexthop, proto 196,
// metric 20 -- is the capture collect-route-fixed.txt shows after the same
// vtysh call on the DX010 (2026-11-15).

// krouteWrite applies one write if it is one of these; ok is false for any
// other command. w is the line's words without sudo.
func (f *FixtureRunner) krouteWrite(w []string) (ok bool, err error) {
	switch {
	case len(w) == 6 && strings.Join(w[:3], " ") == "sonic-db-cli CONFIG_DB hset" && w[3] == "DEVICE_METADATA|localhost":
		if w[4] != "zebra_nexthop" {
			return true, fmt.Errorf("unmodelled DEVICE_METADATA field %s", w[4])
		}
		return true, f.update("metadata", func(t table) error {
			if t[w[3]] == nil {
				return fmt.Errorf("unmodelled: no DEVICE_METADATA|localhost")
			}
			t[w[3]][w[4]] = w[5]
			return nil
		})
	case len(w) >= 4 && strings.Join(w[:4], " ") == "docker exec bgp vtysh":
		return true, f.vtyshLines(w[4:])
	}
	return false, nil
}

// vtyshLines models `vtysh -c 'configure terminal' -c <line>...`: the zebra
// switch, and a static line taken out and put back (zebra re-installs the
// route).
func (f *FixtureRunner) vtyshLines(args []string) error {
	if len(args) > 0 && args[len(args)-1] == "</dev/null" {
		args = args[:len(args)-1]
	}
	var lines []string
	for i := 0; i < len(args); i += 2 {
		if args[i] != "-c" || i+1 >= len(args) {
			return fmt.Errorf("unmodelled vtysh arguments %q", args)
		}
		lines = append(lines, args[i+1])
	}
	if len(lines) == 0 || lines[0] != "configure terminal" {
		return fmt.Errorf("vtysh: %q outside configure terminal", lines)
	}
	body := strings.TrimSpace(f.section("frr_route"))
	var sec map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &sec); err != nil {
		return fmt.Errorf("unmodelled: the capture has no frr_route section")
	}
	var static []string
	_ = json.Unmarshal(sec["static"], &static)
	isStatic := map[string]bool{}
	for _, l := range static {
		isStatic[l] = true
	}
	removed := map[string]bool{}
	reinstall := false
	for _, l := range lines[1:] {
		switch {
		case l == zebraNoKernelNH:
			sec["kernel_nexthops"] = json.RawMessage("false")
		case strings.HasPrefix(l, "no ") && isStatic[strings.TrimPrefix(l, "no ")]:
			removed[strings.TrimPrefix(l, "no ")] = true
		case isStatic[l] && removed[l]:
			reinstall = true
		default:
			return fmt.Errorf("unmodelled vtysh line %q", l)
		}
	}
	b, _ := json.Marshal(sec)
	f.setSection("frr_route", string(b))
	if !reinstall {
		return nil
	}
	k := parseKRoute4(f.section("frr_route"), f.section("route"))
	if !k.known {
		return fmt.Errorf("unmodelled: the capture's route section does not parse")
	}
	if k.kernelNH {
		return fmt.Errorf("unmodelled: a re-install through kernel nexthop objects")
	}
	r := k.installed()
	if r == nil {
		return nil
	}
	var routes []map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(f.section("route"))), &routes)
	var kept []map[string]any
	for _, kr := range routes {
		if kr["dst"] != "default" {
			kept = append(kept, kr)
		}
	}
	for _, h := range r.Nexthops {
		if h.Active {
			kept = append(kept, map[string]any{"dst": "default", "gateway": h.IP, "dev": h.Interface, "protocol": "196", "metric": 20, "flags": []string{}})
			break
		}
	}
	rb, _ := json.Marshal(kept)
	f.setSection("route", string(rb))
	return nil
}
