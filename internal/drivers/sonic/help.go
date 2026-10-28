package sonic

import (
	"context"
	"fmt"
	"strings"
)

// commandChecks are the `config` groups the driver writes through and the
// sub-commands it needs in each (§6.1). Start reads each group's --help and
// fails loudly when one is missing: SONiC moves commands between releases
// (`config interface description` does not exist on 202505).
var commandChecks = []struct {
	group string
	subs  []string
	stp   bool // checked only when the image has the STP daemon
}{
	{group: "interface", subs: []string{"startup", "shutdown", "speed", "autoneg", "fec", "mtu", "storm-control", "breakout", "ip"}},
	{group: "vlan", subs: []string{"add", "member"}},
	{group: "vlan member", subs: []string{"add", "del"}},
	{group: "portchannel", subs: []string{"add", "del", "member"}},
	{group: "portchannel member", subs: []string{"add", "del"}},
	{group: "mirror_session", subs: []string{"span", "remove"}},
	{group: "mirror_session span", subs: []string{"add"}},
	{group: "interface storm-control", subs: []string{"add", "del"}},
	{group: "spanning-tree", subs: []string{"enable", "disable", "priority", "interface"}, stp: true},
	{group: "spanning-tree interface", subs: []string{"bpdu_guard", "portfast", "enable", "disable", "cost"}, stp: true},
	{group: "ntp", subs: []string{"add", "del"}},
	{group: "syslog", subs: []string{"add", "del"}},
	{group: "snmp community", subs: []string{"add", "del"}},
	{group: "interface ip", subs: []string{"add", "remove"}},
	{group: "route", subs: []string{"add", "del"}},
	{group: "dns nameserver", subs: []string{"add", "del"}},
	{group: "clock", subs: []string{"timezone"}},
	{group: "", subs: []string{"hostname"}},
}

// helpCommand is how a group's help is read: `config` needs root even for
// --help.
func helpCommand(group string) string {
	if group == "" {
		return "sudo config --help"
	}
	return "sudo config " + group + " --help"
}

// verifyCommands checks every sub-command the driver uses exists.
func (c *Collector) verifyCommands(ctx context.Context) error {
	c.mu.Lock()
	stp := c.state != nil && c.state.stpFeature
	c.mu.Unlock()
	for _, chk := range commandChecks {
		if chk.stp && !stp {
			continue
		}
		out, err := c.run(ctx, helpCommand(chk.group), "")
		if err != nil {
			return fmt.Errorf("sonic: `%s` failed (the bridge user needs passwordless sudo for config): %w", strings.TrimPrefix(helpCommand(chk.group), "sudo "), err)
		}
		have := helpSubcommands(out)
		var missing []string
		for _, s := range chk.subs {
			if !have[s] {
				missing = append(missing, s)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("sonic: `%s` lacks %s on this image; the driver needs them (docs/drivers/sonic.md §6)", strings.TrimSpace("config "+chk.group), strings.Join(missing, ", "))
		}
	}
	return nil
}

// helpSubcommands reads the names under a click "Commands:" heading.
func helpSubcommands(help string) map[string]bool {
	out := map[string]bool{}
	in := false
	for _, line := range strings.Split(help, "\n") {
		if strings.TrimSpace(line) == "Commands:" {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			if strings.TrimSpace(line) != "" {
				in = false
			}
			continue
		}
		if f := strings.Fields(line); len(f) > 0 {
			out[f[0]] = true
		}
	}
	return out
}
