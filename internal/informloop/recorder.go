package informloop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Reply-log rotation defaults: a bridge on a switch's own flash
// (docs/drivers/sonic-on-switch.md) writes about 40 MB a month, so the cap
// keeps the live file and its generations to 32 MB in all.
const (
	DefaultRecordMaxSize int64 = 8 << 20
	DefaultRecordKeep          = 3
)

// recorder appends NDJSON records to <dir>/<mac>.ndjson. Consecutive plain
// 404s are collapsed into a counter so a device that sits pending overnight
// does not write a line every ten seconds.
//
// The live file is capped at maxSize bytes: a record that would take it past
// the cap first rotates it (<mac>.ndjson -> .1 -> .2 ... -> .keep, the oldest
// dropped) and starts a fresh file, so a rotation always falls between two
// whole lines. A record larger than the cap on its own is written to an empty
// file rather than dropped. A file already over the cap when the bridge starts
// is rotated before the first record.
type recorder struct {
	name     string // the live file
	maxSize  int64  // < 0: no cap
	keep     int
	f        *os.File
	size     int64
	quiet404 int
}

func newRecorder(dir, mac string, maxSize int64, keep int) (*recorder, error) {
	if maxSize == 0 {
		maxSize = DefaultRecordMaxSize
	}
	if keep <= 0 {
		keep = DefaultRecordKeep
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r := &recorder{name: filepath.Join(dir, filepath.Base(mac)+".ndjson"), maxSize: maxSize, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *recorder) open() error {
	f, err := os.OpenFile(r.name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *recorder) write(rec record) error {
	if rec.Status == http.StatusNotFound && rec.Error == "" {
		r.quiet404++
		if r.quiet404 > 1 {
			return nil
		}
		return r.encode(rec)
	}
	if r.quiet404 > 1 {
		if err := r.encode(map[string]any{"at": rec.At, "collapsed_404s": r.quiet404 - 1}); err != nil {
			return err
		}
	}
	r.quiet404 = 0
	return r.encode(rec)
}

// encode writes v as one line, exactly as json.Encoder does (the format the
// replay fixtures and scripts/sanitize-controller.py read), rotating first
// when the line would take the live file past the cap.
func (r *recorder) encode(v any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return err
	}
	if r.f == nil {
		// A failed rotation left no file open; try again.
		if err := r.open(); err != nil {
			return err
		}
	}
	if r.maxSize > 0 && r.size > 0 && r.size+int64(buf.Len()) > r.maxSize {
		if err := r.rotate(); err != nil {
			return fmt.Errorf("rotate %s: %w", r.name, err)
		}
	}
	n, err := r.f.Write(buf.Bytes())
	r.size += int64(n)
	return err
}

// rotate shifts the generations up by one, dropping the oldest, moves the
// live file to .1 and opens a fresh one. The live file is reopened whatever
// happens, so a failed rename costs a rotation, not the log.
func (r *recorder) rotate() error {
	_ = r.f.Close()
	r.f = nil
	gen := func(i int) string { return fmt.Sprintf("%s.%d", r.name, i) }
	var errs []error
	if err := os.Remove(gen(r.keep)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for i := r.keep - 1; i >= 1; i-- {
		if err := os.Rename(gen(i), gen(i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := os.Rename(r.name, gen(1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := r.open(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *recorder) Close() {
	if r.quiet404 > 1 {
		_ = r.encode(map[string]any{"at": time.Now(), "collapsed_404s": r.quiet404 - 1})
	}
	if r.f != nil {
		_ = r.f.Close()
	}
}
