package unifiapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// Network is one of the site's networks: its VLAN and record id.
type Network struct {
	ID      string `json:"_id"`
	Name    string `json:"name"`
	VLAN    int    `json:"vlan"`
	Purpose string `json:"purpose"`
	Enabled bool   `json:"enabled"`
}

// Networks lists the site's networks that a port can carry (corporate,
// guest, VLAN-only). The default network (no VLAN tag) is VLAN 1.
func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	var r struct {
		Data []struct {
			ID      string          `json:"_id"`
			Name    string          `json:"name"`
			VLAN    json.RawMessage `json:"vlan"` // a number, or a string on some records
			Purpose string          `json:"purpose"`
			Enabled *bool           `json:"enabled"`
		} `json:"data"`
	}
	if err := c.do(ctx, "GET", "/proxy/network/api/s/"+c.Site+"/rest/networkconf", nil, &r); err != nil {
		return nil, err
	}
	var out []Network
	for _, n := range r.Data {
		switch n.Purpose {
		case "corporate", "guest", "vlan-only":
		default:
			continue
		}
		vlan, _ := strconv.Atoi(strings.Trim(string(n.VLAN), `"`))
		if vlan == 0 {
			vlan = 1
		}
		out = append(out, Network{ID: n.ID, Name: n.Name, VLAN: vlan, Purpose: n.Purpose, Enabled: n.Enabled == nil || *n.Enabled})
	}
	return out, nil
}

// SeedPortConfig writes the switch's own per-port state into the
// controller as port overrides — native VLAN, tagged set, disabled — for
// every port the controller has not configured yet. It runs once when the
// adoption handshake completes, so the controller's first push describes
// the switch as it is instead of "every port at its defaults", which would
// have the bridge strip whatever the switch had (a Proxmox guest's VLAN
// tag, 2026-09-20). Ports whose VLANs are not site networks are left alone
// and named in the returned notes. UniFi is the source of truth from then on.
func (c *Client) SeedPortConfig(ctx context.Context, mac string, snap *switchmodel.Snapshot) (seeded int, notes []string, err error) {
	dev, err := c.DeviceByMAC(ctx, mac)
	if err != nil {
		return 0, nil, err
	}
	nets, err := c.Networks(ctx)
	if err != nil {
		return 0, nil, err
	}
	overrides, seeded, notes := seedOverrides(snap, nets, dev.PortOverrides)
	if seeded == 0 {
		return 0, notes, nil
	}
	fields := map[string]any{"port_overrides": overrides}
	for _, o := range dev.OOBPortConfig {
		if on, _ := o["enabled"].(bool); on {
			fields["oob_port_config"] = []map[string]any{}
		}
	}
	if err := c.UpdateDevice(ctx, dev.ID, fields); err != nil {
		return 0, notes, fmt.Errorf("seed port config: %w", err)
	}
	return seeded, notes, nil
}

// configured reports whether an override carries real port configuration.
// The controller expands a name-only override into `forward: all` with the
// default network, which is still "unconfigured": only a profile, a
// non-default forwarding mode, a tagged-VLAN choice or port security means
// somebody set the port.
func configured(o map[string]any) bool {
	if _, ok := o["portconf_id"]; ok {
		return true
	}
	if f, _ := o["forward"].(string); f != "" && f != "all" {
		return true
	}
	if t, _ := o["tagged_vlan_mgmt"].(string); t != "" && t != "auto" {
		return true
	}
	if on, _ := o["port_security_enabled"].(bool); on {
		return true
	}
	if ex, ok := o["excluded_networkconf_ids"].([]any); ok && len(ex) > 0 {
		return true
	}
	return false
}

// seedOverrides is the pure part: the full override list to send (existing
// entries kept, seeded ones added), how many ports were seeded, and notes
// for ports that could not be expressed.
func seedOverrides(snap *switchmodel.Snapshot, nets []Network, existing []map[string]any) (overrides []map[string]any, seeded int, notes []string) {
	byVLAN := map[int]string{}
	var defaultID string
	for _, n := range nets {
		if _, dup := byVLAN[n.VLAN]; !dup {
			byVLAN[n.VLAN] = n.ID
		}
		if n.VLAN == 1 && defaultID == "" {
			defaultID = n.ID
		}
	}
	byIdx := map[int]map[string]any{}
	for _, o := range existing {
		if idx := overrideIndex(o); idx > 0 {
			byIdx[idx] = o
		}
	}
	for _, p := range snap.Ports {
		if p.IfName == "" || !p.Present {
			continue // an empty slot has nothing to seed
		}
		o := byIdx[p.Index]
		if o != nil && configured(o) {
			continue
		}
		seed, note := overrideFor(p, byVLAN, defaultID)
		if note != "" {
			notes = append(notes, fmt.Sprintf("port %d (%s): %s", p.Index, p.IfName, note))
		}
		if seed == nil {
			continue
		}
		if o == nil {
			o = map[string]any{"port_idx": p.Index}
			byIdx[p.Index] = o
		}
		for k, v := range seed {
			o[k] = v
		}
		seeded++
	}
	idxs := make([]int, 0, len(byIdx))
	for idx := range byIdx {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		overrides = append(overrides, byIdx[idx])
	}
	return overrides, seeded, notes
}

// overrideFor renders one port's live state as the controller's override,
// nil when the port is at the controller's default (native 1, every VLAN
// tagged, enabled) so nothing needs seeding.
func overrideFor(p switchmodel.Port, byVLAN map[int]string, defaultID string) (map[string]any, string) {
	if !p.Enabled {
		// The combination the UI writes for Port State: Disabled (Network 10.6).
		return map[string]any{
			"forward": "disabled", "port_security_enabled": true, "port_security_mac_address": []string{},
			"native_networkconf_id": "", "tagged_vlan_mgmt": "block_all",
		}, ""
	}
	v := p.VLAN
	native := v.NativeVLAN
	if native == 0 {
		native = 1
	}
	if native == 1 && (v.AllowAll || (v.Mode == "" && len(v.Allowed) == 0)) {
		return nil, "" // the default
	}
	nativeID, ok := byVLAN[native]
	if !ok {
		return nil, fmt.Sprintf("native VLAN %d is not a network in the controller; left at the default (create the network, then set the port)", native)
	}
	switch {
	case v.AllowAll:
		return map[string]any{"forward": "customize", "native_networkconf_id": nativeID, "tagged_vlan_mgmt": "auto"}, ""
	case v.Mode == "access" || len(v.Allowed) == 0:
		return map[string]any{"forward": "native", "native_networkconf_id": nativeID, "tagged_vlan_mgmt": "block_all"}, ""
	}
	allowed := map[int]bool{}
	for _, id := range v.Allowed {
		allowed[id] = true
	}
	var excluded []string
	var unknown []int
	for id := range allowed {
		if _, ok := byVLAN[id]; !ok && id != native {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		sort.Ints(unknown)
		return nil, fmt.Sprintf("tagged VLANs %v are not networks in the controller; left at the default", unknown)
	}
	vlans := make([]int, 0, len(byVLAN))
	for id := range byVLAN {
		vlans = append(vlans, id)
	}
	sort.Ints(vlans)
	for _, id := range vlans {
		if id != native && !allowed[id] {
			excluded = append(excluded, byVLAN[id])
		}
	}
	if excluded == nil {
		excluded = []string{}
	}
	if defaultID != "" && native != 1 && !allowed[1] {
		// VLAN 1 (the untagged network) is excluded like any other.
		_ = defaultID
	}
	return map[string]any{"forward": "customize", "native_networkconf_id": nativeID, "tagged_vlan_mgmt": "custom", "excluded_networkconf_ids": excluded}, ""
}
