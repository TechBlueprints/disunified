package sonic

import (
	"context"
	"os"
)

// FixtureRunner serves a captured collector run (docs/fixtures/sonic-*/
// collect.txt, collect.py's output scrubbed by scripts/sanitize-sonic.py)
// instead of a switch, and records every other command in Commands without
// running it (none on the read side).
type FixtureRunner struct {
	Fixture  string
	Commands []string
	Stdins   []string
}

// NewFixtureRunner loads a capture file.
func NewFixtureRunner(path string) (*FixtureRunner, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &FixtureRunner{Fixture: string(b)}, nil
}

// Run implements sshrun.Runner: the collector gets the capture, everything
// else is recorded.
func (f *FixtureRunner) Run(_ context.Context, command, stdin string) (string, error) {
	if command == collectCommand {
		return f.Fixture, nil
	}
	f.Commands = append(f.Commands, command)
	f.Stdins = append(f.Stdins, stdin)
	return "", nil
}

func (f *FixtureRunner) Close() error { return nil }
