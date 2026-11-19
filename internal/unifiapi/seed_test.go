package unifiapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

func TestSeedOverridesMirrorsTheSwitch(t *testing.T) {
	nets := []Network{{ID: "d", VLAN: 1}, {ID: "n2", VLAN: 2}, {ID: "n5", VLAN: 5}, {ID: "n8", VLAN: 8}, {ID: "n10", VLAN: 10}, {ID: "n69", VLAN: 69}}
	snap := &devicemodel.Snapshot{Ports: []devicemodel.Port{
		{Index: 1, IfName: "vm100-net0", Present: true, Enabled: true, VLAN: devicemodel.PortVLAN{Mode: "trunk", NativeVLAN: 1, AllowAll: true}},            // default: nothing to seed
		{Index: 2, IfName: "vm119-net0", Present: true, Enabled: true, VLAN: devicemodel.PortVLAN{Mode: "access", NativeVLAN: 8}},                           // native 8, block all
		{Index: 3, IfName: "vm999-net0", Present: true, Enabled: true, VLAN: devicemodel.PortVLAN{Mode: "trunk", NativeVLAN: 2, Allowed: []int{2, 10, 69}}}, // native 2, tagged 10+69
		{Index: 4, IfName: "vm101-net0", Present: true, Enabled: false, VLAN: devicemodel.PortVLAN{Mode: "trunk", NativeVLAN: 1, AllowAll: true}},           // disabled
		{Index: 5, IfName: "vm102-net0", Present: true, Enabled: true, VLAN: devicemodel.PortVLAN{Mode: "access", NativeVLAN: 3000}},                        // VLAN unknown to the controller
		{Index: 6, IfName: "vm103-net0", Present: true, Enabled: true, VLAN: devicemodel.PortVLAN{Mode: "access", NativeVLAN: 8}},                           // already configured: untouched
		{Index: 7, Present: false}, // empty slot: seeded disabled
		{Index: 54, IfName: "bond0", Present: true, Enabled: true, VLAN: devicemodel.PortVLAN{Mode: "trunk", NativeVLAN: 5, AllowAll: true}}, // native 5, all tagged
	}}
	existing := []map[string]any{
		{"port_idx": float64(2), "name": "VM-119 net0", "forward": "all", "native_networkconf_id": "d"}, // what the controller makes of a name-only override
		{"port_idx": float64(6), "name": "VM-103", "forward": "native", "native_networkconf_id": "n10", "tagged_vlan_mgmt": "block_all"},
	}
	overrides, seeded, notes := seedOverrides(snap, nets, existing, nil)
	if seeded != 5 {
		t.Errorf("seeded = %d, want 5 (ports 2, 3, 4, 7, 54)", seeded)
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
	if !isDisabledOverride(byIdx[7]) {
		t.Errorf("free slot 7 = %v, want disabled", byIdx[7])
	}
	if byIdx[6]["native_networkconf_id"] != "n10" {
		t.Errorf("an already configured port was overwritten: %v", byIdx[6])
	}
	if byIdx[54]["forward"] != "customize" || byIdx[54]["native_networkconf_id"] != "n5" && byIdx[54]["tagged_vlan_mgmt"] != "auto" {
		t.Errorf("port 54 = %v", byIdx[54])
	}
	// Idempotent: seeding again over its own output changes nothing.
	_, again, _ := seedOverrides(snap, nets, overrides, nil)
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
	// The disabled combination (a free slot's own seed) survives a release,
	// however the controller decoded it.
	d := map[string]any{"port_idx": float64(27), "name": "VM-Open-27", "forward": "disabled", "port_security_enabled": true,
		"port_security_mac_address": []any{}, "native_networkconf_id": "", "tagged_vlan_mgmt": "block_all", "stp_port_mode": true}
	if !releaseOverride(d) || !isDisabledOverride(d) || len(d) != 7 {
		t.Errorf("free-slot override after release: %v", d)
	}
	// What the controller stores for that seed (it adds voice_networkconf_id: "")
	// is left alone, or every provision would strip it and the controller
	// re-add it (live, 2026-09-20: "26 released slots cleared" on each call).
	stored := map[string]any{"forward": "disabled", "name": "VM-Open-48", "native_networkconf_id": "", "port_idx": float64(48),
		"port_security_enabled": true, "port_security_mac_address": []any{}, "tagged_vlan_mgmt": "block_all", "voice_networkconf_id": ""}
	if releaseOverride(stored) || len(stored) != 8 {
		t.Errorf("controller-stored free-slot override was changed: %v", stored)
	}
	free := devicemodel.Port{Index: 48, Enabled: false}
	if _, seeded, _ := seedOverrides(&devicemodel.Snapshot{Ports: []devicemodel.Port{free}}, nil, []map[string]any{stored}, nil); seeded != 0 {
		t.Errorf("controller-stored free-slot override was reseeded")
	}
}

func TestSeedFreeSlotsDisabledAndReseedTheirNextGuest(t *testing.T) {
	nets := []Network{{ID: "n1", VLAN: 1}, {ID: "n10", VLAN: 10}}
	isDefault := func(idx int, name string) bool { return name == "VM-Open-25" || name == "VM-999" }
	free := devicemodel.Port{Index: 25, Enabled: false}
	// A free slot with a stale override from its last guest is seeded
	// disabled and stripped of the old config.
	got, seeded, _ := seedOverrides(&devicemodel.Snapshot{Ports: []devicemodel.Port{free}}, nets,
		[]map[string]any{{"port_idx": float64(25), "name": "VM-Open-25", "forward": "customize", "native_networkconf_id": "n10", "tagged_vlan_mgmt": "auto"}}, isDefault)
	if seeded != 1 || len(got) != 1 || !isDisabledOverride(got[0]) || got[0]["name"] != "VM-Open-25" {
		t.Fatalf("free slot: seeded=%d %v", seeded, got)
	}
	// Idempotent: the same slot again changes nothing.
	if _, seeded, _ = seedOverrides(&devicemodel.Snapshot{Ports: []devicemodel.Port{free}}, nets, got, isDefault); seeded != 0 {
		t.Errorf("second pass seeded %d", seeded)
	}
	// A guest takes the slot (the naming pass already renamed it): its own
	// state replaces the free-slot seed even though that looks configured.
	guest := devicemodel.Port{Index: 25, IfName: "vm999-net0", Present: true, Enabled: true,
		VLAN: devicemodel.PortVLAN{Mode: "access", NativeVLAN: 10}}
	got[0]["name"] = "VM-999"
	got, seeded, _ = seedOverrides(&devicemodel.Snapshot{Ports: []devicemodel.Port{guest}}, nets, got, isDefault)
	if seeded != 1 || got[0]["forward"] != "native" || got[0]["native_networkconf_id"] != "n10" || got[0]["port_security_enabled"] != nil {
		t.Fatalf("guest on a free slot: seeded=%d %v", seeded, got)
	}
	// A guest at the defaults on a free slot: the disabled seed is dropped.
	plain := devicemodel.Port{Index: 25, IfName: "vm999-net0", Present: true, Enabled: true,
		VLAN: devicemodel.PortVLAN{Mode: "trunk", NativeVLAN: 1, AllowAll: true}}
	fresh := []map[string]any{{"port_idx": float64(25), "name": "VM-999"}}
	for k, v := range disabledOverride() {
		fresh[0][k] = v
	}
	got, seeded, _ = seedOverrides(&devicemodel.Snapshot{Ports: []devicemodel.Port{plain}}, nets, fresh, isDefault)
	if seeded != 1 || len(got[0]) != 2 {
		t.Fatalf("plain guest on a free slot: seeded=%d %v", seeded, got)
	}
	// An operator's own name on a disabled port means it is theirs: kept.
	theirs := []map[string]any{{"port_idx": float64(25), "name": "keep off"}}
	for k, v := range disabledOverride() {
		theirs[0][k] = v
	}
	if _, seeded, _ = seedOverrides(&devicemodel.Snapshot{Ports: []devicemodel.Port{guest}}, nets, theirs, isDefault); seeded != 0 {
		t.Errorf("operator-disabled port was reseeded")
	}
}

// A guest NIC switched off on the node (link_down=1) is reported disabled
// under a name the driver gave, and its stored override is the free-slot
// combination: the controller's record of a Proxmox node, read on
// 2026-11-17 after a start that logged "1 ports seeded" with nothing
// changed (port 25 is VM 999's net0; ports 29-51 are free slots). Nothing
// in it needs seeding.
func TestSeedLeavesADisabledGuestAlone(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", "stat-device-proxmox-port-overrides.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Data []struct {
			PortOverrides []map[string]any `json:"port_overrides"`
			PortTable     []struct {
				PortIdx int    `json:"port_idx"`
				Name    string `json:"name"`
				IfName  string `json:"ifname"`
				Enable  bool   `json:"enable"`
			} `json:"port_table"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &rec); err != nil || len(rec.Data) != 1 {
		t.Fatalf("decode: %v (%d records)", err, len(rec.Data))
	}
	d := rec.Data[0]
	reported := map[int]string{}
	snap := &devicemodel.Snapshot{}
	for _, p := range d.PortTable {
		reported[p.PortIdx] = p.Name
		switch {
		case p.PortIdx == 25:
			if p.IfName != "vm999-net0" || p.Enable {
				t.Fatalf("port 25 = %+v, want vm999-net0 disabled", p)
			}
			snap.Ports = append(snap.Ports, devicemodel.Port{Index: 25, IfName: p.IfName, Present: true, Enabled: false})
		case p.PortIdx >= 29 && p.PortIdx <= 51:
			snap.Ports = append(snap.Ports, devicemodel.Port{Index: p.PortIdx}) // free slot
		}
	}
	// The name the bridge reports is the one the driver gave.
	isDefault := func(idx int, name string) bool { return name == reported[idx] }
	before, _ := json.Marshal(d.PortOverrides)
	got, seeded, notes := seedOverrides(snap, nil, d.PortOverrides, isDefault)
	if seeded != 0 || len(notes) != 0 {
		t.Errorf("seeded = %d, notes %v; want 0", seeded, notes)
	}
	if after, _ := json.Marshal(got); string(after) != string(before) {
		t.Errorf("overrides changed:\n%s\nwant\n%s", after, before)
	}
	// The same guest's port on a slot that still holds an enabled guest's
	// config is seeded disabled: once, then not again.
	o := map[string]any{"port_idx": float64(25), "name": reported[25], "forward": "all", "native_networkconf_id": "d"}
	got, seeded, _ = seedOverrides(&devicemodel.Snapshot{Ports: snap.Ports[:1]}, nil, []map[string]any{o}, isDefault)
	if seeded != 1 || !isDisabledOverride(got[0]) {
		t.Fatalf("link_down guest over an enabled override: seeded=%d %v", seeded, got)
	}
	if _, seeded, _ = seedOverrides(&devicemodel.Snapshot{Ports: snap.Ports[:1]}, nil, got, isDefault); seeded != 0 {
		t.Errorf("second pass seeded %d", seeded)
	}
}

// A reserved slot (a lane switch's lanes 3-4 while its cage runs 2x50G,
// docs/drivers/breakout.md 16.15) is the same lane when the cage is
// re-split to four: its override is neither released nor seeded disabled.
// Live, before the flag, the 2x50G seed disabled lanes 3-4 and the re-split
// back brought them up admin down (2026-11-18, DX010 cage 26).
func TestSeedKeepsReservedSlots(t *testing.T) {
	nets := []Network{{ID: "n1", VLAN: 1}, {ID: "n10", VLAN: 10}}
	lane := map[string]any{"port_idx": float64(3), "name": "Eth26/3", "forward": "customize", "native_networkconf_id": "n10", "tagged_vlan_mgmt": "auto"}
	slot := devicemodel.Port{Index: 3, Enabled: true, Reserved: true}
	got, seeded, _ := seedOverrides(&devicemodel.Snapshot{Ports: []devicemodel.Port{slot}}, nets, []map[string]any{lane}, nil)
	if seeded != 0 || len(got) != 1 || got[0]["forward"] != "customize" || got[0]["native_networkconf_id"] != "n10" {
		t.Errorf("reserved slot: seeded=%d %v", seeded, got)
	}
}
