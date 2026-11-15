// Package eos collects switch state from an Arista EOS device.
//
// Everything here was written against EOS 4.26.14M on a DCS-7160-48TC6-F —
// the last EOS train that platform can run — using the captured output in
// docs/fixtures/arista-eos-4.26.14M. Two transports produce identical JSON: eAPI
// (JSON-RPC over HTTPS, needs credentials and `management api http-commands`
// enabled) and SSH (`show ... | json` over a key-authenticated CLI session,
// needs nothing). The collector does not care which.
package aristaeos

import (
	"context"
	"encoding/json"
	"strings"
)

// Transport runs EOS show commands and returns each command's JSON output in
// order. Implementations must fail the whole call when any command errors —
// EOS answers `% Invalid input` for a command this version lacks, and a
// collector that silently reports zeros for such a command is worse than one
// that refuses to start.
type Transport interface {
	Run(ctx context.Context, cmds []string) ([]json.RawMessage, error)
	// Configure runs CLI commands that produce no JSON (enable, configure,
	// interface ..., shutdown, write). It fails if EOS rejects any of them.
	Configure(ctx context.Context, cmds []string) error
	Close() error
}

// TextRunner is an optional transport capability: run show commands and
// return each one's plain-text output. Some counters exist only in text
// form on EOS 4.26 (FEC codewords in `show interfaces X phy detail`); a
// transport without it simply leaves those fields unset.
type TextRunner interface {
	RunText(ctx context.Context, cmds []string) ([]string, error)
}

// ConfigWarner is an optional transport capability: Configure that also
// returns the warnings EOS printed for the batch, in order. A warning is
// not an error to EOS -- a list the hardware cannot program still commits,
// with `! Error in ACL commit, ...` as a warning (4.26.14M,
// docs/drivers/arista-eos.md §9.4) -- so Configure alone never shows it.
// eAPI returns each command's warnings in its result (`{"warnings":
// [...]}`); the CLI prints them as `! ` lines (docs/fixtures/
// arista-eos-4.26.14M/acl-commit-warning/).
type ConfigWarner interface {
	ConfigureWarn(ctx context.Context, cmds []string) ([]string, error)
}

// resultWarnings are the warnings in eAPI results, in command order.
func resultWarnings(results []json.RawMessage) []string {
	var out []string
	for _, r := range results {
		var v struct {
			Warnings []string `json:"warnings"`
		}
		if json.Unmarshal(r, &v) == nil {
			out = append(out, v.Warnings...)
		}
	}
	return out
}

// cliWarnings are the `! ` messages in CLI output, each with the lines that
// continue it, without the `! ` and the CLI's ` at line N` suffix -- the
// text eAPI returns for the same warning.
func cliWarnings(out string) []string {
	var warns []string
	cur := -1
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "! "):
			warns = append(warns, strings.TrimPrefix(line, "! "))
			cur = len(warns) - 1
		case cur >= 0 && line != "" && !strings.HasPrefix(line, ">") && !strings.HasPrefix(line, "{") && !strings.HasPrefix(line, "%"):
			warns[cur] += "\n" + line
		default:
			cur = -1
		}
	}
	for i, w := range warns {
		if j := strings.LastIndex(w, " at line "); j >= 0 && strings.Trim(w[j+len(" at line "):], "0123456789") == "" {
			warns[i] = w[:j]
		}
	}
	return warns
}

// configureWarn runs a config batch through t, with its warnings when the
// transport can report them.
func configureWarn(ctx context.Context, t Transport, cmds []string) ([]string, error) {
	if w, ok := t.(ConfigWarner); ok {
		return w.ConfigureWarn(ctx, cmds)
	}
	return nil, t.Configure(ctx, cmds)
}
