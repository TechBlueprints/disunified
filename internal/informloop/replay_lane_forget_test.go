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

// The controller forgets an adopted lane switch (docs/drivers/breakout.md
// 16): its next inform is answered with the setdefault captured on Network
// 11.0.99 (reply-setdefault.ndjson), the loop calls OnUnadopted -- main's
// detach -- and the lane switch is pending again, on the default key, with
// no push left to reconcile; the lanes are the parent's cage's again. The
// parent is the DX010 captured with cage 3 split (collect-split-dac.txt).
func TestForgottenLaneSwitchHandsItsLanesBack(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", "reply-setdefault.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7", "collect-split-dac.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var buf syncBuf
	logger := log.New(&buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	if _, err := coll.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const mac, name = "02:00:00:00:00:aa", "sw-cage3"
	if err := coll.AttachLaneSwitch(3, devicemodel.Attachment{MAC: mac, Name: name}); err != nil {
		t.Fatal(err)
	}
	o, err := breakout.ParseOptions(map[string]string{"parent": "sw", "cage": "3", "mac": mac, "name": name})
	if err != nil {
		t.Fatal(err)
	}
	lanes := breakout.New(o, coll, coll)
	lanes.Log = logger
	snap, err := lanes.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	const key = "0123456789abcdef0123456789abcdef"
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		pkt, err := inform.Decode(body, key)
		if err != nil {
			// The forgotten device informs on the default key: pending, 404.
			if _, derr := inform.Decode(body, inform.DefaultKey); derr == nil {
				keys = append(keys, "default")
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(500)
			return
		}
		keys = append(keys, "adopted")
		var m [6]byte
		copy(m[:], pkt.MAC[:])
		enc, _ := (&inform.Packet{MAC: m, Payload: rec.Payload}).EncodeGCM(key)
		w.Header().Set("Content-Type", "application/x-binary")
		w.Write(enc)
	}))
	defer srv.Close()
	desc := inform.Descriptor{MAC: snap.System.MAC}
	// The push it had applied: every lane as it is.
	st := device.State{Adopted: true, Key: key, UseAESGCM: true, InformURL: srv.URL, CfgVersion: "c09c23495493524e", SystemCfg: "switch.port.1.name=Eth3/1\n"}
	sess := device.NewSession(desc, srv.URL, st, nil, time.Now())
	sess.SetSnapshot(snap)
	unadopted := 0
	l, err := New(desc, sess, Config{Logger: logger, Collector: lanes, Controller: lanes, OnUnadopted: func() {
		unadopted++
		coll.DetachLaneSwitch(3)
	}})
	if err != nil {
		t.Fatal(err)
	}
	l.client = srv.Client()

	writes := len(fr.Recorded())
	l.informOnce(context.Background())
	if unadopted != 1 {
		t.Fatalf("OnUnadopted ran %d times, want 1\n%s", unadopted, buf.String())
	}
	if l.State() != StatePending || sess.Adopted() || sess.AuthKey() != inform.DefaultKey {
		t.Errorf("after the forget: state %s, adopted %v", l.State(), sess.Adopted())
	}
	if v, err := coll.Lanes(3); err != nil || v.Child.MAC != "" {
		t.Errorf("the cage is still attached: %+v, %v", v.Child, err)
	}
	if _, _, ok := sess.Applied(); ok {
		t.Error("the forgotten lane switch still holds a push to reconcile")
	}
	// Pending again under the same MAC, and nothing is written or refused.
	l.informOnce(context.Background())
	l.informOnce(context.Background())
	if got := strings.Join(keys, ","); got != "adopted,default,default" {
		t.Errorf("informs went out on %s, want adopted,default,default", got)
	}
	if n := len(fr.Recorded()); n != writes {
		t.Errorf("the forget wrote to the switch: %q", fr.Recorded()[writes:])
	}
	if s := buf.String(); strings.Contains(s, "not attached") || !strings.Contains(s, "setdefault received") {
		t.Errorf("log:\n%s", s)
	}
}
