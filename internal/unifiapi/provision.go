package unifiapi

import (
	"context"
	"fmt"
	"sort"

	"github.com/TechBlueprints/disunified/internal/switchmodel"
)

// ProvisionNames names the device and its ports after the switch using
// namer: on first provision and whenever the port layout changes. Only
// names that are still controller defaults are touched, so operator renames
// survive. isDefaultPortName reports whether a controller-side port name is
// a default for that port index (the caller combines the namer's defaults
// with the profile's and the controller's generic names).
func (c *Client) ProvisionNames(ctx context.Context, mac string, snap *switchmodel.Snapshot, namer switchmodel.Namer, defaultDeviceNames []string, isDefaultPortName func(idx int, name string) bool) (renamedDevice bool, renamedPorts int, err error) {
	r, err := c.Provision(ctx, mac, snap, namer, defaultDeviceNames, isDefaultPortName, false)
	return r.RenamedDevice, r.RenamedPorts, err
}

// ProvisionResult is what one Provision call changed.
type ProvisionResult struct {
	RenamedDevice bool
	RenamedPorts  int
	Seeded        int      // ports whose config was written from the switch
	Cleared       int      // released slots whose stale overrides were dropped
	Notes         []string // ports that could not be seeded, with the reason
}

// Provision names the device and its ports (see ProvisionNames) and, when
// seed is set, also writes each configured port's live state as its port
// override where the controller has none yet (see SeedPortConfig), in one
// read-modify-write: two separate updates raced on a stale read and lost a
// name (2026-09-20). Slots that are empty in the snapshot (a guest deleted)
// lose their override except its name and, when seeding, are written
// disabled as the switch reports them; a later guest on that slot is
// seeded from its own state (see seedOverrides).
func (c *Client) Provision(ctx context.Context, mac string, snap *switchmodel.Snapshot, namer switchmodel.Namer, defaultDeviceNames []string, isDefaultPortName func(idx int, name string) bool, seed bool) (ProvisionResult, error) {
	var res ProvisionResult
	dev, err := c.DeviceByMAC(ctx, mac)
	if err != nil {
		return res, err
	}
	var nets []Network
	if seed {
		if nets, err = c.Networks(ctx); err != nil {
			return res, err
		}
	}
	fields := map[string]any{}

	want := namer.DeviceName(snap.System)
	isDefault := dev.Name == "" || dev.Name == dev.Model
	for _, d := range append(append([]string(nil), defaultDeviceNames...), namer.DefaultDeviceNames(snap.System)...) {
		isDefault = isDefault || dev.Name == d
	}
	if isDefault && want != "" && dev.Name != want {
		fields["name"] = want
		res.RenamedDevice = true
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
		if wantName == "" || cur == wantName || (cur != "" && !isDefaultPortName(p.Index, cur)) {
			continue
		}
		o := overrides[p.Index]
		if o == nil {
			o = map[string]any{"port_idx": p.Index}
			overrides[p.Index] = o
		}
		o["name"] = wantName
		changed = true
		res.RenamedPorts++
	}
	// Released slots: drop whatever config the controller kept for them.
	for _, p := range snap.Ports {
		if p.IfName == "" && !p.Present {
			if o, ok := overrides[p.Index]; ok && releaseOverride(o) {
				changed = true
				res.Cleared++
				if len(o) == 1 { // nothing but port_idx
					delete(overrides, p.Index)
				}
			}
		}
	}
	if seed {
		list := make([]map[string]any, 0, len(overrides))
		for _, o := range overrides {
			list = append(list, o)
		}
		var seeded []map[string]any
		seeded, res.Seeded, res.Notes = seedOverrides(snap, nets, list, isDefaultPortName)
		if res.Seeded > 0 {
			overrides = map[int]map[string]any{}
			for _, o := range seeded {
				overrides[overrideIndex(o)] = o
			}
			changed = true
		}
	}
	if changed {
		list := make([]map[string]any, 0, len(overrides))
		for _, o := range overrides {
			list = append(list, o)
		}
		sort.Slice(list, func(i, j int) bool { return overrideIndex(list[i]) < overrideIndex(list[j]) })
		fields["port_overrides"] = list
	}
	if len(fields) == 0 {
		return res, nil
	}
	for _, o := range dev.OOBPortConfig {
		if on, _ := o["enabled"].(bool); on {
			fields["oob_port_config"] = []map[string]any{} // see Device.OOBPortConfig
		}
	}
	if err := c.UpdateDevice(ctx, dev.ID, fields); err != nil {
		return res, fmt.Errorf("provision: %w", err)
	}
	return res, nil
}

// overrideIndex reads port_idx whether it came from JSON (float64) or us (int).
func overrideIndex(o map[string]any) int {
	switch v := o["port_idx"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}
