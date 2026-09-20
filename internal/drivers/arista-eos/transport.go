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
