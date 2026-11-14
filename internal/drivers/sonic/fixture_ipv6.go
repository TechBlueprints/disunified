package sonic

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The FixtureRunner's model of ipv6.go's kernel writes (§11.22) on the
// capture's ipv6_ra and addr sections. The kernel's own answer to
// accept_ra 2 -- a SLAAC address and the RA's routes, once the next RA
// arrives -- is not modelled: the captures before and after it are
// (collect-ipv6-off.txt, collect-ipv6-slaac.txt, collect-ipv6-on.txt).

var acceptRARe = regexp.MustCompile(`^echo ([0-2]) \| sudo tee /proc/sys/net/ipv6/conf/(\S+)/accept_ra >/dev/null$`)

// ipv6Write applies one write if it is one of these; ok is false for any
// other command. w is the line's words without sudo.
func (f *FixtureRunner) ipv6Write(line string, w []string) (ok bool, err error) {
	if m := acceptRARe.FindStringSubmatch(line); m != nil {
		return true, f.raSection(func(ra map[string]string, _ *[]raRoute) error {
			if _, ok := ra[m[2]]; !ok {
				return fmt.Errorf("tee: /proc/sys/net/ipv6/conf/%s/accept_ra: No such file or directory", m[2])
			}
			ra[m[2]] = m[1]
			return nil
		})
	}
	switch {
	case len(w) == 9 && strings.Join(w[:4], " ") == "ip -6 addr flush" && w[4] == "dev" && strings.Join(w[6:], " ") == "scope global dynamic":
		return true, f.flushDynamic6(w[5])
	case len(w) == 8 && strings.Join(w[:4], " ") == "ip -6 route flush" && w[4] == "dev" && w[6] == "proto":
		return true, f.raSection(func(_ map[string]string, rt *[]raRoute) error {
			var keep []raRoute
			for _, r := range *rt {
				if !(r.Dev == w[5] && r.Protocol == w[7]) {
					keep = append(keep, r)
				}
			}
			*rt = keep
			return nil
		})
	case len(w) == 7 && strings.Join(w[:4], " ") == "ip -6 route del" && w[5] == "dev":
		return true, f.raSection(func(_ map[string]string, rt *[]raRoute) error {
			for i, r := range *rt {
				if r.Dst == w[4] && r.Dev == w[6] {
					*rt = append((*rt)[:i], (*rt)[i+1:]...)
					return nil
				}
			}
			return fmt.Errorf("exit status 2: RTNETLINK answers: No such process")
		})
	case len(w) >= 2 && w[0] == "ip" && w[1] == "-6":
		return true, fmt.Errorf("unmodelled: %s", strings.Join(w, " "))
	}
	return false, nil
}

// raSection edits the capture's ipv6_ra section in collect.py's layout.
func (f *FixtureRunner) raSection(fn func(map[string]string, *[]raRoute) error) error {
	var sec struct {
		AcceptRA map[string]string `json:"accept_ra"`
		Routes   []raRoute         `json:"routes"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(f.section("ipv6_ra"))), &sec); err != nil || sec.AcceptRA == nil {
		return fmt.Errorf("unmodelled: the capture has no ipv6_ra section")
	}
	if err := fn(sec.AcceptRA, &sec.Routes); err != nil {
		return err
	}
	if sec.Routes == nil {
		sec.Routes = []raRoute{}
	}
	ra, _ := json.Marshal(sec.AcceptRA)
	rt, _ := json.Marshal(sec.Routes)
	f.setRaw("ipv6_ra", map[string]json.RawMessage{"accept_ra": ra, "routes": rt})
	return nil
}

// flushDynamic6 drops an interface's dynamic global IPv6 addresses from
// the capture's `ip -j addr`.
func (f *FixtureRunner) flushDynamic6(iface string) error {
	var links []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(f.section("addr"))), &links); err != nil {
		return fmt.Errorf("addr section: %w", err)
	}
	found := false
	for _, l := range links {
		if l["ifname"] != iface {
			continue
		}
		found = true
		infos, _ := l["addr_info"].([]any)
		var kept []any
		for _, a := range infos {
			m, _ := a.(map[string]any)
			if m["family"] == "inet6" && m["scope"] == "global" && m["dynamic"] == true {
				continue
			}
			kept = append(kept, a)
		}
		l["addr_info"] = kept
	}
	if !found {
		return fmt.Errorf("exit status 1: Device %q does not exist", iface)
	}
	b, err := json.Marshal(links)
	if err != nil {
		return err
	}
	f.setSection("addr", string(b))
	return nil
}
