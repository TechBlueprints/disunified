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

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Replay: real controller replies (docs/fixtures/controller-10.6.106/
// replies*.ndjson — the bridge's own reply logs from Network 10.6.106,
// identifiers and secrets replaced) are served to the loop by a fake
// controller, encrypted with a test key the way the controller encrypts
// them, and the loop drives a real driver against its real captures. What
// must come out the other end is the exact switch configuration each push
// produced live. Each driver's case is in replay_<driver>_test.go: the
// replies file, the collector, and per-reply expectations.
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

func loadReplies(t *testing.T, name string) []replayRecord {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106", name))
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

// replayDriver is one driver under replay.
type replayDriver struct {
	Name      string
	Replies   string // fixture file under controller-10.6.106/
	Collector devicemodel.Device
	Control   devicemodel.Controller
	Snapshot  *devicemodel.Snapshot
	GatewayIP string
	// Written returns the switch writes recorded since the last call, one
	// per line, as the driver's own transport records them.
	Written func() string
	// Loop is applied to the loop config (control flags).
	Loop func(*Config)
	// Expect: what each reply must (not) have written to the switch.
	Expect map[int]replayExpect
	// Check runs after each reply with the loop state, for the driver's
	// own assertions (adoption sequence, locate, log lines).
	Check func(t *testing.T, i int, rec replayRecord, sess *device.Session, l *Loop, logText string)
}

type replayExpect struct{ must, mustNot []string }

// runReplay drives the driver through every recorded reply and checks
// the common contract: the reported cfgversion follows each applied
// system_cfg, a system_cfg is never silently ignored when the driver's
// expectations say it writes, every reply is served, and the last inform
// the fake controller decoded looks like a real switch's.
func runReplay(t *testing.T, d replayDriver) {
	t.Helper()
	replies := loadReplies(t, d.Replies)
	if len(replies) < 5 {
		t.Fatalf("only %d replies in the fixture", len(replies))
	}
	snap := d.Snapshot
	ip := "192.0.2.9"
	if len(snap.System.Addresses) > 0 {
		ip = snap.System.Addresses[0].IP
	}
	desc, err := device.DescriptorFor("UDC48X6", snap, device.Identity{MAC: snap.System.MAC, IP: ip, UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	caps := device.DefaultCapabilities
	if c, ok := d.Collector.(devicemodel.Capable); ok {
		caps = c.Capabilities()
	}
	desc.FWCaps = device.FWCapsFor(caps)

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
	sess.SetCapabilities(caps)
	sess.SetSnapshot(snap)
	var logBuf strings.Builder
	cfg := Config{
		Logger: log.New(&logBuf, "", 0), Interval: time.Minute, CollectTimeout: 10 * time.Second,
		Collector: d.Collector, Controller: d.Control,
	}
	if d.Loop != nil {
		d.Loop(&cfg)
	}
	l, err := New(desc, sess, cfg)
	if err != nil {
		t.Fatal(err)
	}
	l.client = srv.Client()
	l.cfg.GatewayIP = d.GatewayIP
	ctx := context.Background()
	for i, rec := range replies {
		l.informOnce(ctx)
		got := d.Written()
		var p map[string]any
		_ = json.Unmarshal(rec.Payload, &p)
		if typ, _ := p["_type"].(string); typ == "setparam" && p["system_cfg"] != nil {
			ver, _ := p["cfgversion"].(string)
			if applied, _, _ := sess.Applied(); applied != ver {
				t.Errorf("%s reply %d: after system_cfg %s the device reports cfgversion %q", d.Name, i, ver, applied)
			}
		}
		if d.Check != nil {
			d.Check(t, i, rec, sess, l, logBuf.String())
		}
		for _, m := range d.Expect[i].must {
			if !strings.Contains(got, m) {
				t.Errorf("%s reply %d: expected switch write %q; got:\n%s", d.Name, i, m, got)
			}
		}
		for _, m := range d.Expect[i].mustNot {
			if strings.Contains(got, m) {
				t.Errorf("%s reply %d: switch writes must not contain %q; got:\n%s", d.Name, i, m, got)
			}
		}
	}
	if next != len(replies) {
		t.Fatalf("%s: served %d of %d replies", d.Name, next, len(replies))
	}
	// And the payload the fake controller last decoded is a real-looking inform.
	for _, k := range []string{"uplink", "if_table", "lldp_table", "port_table", "connect_request_ip", "gateway_mac"} {
		if _, ok := lastInform[k]; !ok {
			t.Errorf("%s: last inform lacks %q", d.Name, k)
		}
	}
}
