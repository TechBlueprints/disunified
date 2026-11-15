package aristaeos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FixtureTransport serves captured EOS output from a directory instead of a
// switch: <cmd>.json for JSON commands and <cmd>.txt for text ones, where
// the file name is the command with spaces -> "-" and "/" -> "_" (what the
// capture procedure in docs/adding-a-device.md produces). It is exported so
// tests in other packages (the wire-contract and replay tests) can drive
// the real driver against real captures.
//
// Config batches are recorded in Configured and, unless Stateless is set,
// remembered: a later read shows what was written, as the switch would
// (fixture_state.go), so a reconcile after an applied push can be held to
// writing nothing and a sequence (a rule added, then removed) replays in
// order. A reply nothing has written to is the capture byte for byte. A
// write the transport cannot show in a later reply from captured forms is
// recorded in Unmodelled and changes nothing a read returns.
//
// Changing Dir or an Overrides entry replaces the switch's state for the
// commands it changes: the new capture is the switch now, what was written
// before included.
type FixtureTransport struct {
	Dir        string
	Overrides  map[string]string // command -> fixture file name, for scenario variants (JSON and text)
	Runs       [][]string
	TextRuns   [][]string
	Configured [][]string
	// Unmodelled is every written command no later read shows, as
	// "<mode>: <command>" ("interface Ethernet2: channel-group 5 mode
	// active"), in order.
	Unmodelled []string
	// Stateless serves the captures whatever was written (the transport
	// before 2026-11-15).
	Stateless bool
	// Warnings are the warnings EOS returned for a config command, as a
	// capture shows them (ConfigureWarn returns them for each batch that
	// runs the command).
	Warnings map[string][]string

	mu       sync.Mutex
	docs     map[string]*fixtureDoc // command -> its reply as written to
	sessions map[string]*fixtureSession
	// was is each interface's status before a modelled shutdown, for the
	// `no shutdown` that brings it back.
	was map[string]ifStatus
}

// NewFixtureTransport serves docs/fixtures/<dir>.
func NewFixtureTransport(dir string) *FixtureTransport { return &FixtureTransport{Dir: dir} }

func fixtureName(cmd, ext string) string {
	return strings.NewReplacer(" ", "-", "/", "_").Replace(cmd) + ext
}

// path is the capture a command is served from.
func (f *FixtureTransport) path(cmd string) string {
	name := fixtureName(cmd, ".json")
	if o, ok := f.Overrides[cmd]; ok {
		name = o
	}
	return filepath.Join(f.Dir, name)
}

// Run implements Transport.
func (f *FixtureTransport) Run(_ context.Context, cmds []string) ([]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Runs = append(f.Runs, cmds)
	out := make([]json.RawMessage, 0, len(cmds))
	for _, c := range cmds {
		if c == "enable" {
			out = append(out, json.RawMessage("{}"))
			continue
		}
		p := f.path(c)
		if d := f.docs[c]; d != nil && d.path == p {
			b, err := json.Marshal(d.v)
			if err != nil {
				return nil, fmt.Errorf("fixture %q as written: %w", c, err)
			}
			out = append(out, b)
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("no fixture for %q: %w", c, err)
		}
		out = append(out, finiteJSON(b)) // as the live transports do
	}
	return out, nil
}

// RunText implements TextRunner from <cmd>.txt fixtures. No write changes a
// text reply: the driver reads only counters as text.
func (f *FixtureTransport) RunText(_ context.Context, cmds []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.TextRuns = append(f.TextRuns, cmds)
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		if c == "enable" {
			out = append(out, "")
			continue
		}
		name := fixtureName(c, ".txt")
		if o, ok := f.Overrides[c]; ok {
			name = o
		}
		b, err := os.ReadFile(filepath.Join(f.Dir, name))
		if err != nil {
			return nil, fmt.Errorf("no text fixture for %q: %w", c, err)
		}
		out = append(out, string(b))
	}
	return out, nil
}

// Configure records the batch and, unless Stateless, applies it to the
// replies it changes.
func (f *FixtureTransport) Configure(_ context.Context, cmds []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Configured = append(f.Configured, cmds)
	if !f.Stateless {
		f.configure(cmds)
	}
	return nil
}

// Close is a no-op.
func (f *FixtureTransport) Close() error { return nil }

// fixtureDoc is one command's reply, decoded (numbers kept as written).
type fixtureDoc struct {
	path string
	v    map[string]any
}

// doc is cmd's reply for writing to: the one already written to, or the
// capture, decoded. nil when there is no capture.
func (f *FixtureTransport) doc(cmd string) *fixtureDoc {
	p := f.path(cmd)
	if d := f.docs[cmd]; d != nil && d.path == p {
		return d
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(finiteJSON(b)))
	dec.UseNumber() // counters are uint64: a float64 would round them
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	if f.docs == nil {
		f.docs = map[string]*fixtureDoc{}
	}
	d := &fixtureDoc{path: p, v: v}
	f.docs[cmd] = d
	return d
}

// ConfigureWarn implements ConfigWarner: Configure, with the Warnings of the
// batch's commands.
func (f *FixtureTransport) ConfigureWarn(ctx context.Context, cmds []string) ([]string, error) {
	if err := f.Configure(ctx, cmds); err != nil {
		return nil, err
	}
	var out []string
	for _, c := range cmds {
		out = append(out, f.Warnings[c]...)
	}
	return out, nil
}
