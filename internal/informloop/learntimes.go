package informloop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// LearnTimesFile is the learn-time tracker's file in a device's state
// directory (Config.StateDir), next to device.json and never inside it.
const LearnTimesFile = "mac-learned.json"

// macKey is one learned address on one port: the identity a switch's FDB
// gives an entry. An address that moves port, or ages out and comes back,
// is a new key and starts again.
type macKey struct {
	MAC  string
	VLAN int
	Port int
}

// learnTimes keeps a learn time for the MAC entries a driver reports
// without one (a Linux bridge FDB, SONiC's FDB_TABLE, a configured child):
// a UniFi switch reports an entry's `uptime` as the seconds since it was
// learned, and the controller takes a wired client's connected-since from
// it, so an uptime that restarts with every collect looks like every client
// reconnecting (docs/mac-table-times.md).
//
//   - An entry that arrives with LearnedAt set is left alone and not tracked.
//   - First sight of a key: LearnedAt = the entry's LastSeen if set, else
//     the snapshot time. Later sights: the remembered time.
//   - A key absent from a successful collect is forgotten (apply is called
//     only for those); a failed collect changes nothing.
//   - With a path, the map is loaded at start and written (0600, temp file
//     and rename) only when its set of keys changes, so a bridge restart
//     does not reset every client. An address present on both sides of the
//     gap is treated as continuous. Without a path it lives in memory.
type learnTimes struct {
	path    string
	learned map[macKey]time.Time
	logf    func(format string, args ...any)
	saveErr string // the last save error logged, to log on change only
}

// learnTimesRecord is the file's shape: one row per tracked key, sorted.
type learnTimesRecord struct {
	MAC     string `json:"mac"`
	VLAN    int    `json:"vlan"`
	Port    int    `json:"port"`
	Learned int64  `json:"learned"` // Unix seconds
}

// newLearnTimes loads the tracker from dir/LearnTimesFile ("" = in memory
// only). A missing file is an empty tracker; an unreadable one is logged
// and starts empty (the times restart, as they did before the tracker).
func newLearnTimes(dir string, logf func(string, ...any)) *learnTimes {
	t := &learnTimes{learned: map[macKey]time.Time{}, logf: logf}
	if dir == "" {
		return t
	}
	t.path = filepath.Join(dir, LearnTimesFile)
	b, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return t
	}
	var rows []learnTimesRecord
	if err == nil {
		err = json.Unmarshal(b, &rows)
	}
	if err != nil {
		t.logf("MAC learn times %s not loaded, starting afresh: %v", t.path, err)
		return t
	}
	for _, r := range rows {
		t.learned[macKey{r.MAC, r.VLAN, r.Port}] = time.Unix(r.Learned, 0)
	}
	return t
}

// apply returns snap with a learn time on every MAC entry that lacks one,
// the same time in the port's table and the switch's, and makes snap's
// keys the tracked set. Call it with each successful collect's snapshot
// only. The driver's snapshot is not modified: a copy carries the times.
func (t *learnTimes) apply(snap *devicemodel.Snapshot) *devicemodel.Snapshot {
	if snap == nil {
		return nil
	}
	at := snap.TakenAt
	if at.IsZero() {
		at = time.Now()
	}
	seen := map[macKey]time.Time{}
	visit := func(m devicemodel.MACEntry) {
		if !m.LearnedAt.IsZero() {
			return
		}
		k := macKey{m.MAC, m.VLAN, m.PortIndex}
		if _, ok := seen[k]; ok {
			return
		}
		if prev, ok := t.learned[k]; ok {
			seen[k] = prev
			return
		}
		first := at
		if !m.LastSeen.IsZero() && m.LastSeen.Before(at) {
			first = m.LastSeen
		}
		seen[k] = first.Truncate(time.Second)
	}
	for _, m := range snap.MACTable {
		visit(m)
	}
	for _, p := range snap.Ports {
		for _, m := range p.MACs {
			visit(m)
		}
	}
	changed := len(seen) != len(t.learned)
	for k := range seen {
		if _, ok := t.learned[k]; !ok {
			changed = true
		}
	}
	t.learned = seen
	if changed {
		t.save()
	}
	if len(seen) == 0 {
		return snap
	}
	fill := func(macs []devicemodel.MACEntry) []devicemodel.MACEntry {
		out := slices.Clone(macs)
		for i := range out {
			if out[i].LearnedAt.IsZero() {
				out[i].LearnedAt = seen[macKey{out[i].MAC, out[i].VLAN, out[i].PortIndex}]
			}
		}
		return out
	}
	s := *snap
	s.MACTable = fill(snap.MACTable)
	s.Ports = slices.Clone(snap.Ports)
	for i := range s.Ports {
		if len(s.Ports[i].MACs) > 0 {
			s.Ports[i].MACs = fill(s.Ports[i].MACs)
		}
	}
	return &s
}

// save writes the tracked set (no path: nothing). A failure is logged once
// per distinct error; the times stay in memory.
func (t *learnTimes) save() {
	if t.path == "" {
		return
	}
	err := t.write()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != t.saveErr {
		t.saveErr = msg
		if err != nil {
			t.logf("MAC learn times not saved (held in memory): %v", err)
		}
	}
}

func (t *learnTimes) write() error {
	rows := make([]learnTimesRecord, 0, len(t.learned))
	for k, v := range t.learned {
		rows = append(rows, learnTimesRecord{MAC: k.MAC, VLAN: k.VLAN, Port: k.Port, Learned: v.Unix()})
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.MAC != b.MAC {
			return a.MAC < b.MAC
		}
		if a.VLAN != b.VLAN {
			return a.VLAN < b.VLAN
		}
		return a.Port < b.Port
	})
	b, err := json.MarshalIndent(rows, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	return os.Rename(tmp, t.path)
}
