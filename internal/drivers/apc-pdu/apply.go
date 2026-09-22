package apcpdu

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// ApplyOutlets makes the listed outlets match desired. It is a diff: the loop
// re-applies the same intent on every cycle that carries no new push, and an
// outlet relay is real load, so only differences are written.
//
// The two halves go to different transports because the card allows nothing
// else. Switching is an SNMP SET on the control table. Naming is a partial
// config.ini uploaded over FTP, because the outlet-name objects are read-only
// over SNMP. The names are batched into ONE upload: the card applies a config
// asynchronously, a few seconds after the transfer, and a second upload
// arriving while the first is still being applied is silently dropped.
func (c *Collector) ApplyOutlets(ctx context.Context, desired []devicemodel.OutletDesired) (int, error) {
	c.mu.Lock()
	last := c.last
	c.mu.Unlock()
	if last == nil {
		return 0, fmt.Errorf("apc-pdu: no snapshot yet")
	}
	cur := map[int]devicemodel.Outlet{}
	for _, o := range last.Outlets {
		cur[o.Index] = o
	}

	changed := 0
	names := map[int]string{}
	var errs []string
	for _, d := range desired {
		have, ok := cur[d.Index]
		if !ok {
			// The controller addressed an outlet this PDU does not have.
			// Ignore it rather than guessing an OID: writing a command to an
			// index the card does not know is how you find out it does.
			c.warnOnce(fmt.Sprintf("outlet %d", d.Index),
				fmt.Sprintf("apc-pdu: controller pushed outlet %d, device has %d outlets; ignored", d.Index, len(last.Outlets)))
			continue
		}
		if have.Switchable && d.On != have.On {
			cmd := outletCmdOff
			if d.On {
				cmd = outletCmdOn
			}
			if err := c.r.SetInt(ctx, fmt.Sprintf("%s.%d", oidOutletCtlCmd, d.Index), cmd); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			changed++
		}
		if d.Name != "" && d.Name != have.Name {
			names[d.Index] = d.Name
		}
	}

	if len(names) > 0 {
		if err := c.r.PutConfig(ctx, outletNameConfig(names)); err != nil {
			errs = append(errs, err.Error())
		} else {
			changed += len(names)
		}
	}
	if len(errs) > 0 {
		return changed, fmt.Errorf("apc-pdu: %s", strings.Join(errs, "; "))
	}
	return changed, nil
}

// outletNameConfig renders the partial config.ini that renames outlets. A bare
// section plus the keys is enough -- the card does not need the file's header
// or the rest of its configuration -- and it applies live, with no reboot.
//
// The card truncates a name at 23 characters, so it is cut here instead: a
// value the device silently shortens would differ from the desired name on
// every following cycle and be rewritten forever.
func outletNameConfig(names map[int]string) []byte {
	idx := make([]int, 0, len(names))
	for i := range names {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var b strings.Builder
	b.WriteString("[RackPDUOutlet]\r\n")
	for _, i := range idx {
		b.WriteString(fmt.Sprintf("Name%d=%s\r\n", i, truncateName(names[i])))
	}
	return []byte(b.String())
}

// outletNameMax is the card's own limit on an outlet name.
const outletNameMax = 23

func truncateName(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	if len(s) > outletNameMax {
		return s[:outletNameMax]
	}
	return s
}

// CycleOutlet power-cycles one outlet: the card's own immediate-reboot
// command, which is an off/on with the configured reboot duration between.
func (c *Collector) CycleOutlet(ctx context.Context, idx int) error {
	return c.r.SetInt(ctx, fmt.Sprintf("%s.%d", oidOutletCtlCmd, idx), outletCmdReboot)
}

func (c *Collector) warnOnce(key, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.warned[key] {
		return
	}
	c.warned[key] = true
	log.Print(msg)
}
