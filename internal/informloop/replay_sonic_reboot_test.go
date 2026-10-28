package informloop

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// The UI's Manage -> Restart, recorded from the SONiC lab switch
// (replies-sonic.ndjson: {"_type":"reboot","reboot_type":"soft",...}),
// served to an adopted bridge running the sonic driver against the VS
// capture: with control.reboot it runs `sudo reboot` on the switch, without
// it the reboot is emulated and nothing is written.
func TestReplaySonicRebootReply(t *testing.T) {
	replies := loadReplies(t, "replies-sonic.ndjson")
	var reboot replayRecord
	for _, r := range replies {
		var p map[string]any
		_ = json.Unmarshal(r.Payload, &p)
		if p["_type"] == "reboot" {
			reboot = r
		}
	}
	if reboot.Payload == nil {
		t.Fatal("no _type reboot reply in replies.ndjson")
	}
	for _, real := range []bool{true, false} {
		fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238524-ff3c0c9f6", "collect.txt"))
		if err != nil {
			t.Fatal(err)
		}
		var buf strings.Builder
		logger := log.New(&buf, "", 0)
		coll := sonic.NewCollector(fr)
		coll.Log = logger
		snap, err := coll.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		const key = "0123456789abcdef0123456789abcdef"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			pkt, err := inform.Decode(body, key)
			if err != nil {
				t.Errorf("decode inform: %v", err)
				w.WriteHeader(500)
				return
			}
			var mac [6]byte
			copy(mac[:], pkt.MAC[:])
			enc, err := (&inform.Packet{MAC: mac, Payload: reboot.Payload}).EncodeGCM(key)
			if err != nil {
				t.Fatal(err)
			}
			w.Header().Set("Content-Type", "application/x-binary")
			w.Write(enc)
		}))
		desc := inform.Descriptor{MAC: snap.System.MAC}
		sess := device.NewSession(desc, srv.URL, device.State{Adopted: true, Key: key, UseAESGCM: true, InformURL: srv.URL, CfgVersion: "v0"}, nil, time.Now())
		sess.SetSnapshot(snap)
		l, err := New(desc, sess, Config{Logger: logger, Collector: coll, Controller: coll, ControlReboot: real})
		if err != nil {
			t.Fatal(err)
		}
		l.client = srv.Client()
		l.informOnce(context.Background())
		srv.Close()

		rebooted := false
		for _, c := range fr.Commands {
			if c == "sudo reboot" {
				rebooted = true
			}
		}
		if strings.Contains(buf.String(), `UNHANDLED _type "reboot"`) {
			t.Errorf("control.reboot=%v: the reboot reply was logged as unhandled:\n%s", real, buf.String())
		}
		switch {
		case real && (!rebooted || !strings.Contains(buf.String(), "reboot requested: reloading the switch")):
			t.Errorf("control.reboot on: want `sudo reboot`; commands %q, log:\n%s", fr.Commands, buf.String())
		case !real && (rebooted || !strings.Contains(buf.String(), "reboot requested (emulated")):
			t.Errorf("control.reboot off: want an emulated reboot; commands %q, log:\n%s", fr.Commands, buf.String())
		}
	}
}
