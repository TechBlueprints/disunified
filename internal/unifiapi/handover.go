package unifiapi

import (
	"context"
	"fmt"
)

// The lane switch's hand-over (docs/drivers/breakout.md 16.7). While a split
// cage is its device's fold it reports its lanes' form (SFP28 for 25G
// lanes), and the operator may have saved a forced lane speed with a FEC on
// it. Once a lane switch has the lanes the cage reports as QSFP28 again,
// for which the controller refuses that pair -- and it checks every entry
// of port_overrides on every save, so each save of any port of the device
// is refused (api.err.InvalidFecMode with the cage's port_idx; Network
// 11.0.99, 2026-11-16) until the entry changes.
//
// This is the one place the bridge changes an override an operator
// configured: the cage's entry goes to Auto, which on a cage with a lane
// switch keeps the cage as it is (the module's layout, or the mode it has).

// laneSpeedFloor is the lowest speed (Mb/s) the controller takes on a
// QSFP28 port together with a FEC: a forced speed below it is a lane speed.
const laneSpeedFloor = 40000

// cageToAuto returns the overrides with port's entry set to Auto when it
// holds a forced lane speed: autoneg true; speed, full_duplex and fec_mode
// removed. before holds those four keys as they were; changed is false
// (and the list is existing) when the entry needs nothing.
func cageToAuto(existing []map[string]any, port int) (list []map[string]any, before map[string]any, changed bool) {
	for i, o := range existing {
		if overrideIndex(o) != port {
			continue
		}
		speed, _ := o["speed"].(float64)
		if an, ok := o["autoneg"].(bool); !ok || an || speed <= 0 || speed >= laneSpeedFloor {
			return existing, nil, false
		}
		before = map[string]any{}
		next := map[string]any{}
		for k, v := range o {
			switch k {
			case "autoneg", "speed", "full_duplex", "fec_mode":
				before[k] = v
			default:
				next[k] = v
			}
		}
		next["autoneg"] = true
		list = append(append(append([]map[string]any{}, existing[:i]...), next), existing[i+1:]...)
		return list, before, true
	}
	return existing, nil, false
}

// CageToAuto sets the port's override on the device to Auto when it holds
// a forced speed below 40G (see above), one read-modify-write of
// port_overrides. before is the entry's speed keys as they were; nil when
// nothing needed changing.
func (c *Client) CageToAuto(ctx context.Context, mac string, port int) (before map[string]any, err error) {
	dev, err := c.DeviceByMAC(ctx, mac)
	if err != nil {
		return nil, err
	}
	list, before, changed := cageToAuto(dev.PortOverrides, port)
	if !changed {
		return nil, nil
	}
	if err := c.UpdateDevice(ctx, dev.ID, map[string]any{"port_overrides": list}); err != nil {
		return nil, fmt.Errorf("port %d to Auto: %w", port, err)
	}
	return before, nil
}
