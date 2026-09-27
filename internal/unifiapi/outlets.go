package unifiapi

import (
	"context"
	"fmt"
	"sort"
)

// SeedOutletOverrides sets the controller's override to relay_state:false
// for each of placeholders -- the outlet slots the claimed model draws but
// the device does not have (device.PlaceholderOutlets). The device reports
// those rows off with no relay on every inform, but the controller's picture
// and stored table follow its override, whose default is "on" (a UPS 2U
// adopted 2026-09-26 drew all eight outlets powered with two real). Rows the
// device has are never touched, and an override that already says off is
// left alone, so a steady state writes nothing. Returns how many overrides
// changed.
func (c *Client) SeedOutletOverrides(ctx context.Context, mac string, placeholders []int) (int, error) {
	if len(placeholders) == 0 {
		return 0, nil
	}
	dev, err := c.DeviceByMAC(ctx, mac)
	if err != nil {
		return 0, err
	}
	list, changed := placeholderOverrides(dev.OutletOverrides, placeholders)
	if changed == 0 {
		return 0, nil
	}
	fields := map[string]any{"outlet_overrides": list}
	for _, o := range dev.OOBPortConfig {
		if on, _ := o["enabled"].(bool); on {
			fields["oob_port_config"] = []map[string]any{} // see Device.OOBPortConfig
		}
	}
	if err := c.UpdateDevice(ctx, dev.ID, fields); err != nil {
		return 0, fmt.Errorf("seed outlet overrides: %w", err)
	}
	return changed, nil
}

// placeholderOverrides returns existing with relay_state forced off on the
// placeholder indices (an override is added for a placeholder that has
// none), sorted by index, and how many entries changed. Every other entry
// is passed through untouched, names included.
func placeholderOverrides(existing []map[string]any, placeholders []int) ([]map[string]any, int) {
	byIdx := map[int]map[string]any{}
	list := make([]map[string]any, 0, len(existing)+len(placeholders))
	for _, o := range existing {
		list = append(list, o)
		byIdx[outletIndex(o)] = o
	}
	changed := 0
	for _, i := range placeholders {
		o, ok := byIdx[i]
		if !ok {
			o = map[string]any{"index": i}
			byIdx[i] = o
			list = append(list, o)
		}
		if on, isBool := o["relay_state"].(bool); !isBool || on {
			o["relay_state"] = false
			changed++
		}
	}
	sort.SliceStable(list, func(a, b int) bool { return outletIndex(list[a]) < outletIndex(list[b]) })
	return list, changed
}

// outletIndex reads index whether it came from JSON (float64) or us (int).
func outletIndex(o map[string]any) int {
	switch v := o["index"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}
