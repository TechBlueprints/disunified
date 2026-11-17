package informloop

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The one command in the recorded replies that the session does not act
// on is build-ssh-session (replies.ndjson, Network 10.6.106, 2026-09-19).
// Served once it is logged as before, with the reply's key names; served
// again on the next informs it is a WARNING with its count; after a quiet
// spell longer than the window it is a first sight again.
func TestUnhandledCommandRepeats(t *testing.T) {
	var payload []byte
	for _, rec := range loadReplies(t, "replies.ndjson") {
		var p map[string]any
		_ = json.Unmarshal(rec.Payload, &p)
		if p["cmd"] == "build-ssh-session" {
			payload = rec.Payload
		}
	}
	if payload == nil {
		t.Fatal("the fixture no longer has a build-ssh-session reply")
	}

	l := &Loop{}
	at := time.Date(2026, 9, 19, 23, 41, 14, 0, time.UTC)
	first := l.noteUnhandled(at, "cmd", "build-ssh-session", payload)
	if !strings.HasPrefix(first, `UNHANDLED cmd "build-ssh-session" (reply keys: `) {
		t.Errorf("first sight: %q", first)
	}
	for _, key := range []string{"_type", "cmd", "stun", "turn", "username", "uuid"} {
		if !strings.Contains(first, key) {
			t.Errorf("first sight does not name the reply's key %q: %q", key, first)
		}
	}
	// Names only: none of the reply's values.
	var fields map[string]any
	_ = json.Unmarshal(payload, &fields)
	for k, v := range fields {
		if s, ok := v.(string); ok && k != "cmd" && k != "_type" && strings.Contains(first, s) {
			t.Errorf("first sight carries the value of %q: %q", k, first)
		}
	}

	second := l.noteUnhandled(at.Add(70*time.Second), "cmd", "build-ssh-session", payload)
	if !strings.HasPrefix(second, `WARNING: UNHANDLED cmd "build-ssh-session"`) || !strings.Contains(second, "received 2 times in 1m10s") {
		t.Errorf("second sight within the window: %q", second)
	}
	third := l.noteUnhandled(at.Add(140*time.Second), "cmd", "build-ssh-session", payload)
	if !strings.Contains(third, "received 3 times in 2m20s") {
		t.Errorf("third sight: %q", third)
	}

	// The same name as a reply type is a run of its own.
	if other := l.noteUnhandled(at.Add(150*time.Second), "_type", "build-ssh-session", payload); strings.HasPrefix(other, "WARNING") {
		t.Errorf("a reply type must not join the command's run: %q", other)
	}

	// A quiet spell longer than the window ends the run.
	later := l.noteUnhandled(at.Add(140*time.Second+unhandledRepeatWithin+time.Second), "cmd", "build-ssh-session", payload)
	if strings.HasPrefix(later, "WARNING") {
		t.Errorf("after the window: %q", later)
	}
}
