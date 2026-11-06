package informloop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRecords is the controller's real replies (the replay fixture), as
// the recorder takes them. Plain 404s are left out so every record is
// written as one line of its own.
func fixtureRecords(t *testing.T) []record {
	t.Helper()
	f, err := os.Open("../../docs/fixtures/controller-10.6.106/replies.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.Status == http.StatusNotFound && r.Error == "" {
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) < 8 {
		t.Fatalf("only %d fixture records", len(out))
	}
	return out
}

// line is a record exactly as the recorder wrote it before rotation
// existed: json.Encoder's output, one line.
func line(t *testing.T, r record) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(r); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

const testMAC = "02:00:00:00:00:01"

func readGen(t *testing.T, dir string, gen int) ([]byte, bool) {
	t.Helper()
	name := filepath.Join(dir, testMAC+".ndjson")
	if gen > 0 {
		name += fmt.Sprintf(".%d", gen)
	}
	b, err := os.ReadFile(name)
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return b, true
}

// wholeLines fails unless b is a run of complete JSON lines.
func wholeLines(t *testing.T, what string, b []byte) int {
	t.Helper()
	if len(b) == 0 {
		return 0
	}
	if b[len(b)-1] != '\n' {
		t.Fatalf("%s ends mid-line", what)
	}
	n := 0
	for _, l := range bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n")) {
		if !json.Valid(l) {
			t.Fatalf("%s: line %d is not a whole JSON record", what, n+1)
		}
		n++
	}
	return n
}

func TestRecorderRotatesAtRecordBoundary(t *testing.T) {
	recs := fixtureRecords(t)
	a, b, c := line(t, recs[0]), line(t, recs[1]), line(t, recs[2])
	dir := t.TempDir()
	// Room for a and b exactly: c would cross the cap by its whole length.
	r, err := newRecorder(dir, testMAC, int64(len(a)+len(b)), 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs[:2] {
		if err := r.write(rec); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := readGen(t, dir, 1); ok {
		t.Fatal("rotated at exactly the cap; only a record past it should rotate")
	}
	if err := r.write(recs[2]); err != nil {
		t.Fatal(err)
	}
	r.Close()
	old, _ := readGen(t, dir, 1)
	live, _ := readGen(t, dir, 0)
	if want := append(append([]byte{}, a...), b...); !bytes.Equal(old, want) {
		t.Errorf(".1 = %d bytes, want the first two records byte for byte (%d)", len(old), len(want))
	}
	if !bytes.Equal(live, c) {
		t.Errorf("live file = %q, want the third record alone", live)
	}
}

func TestRecorderShiftsGenerationsAndDropsOldest(t *testing.T) {
	recs := fixtureRecords(t)
	dir := t.TempDir()
	// A cap of one byte rotates before every record after the first: each
	// file then holds exactly one record, so the generations are traceable.
	r, err := newRecorder(dir, testMAC, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	n := 6
	for _, rec := range recs[:n] {
		if err := r.write(rec); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	// live = record 5, .1 = 4, .2 = 3, .3 = 2; records 0 and 1 dropped.
	for gen := 0; gen <= 3; gen++ {
		got, ok := readGen(t, dir, gen)
		if !ok {
			t.Fatalf("generation %d missing", gen)
		}
		if want := line(t, recs[n-1-gen]); !bytes.Equal(got, want) {
			t.Errorf("generation %d holds the wrong record (%d bytes, want %d)", gen, len(got), len(want))
		}
	}
	if _, ok := readGen(t, dir, 4); ok {
		t.Error("a fourth generation was kept with keep: 3")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 4 {
		t.Errorf("%d files in the log dir, want the live file and three generations", len(entries))
	}
}

func TestRecorderNeverSplitsALine(t *testing.T) {
	recs := fixtureRecords(t)
	dir := t.TempDir()
	// A cap below the largest record (system_cfg pushes run to tens of KB):
	// an oversized record still lands whole, alone in a fresh file.
	r, err := newRecorder(dir, testMAC, 4096, 50)
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for _, rec := range recs {
		if err := r.write(rec); err != nil {
			t.Fatal(err)
		}
		want = append(want, line(t, rec)...)
	}
	r.Close()
	var got []byte
	total := 0
	for gen := 50; gen >= 0; gen-- {
		b, ok := readGen(t, dir, gen)
		if !ok {
			continue
		}
		n := wholeLines(t, fmt.Sprintf("generation %d", gen), b)
		if n > 1 && len(b) > 4096 {
			t.Errorf("generation %d: %d records in %d bytes, over the 4096 cap", gen, n, len(b))
		}
		total += n
		got = append(got, b...)
	}
	if total != len(recs) {
		t.Errorf("%d records across the generations, wrote %d", total, len(recs))
	}
	if !bytes.Equal(got, want) {
		t.Error("the generations, oldest first, are not the records written, byte for byte")
	}
}

func TestRecorderRotatesFileAlreadyOverCapOnRestart(t *testing.T) {
	recs := fixtureRecords(t)
	dir := t.TempDir()
	var prior []byte
	for _, rec := range recs[:4] {
		prior = append(prior, line(t, rec)...)
	}
	live := filepath.Join(dir, testMAC+".ndjson")
	if err := os.WriteFile(live, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	// Restarted with a cap the existing file already exceeds.
	r, err := newRecorder(dir, testMAC, int64(len(prior)/2), 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readGen(t, dir, 1); ok {
		t.Fatal("rotated at open; rotation happens only before a record")
	}
	if err := r.write(recs[4]); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if old, _ := readGen(t, dir, 1); !bytes.Equal(old, prior) {
		t.Error(".1 is not the file the bridge restarted on")
	}
	if got, _ := readGen(t, dir, 0); !bytes.Equal(got, line(t, recs[4])) {
		t.Error("the first record after the restart is not alone in a fresh live file")
	}
}

func TestRecorderAppendsUnderCapOnRestart(t *testing.T) {
	recs := fixtureRecords(t)
	dir := t.TempDir()
	first := line(t, recs[0])
	if err := os.WriteFile(filepath.Join(dir, testMAC+".ndjson"), first, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := newRecorder(dir, testMAC, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.write(recs[1]); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if got, _ := readGen(t, dir, 0); !bytes.Equal(got, append(append([]byte{}, first...), line(t, recs[1])...)) {
		t.Error("restart under the cap did not append to the existing file")
	}
}

func TestRecorderDefaults(t *testing.T) {
	r, err := newRecorder(t.TempDir(), testMAC, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.maxSize != 8<<20 || r.keep != 3 {
		t.Errorf("unset cap and keep = %d bytes, %d generations; want 8 MB and 3", r.maxSize, r.keep)
	}
	if DefaultRecordMaxSize != 8<<20 || DefaultRecordKeep != 3 {
		t.Error("defaults changed: update deploy/config.example.yaml and docs/install.md with them")
	}
}

func TestRecorderNoCap(t *testing.T) {
	recs := fixtureRecords(t)
	dir := t.TempDir()
	r, err := newRecorder(dir, testMAC, -1, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if err := r.write(rec); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	if _, ok := readGen(t, dir, 1); ok {
		t.Error("max_size off rotated")
	}
}

// The collapsed-404 counter is a line like any other: it rotates the file
// too, and Close's flush lands whole.
func TestRecorderCollapsed404sRotate(t *testing.T) {
	dir := t.TempDir()
	recs := fixtureRecords(t)
	r, err := newRecorder(dir, testMAC, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	pending := record{At: recs[0].At, Status: http.StatusNotFound}
	for range 4 {
		if err := r.write(pending); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.write(recs[0]); err != nil {
		t.Fatal(err)
	}
	r.Close()
	// .2 = the first 404, .1 = collapsed_404s: 3, live = the record.
	if b, _ := readGen(t, dir, 1); !strings.Contains(string(b), `"collapsed_404s":3`) || wholeLines(t, ".1", b) != 1 {
		t.Errorf(".1 = %q, want the collapsed-404 counter alone", b)
	}
	if b, _ := readGen(t, dir, 2); wholeLines(t, ".2", b) != 1 || !strings.Contains(string(b), `"status":404`) {
		t.Errorf(".2 = %q, want the first 404", b)
	}
}
