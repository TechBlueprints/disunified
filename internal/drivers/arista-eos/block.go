package aristaeos

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// UniFi's client Block (docs/drivers/arista-eos.md §9.9). The 7160 binds no
// MAC ACL to a switchport on 4.26.14M (§9.2), but it has static MAC drop
// entries: `mac address-table static <mac> vlan <v> drop`, which, as
// Arista's EOS manual says of drop entries, discards frames whose source or
// destination is that MAC in that VLAN (the command and its read-back form
// captured 2026-11-15, docs/fixtures/arista-eos-4.26.14M/mac-drop/). A Block
// is one entry per blocked client per VLAN the switch carries, as UniFi's
// own SONiC switch drops a blocked source in every VLAN (docs/drivers/
// sonic.md §11.33). The controller's list is the switch's: a drop entry it
// does not name goes, one made by hand included. A client that is the
// switch's own MAC or its gateway is held with a note.

var macDropRe = regexp.MustCompile(`^mac address-table static ([0-9a-f]{4}\.[0-9a-f]{4}\.[0-9a-f]{4}) vlan (\d+) drop$`)

// macDropsFrom are the running config's drop entries, as their lines.
func macDropsFrom(rc showRunningConfig) map[string]bool {
	out := map[string]bool{}
	for line := range rc.Cmds {
		if macDropRe.MatchString(line) {
			out[line] = true
		}
	}
	return out
}

// eosMAC is a MAC in EOS's dotted form (0200.0000.0001).
func eosMAC(hw net.HardwareAddr) string {
	h := strings.ReplaceAll(hw.String(), ":", "")
	return h[0:4] + "." + h[4:8] + "." + h[8:12]
}

// macDropLine is one drop entry as EOS shows it.
func macDropLine(hw net.HardwareAddr, vlan int) string {
	return "mac address-table static " + eosMAC(hw) + " vlan " + strconv.Itoa(vlan) + " drop"
}

// blockCommands are the config lines that take the switch's drop entries
// (have) to the controller's blocked clients in every VLAN of vlans.
func blockCommands(have map[string]bool, blocked []string, vlans []int, self, gateway string, note func(string, ...any)) (cmds []string, want map[string]bool) {
	want = map[string]bool{}
	for _, m := range blocked {
		hw, err := net.ParseMAC(m)
		if err != nil || len(hw) != 6 {
			note("blocked client %q: not a MAC address; not written", m)
			continue
		}
		if s := hw.String(); s == strings.ToLower(self) || s == strings.ToLower(gateway) {
			note("blocked client %s: it is the switch's own MAC or its gateway's; not written, or the switch would lose the controller", s)
			continue
		}
		if hw[0]&1 != 0 {
			note("blocked client %s: a multicast address, which a drop entry cannot hold; not written", hw)
			continue
		}
		for _, v := range vlans {
			want[macDropLine(hw, v)] = true
		}
	}
	var gone, add []string
	for l := range have {
		if !want[l] {
			gone = append(gone, "no "+l)
		}
	}
	for l := range want {
		if !have[l] {
			add = append(add, l)
		}
	}
	sort.Strings(gone)
	sort.Strings(add)
	return append(gone, add...), want
}

// blockNote logs a hold once per text (ApplyDevice runs every cycle).
func (c *Collector) blockNote(f string, a ...any) {
	msg := "arista: " + fmt.Sprintf(f, a...)
	c.mu.Lock()
	seen := c.blockNoted[msg]
	if c.blockNoted == nil {
		c.blockNoted = map[string]bool{}
	}
	c.blockNoted[msg] = true
	c.mu.Unlock()
	if !seen {
		c.logf("%s", msg)
	}
}

// blockCommandsFor is blockCommands for a desired state against the last
// collect; nil when the controller has not said which clients are blocked.
func (c *Collector) blockCommandsFor(d devicemodel.DeviceDesired, snap *devicemodel.Snapshot) ([]string, map[string]bool) {
	if !d.BlockedSet {
		return nil, nil
	}
	c.mu.Lock()
	have := c.macDrops
	c.mu.Unlock()
	return blockCommands(have, d.BlockedClients, snap.VLANs, snap.System.MAC, snap.System.GatewayMAC, c.blockNote)
}
