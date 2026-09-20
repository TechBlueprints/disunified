package unifiapi

import (
	"reflect"
	"testing"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

func TestSeedOverridesMirrorsTheSwitch(t *testing.T) {
	nets := []Network{{ID: "d", VLAN: 1}, {ID: "n2", VLAN: 2}, {ID: "n5", VLAN: 5}, {ID: "n8", VLAN: 8}, {ID: "n10", VLAN: 10}, {ID: "n69", VLAN: 69}}
	snap := &switchmodel.Snapshot{Ports: []switchmodel.Port{
		{Index: 1, IfName: "vm100-net0", Present: true, Enabled: true, VLAN: switchmodel.PortVLAN{Mode: "trunk", NativeVLAN: 1, AllowAll: true}},            // default: nothing to seed
		{Index: 2, IfName: "vm119-net0", Present: true, Enabled: true, VLAN: switchmodel.PortVLAN{Mode: "access", NativeVLAN: 8}},                           // native 8, block all
		{Index: 3, IfName: "vm999-net0", Present: true, Enabled: true, VLAN: switchmodel.PortVLAN{Mode: "trunk", NativeVLAN: 2, Allowed: []int{2, 10, 69}}}, // native 2, tagged 10+69
		{Index: 4, IfName: "vm101-net0", Present: true, Enabled: false, VLAN: switchmodel.PortVLAN{Mode: "trunk", NativeVLAN: 1, AllowAll: true}},           // disabled
		{Index: 5, IfName: "vm102-net0", Present: true, Enabled: true, VLAN: switchmodel.PortVLAN{Mode: "access", NativeVLAN: 3000}},                        // VLAN unknown to the controller
		{Index: 6, IfName: "vm103-net0", Present: true, Enabled: true, VLAN: switchmodel.PortVLAN{Mode: "access", NativeVLAN: 8}},                           // already configured: untouched
		{Index: 7, Present: false}, // empty slot
		{Index: 54, IfName: "bond0", Present: true, Enabled: true, VLAN: switchmodel.PortVLAN{Mode: "trunk", NativeVLAN: 5, AllowAll: true}}, // native 5, all tagged
	}}
	existing := []map[string]any{
		{"port_idx": float64(2), "name": "VM-119 net0", "forward": "all", "native_networkconf_id": "d"}, // what the controller makes of a name-only override
		{"port_idx": float64(6), "name": "VM-103", "forward": "native", "native_networkconf_id": "n10", "tagged_vlan_mgmt": "block_all"},
	}
	overrides, seeded, notes := seedOverrides(snap, nets, existing)
	if seeded != 4 {
		t.Errorf("seeded = %d, want 4 (ports 2, 3, 4, 54)", seeded)
	}
	if len(notes) != 1 || notes[0] != "port 5 (vm102-net0): native VLAN 3000 is not a network in the controller; left at the default (create the network, then set the port)" {
		t.Errorf("notes = %v", notes)
	}
	byIdx := map[int]map[string]any{}
	for _, o := range overrides {
		var idx int
		switch v := o["port_idx"].(type) {
		case int:
			idx = v
		case float64:
			idx = int(v) // existing entries come from JSON
		}
		cp := map[string]any{}
		for k, v := range o {
			if k != "port_idx" {
				cp[k] = v
			}
		}
		byIdx[idx] = cp
	}
	if _, ok := byIdx[1]; ok {
		t.Errorf("a default port was seeded: %v", byIdx[1])
	}
	want2 := map[string]any{"name": "VM-119 net0", "forward": "native", "native_networkconf_id": "n8", "tagged_vlan_mgmt": "block_all"}
	if !reflect.DeepEqual(byIdx[2], want2) {
		t.Errorf("port 2 = %v\nwant %v", byIdx[2], want2)
	}
	want3 := map[string]any{"forward": "customize", "native_networkconf_id": "n2", "tagged_vlan_mgmt": "custom", "excluded_networkconf_ids": []string{"d", "n5", "n8"}}
	if !reflect.DeepEqual(byIdx[3], want3) {
		t.Errorf("port 3 = %v\nwant %v", byIdx[3], want3)
	}
	if byIdx[4]["forward"] != "disabled" || byIdx[4]["port_security_enabled"] != true || byIdx[4]["tagged_vlan_mgmt"] != "block_all" {
		t.Errorf("port 4 (disabled) = %v", byIdx[4])
	}
	if byIdx[6]["native_networkconf_id"] != "n10" {
		t.Errorf("an already configured port was overwritten: %v", byIdx[6])
	}
	if byIdx[54]["forward"] != "customize" || byIdx[54]["native_networkconf_id"] != "n5" && byIdx[54]["tagged_vlan_mgmt"] != "auto" {
		t.Errorf("port 54 = %v", byIdx[54])
	}
	// Idempotent: seeding again over its own output changes nothing.
	_, again, _ := seedOverrides(snap, nets, overrides)
	if again != 0 {
		t.Errorf("second seed wrote %d ports", again)
	}
}

func TestReleaseOverrideKeepsOnlyTheName(t *testing.T) {
	o := map[string]any{"port_idx": float64(25), "name": "VM-Open-25", "forward": "customize", "native_networkconf_id": "n2", "excluded_networkconf_ids": []any{"n3"}}
	if !releaseOverride(o) {
		t.Fatal("config should have been removed")
	}
	if want := (map[string]any{"port_idx": float64(25), "name": "VM-Open-25"}); !reflect.DeepEqual(o, want) {
		t.Errorf("after release: %v, want %v", o, want)
	}
	if releaseOverride(o) {
		t.Error("a second release must be a no-op (idempotent provisioning)")
	}
	bare := map[string]any{"port_idx": float64(26)}
	if releaseOverride(bare) || len(bare) != 1 {
		t.Errorf("bare override changed: %v", bare)
	}
}
