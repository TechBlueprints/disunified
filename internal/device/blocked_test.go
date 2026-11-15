package device

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/inform"
)

// UniFi's client Block reaches a switch as the setparam's blocked_sta
// (docs/feature-map.md §3): every setparam the controller sends a bridged
// switch carries it, the config pushes and the interval-only ones alike.
// The captures: Network 10.6.106's reply logs (no client blocked: "") and
// 11.0.99's Arista reply log (one client blocked, scrubbed to
// 02:00:00:00:00:01).

// replyPayloads are the payloads of a captured reply log, in order.
func replyPayloads(t *testing.T, ver, name string) []json.RawMessage {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "docs", "fixtures", "controller-"+ver, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var rec struct {
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && len(rec.Payload) > 0 {
			out = append(out, rec.Payload)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// blockedEffects replays payloads through s and returns the
// EffectBlockedClients texts in order.
func blockedEffects(s *Session, payloads []json.RawMessage) []string {
	var out []string
	for _, p := range payloads {
		for _, e := range s.Apply(time.Now(), p) {
			if e.Kind == EffectBlockedClients {
				out = append(out, e.Text)
			}
		}
	}
	return out
}

func adoptedSession(store *Store) *Session {
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true}
	return NewSession(testDesc(), "http://192.0.2.1:8080/inform", st, store, time.Now())
}

func TestBlockedClientsFromTheReplies(t *testing.T) {
	s := adoptedSession(nil)
	if _, known := s.BlockedClients(); known {
		t.Fatal("known before any setparam")
	}
	// One client blocked: the field on every setparam, the effect once.
	got := blockedEffects(s, replyPayloads(t, "11.0.99", "replies-arista.ndjson"))
	if want := []string{"02:00:00:00:00:01"}; !reflect.DeepEqual(got, want) {
		t.Errorf("effects = %q, want %q (one change, then the same list on every setparam)", got, want)
	}
	macs, known := s.BlockedClients()
	if !known || !reflect.DeepEqual(macs, []string{"02:00:00:00:00:01"}) {
		t.Errorf("blocked = %q known %v", macs, known)
	}
	// None blocked ("" in every 10.6.106 setparam): the list empties once.
	got = blockedEffects(s, replyPayloads(t, "10.6.106", "replies.ndjson"))
	if want := []string{""}; !reflect.DeepEqual(got, want) {
		t.Errorf("effects = %q, want one change to none", got)
	}
	if macs, known := s.BlockedClients(); !known || len(macs) != 0 {
		t.Errorf("blocked = %q known %v, want none, known", macs, known)
	}
}

// A noop or a cmd says nothing about blocks; only a setparam does.
func TestBlockedClientsOnlyFromSetparam(t *testing.T) {
	s := adoptedSession(nil)
	var noops []json.RawMessage
	for _, p := range replyPayloads(t, "11.0.99", "replies-arista.ndjson") {
		var r struct {
			Type string `json:"_type"`
		}
		_ = json.Unmarshal(p, &r)
		if r.Type != "setparam" {
			noops = append(noops, p)
		}
	}
	if len(noops) == 0 {
		t.Fatal("capture changed: no reply other than setparam")
	}
	if got := blockedEffects(s, noops); len(got) != 0 {
		t.Errorf("effects from noops: %q", got)
	}
	if _, known := s.BlockedClients(); known {
		t.Error("a noop made the list known")
	}
}

// The list survives a restart, so a blocked client is not let through
// until the controller's next setparam.
func TestBlockedClientsPersist(t *testing.T) {
	store := &Store{Path: filepath.Join(t.TempDir(), "device.json")}
	s := adoptedSession(store)
	blockedEffects(s, replyPayloads(t, "11.0.99", "replies-arista.ndjson")[:1])
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	s2 := NewSession(testDesc(), "http://192.0.2.1:8080/inform", st, store, time.Now())
	if macs, known := s2.BlockedClients(); !known || !reflect.DeepEqual(macs, []string{"02:00:00:00:00:01"}) {
		t.Errorf("after a restart: %q known %v", macs, known)
	}
}

// The captured values as the controller writes them: one MAC, and two
// joined by a newline (the Block of a test client beside the one the site
// had, controller-11.0.99/replies-*-block.ndjson).
func TestParseBlockedSTA(t *testing.T) {
	var one string
	for _, p := range replyPayloads(t, "11.0.99", "replies-arista.ndjson") {
		var r struct {
			BlockedSTA *string `json:"blocked_sta"`
		}
		if json.Unmarshal(p, &r) == nil && r.BlockedSTA != nil {
			one = *r.BlockedSTA
			break
		}
	}
	if got := ParseBlockedSTA(one); !reflect.DeepEqual(got, []string{one}) {
		t.Errorf("captured %q parsed %q", one, got)
	}
	if got := ParseBlockedSTA(""); got != nil {
		t.Errorf("empty parsed %q", got)
	}
	two := "02:00:00:00:b1:01\n02:00:00:00:b1:02"
	found := false
	for _, p := range replyPayloads(t, "11.0.99", "replies-arista-block.ndjson") {
		var r struct {
			BlockedSTA *string `json:"blocked_sta"`
		}
		if json.Unmarshal(p, &r) == nil && r.BlockedSTA != nil {
			found = found || *r.BlockedSTA == two
		}
	}
	if !found {
		t.Fatalf("capture changed: no blocked_sta %q", two)
	}
	if got := ParseBlockedSTA(two); !reflect.DeepEqual(got, []string{"02:00:00:00:b1:01", "02:00:00:00:b1:02"}) {
		t.Errorf("two clients parsed %q", got)
	}
}

// The Block and Unblock of a test client, as the DX010's bridge received
// them: each setparam carries the whole list (the Block's 31 s after it was
// made, the Unblock's 6 s after), and the block-sta / unblock-sta cmds come
// later and change nothing more.
func TestBlockAndUnblockOnTheDX010(t *testing.T) {
	s := adoptedSession(nil)
	got := blockedEffects(s, replyPayloads(t, "11.0.99", "replies-dx010-block.ndjson"))
	want := []string{"02:00:00:00:b1:01,02:00:00:00:b1:02", "02:00:00:00:b1:02"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effects = %q, want %q", got, want)
	}
	if macs, _ := s.BlockedClients(); !reflect.DeepEqual(macs, []string{"02:00:00:00:b1:02"}) {
		t.Errorf("after the Unblock: %q", macs)
	}
	for _, p := range replyPayloads(t, "11.0.99", "replies-dx010-block.ndjson") {
		for _, e := range s.Apply(time.Now(), p) {
			if e.Kind == inform.EffectUnknownCmd {
				t.Errorf("a Block cmd is unhandled: %q", e.Text)
			}
		}
	}
}

// The Arista's bridge got the cmds first: block-sta at its next inform
// after the Block, the list in a setparam 85 s later, unblock-sta at the
// next inform after the Unblock and no setparam yet. The cmds change a list
// the session knows; on one it does not know they change nothing (one Block
// is not the list).
func TestBlockAndUnblockCmdsOnTheArista(t *testing.T) {
	arista := replyPayloads(t, "11.0.99", "replies-arista-block.ndjson")
	// Known before: the site's one blocked client, as the DX010's last
	// setparam of the capture carries it (the same scrub).
	dx := replyPayloads(t, "11.0.99", "replies-dx010-block.ndjson")
	s := adoptedSession(nil)
	blockedEffects(s, dx)
	got := blockedEffects(s, arista)
	want := []string{"02:00:00:00:b1:01,02:00:00:00:b1:02", "02:00:00:00:b1:02"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("effects = %q, want %q (block-sta, then the setparam changes nothing, then unblock-sta)", got, want)
	}
	fresh := adoptedSession(nil)
	got = blockedEffects(fresh, arista)
	// block-sta onto an unknown list does nothing; the setparam makes the
	// list known, and unblock-sta then applies.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unknown list: effects = %q, want %q (block-sta ignored)", got, want)
	}
}
