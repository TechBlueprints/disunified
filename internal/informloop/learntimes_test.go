package informloop

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/proxmox"
)

const proxmoxNode2 = "collect-node2.txt"

// laterCollector serves the real driver's collect of a fixed capture as if
// it were read `shift` after base (the first snapshot's time): the snapshot
// time and every entry's times move together, so a capture that does not
// change stands for a node whose FDB did not change. The driver stamps each
// collect with the wall clock, so the collect is moved to base+shift
// exactly; shifting the fresh collect by `shift` alone would carry the test's
// own running time into every uptime, one second too many whenever it and
// the first collect's fraction of a second (the tracker keeps whole seconds)
// add up to a second. drop removes one address from the collect (it aged
// out); fail makes the collect fail.
type laterCollector struct {
	c     devicemodel.Collector
	base  time.Time
	shift time.Duration
	drop  string
	fail  bool
}

func (l *laterCollector) Collect(ctx context.Context) (*devicemodel.Snapshot, error) {
	if l.fail {
		return nil, errors.New("node unreachable")
	}
	snap, err := l.c.Collect(ctx)
	if err != nil {
		return nil, err
	}
	return later(snap, l.base.Add(l.shift).Sub(snap.TakenAt), l.drop), nil
}

func later(snap *devicemodel.Snapshot, shift time.Duration, drop string) *devicemodel.Snapshot {
	move := func(macs []devicemodel.MACEntry) []devicemodel.MACEntry {
		var out []devicemodel.MACEntry
		for _, m := range macs {
			if m.MAC == drop {
				continue
			}
			if !m.LastSeen.IsZero() {
				m.LastSeen = m.LastSeen.Add(shift)
			}
			if !m.LearnedAt.IsZero() {
				m.LearnedAt = m.LearnedAt.Add(shift)
			}
			out = append(out, m)
		}
		return out
	}
	s := *snap
	s.TakenAt = snap.TakenAt.Add(shift)
	s.MACTable = move(snap.MACTable)
	s.Ports = slices.Clone(snap.Ports)
	for i := range s.Ports {
		s.Ports[i].MACs = move(s.Ports[i].MACs)
	}
	return &s
}

// fdbUpdated is the capture's FDB: each address/VLAN's `updated` seconds.
func fdbUpdated(t *testing.T, path string) map[string]int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	var line string
	for sc.Scan() {
		if sc.Text() == "@@@ fdb" && sc.Scan() {
			line = sc.Text()
			break
		}
	}
	var rows []struct {
		MAC     string `json:"mac"`
		VLAN    int    `json:"vlan"`
		Updated int    `json:"updated"`
	}
	if err := json.Unmarshal([]byte(line), &rows); err != nil {
		t.Fatalf("fdb section: %v", err)
	}
	out := map[string]int{}
	for _, r := range rows {
		out[fmt.Sprintf("%s/%d", strings.ToLower(r.MAC), r.VLAN)] = r.Updated
	}
	return out
}

type macTimes struct{ age, uptime int }

// portMACTimes is every port's mac_table in the session's inform, keyed
// address/VLAN/port.
func portMACTimes(t *testing.T, sess *device.Session) map[string]macTimes {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]macTimes{}
	for _, p := range m["port_table"].([]any) {
		pe := p.(map[string]any)
		mt, _ := pe["mac_table"].([]any)
		for _, e := range mt {
			e := e.(map[string]any)
			out[fmt.Sprintf("%v/%v/%v", e["mac"], e["vlan"], pe["port_idx"])] = macTimes{int(e["age"].(float64)), int(e["uptime"].(float64))}
		}
	}
	return out
}

func proxmoxLoop(t *testing.T, cc *laterCollector, snap *devicemodel.Snapshot, dir string) (*Loop, *device.Session) {
	t.Helper()
	desc, err := device.DescriptorFor("UDC48X6", snap, device.Identity{MAC: snap.System.MAC, IP: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef"}, nil, time.Now())
	sess.SetSnapshot(snap)
	l, err := New(desc, sess, Config{Logger: log.New(testLogWriter{t}, "", 0), Collector: cc, StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return l, sess
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}

// The learn-time tracker over the real Proxmox capture, collected again and
// again with the clock moved: an address that stays keeps its learn time
// (uptime grows by the interval) while its age stays the FDB's `updated`;
// an address that ages out and comes back starts again; a failed collect
// forgets nothing; the file is written only when the set of addresses
// changes, and a loop started from it continues the times.
func TestLearnTimesProxmox(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "fixtures", "proxmox-9.1.6", proxmoxNode2)
	updated := fdbUpdated(t, path)
	fr, err := proxmox.NewFixtureRunner(path)
	if err != nil {
		t.Fatal(err)
	}
	coll := proxmox.NewCollector(fr)
	coll.ManageLLDP = false
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	driverSnap := snap
	dir := t.TempDir()
	cc := &laterCollector{c: coll, base: snap.TakenAt}
	l, sess := proxmoxLoop(t, cc, snap, dir)
	ctx := context.Background()

	first := portMACTimes(t, sess)
	if len(first) < 10 {
		t.Fatalf("%d port MAC entries in the capture's payload; want the node's FDB", len(first))
	}
	for k, v := range first {
		mv := k[:strings.LastIndex(k, "/")]
		u, ok := updated[mv]
		if !ok {
			t.Errorf("%s: not in the capture's FDB", k)
			continue
		}
		// First sight: learned when last seen, so uptime = age = updated.
		if v.age != u || v.uptime != u {
			t.Errorf("first sight %s: age %d uptime %d, want both the FDB's updated %d", k, v.age, v.uptime, u)
		}
	}
	for _, m := range driverSnap.MACTable {
		if !m.LearnedAt.IsZero() {
			t.Fatalf("the driver's snapshot was modified: %+v", m)
		}
	}

	// The file: in the state directory, 0600, one row per address.
	file := filepath.Join(dir, LearnTimesFile)
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("%s mode %v, want 0600", file, fi.Mode().Perm())
	}
	var rows []learnTimesRecord
	b, _ := os.ReadFile(file)
	if err := json.Unmarshal(b, &rows); err != nil || len(rows) < len(first) {
		t.Fatalf("%s: %d rows (%v), want at least the %d port entries", file, len(rows), err, len(first))
	}

	step := func(shift time.Duration, drop string) map[string]macTimes {
		t.Helper()
		cc.shift, cc.drop = shift, drop
		l.collect(ctx)
		return portMACTimes(t, sess)
	}
	at := func(when string, got map[string]macTimes, k string, age, uptime int) {
		t.Helper()
		if v, ok := got[k]; !ok || v.age != age || v.uptime != uptime {
			t.Errorf("%s %s: %+v (present %v), want age %d uptime %d", when, k, v, ok, age, uptime)
		}
	}

	// Five minutes later, the same FDB: every uptime grew by 300, ages did not.
	got := step(300*time.Second, "")
	for k, v := range first {
		at("+300s", got, k, v.age, v.uptime+300)
	}
	// Nothing changed: the file is not rewritten.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	step(400*time.Second, "")
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file rewritten with no change in the addresses: %v", err)
	}

	// One address ages out, then comes back: it starts again.
	var gone string
	for k := range first {
		if gone == "" || k < gone {
			gone = k
		}
	}
	goneMAC := gone[:strings.Index(gone, "/")]
	isGone := func(k string) bool { return strings.HasPrefix(k, goneMAC+"/") }
	got = step(600*time.Second, goneMAC)
	for k := range got {
		if isGone(k) {
			t.Fatalf("%s still reported after it was dropped", k)
		}
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("address set changed but the file was not written: %v", err)
	}
	got = step(900*time.Second, "")
	for k, v := range first {
		if isGone(k) {
			at("returned", got, k, v.age, v.age)
		}
	}

	// A failed collect forgets nothing.
	cc.fail = true
	l.collect(ctx)
	cc.fail = false
	got = step(1500*time.Second, "")
	for k, v := range first {
		if isGone(k) {
			at("after a failed collect", got, k, v.age, v.age+600)
			continue
		}
		at("after a failed collect", got, k, v.age, v.uptime+1500)
	}

	// A bridge restart: a new loop from the same state directory, its
	// first snapshot read later still, continues every time.
	restart := later(snap, 1800*time.Second, "")
	_, sess2 := proxmoxLoop(t, &laterCollector{c: coll, base: snap.TakenAt, shift: 1800 * time.Second}, restart, dir)
	got = portMACTimes(t, sess2)
	for k, v := range first {
		if isGone(k) {
			at("after a restart", got, k, v.age, v.age+900)
			continue
		}
		at("after a restart", got, k, v.age, v.uptime+1800)
	}

	// Without a state directory the tracker works in memory.
	cc2 := &laterCollector{c: coll, base: snap.TakenAt}
	l3, sess3 := proxmoxLoop(t, cc2, snap, "")
	cc2.shift = 60 * time.Second
	l3.collect(ctx)
	got = portMACTimes(t, sess3)
	for k, v := range first {
		at("in memory", got, k, v.age, v.uptime+60)
	}
}

// A configured child is seen at every collect and learned when first
// reported: its uptime grows, its age stays 0.
func TestLearnTimesChild(t *testing.T) {
	snap := dx010Snapshot(t)
	desc, err := device.DescriptorFor("USWF07D", snap, device.Identity{MAC: snap.System.MAC, IP: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef"}, nil, time.Now())
	sess.SetSnapshot(snap)
	cc := &laterCollector{c: fixedCollector{snap}, base: snap.TakenAt}
	l, err := New(desc, sess, Config{
		Logger: log.New(testLogWriter{t}, "", 0), Collector: cc,
		Children: []Child{{Port: 2, MAC: "02:00:00:00:00:a2", Name: "lane-switch"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	k := "02:00:00:00:00:a2/1/2"
	if v := portMACTimes(t, sess)[k]; v != (macTimes{}) {
		t.Errorf("child at start: %+v, want age 0 uptime 0", v)
	}
	cc.shift = 120 * time.Second
	l.collect(context.Background())
	if v := portMACTimes(t, sess)[k]; v != (macTimes{0, 120}) {
		t.Errorf("child after 120 s: %+v, want age 0 uptime 120", v)
	}
}
