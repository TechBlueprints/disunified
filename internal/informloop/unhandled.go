package informloop

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// A reply the session does not act on (docs/gateways.md, "Since then").
//
// Some of the controller's commands are requests: it repeats one every
// inform until the device answers, and the device can stay unprovisioned
// meanwhile. That is a third party's report for an emulated gateway and
// its `wg-genkeypair` command (about once a minute, on Network 10.6.106);
// not captured here: in the deployed bridges' reply logs (2026-09-20 to
// 2026-11-17, about 475 000 replies) no command carries a `request_id`,
// and each command the session did not handle at the time came once or
// twice in all.
//
// So an unhandled command or reply type that comes again within
// unhandledRepeatWithin is logged as a WARNING with its count. The window
// is several of the controller's inform intervals (65-80 s on this site),
// so a repeat on every inform is caught and the same button pressed twice
// in a day is not.
//
// Every line also names the reply's top-level keys (names only, never a
// value), so a field that marks a request shows on the first sight of it.
// No key name is tested for: none that marks a request has been captured.
const unhandledRepeatWithin = 10 * time.Minute

// unhandledRun is one unhandled command's (or reply type's) run of
// repeats.
type unhandledRun struct {
	count       int
	first, last time.Time
}

// noteUnhandled returns the log line for a reply the session did not act
// on: what is "cmd" or "_type", name the command or the type, payload the
// decoded reply. Called from informOnce only.
func (l *Loop) noteUnhandled(now time.Time, what, name string, payload []byte) string {
	if l.unhandled == nil {
		l.unhandled = map[string]*unhandledRun{}
	}
	id := what + " " + name
	run := l.unhandled[id]
	if run == nil || now.Sub(run.last) > unhandledRepeatWithin {
		run = &unhandledRun{first: now}
		l.unhandled[id] = run
	}
	run.count++
	run.last = now

	line := fmt.Sprintf("UNHANDLED %s %q", what, name)
	if keys := replyKeys(payload); keys != "" {
		line += " (reply keys: " + keys + ")"
	}
	if run.count < 2 {
		return line
	}
	return fmt.Sprintf("WARNING: %s received %d times in %s: the controller repeats what it is waiting on, "+
		"and the device may stay unprovisioned until this is answered",
		line, run.count, now.Sub(run.first).Round(time.Second))
}

// replyKeys lists a reply's top-level key names, sorted.
func replyKeys(payload []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
