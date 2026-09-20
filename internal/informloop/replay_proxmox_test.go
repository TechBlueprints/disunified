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
	"github.com/TechBlueprints/switch-to-unifi/internal/drivers/proxmox"
)

// Replay for the Proxmox driver: the controller's recorded replies to
// pve-proxmox-2 (docs/fixtures/controller-10.6.106/replies-proxmox.ndjson,
// cut from the bridge's own reply log on 2026-09-20 and scrubbed) drive the
// real driver against the real node capture. The pushes were made live
// through the controller's API: VM 999's port to native VLAN 2 with VLANs
// 10 and 69 tagged, then back to "All"; what must come out is the `qm set`
// each one produced on the node.
func loadProxmoxReplies(t *testing.T) []replayRecord {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106", "replies-proxmox.ndjson"))
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

func TestReplayControllerRepliesProxmox(t *testing.T) {
	replies := loadProxmoxReplies(t)
	if len(replies) != 6 {
		t.Fatalf("expected 6 replies in the fixture, got %d", len(replies))
	}
	fr, err := proxmox.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "proxmox-9.1.6", "collect-node2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	coll := proxmox.NewCollector(fr)
	coll.ManageLLDP = false
	coll.Log = log.New(os.Stderr, "", 0)
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := device.DescriptorFor("UDC48X6", snap, device.Identity{MAC: snap.System.MAC, IP: snap.System.Addresses[0].IP, UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	desc.FWCaps = device.FWCapsFor(coll.Capabilities())

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
		if m := authKeyRe.FindString(string(rec.Payload)); m != "" {
			key = strings.TrimPrefix(m, "authkey=")
		}
		w.Header().Set("Content-Type", "application/x-binary")
		w.Write(enc)
	}))
	defer srv.Close()

	sess := device.NewSession(desc, srv.URL, device.State{InformURL: srv.URL}, nil, time.Now())
	sess.SetCapabilities(coll.Capabilities())
	sess.SetSnapshot(snap)
	l, err := New(desc, sess, Config{
		Logger: log.New(os.Stderr, "", 0), Interval: time.Minute, CollectTimeout: 10 * time.Second,
		Collector: coll, Controller: coll,
	})
	if err != nil {
		t.Fatal(err)
	}
	l.client = srv.Client()
	l.cfg.GatewayIP = "192.0.2.2"
	ctx := context.Background()

	// VM 999 is the last guest in the capture: its scrubbed MAC is in the
	// fixture's own config line, which is what the driver rewrites.
	type expect struct{ must, mustNot []string }
	expectations := map[int]expect{
		// 0: 404 pending, 1: adopt via mgmt_cfg, 2-3: first pushes, every port at its default
		2: {mustNot: []string{"qm set"}},
		3: {mustNot: []string{"qm set"}},
		4: {must: []string{"qm set 999 --net0 'virtio=", ",bridge=vmbr0,tag=2,trunks=10;69'"}}, // native kids (2), tagged airstream (10) + visitor (69)
		// 5: back to All. The fixture is stateless (every collect re-reads the
		// capture, where VM 999 is at its default), so this push matches the
		// captured state and must write nothing — that is the assertion.
		5: {mustNot: []string{"qm set"}},
	}
	for i, rec := range replies {
		before := len(fr.Commands)
		l.informOnce(ctx)
		got := strings.Join(fr.Commands[before:], "\n")
		var p map[string]any
		_ = json.Unmarshal(rec.Payload, &p)
		if typ, _ := p["_type"].(string); typ == "setparam" && p["system_cfg"] != nil {
			ver, _ := p["cfgversion"].(string)
			if applied, _, _ := sess.Applied(); applied != ver {
				t.Errorf("reply %d: after system_cfg %s the device reports cfgversion %q", i, ver, applied)
			}
		}
		switch i {
		case 0:
			if sess.Adopted() || l.state != StatePending {
				t.Errorf("reply 0: the device must still be pending")
			}
		case 1:
			if !sess.Adopted() || l.state != StateAdopting {
				t.Errorf("after the adopt reply: adopted=%v state=%v", sess.Adopted(), l.state)
			}
		case 2:
			if l.state != StateConnected {
				t.Errorf("after the first system_cfg: state=%v", l.state)
			}
		}
		for _, m := range expectations[i].must {
			if !strings.Contains(got, m) {
				t.Errorf("reply %d: expected node command containing %q; got:\n%s", i, m, got)
			}
		}
		for _, m := range expectations[i].mustNot {
			if strings.Contains(got, m) {
				t.Errorf("reply %d: node commands must not contain %q; got:\n%s", i, m, got)
			}
		}
	}
	if next != len(replies) {
		t.Fatalf("served %d of %d replies", next, len(replies))
	}
	for _, k := range []string{"uplink", "if_table", "lldp_table", "port_table", "connect_request_ip", "gateway_mac"} {
		if _, ok := lastInform[k]; !ok {
			t.Errorf("last inform lacks %q", k)
		}
	}
}
