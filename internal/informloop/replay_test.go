package informloop

import (
	"bufio"
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

	"github.com/TechBlueprints/switch-to-unifi/internal/device"
	"github.com/TechBlueprints/switch-to-unifi/internal/drivers/aristaeos"
)

// Replay: real controller replies (docs/fixtures/controller-10.6.106/
// replies.ndjson — the bridge's own reply log from Network 10.6.106 against
// the Arista, identifiers and secrets replaced) are served to the loop by a
// fake controller, encrypted with a test key the way the controller
// encrypts them, and the loop drives the real Arista driver against the real
// EOS captures. What must come out the other end is the exact switch
// configuration each push produced live.
const replayKey = "0123456789abcdef0123456789abcdef"

type replayRecord struct {
	Status  int             `json:"status"`
	Payload json.RawMessage `json:"payload"`
	Effects []struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"effects"`
}

func loadReplies(t *testing.T) []replayRecord {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106", "replies.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []replayRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var r replayRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestReplayControllerReplies(t *testing.T) {
	replies := loadReplies(t)
	if len(replies) < 5 {
		t.Fatalf("only %d replies in the fixture", len(replies))
	}

	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "eos-4.26.14M"))
	coll := aristaeos.NewCollector(ft)
	coll.Log = log.New(os.Stderr, "", 0)
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := device.DescriptorFor("UDC48X6", snap, device.Identity{MAC: snap.System.MAC, IP: "192.0.2.9", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}

	// The fake controller: decodes what the device sends (so a malformed
	// inform fails the test) and answers with the next recorded reply.
	next := 0
	var lastInform map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read inform: %v", err)
		}
		pkt, err := inform.Decode(body, replayKey)
		if err != nil {
			t.Errorf("controller could not decode our inform: %v", err)
			w.WriteHeader(500)
			return
		}
		if err := json.Unmarshal(pkt.Payload, &lastInform); err != nil {
			t.Errorf("our inform is not JSON: %v", err)
		}
		if next >= len(replies) {
			w.WriteHeader(404)
			return
		}
		rec := replies[next]
		next++
		if rec.Status != 200 {
			w.WriteHeader(rec.Status)
			return
		}
		var mac [6]byte
		copy(mac[:], pkt.MAC[:])
		enc, err := (&inform.Packet{MAC: mac, Payload: rec.Payload}).EncodeGCM(replayKey)
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/x-binary")
		w.Write(enc)
	}))
	defer srv.Close()

	st := device.State{Key: replayKey, Adopted: true, UseAESGCM: true, InformURL: srv.URL}
	sess := device.NewSession(desc, srv.URL, st, nil, time.Now())
	sess.SetSnapshot(snap)
	logger := log.New(os.Stderr, "", 0)
	l, err := New(desc, sess, Config{
		Logger: logger, Interval: time.Minute, CollectTimeout: 10 * time.Second,
		Collector: coll, Controller: coll,
		ControlIGMP: true, ControlNTP: true, ControlSyslog: true, ControlSNMP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	l.client = srv.Client()
	ctx := context.Background()

	var logBuf strings.Builder
	l.cfg.Logger = log.New(&logBuf, "", 0)
	l.cfg.GatewayIP = "192.0.2.1" // the gateway in the scrubbed EOS ARP fixture

	// What each recorded push must do to the switch, as a diff against the
	// captured switch state (the fixture transport is stateless, so a push
	// that matches the captured state produces no port-2 lines — which is
	// itself the assertion for the "back to normal" pushes).
	type expect struct{ must, mustNot []string }
	expectations := map[int]expect{
		1: {must: []string{"interface Ethernet2 | shutdown"}},                                                                       // first full push: port 2 disabled
		4: {must: []string{"interface Ethernet2 | shutdown"}},                                                                       // Port State: Disabled
		5: {mustNot: []string{"interface Ethernet2 | shutdown"}},                                                                    // Port State: Active
		6: {must: []string{"interface Ethernet2 | description Ethernet2 | switchport mode trunk | switchport trunk native vlan 2"}}, // native VLAN kids
		7: {mustNot: []string{"switchport trunk native vlan 2"}},                                                                    // native VLAN back to Default
	}
	for i, rec := range replies {
		before := len(ft.Configured)
		l.informOnce(ctx)
		var p map[string]any
		_ = json.Unmarshal(rec.Payload, &p)
		typ, _ := p["_type"].(string)
		cmd, _ := p["cmd"].(string)
		var batches []string
		for _, b := range ft.Configured[before:] {
			batches = append(batches, strings.Join(b, " | "))
		}
		got := strings.Join(batches, "\n")
		if typ == "setparam" && p["system_cfg"] != nil {
			ver, _ := p["cfgversion"].(string)
			if applied, _, _ := sess.Applied(); applied != ver {
				t.Errorf("reply %d: after system_cfg %s the device reports cfgversion %q", i, ver, applied)
			}
			if len(batches) == 0 {
				t.Errorf("reply %d: system_cfg %s produced no switch configuration", i, ver)
			}
		}
		if typ == "cmd" && cmd == "build-ssh-session" && !strings.Contains(logBuf.String(), `UNHANDLED cmd "build-ssh-session"`) {
			t.Errorf("reply %d: build-ssh-session must be logged as unhandled", i)
		}
		for _, m := range expectations[i].must {
			if !strings.Contains(got, m) {
				t.Errorf("reply %d: expected switch configuration %q; got:\n%s", i, m, got)
			}
		}
		for _, m := range expectations[i].mustNot {
			if strings.Contains(got, m) {
				t.Errorf("reply %d: switch configuration must not contain %q; got:\n%s", i, m, got)
			}
		}
	}
	if next != len(replies) {
		t.Fatalf("served %d of %d replies", next, len(replies))
	}
	// And the payload the fake controller last decoded is a real-looking inform.
	for _, k := range []string{"uplink", "if_table", "lldp_table", "port_table", "connect_request_ip", "gateway_mac"} {
		if _, ok := lastInform[k]; !ok {
			t.Errorf("last inform lacks %q", k)
		}
	}
}
