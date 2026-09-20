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
	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// Replay: real controller replies (docs/fixtures/controller-10.6.106/
// replies.ndjson — the bridge's own reply log from Network 10.6.106 against
// the Arista, identifiers and secrets replaced) are served to the loop by a
// fake controller, encrypted with a test key the way the controller
// encrypts them, and the loop drives the real Arista driver against the real
// EOS captures. What must come out the other end is the exact switch
// configuration each push produced live.
// The device starts unadopted (default key); the adopt reply in the fixture
// carries the (scrubbed) site key the rest of the replay is encrypted with.
const defaultKey = "ba86f2bbe107c7c57eb5f2690775c712" // MD5("ubnt")

type replayRecord struct {
	Status  int             `json:"status"`
	SentGCM bool            `json:"sent_gcm"`
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

	// The fake controller: decodes what the device sends with the key the
	// real controller would hold at that point (default until it hands out
	// the site key in the adopt reply), and answers with the next recorded
	// reply, encrypted the way the controller did (CBC before, GCM after).
	next := 0
	key := defaultKey
	var lastInform map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read inform: %v", err)
		}
		pkt, err := inform.Decode(body, key)
		if err != nil {
			t.Errorf("controller could not decode our inform with key %s: %v", key, err)
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
		p := &inform.Packet{MAC: mac, Payload: rec.Payload}
		var enc []byte
		if rec.SentGCM {
			enc, err = p.EncodeGCM(key)
		} else {
			enc, err = p.Encode(key)
		}
		if err != nil {
			t.Fatal(err)
		}
		// After handing out the site key the controller expects it next.
		if m := authKeyRe.FindString(string(rec.Payload)); m != "" {
			key = strings.TrimPrefix(m, "authkey=")
		}
		w.Header().Set("Content-Type", "application/x-binary")
		w.Write(enc)
	}))
	defer srv.Close()

	// Fresh device: not adopted, default key, as the bridge starts.
	sess := device.NewSession(desc, srv.URL, device.State{InformURL: srv.URL}, nil, time.Now())
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
		// 0-1: HTTP 400 (the controller's cooldown after a forget), 2: 404 pending,
		// 3: adopt via mgmt_cfg, 4: first system_cfg after adoption
		5:  {must: []string{"interface Ethernet2 | shutdown"}},                                                                       // first full push of the day: port 2 disabled
		8:  {must: []string{"interface Ethernet2 | shutdown"}},                                                                       // Port State: Disabled
		9:  {mustNot: []string{"interface Ethernet2 | shutdown"}},                                                                    // Port State: Active
		10: {must: []string{"interface Ethernet2 | description Ethernet2 | switchport mode trunk | switchport trunk native vlan 2"}}, // native VLAN kids
		11: {mustNot: []string{"switchport trunk native vlan 2"}},                                                                    // native VLAN back to Default
		// 12: set-locate, 13: unset-locate
	}
	var connected int
	l.cfg.OnConnected = func(*switchmodel.Snapshot) { connected++ }
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
		locating := func() bool {
			var m map[string]any
			_ = json.Unmarshal(sess.BuildPayload(time.Now()), &m)
			v, _ := m["locating"].(bool)
			return v
		}
		switch i {
		case 0, 1, 2:
			if sess.Adopted() || l.state != StatePending {
				t.Errorf("reply %d: the device must still be pending (adopted=%v state=%v)", i, sess.Adopted(), l.state)
			}
		case 3:
			if !sess.Adopted() || sess.AuthKey() != key || l.state != StateAdopting {
				t.Errorf("after the adopt reply: adopted=%v key=%s state=%v", sess.Adopted(), sess.AuthKey(), l.state)
			}
		case 4:
			if l.state != StateConnected || connected != 1 {
				t.Errorf("after the first system_cfg: state=%v, OnConnected calls=%d", l.state, connected)
			}
		case 12:
			if !locating() {
				t.Errorf("set-locate must turn locating on in the next inform")
			}
		case 13:
			if locating() {
				t.Errorf("unset-locate must turn locating off")
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
