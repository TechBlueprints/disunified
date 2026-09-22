package aristaeos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FixtureTransport serves captured EOS output from a directory instead of a
// switch: <cmd>.json for JSON commands and <cmd>.txt for text ones, where
// the file name is the command with spaces -> "-" and "/" -> "_" (what the
// capture procedure in docs/adding-a-device.md produces). Config batches
// are recorded, not executed. It is exported so tests in other packages
// (the wire-contract and replay tests) can drive the real driver against
// real captures.
type FixtureTransport struct {
	Dir        string
	Overrides  map[string]string // command -> fixture file name, for scenario variants
	Runs       [][]string
	TextRuns   [][]string
	Configured [][]string
}

// NewFixtureTransport serves docs/fixtures/<dir>.
func NewFixtureTransport(dir string) *FixtureTransport { return &FixtureTransport{Dir: dir} }

func fixtureName(cmd, ext string) string {
	return strings.NewReplacer(" ", "-", "/", "_").Replace(cmd) + ext
}

// Run implements Transport.
func (f *FixtureTransport) Run(_ context.Context, cmds []string) ([]json.RawMessage, error) {
	f.Runs = append(f.Runs, cmds)
	out := make([]json.RawMessage, 0, len(cmds))
	for _, c := range cmds {
		if c == "enable" {
			out = append(out, json.RawMessage("{}"))
			continue
		}
		name := fixtureName(c, ".json")
		if o, ok := f.Overrides[c]; ok {
			name = o
		}
		b, err := os.ReadFile(filepath.Join(f.Dir, name))
		if err != nil {
			return nil, fmt.Errorf("no fixture for %q: %w", c, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// RunText implements TextRunner from <cmd>.txt fixtures.
func (f *FixtureTransport) RunText(_ context.Context, cmds []string) ([]string, error) {
	f.TextRuns = append(f.TextRuns, cmds)
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		if c == "enable" {
			out = append(out, "")
			continue
		}
		b, err := os.ReadFile(filepath.Join(f.Dir, fixtureName(c, ".txt")))
		if err != nil {
			return nil, fmt.Errorf("no text fixture for %q: %w", c, err)
		}
		out = append(out, string(b))
	}
	return out, nil
}

// Configure records the batch.
func (f *FixtureTransport) Configure(_ context.Context, cmds []string) error {
	f.Configured = append(f.Configured, cmds)
	return nil
}

// Close is a no-op.
func (f *FixtureTransport) Close() error { return nil }
