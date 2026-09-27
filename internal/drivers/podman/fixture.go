package podman

import (
	"context"
	"os"
	"regexp"
	"strings"
)

// FixtureRunner serves a captured collector run (docs/fixtures/podman-*/
// collect.txt, the script's output scrubbed by scripts/sanitize-podman.py)
// instead of a host, and records every other command in Commands without
// running it. A slot-file write is applied to the capture's "slots"
// section, as the host would keep it, so the next poll reads the
// assignments back like a real host.
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

var slotsWrite = regexp.MustCompile(`^mkdir -p '[^']*' && cat > '([^']*)\.tmp' && mv -f `)

// Run implements sshrun.Runner: the collector script gets the capture,
// everything else is recorded.
func (f *FixtureRunner) Run(_ context.Context, command, stdin string) (string, error) {
	if strings.HasPrefix(command, "bash -s") {
		return f.Fixture, nil
	}
	f.Commands = append(f.Commands, command)
	f.Stdins = append(f.Stdins, stdin)
	if slotsWrite.MatchString(command) {
		f.setSection("slots", stdin)
	}
	return "", nil
}

// setSection replaces a section's body in the capture.
func (f *FixtureRunner) setSection(name, body string) {
	head := "@@@ " + name + "\n"
	i := strings.Index(f.Fixture, head)
	if i < 0 {
		return
	}
	rest := f.Fixture[i+len(head):]
	j := strings.Index(rest, "@@@ ")
	if j < 0 {
		return
	}
	f.Fixture = f.Fixture[:i] + head + strings.TrimRight(body, "\n") + "\n" + rest[j:]
}

// RemoveContainer drops a container from the capture's ps and inspect
// sections, as `podman rm` would, for the slot-release tests.
func (f *FixtureRunner) RemoveContainer(name string) {
	for _, sec := range []string{"ps", "inspect"} {
		head := "@@@ " + sec + "\n"
		i := strings.Index(f.Fixture, head)
		if i < 0 {
			continue
		}
		rest := f.Fixture[i+len(head):]
		j := strings.Index(rest, "@@@ ")
		body := rest[:j]
		// Each container is one JSON object in a top-level array; drop the
		// object that names it. The objects are flat enough for a bracket
		// count to find their bounds.
		key := `"` + name + `"`
		k := strings.Index(body, key)
		if k < 0 {
			continue
		}
		start := strings.LastIndex(body[:k], "{")
		depth, end := 0, -1
		for p := start; p < len(body); p++ {
			switch body[p] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = p + 1
				}
			}
			if end > 0 {
				break
			}
		}
		if start < 0 || end < 0 {
			continue
		}
		cut := body[:start] + body[end:]
		cut = strings.ReplaceAll(cut, ", ,", ",")
		cut = strings.ReplaceAll(cut, "[, ", "[")
		cut = strings.ReplaceAll(cut, ", ]", "]")
		cut = strings.ReplaceAll(cut, ",]", "]")
		f.Fixture = f.Fixture[:i] + head + cut + rest[j:]
	}
}

func (f *FixtureRunner) Close() error { return nil }
