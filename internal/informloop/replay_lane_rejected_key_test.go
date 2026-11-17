package informloop

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/inform"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/breakout"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// A Forget the bridge missed (rejectedkey.go, docs/drivers/breakout.md
// 16.11): the controller's answers are replayed from reply logs, each at
// its recorded time, to an adopted automatic lane switch on the DX010's
// cage 3 (collect-split-dac.txt).

// replyStep is one recorded answer: a status, the decrypted payload of a
// 200, or no answer at all (the recorded error: the controller down).
type replyStep struct {
	At          time.Time       `json:"at"`
	Status      int             `json:"status"`
	BodyLen     int             `json:"body_len"`
	SentAdopted bool            `json:"sent_adopted"`
	Error       string          `json:"error"`
	Payload     json.RawMessage `json:"payload"`
	Effects     []struct {
		Kind string `json:"kind"`
	} `json:"effects"`
}

func readSteps(t *testing.T, parts ...string) []replyStep {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{"..", "..", "docs", "fixtures"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	var out []replyStep
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var s replyStep
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatal(err)
		}
		if s.Status == 0 && s.Error == "" {
			continue // a recorder line with nothing sent (the log's start marker)
		}
		out = append(out, s)
	}
	return out
}

// noopReply is a 200's payload where a status-only record has none: the
// lane switch's own recorded noop.
func noopReply(t *testing.T) json.RawMessage {
	t.Helper()
	for _, s := range readSteps(t, "controller-11.0.99", "replies-lane-switch-dx010-cage3.ndjson") {
		if strings.Contains(string(s.Payload), `"noop"`) {
			return s.Payload
		}
	}
	t.Fatal("no noop reply in the lane switch's log")
	return nil
}

type laneReplay struct {
	t         *testing.T
	l         *Loop
	sess      *device.Session
	coll      *sonic.Collector
	fr        *sonic.FixtureRunner
	buf       *syncBuf
	unadopted int
	keys      []string
	steps     []replyStep
	next      int
	noop      json.RawMessage
}

// newLaneReplay is cage 3's lane switch, attached and adopted on key with
// the push it had applied, answered by steps in order.
func newLaneReplay(t *testing.T, adopted, forgetOnRejected bool, steps []replyStep) *laneReplay {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7", "collect-split-dac.txt"))
	if err != nil {
		t.Fatal(err)
	}
	r := &laneReplay{t: t, fr: fr, buf: &syncBuf{}, steps: steps, noop: noopReply(t)}
	logger := log.New(r.buf, "", 0)
	r.coll = sonic.NewCollector(fr)
	r.coll.Log = logger
	if _, err := r.coll.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const mac, name = "02:00:00:00:00:aa", "sw-cage3"
	if adopted {
		if err := r.coll.AttachLaneSwitch(3, devicemodel.Attachment{MAC: mac, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	o, err := breakout.ParseOptions(map[string]string{"parent": "sw", "cage": "3", "mac": mac, "name": name})
	if err != nil {
		t.Fatal(err)
	}
	lanes := breakout.New(o, r.coll, r.coll)
	lanes.Log = logger
	snap, err := lanes.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	const key = "0123456789abcdef0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		k := key
		pkt, err := inform.Decode(body, key)
		if err != nil {
			if pkt, err = inform.Decode(body, inform.DefaultKey); err != nil {
				w.WriteHeader(500)
				return
			}
			k = inform.DefaultKey
			r.keys = append(r.keys, "default")
		} else {
			r.keys = append(r.keys, "adopted")
		}
		s := r.steps[r.next-1]
		switch {
		case s.Error != "":
			// The controller was down: no answer (the recorded refusals).
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		case s.Status != http.StatusOK:
			w.WriteHeader(s.Status)
		default:
			payload := s.Payload
			if len(payload) == 0 {
				payload = r.noop
			}
			var m [6]byte
			copy(m[:], pkt.MAC[:])
			enc, _ := (&inform.Packet{MAC: m, Payload: payload}).EncodeGCM(k)
			w.Header().Set("Content-Type", "application/x-binary")
			w.Write(enc)
		}
	}))
	t.Cleanup(srv.Close)
	desc := inform.Descriptor{MAC: snap.System.MAC}
	st := device.State{Key: inform.DefaultKey, InformURL: srv.URL}
	if adopted {
		st = device.State{Adopted: true, Key: key, UseAESGCM: true, InformURL: srv.URL, CfgVersion: "c09c23495493524e", SystemCfg: "switch.port.1.name=Eth3/1\n"}
	}
	r.sess = device.NewSession(desc, srv.URL, st, nil, time.Now())
	r.sess.SetSnapshot(snap)
	r.l, err = New(desc, r.sess, Config{Logger: logger, Collector: lanes, Controller: lanes, ForgetOnRejectedKey: forgetOnRejected, OnUnadopted: func() {
		r.unadopted++
		r.coll.DetachLaneSwitch(3)
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.l.client = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	return r
}

// step informs once, answered by the next recorded step at its time.
func (r *laneReplay) step() replyStep {
	s := r.steps[r.next]
	r.next++
	r.l.now = func() time.Time { return s.At }
	r.l.informOnce(context.Background())
	return s
}

func (r *laneReplay) attached() bool {
	v, err := r.coll.Lanes(3)
	return err == nil && v.Child.MAC != ""
}

// The recorded forget of 2026-11-16 with its setdefault withheld: the
// 200 before it, then the three 400s the old key got (14:04:55 to
// 14:07:23, 2 min 28 s) and the 404 on the default key after. The third
// 400 is the Forget: detached, pending on the default key, no push left.
func TestMissedForgetIsReadFromTheRefusedKey(t *testing.T) {
	all := readSteps(t, "controller-11.0.99", "replies-lane-switch-dx010-cage3.ndjson")
	var steps []replyStep
	for i, s := range all {
		if len(s.Effects) == 1 && s.Effects[0].Kind == "unknown-type" { // the setdefault, which this replay withholds
			steps = append(steps, all[i-1])
			for _, n := range all[i+1:] {
				steps = append(steps, n)
				if n.Status == http.StatusNotFound {
					break
				}
			}
			break
		}
	}
	if len(steps) != 5 || steps[1].Status != 400 || steps[3].Status != 400 || steps[4].Status != 404 {
		t.Fatalf("the recorded forget is not 200, 400 x3, 404: %+v", steps)
	}
	r := newLaneReplay(t, true, true, steps)
	for i := 0; i < 3; i++ {
		r.step()
		if r.unadopted != 0 || !r.sess.Adopted() || !r.attached() {
			t.Fatalf("forgotten after step %d (unadopted %d, adopted %v)\n%s", i, r.unadopted, r.sess.Adopted(), r.buf.String())
		}
	}
	// Adopted, the lane switch reconciled its push every cycle; from the
	// forgetting inform on it writes nothing.
	writes := len(r.fr.Recorded())
	r.step() // the third 400
	if r.unadopted != 1 {
		t.Fatalf("OnUnadopted ran %d times, want 1\n%s", r.unadopted, r.buf.String())
	}
	if r.l.State() != StatePending || r.sess.Adopted() || r.sess.AuthKey() != inform.DefaultKey || r.attached() {
		t.Errorf("after the run: state %s, adopted %v, attached %v", r.l.State(), r.sess.Adopted(), r.attached())
	}
	if _, _, ok := r.sess.Applied(); ok {
		t.Error("the forgotten lane switch still holds a push to reconcile")
	}
	r.step() // pending: the default key, 404
	if got := strings.Join(r.keys, ","); got != "adopted,adopted,adopted,adopted,default" {
		t.Errorf("informs went out on %s", got)
	}
	if r.unadopted != 1 {
		t.Errorf("OnUnadopted ran %d times", r.unadopted)
	}
	if n := len(r.fr.Recorded()); n != writes {
		t.Errorf("the forget wrote to the switch: %q", r.fr.Recorded()[writes:])
	}
	if s := r.buf.String(); !strings.Contains(s, "refused this device's key 3 times over 2m28s") || !strings.Contains(s, "forgotten (the setdefault missed)") {
		t.Errorf("log:\n%s", s)
	}
}

// liveRefusals is the live run's 400s (2026-11-16 23:07:35 to 23:09:39,
// replies-lane-switch-dx010-cage3-missed-forget.ndjson): the bridge
// restarted on an adopted state file whose Forget it had already been
// sent, and informed at its 10 s start-up cadence. The 13th is the one the
// running bridge released the lane switch on ("inferred": "setdefault"),
// and the 404 on the default key came next.
func liveRefusals(t *testing.T) (refused []replyStep, after replyStep) {
	t.Helper()
	steps := readSteps(t, "controller-11.0.99", "replies-lane-switch-dx010-cage3-missed-forget.ndjson")
	for i, s := range steps {
		if s.Status == http.StatusBadRequest {
			refused = append(refused, s)
			continue
		}
		if len(refused) > 0 {
			return refused, steps[i]
		}
	}
	t.Fatal("no run of 400s in the live record")
	return nil, replyStep{}
}

// The live run replayed: the count is reached at the third 400 and the two
// minutes at the 13th (2 min 3 s; the 12th was at 1 min 53 s), where the
// bridge on the switch released the lane switch.
func TestMissedForgetLiveRunReleasesOnTheThirteenth(t *testing.T) {
	refused, after := liveRefusals(t)
	if len(refused) != 13 || after.Status != http.StatusNotFound || after.SentAdopted {
		t.Fatalf("the live record is not 400 x13 then 404 on the default key: %d, %+v", len(refused), after)
	}
	r := newLaneReplay(t, true, true, append(refused, after))
	for i := 0; i < 12; i++ {
		r.step()
		if r.unadopted != 0 || !r.attached() {
			t.Fatalf("released at 400 number %d, before two minutes\n%s", i+1, r.buf.String())
		}
	}
	r.step()
	if r.unadopted != 1 || r.attached() || r.sess.Adopted() || r.l.State() != StatePending {
		t.Fatalf("not released at the 13th: unadopted %d, adopted %v, state %s\n%s", r.unadopted, r.sess.Adopted(), r.l.State(), r.buf.String())
	}
	r.step()
	if got := r.keys[len(r.keys)-1]; got != "default" {
		t.Errorf("the inform after the release went out on the %s key", got)
	}
	if !strings.Contains(r.buf.String(), "refused this device's key 13 times over 2m3s") {
		t.Errorf("log:\n%s", r.buf.String())
	}
}

// The live run's 400s on a device that is not a lane switch: logged once,
// the key and the attachment kept.
func TestRefusedKeyOnlyWarnsElsewhere(t *testing.T) {
	steps, _ := liveRefusals(t)
	r := newLaneReplay(t, true, false, steps)
	for range steps {
		r.step()
	}
	if r.unadopted != 0 || !r.sess.Adopted() || !r.attached() {
		t.Fatalf("released: unadopted %d, adopted %v", r.unadopted, r.sess.Adopted())
	}
	if n := strings.Count(r.buf.String(), "WARNING: the controller has refused this device's key"); n != 1 {
		t.Errorf("warned %d times, want 1\n%s", n, r.buf.String())
	}
}

// What a controller restart looks like to an adopted bridge, from the
// deployed reply logs (status lines only): the PDU's of 2026-10-01 (four
// refused connections over 4 min 39 s, a 503, then 200) and the Arista's
// across Network 11.0.97's install on 2026-11-13 (one refusal, then 200 on
// the same key). Neither has a 400, and neither releases the lane switch.
func TestControllerRestartIsNotAForget(t *testing.T) {
	for _, f := range []string{"statuses-controller-restart-20261001-pdu.ndjson", "statuses-upgrade-to-11.0.97-20261113-arista.ndjson"} {
		t.Run(f, func(t *testing.T) {
			steps := readSteps(t, "controller-10.6.109", f)
			r := newLaneReplay(t, true, true, steps)
			for range steps {
				r.step()
			}
			if r.unadopted != 0 || !r.sess.Adopted() || !r.attached() || r.l.State() != StateConnected {
				t.Fatalf("released by a restart: unadopted %d, adopted %v, state %s\n%s", r.unadopted, r.sess.Adopted(), r.l.State(), r.buf.String())
			}
		})
	}
}

// The 400s that were not a forgotten key: the default key's, for a minute
// after a Forget (the Arista's re-adoption on 10.6.106, 2026-09-20: two
// 400s, then 404). A pending lane switch that gets them stays pending, and
// nothing is handed over or back.
func TestDefaultKeyRefusalsAfterAForgetChangeNothing(t *testing.T) {
	var steps []replyStep
	for _, s := range readSteps(t, "controller-10.6.106", "replies.ndjson") {
		if s.Status != http.StatusBadRequest && s.Status != http.StatusNotFound {
			break
		}
		steps = append(steps, s)
	}
	if len(steps) != 3 || steps[0].Status != 400 || steps[0].SentAdopted {
		t.Fatalf("the recorded re-adoption does not start 400 x2, 404 on the default key: %+v", steps)
	}
	r := newLaneReplay(t, false, true, steps)
	for range steps {
		r.step()
	}
	if r.unadopted != 0 || r.l.State() != StatePending || r.attached() {
		t.Fatalf("unadopted %d, state %s, attached %v", r.unadopted, r.l.State(), r.attached())
	}
	if strings.Contains(r.buf.String(), "refused this device's key") {
		t.Errorf("log:\n%s", r.buf.String())
	}
}
