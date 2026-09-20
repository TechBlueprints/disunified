package proxmox

import (
	"context"
	"os"
	"regexp"
	"strings"
)

// FixtureRunner serves a captured collector run (docs/fixtures/proxmox-*/
// collect-*.txt, the script's output scrubbed by scripts/sanitize-proxmox.py)
// instead of a node, and records every other command (the writes) in
// Commands without running it. Exported so the wire-contract and replay
// tests in other packages can drive the real driver against real captures.
// A tag rewrite ("qm set 100 --tags '...'") is applied to the capture's
// copy of that guest's config, as the cluster would, so the next poll
// sees the tags the driver wrote and converges like a real node.
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
	if m := tagWrite.FindStringSubmatch(command); m != nil {
		f.applyTags(m[1], m[2], m[3])
	}
	return "", nil
}

var tagWrite = regexp.MustCompile(`^(qm|pct) set (\d+) --tags '([^']*)'$`)

// applyTags rewrites the "tags:" line of guest id's config in the capture.
func (f *FixtureRunner) applyTags(tool, id, tags string) {
	dir := "qemu-server"
	if tool == "pct" {
		dir = "lxc"
	}
	head := regexp.MustCompile(`(?m)^## /etc/pve/nodes/[^/\n]+/` + dir + `/` + id + `\.conf\n`)
	loc := head.FindStringIndex(f.Fixture)
	if loc == nil {
		return
	}
	rest := f.Fixture[loc[1]:]
	end := len(rest)
	if i := strings.Index(rest, "\n## "); i >= 0 {
		end = i + 1
	}
	if i := strings.Index(rest, "\n@@@ "); i >= 0 && i+1 < end {
		end = i + 1
	}
	body := rest[:end]
	line := "tags: " + tags + "\n"
	if tagLine := regexp.MustCompile(`(?m)^tags: .*\n`); tagLine.MatchString(body) {
		body = tagLine.ReplaceAllLiteralString(body, line)
	} else {
		body += line
	}
	f.Fixture = f.Fixture[:loc[1]] + body + rest[end:]
}

// Close implements Runner.
func (f *FixtureRunner) Close() error { return nil }
