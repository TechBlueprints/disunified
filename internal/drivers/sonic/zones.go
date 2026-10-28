package sonic

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The controller's time zone (docs/drivers/sonic.md §7.1, §7.5). UniFi
// pushes system.timezone as a POSIX TZ string ("MST7MDT,M3.2.0,M11.1.0" on
// 10.6.106, in every push), while `config clock timezone` takes only a zone
// name from `timedatectl list-timezones` (it refuses anything else: "does
// not conform format"). So the driver reads the switch's own zone list with
// each zone's POSIX rule -- the last line of its TZif file -- once per run,
// and maps the pushed rule onto a zone that has it.

// zonesCommand prints every zone timedatectl lists with its POSIX rule, as
// a tagged section ("<zone>\t<rule>" per line). Read-only, no sudo; ~0.7 s
// for ~600 zones on the VS.
const zonesCommand = "bash -s -- disunified-zones"

const zonesScript = `echo '@@@ zones'
cd /usr/share/zoneinfo && timedatectl list-timezones | while IFS= read -r z; do printf '%s\t%s\n' "$z" "$(tail -n 1 "$z")"; done
echo '@@@ end'
`

type zone struct{ name, rule string }

func parseZones(out string) []zone {
	var zs []zone
	for _, line := range strings.Split(sections(out)["zones"], "\n") {
		name, rule, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if ok && name != "" {
			zs = append(zs, zone{name: name, rule: strings.TrimSpace(rule)})
		}
	}
	return zs
}

// zoneNameRe is what the driver ever writes as a zone: a conservative zone
// name (letters, digits, '/', '_', '+', '-'), never anything a shell or
// SONiC's CLI would read otherwise.
var zoneNameRe = regexp.MustCompile(`^[A-Za-z0-9/_+-]{1,64}$`)

// posixTZRe is what a pushed POSIX TZ string may contain; it is compared,
// never written.
var posixTZRe = regexp.MustCompile(`^[A-Za-z0-9<>+\-:,./]{1,64}$`)

// zoneTable is the switch's zone list, read once per run.
func (c *Collector) zoneTable(ctx context.Context) ([]zone, error) {
	c.mu.Lock()
	zs := c.zones
	c.mu.Unlock()
	if zs != nil {
		return zs, nil
	}
	out, err := c.run(ctx, zonesCommand, zonesScript)
	if err != nil {
		return nil, fmt.Errorf("reading the switch's time zones: %w", err)
	}
	zs = parseZones(out)
	if len(zs) == 0 {
		return nil, fmt.Errorf("the switch listed no time zones (timedatectl list-timezones)")
	}
	c.mu.Lock()
	c.zones = zs
	c.mu.Unlock()
	return zs, nil
}

// zoneFor maps the pushed time zone onto the zone to configure, given the
// switch's current one. A zone name the switch lists is taken as is. A
// POSIX rule is matched against every zone's own rule: if the current zone
// already has it, nothing changes (an operator's America/Denver stays when
// the push says MST7MDT,M3.2.0,M11.1.0); otherwise the zone named like the
// rule's leading part (MST7MDT), else one named by its standard-time
// abbreviation (MST for "MST7"), else the first match by name. ok false
// with a reason when nothing on the switch has that rule.
func zoneFor(pushed, current string, zs []zone) (target string, ok bool, why string) {
	byName := map[string]string{}
	for _, z := range zs {
		byName[z.name] = z.rule
	}
	if _, listed := byName[pushed]; listed {
		return pushed, true, ""
	}
	if !posixTZRe.MatchString(pushed) {
		return "", false, fmt.Sprintf("%q is neither a zone this switch lists nor a POSIX TZ rule", pushed)
	}
	var matches []string
	for _, z := range zs {
		if z.rule == pushed {
			matches = append(matches, z.name)
		}
	}
	if len(matches) == 0 {
		return "", false, fmt.Sprintf("no zone on this switch has the rule %q", pushed)
	}
	sort.Strings(matches)
	if hasString(matches, current) {
		return current, true, ""
	}
	head, _, _ := strings.Cut(pushed, ",")
	std := strings.TrimLeft(head, "<")
	if i := strings.IndexAny(std, "0123456789+-<>"); i > 0 {
		std = std[:i]
	}
	for _, want := range []string{head, std} {
		if hasString(matches, want) {
			return want, true, ""
		}
	}
	return matches[0], true, ""
}
