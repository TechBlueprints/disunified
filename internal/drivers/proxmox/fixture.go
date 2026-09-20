package proxmox

import (
	"context"
	"os"
	"strings"
)

// FixtureRunner serves a captured collector run (docs/fixtures/proxmox-*/
// collect-*.txt, the script's output scrubbed by scripts/sanitize-text-fixture.py)
// instead of a node, and records every other command (the writes) in
// Commands without running it. Exported so the wire-contract and replay
// tests in other packages can drive the real driver against real captures.
type FixtureRunner struct {
	Fixture  string
	Commands []string
	Stdins   []string // what each recorded command received on stdin
}

// NewFixtureRunner loads a capture file.
func NewFixtureRunner(path string) (*FixtureRunner, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &FixtureRunner{Fixture: string(b)}, nil
}

// Run implements Runner: the collector script gets the capture, everything
// else is recorded.
func (f *FixtureRunner) Run(_ context.Context, command, stdin string) (string, error) {
	if strings.HasPrefix(command, "bash -s") {
		return f.Fixture, nil
	}
	f.Commands = append(f.Commands, command)
	f.Stdins = append(f.Stdins, stdin)
	return "", nil
}

// Close implements Runner.
func (f *FixtureRunner) Close() error { return nil }
