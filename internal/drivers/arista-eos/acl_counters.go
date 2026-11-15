package aristaeos

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The read-back of the driver's ACLs (docs/drivers/arista-eos.md §9.6): on
// every collect, whether each list is active in the hardware on the
// interface it is bound to, and the rogue DHCP replies its guard dropped.
//
// Every interface carries a list of its own with `counters per-entry`, so
// the guard's drop entry in that list counts the replies dropped on that
// interface alone (verified 2026-11-14: separate lists get separate
// hardware ACL ids, entries and counter indices; one list bound twice gets
// one set). The count is Port.Health.RogueDHCPDrops: the lanes of a split
// cage are summed into their port, and an aggregate's list is reported on
// its first member, the port UniFi shows the aggregate by.

// showIPACLs is `show ip access-lists <name>` (4.26.14M JSON, the fields
// read). An entry nothing has matched yet carries an empty counterData;
// a remark carries none.
type showIPACLs struct {
	ACLList []struct {
		Name            string `json:"name"`
		CountersEnabled bool   `json:"countersEnabled"`
		Sequence        []struct {
			SequenceNumber int    `json:"sequenceNumber"`
			Text           string `json:"text"`
			Action         string `json:"action"`
			CounterData    struct {
				PacketCount uint64 `json:"packetCount"`
			} `json:"counterData"`
		} `json:"sequence"`
	} `json:"aclList"`
}

// showIPACLSummary is `show ip access-lists summary` (4.26.14M JSON): where
// each list is configured and where the hardware has it.
type showIPACLSummary struct {
	ACLList []struct {
		Name                        string `json:"name"`
		NotFullySupportedByHardware bool   `json:"notFullySupportedByHardware"`
		// CountersIncomplete: some of the list's entries got no counter,
		// the chip's 4095 being taken (measured 2026-11-14, §9.1).
		CountersIncomplete     bool `json:"countersIncomplete"`
		ConfiguredIngressIntfs []struct {
			Name string `json:"name"`
		} `json:"configuredIngressIntfs"`
		ActiveIngressIntfs []struct {
			Name string `json:"name"`
		} `json:"activeIngressIntfs"`
	} `json:"aclList"`
}

// readACLs records the driver's lists from the running configuration and,
// when there are any, reads their state and guard counters into ports. A
// failed read is logged and leaves the counters unreported; it never fails
// the collect.
func (c *Collector) readACLs(ctx context.Context, rc showRunningConfig, sets ...[]devicemodel.Port) {
	st := aclFromRunningConfig(rc)
	c.mu.Lock()
	c.acl = st
	c.mu.Unlock()
	if len(st.lists) == 0 {
		return
	}
	names := make([]string, 0, len(st.lists))
	for n := range st.lists {
		names = append(names, n)
	}
	sort.Strings(names)
	cmds := []string{"enable", "show ip access-lists summary"}
	for _, n := range names {
		cmds = append(cmds, "show ip access-lists "+n)
	}
	out, err := c.t.Run(ctx, cmds)
	if err == nil && len(out) != len(cmds) {
		err = fmt.Errorf("%d replies to %d commands", len(out), len(cmds))
	}
	if err != nil {
		c.logf("arista: ACL read-back: %v; rogue DHCP counts not reported this cycle", err)
		return
	}
	var sum showIPACLSummary
	incomplete := map[string]bool{}
	if err := json.Unmarshal(out[1], &sum); err != nil {
		c.logf("arista: ACL read-back: summary: %v", err)
	} else {
		c.noteACLStatus(aclStatus(sum, st))
		for _, a := range sum.ACLList {
			incomplete[a.Name] = a.CountersIncomplete
		}
	}
	drops := map[string]uint64{}
	for i, n := range names {
		if incomplete[n] {
			continue // the drop entry may be one without a counter: no count beats a false 0
		}
		var l showIPACLs
		if err := json.Unmarshal(out[2+i], &l); err != nil {
			c.logf("arista: ACL read-back: %s: %v", n, err)
			continue
		}
		if d, ok := guardDrops(l, n); ok {
			drops[n] = d
		}
	}
	for _, ports := range sets {
		applyGuardDrops(ports, st, drops)
	}
}

// aclStatus is every way the hardware disagrees with the configuration for
// the driver's lists, "" when it agrees: a list not active on an interface
// it is bound to, or one with a rule the hardware cannot hold in full.
func aclStatus(sum showIPACLSummary, st aclState) string {
	var problems []string
	for _, a := range sum.ACLList {
		if _, ours := st.lists[a.Name]; !ours {
			continue
		}
		active := map[string]bool{}
		for _, i := range a.ActiveIngressIntfs {
			active[i.Name] = true
		}
		for _, i := range a.ConfiguredIngressIntfs {
			if !active[i.Name] {
				problems = append(problems, a.Name+" configured on "+i.Name+" but not active there")
			}
		}
		if a.NotFullySupportedByHardware {
			problems = append(problems, a.Name+" has rules the hardware does not fully support")
		}
		if a.CountersIncomplete {
			problems = append(problems, a.Name+" has entries without a counter (the chip's counters are taken); its rogue DHCP count is not reported")
		}
	}
	sort.Strings(problems)
	return strings.Join(problems, "; ")
}

// noteACLStatus logs the hardware state when it changes.
func (c *Collector) noteACLStatus(s string) {
	c.mu.Lock()
	changed := s != c.aclStatus
	c.aclStatus = s
	c.mu.Unlock()
	if !changed {
		return
	}
	if s == "" {
		c.logf("arista: ACL read-back: every list of the driver's is active where it is bound")
		return
	}
	c.logf("arista: ACL read-back: %s", s)
}

// guardDrops is the packets the guard block's drop entries of one list have
// matched since the list was written, and whether the list has a guard
// whose entries are counted at all.
func guardDrops(l showIPACLs, name string) (uint64, bool) {
	for _, a := range l.ACLList {
		if a.Name != name {
			continue
		}
		if !a.CountersEnabled {
			return 0, false
		}
		var n uint64
		inGuard, found := false, false
		for _, s := range a.Sequence {
			if f := strings.Fields(s.Text); len(f) >= 3 && f[0] == "remark" && f[1] == aclRemark {
				inGuard = f[2] == blockDHCPGuard
				found = found || inGuard
				continue
			}
			if inGuard && s.Action == "deny" {
				n += s.CounterData.PacketCount
			}
		}
		return n, found
	}
	return 0, false
}

// applyGuardDrops adds each list's count to the port its interface belongs
// to. A list bound to more than one interface (by hand) counts for all of
// them together and is reported on none.
func applyGuardDrops(ports []devicemodel.Port, st aclState, drops map[string]uint64) {
	uses := map[string]int{}
	for _, name := range st.bound {
		uses[name]++
	}
	owner := map[string]int{} // interface -> index into ports
	for i, p := range ports {
		if p.LAGID > 0 {
			po := "Port-Channel" + fmt.Sprint(p.LAGID)
			if j, ok := owner[po]; !ok || p.Index < ports[j].Index {
				owner[po] = i
			}
			continue
		}
		for _, l := range laneNames(p) {
			owner[l] = i
		}
	}
	for iface, name := range st.bound {
		d, ok := drops[name]
		if !ok || uses[name] != 1 {
			continue
		}
		if i, ok := owner[iface]; ok {
			ports[i].Health.RogueDHCPDrops += d
		}
	}
}

func (c *Collector) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log.Printf(format, args...)
	}
}
