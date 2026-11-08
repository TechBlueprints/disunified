package sonic

import (
	"fmt"
	"strings"
)

// loadBGP models `vtysh -f` over the capture's frr_bgp section (bgp.go): a
// top-level `no <object>` removes an object bgpd runs (FRR refuses one it
// does not have), any other top-level line opening an object adds it. A
// capture without the section has no readable bgpd.
func (f *FixtureRunner) loadBGP(file string) error {
	if f.BGPRefuse != "" {
		return fmt.Errorf("exit status 2: %s", f.BGPRefuse)
	}
	body, ok := sections(f.Fixture)["frr_bgp"]
	if !ok || strings.TrimSpace(body) == "error" {
		return fmt.Errorf("vtysh: bgpd is not running")
	}
	var order []string
	have := map[string]bool{}
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l != "" && !have[l] {
			have[l] = true
			order = append(order, l)
		}
	}
	for i, line := range strings.Split(file, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if o, ok := strings.CutPrefix(line, "no "); ok {
			if !have[o] {
				return fmt.Errorf("line %d: %% %s does not exist", i+1, o)
			}
			delete(have, o)
			continue
		}
		if o := frrObject(line); o != "" && !have[o] {
			have[o] = true
			order = append(order, o)
		}
	}
	var out []string
	for _, o := range order {
		if have[o] {
			out = append(out, o)
		}
	}
	f.UseSection("frr_bgp", strings.Join(out, "\n"))
	return nil
}
