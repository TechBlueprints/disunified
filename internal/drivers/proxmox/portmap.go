package proxmox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// portMap is the persistent assignment of guest NICs to switch ports. A
// UniFi port index carries the controller's per-port config (VLANs, state),
// so a guest must keep its port for life: the map is seeded from the
// cluster-wide guest list in (vmid, net) order and then only ever grows.
// A guest that disappears releases its slot, but the slot is reused only
// when no never-used slot is left, oldest release first, so a deleted VM's
// port config does not land on the next VM created.
//
// Every node's bridge sees the same cluster-wide list (Proxmox replicates
// guest configs to every node), so the three switches number a VM the same
// way: VM 100 is port 1 on every node, up on the node it runs on.
type portMap struct {
	path  string // "" = in memory only
	Slots map[int]*slot
}

type slot struct {
	Key      string    `json:"key"`
	Released time.Time `json:"released,omitempty"`
}

func loadPortMap(dir string) (*portMap, error) {
	m := &portMap{Slots: map[int]*slot{}}
	if dir == "" {
		return m, nil
	}
	m.path = filepath.Join(dir, "proxmox-ports.json")
	b, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Slots map[int]*slot `json:"slots"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Slots != nil {
		m.Slots = f.Slots
	}
	return m, nil
}

func (m *portMap) save() error {
	if m.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(struct {
		Slots map[int]*slot `json:"slots"`
	}{m.Slots}, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// assign gives every key a slot in [1, n] and returns key -> slot. keys
// must be in the deterministic order new guests are numbered in. It
// reports whether the map changed (the caller persists it).
func (m *portMap) assign(keys []string, n int, now time.Time) (map[string]int, bool) {
	changed := false
	byKey := map[string]int{}
	present := map[string]bool{}
	for _, k := range keys {
		present[k] = true
	}
	for idx, s := range m.Slots {
		if idx < 1 || idx > n {
			continue
		}
		if present[s.Key] {
			byKey[s.Key] = idx
			if !s.Released.IsZero() {
				s.Released = time.Time{} // it came back
				changed = true
			}
		} else if s.Released.IsZero() {
			s.Released = now
			changed = true
		}
	}
	for _, k := range keys {
		if _, ok := byKey[k]; ok {
			continue
		}
		idx := m.freeSlot(n)
		if idx == 0 {
			break // more guests than ports: the rest are not shown
		}
		m.Slots[idx] = &slot{Key: k}
		byKey[k] = idx
		changed = true
	}
	return byKey, changed
}

// freeSlot picks the lowest never-used slot, else the longest-released one.
func (m *portMap) freeSlot(n int) int {
	for i := 1; i <= n; i++ {
		if _, used := m.Slots[i]; !used {
			return i
		}
	}
	var candidates []int
	for i := 1; i <= n; i++ {
		if s := m.Slots[i]; s != nil && !s.Released.IsZero() {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return 0
	}
	sort.Slice(candidates, func(a, b int) bool {
		return m.Slots[candidates[a]].Released.Before(m.Slots[candidates[b]].Released)
	})
	return candidates[0]
}
