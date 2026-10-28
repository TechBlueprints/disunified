package informloop

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The UI's Locate, recorded (replies.ndjson: set-locate, then
// unset-locate), served to an adopted bridge running the sonic driver,
// which implements devicemodel.Locator: on a platform whose API sets the
// status LED (the DX010, modelled) the loop hands both to the driver, which
// blinks the LED and leaves it green; on the VS the API fails, that is
// logged once, and the session reports locating either way.
func TestReplaySonicLocate(t *testing.T) {
	var set, unset json.RawMessage
	for _, r := range loadReplies(t, "replies.ndjson") {
		var p map[string]any
		_ = json.Unmarshal(r.Payload, &p)
		switch p["cmd"] {
		case "set-locate":
			set = r.Payload
		case "unset-locate":
			unset = r.Payload
		}
	}
	if set == nil || unset == nil {
		t.Fatal("no set-locate / unset-locate reply in replies.ndjson")
	}
	led := regexp.MustCompile(`set_status_led\("(green|off)"\)`)
	for _, works := range []bool{true, false} {
		fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238524-ff3c0c9f6", "collect.txt"))
		if err != nil {
			t.Fatal(err)
		}
		fr.LEDWorks = works
		var mu syncBuf
		logger := log.New(&mu, "", 0)
		coll := sonic.NewCollector(fr)
		coll.Log = logger
		coll.BlinkInterval = 5 * time.Millisecond
		snap, err := coll.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		const key = "0123456789abcdef0123456789abcdef"
		next := set
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			pkt, err := inform.Decode(body, key)
			if err != nil {
				w.WriteHeader(500)
				return
			}
			var mac [6]byte
			copy(mac[:], pkt.MAC[:])
			enc, _ := (&inform.Packet{MAC: mac, Payload: next}).EncodeGCM(key)
			w.Header().Set("Content-Type", "application/x-binary")
			w.Write(enc)
		}))
		desc := inform.Descriptor{MAC: snap.System.MAC}
		sess := device.NewSession(desc, srv.URL, device.State{Adopted: true, Key: key, UseAESGCM: true, InformURL: srv.URL, CfgVersion: "v0"}, nil, time.Now())
		sess.SetSnapshot(snap)
		l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll})
		if err != nil {
			t.Fatal(err)
		}
		l.client = srv.Client()
		locating := func() bool {
			var m map[string]any
			_ = json.Unmarshal(sess.BuildPayload(time.Now()), &m)
			v, _ := m["locating"].(bool)
			return v
		}
		leds := func() []string {
			var out []string
			for _, c := range fr.Recorded() {
				if m := led.FindStringSubmatch(c); m != nil {
					out = append(out, m[1])
				}
			}
			return out
		}

		l.informOnce(context.Background())
		if !locating() {
			t.Errorf("LED %v: set-locate must report locating", works)
		}
		if works {
			deadline := time.Now().Add(5 * time.Second)
			for len(leds()) < 3 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
		}
		next = unset
		l.informOnce(context.Background())
		srv.Close()
		if locating() {
			t.Errorf("LED %v: unset-locate must stop locating", works)
		}
		got := leds()
		switch {
		case works && (len(got) < 4 || got[0] != "off" || got[len(got)-1] != "green"):
			t.Errorf("LED writes %v: want a blink starting off and ending green", got)
		case !works && len(got) != 1:
			t.Errorf("VS: LED writes %v, want the one that found no LED", got)
		}
		logs := mu.String()
		if !strings.Contains(logs, "locate on (reported as locating; the driver drives the device's LED)") {
			t.Errorf("log:\n%s", logs)
		}
		if !works && strings.Count(logs, "no controllable status LED") != 1 {
			t.Errorf("VS: the missing LED must be logged once:\n%s", logs)
		}
		coll.Close()
	}
}

// syncBuf is a log sink the locate blink's goroutine may write to.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
