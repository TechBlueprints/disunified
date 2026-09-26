package apcups

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The outlet command word: two registers at 1538, big-endian by register,
// carrying a command bit and a target-group bit -- exactly what NUT's
// apc_modbus builds for load.on / load.off / load.cycle on a group
// (drivers/apc_modbus.c, _apc_modbus_build_outlet_cmd + _apc_modbus_from_uint64).
// Only the immediate forms are used: the delay modifiers are the unit's own
// scheduling, which the controller does not drive.
//
// This path has NOT been exercised against the unit (Clint's decision,
// 2026-09-26): the frame is held byte-exact to the reference encoding by the
// tests, the apply is a strict diff, and the Main group is never a target.
const (
	regOutletCommand = 1538

	cmdCancel         = 1 << 0
	cmdOutputOn       = 1 << 1
	cmdOutputOff      = 1 << 2
	cmdOutputShutdown = 1 << 3
	cmdOutputReboot   = 1 << 4
	modUseOnDelay     = 1 << 6
	modUseOffDelay    = 1 << 7
)

// groupTarget is the target bit for outlet group g (0 = Main, 1..3 = the
// switched groups): bit 8 + g. Main's bit exists in the register; nothing
// here ever sets it.
func groupTarget(g int) uint32 { return 1 << (8 + uint(g)) }

// commandWords renders cmd|target as the two registers the unit expects:
// the first holds the high word (NUT's _apc_modbus_from_uint64 order).
func commandWords(cmd, target uint32) []uint16 {
	v := cmd | target
	return []uint16{uint16(v >> 16), uint16(v)}
}

// outletsByIndex is the last snapshot's outlets, keyed by index.
func (c *Collector) outletsByIndex() (map[int]devicemodel.Outlet, error) {
	c.mu.Lock()
	last := c.last
	c.mu.Unlock()
	if last == nil {
		return nil, fmt.Errorf("apc-ups: no snapshot yet")
	}
	cur := make(map[int]devicemodel.Outlet, len(last.Outlets))
	for _, o := range last.Outlets {
		cur[o.Index] = o
	}
	return cur, nil
}

// ApplyOutlets makes the listed outlet groups match desired. It is a strict
// diff: the loop re-applies the same intent on every cycle, and a group
// relay is a rack of load, so only a difference is written. The Main group
// and unknown indices are never written -- warned once and left alone, so
// a controller that keeps asking is corrected by the reported table rather
// than obeyed. Names are ignored: the controller owns them and never pushes
// them.
func (c *Collector) ApplyOutlets(ctx context.Context, desired []devicemodel.OutletDesired) (int, error) {
	cur, err := c.outletsByIndex()
	if err != nil {
		return 0, err
	}
	changed := 0
	var errs []string
	for _, d := range desired {
		have, ok := cur[d.Index]
		if !ok {
			c.warnOnce(fmt.Sprintf("outlet %d", d.Index), fmt.Sprintf("apc-ups: controller pushed outlet %d, the unit has %d outlet groups; ignored", d.Index, len(cur)))
			continue
		}
		if d.On == have.On {
			continue
		}
		if !have.Switchable {
			c.warnOnce("main-group", fmt.Sprintf("apc-ups: controller asked to switch outlet %d (the unswitched main group) %s; refused -- it carries the whole output", d.Index, onOff(d.On)))
			continue
		}
		cmd := uint32(cmdOutputOff)
		if d.On {
			cmd = cmdOutputOn
		}
		if err := c.r.WriteRegisters(ctx, regOutletCommand, commandWords(cmd, groupTarget(d.Index-1))); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		log.Printf("apc-ups: outlet %d switched %s", d.Index, onOff(d.On))
		changed++
	}
	if len(errs) > 0 {
		return changed, fmt.Errorf("apc-ups: %s", strings.Join(errs, "; "))
	}
	return changed, nil
}

// PlanOutlets reports which outlets ApplyOutlets would switch, without
// writing: the loop holds the first push of a run when this is non-empty.
func (c *Collector) PlanOutlets(desired []devicemodel.OutletDesired) []int {
	cur, err := c.outletsByIndex()
	if err != nil {
		return nil
	}
	var changed []int
	for _, d := range desired {
		if have, ok := cur[d.Index]; ok && have.Switchable && d.On != have.On {
			changed = append(changed, d.Index)
		}
	}
	return changed
}

// CycleOutlet power-cycles one switched group: the unit's own reboot
// command (relay open for its configured reboot delay, then closed). The
// Main group is refused.
func (c *Collector) CycleOutlet(ctx context.Context, idx int) error {
	cur, err := c.outletsByIndex()
	if err != nil {
		return err
	}
	have, ok := cur[idx]
	if !ok {
		return fmt.Errorf("apc-ups: outlet %d: the unit has no such outlet group", idx)
	}
	if !have.Switchable {
		return fmt.Errorf("apc-ups: outlet %d is the unswitched main group; a power cycle would drop the whole output; refused", idx)
	}
	return c.r.WriteRegisters(ctx, regOutletCommand, commandWords(cmdOutputReboot, groupTarget(idx-1)))
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

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
