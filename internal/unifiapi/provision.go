package unifiapi

import (
	"context"
	"fmt"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// ProvisionNames names the device and its ports after the switch using
// namer: on first provision and whenever the port layout changes. Only
// names that are still controller defaults are touched, so operator renames
// survive. isDefaultPortName reports whether a controller-side port name is
// a default for that port index (the caller combines the namer's defaults
// with the profile's and the controller's generic names).
func (c *Client) ProvisionNames(ctx context.Context, mac string, snap *switchmodel.Snapshot, namer switchmodel.Namer, defaultDeviceNames []string, isDefaultPortName func(idx int, name string) bool) (renamedDevice bool, renamedPorts int, err error) {
	dev, err := c.DeviceByMAC(ctx, mac)
	if err != nil {
		return false, 0, err
	}
	fields := map[string]any{}

	want := namer.DeviceName(snap.System)
	isDefault := dev.Name == "" || dev.Name == dev.Model
	for _, d := range defaultDeviceNames {
		isDefault = isDefault || dev.Name == d
	}
	if isDefault && want != "" && dev.Name != want {
		fields["name"] = want
		renamedDevice = true
	}

	// Current names as the controller shows them: the override wins, else
	// the port_table name it reports back.
	current := map[int]string{}
	for _, p := range dev.PortTable {
		current[p.PortIdx] = p.Name
	}
	overrides := map[int]map[string]any{}
	for _, o := range dev.PortOverrides {
		idx, ok := o["port_idx"].(float64)
		if !ok {
			continue
		}
		overrides[int(idx)] = o
		if n, ok := o["name"].(string); ok && n != "" {
			current[int(idx)] = n
		}
	}
	changed := false
	for _, p := range snap.Ports {
		wantName := namer.PortName(p)
		cur := current[p.Index]
		if cur == wantName || (cur != "" && !isDefaultPortName(p.Index, cur)) {
			continue
		}
		o := overrides[p.Index]
		if o == nil {
			o = map[string]any{"port_idx": p.Index}
			overrides[p.Index] = o
		}
		o["name"] = wantName
		changed = true
		renamedPorts++
	}
	if changed {
		list := make([]map[string]any, 0, len(overrides))
		for _, o := range overrides {
			list = append(list, o)
		}
		fields["port_overrides"] = list
	}
	if len(fields) == 0 {
		return false, 0, nil
	}
	if err := c.UpdateDevice(ctx, dev.ID, fields); err != nil {
		return false, 0, fmt.Errorf("provision names: %w", err)
	}
	return renamedDevice, renamedPorts, nil
}
