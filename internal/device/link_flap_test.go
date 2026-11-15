package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	aristaeos "github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
)

// What a real UniFi switch's link_down_count and link_flap_count count, from
// two switches' own informs (mca-dump) and their own logs, read the same
// minute (controller-11.0.99/link-flap-count-20261115.json):
//
//   - link_down_count is the port's link drops since boot: on the 7.6.11
//     switch, whose log reaches back to its boot, it equals the logged
//     "LINK-W-Down" lines of every port.
//   - link_flap_count is not a drop count. It counts the switch's own
//     link-flap detector ("Link flapping detected on port X, flapped 1
//     time(s)": LNK_STABL on 7.6.11, DRIVER on the 7.5.15 USXG): one per
//     detection, 7 drops gave 1, two drops 8 s apart gave none, and on the
//     USXG it exceeds the drops (flaps too short to be logged as a drop).
//     The key is sent only where it is not zero.
//
// No driver has such a detector, so the bridge sends no link_flap_count; the
// controller's link-flap alert follows link_down_count's growth
// (alert_rates_test.go), and none followed these detections.
const linkFlapCapture = "controller-11.0.99/link-flap-count-20261115.json"

type linkPort struct {
	idx            int
	down, flap     float64
	hasFlap        bool
	logDown, logUp int
	logFlap        int
}

func linkPorts(t *testing.T, sw map[string]any, ifIndex func(string) int, downRe, upRe, flapRe *regexp.Regexp) map[int]*linkPort {
	t.Helper()
	out := map[int]*linkPort{}
	for _, e := range sw["ports"].([]any) {
		p := e.(map[string]any)
		lp := &linkPort{idx: int(p["port_idx"].(float64))}
		lp.down, _ = p["link_down_count"].(float64)
		lp.flap, lp.hasFlap = p["link_flap_count"].(float64)
		out[lp.idx] = lp
	}
	for _, l := range sw["log"].([]any) {
		l := l.(string)
		for re, n := range map[*regexp.Regexp]func(*linkPort){
			downRe: func(p *linkPort) { p.logDown++ },
			upRe:   func(p *linkPort) { p.logUp++ },
			flapRe: func(p *linkPort) { p.logFlap++ },
		} {
			if m := re.FindStringSubmatch(l); m != nil {
				p := out[ifIndex(m[1])]
				if p == nil {
					t.Fatalf("log line names no port of the inform: %q", l)
				}
				n(p)
			}
		}
	}
	return out
}

func TestLinkCountsOnRealSwitches(t *testing.T) {
	sws := loadFixtureJSON(t, linkFlapCapture)["switches"].([]any)
	if len(sws) != 2 {
		t.Fatalf("capture has %d switches, want 2", len(sws))
	}

	// The 7.6.11 switch: its log is complete since boot.
	sw := sws[0].(map[string]any)
	if sw["version"] != "7.6.11.17272" || sw["log_since_boot"] != true {
		t.Fatalf("first switch: version %v, log since boot %v", sw["version"], sw["log_since_boot"])
	}
	ifRe := regexp.MustCompile(`^t([wef])1/0/(\d+)$`)
	base := map[string]int{"w": 0, "e": 16, "f": 48}
	ports := linkPorts(t, sw, func(name string) int {
		m := ifRe.FindStringSubmatch(name)
		n, _ := strconv.Atoi(m[2])
		return base[m[1]] + n
	},
		regexp.MustCompile(`LINK-W-Down:\s+(t[wef]1/0/\d+)$`),
		regexp.MustCompile(`LINK-I-Up:\s+(t[wef]1/0/\d+)$`),
		regexp.MustCompile(`Link flapping detected on port (t[wef]1/0/\d+), flapped 1 time\(s\)`))
	if len(ports) != 52 {
		t.Fatalf("first switch: %d ports, want 52", len(ports))
	}
	for _, p := range ports {
		if float64(p.logDown) != p.down {
			t.Errorf("port %d: link_down_count %v, the log has %d drops since boot", p.idx, p.down, p.logDown)
		}
		if p.hasFlap && p.flap == 0 {
			t.Errorf("port %d: link_flap_count sent as 0 (a real switch omits it)", p.idx)
		}
		if p.flap < float64(p.logFlap) {
			t.Errorf("port %d: link_flap_count %v, below the log's %d detections", p.idx, p.flap, p.logFlap)
		}
	}
	// One detection each, whatever the drops (port 27's one flap has no
	// line since boot and no drop: not explained by the capture).
	for idx, want := range map[int][2]float64{20: {7, 1}, 26: {1, 1}, 49: {1, 1}, 11: {2, 0}, 27: {0, 1}} {
		p := ports[idx]
		if p.down != want[0] || p.flap != want[1] {
			t.Errorf("port %d: link_down_count %v, link_flap_count %v, want %v and %v", idx, p.down, p.flap, want[0], want[1])
		}
		if want[1] == 1 && idx != 27 && p.logFlap != 1 {
			t.Errorf("port %d: %d logged detections, want 1", idx, p.logFlap)
		}
	}

	// The 7.5.15 USXG (log from the day before only): more flaps than drops.
	sw = sws[1].(map[string]any)
	if sw["version"] != "7.5.15.17146" {
		t.Fatalf("second switch: version %v", sw["version"])
	}
	ports = linkPorts(t, sw, func(name string) int { n, _ := strconv.Atoi(name); return n },
		regexp.MustCompile(`TRAPMGR: Link Down: 0/(\d+)$`),
		regexp.MustCompile(`TRAPMGR: Link Up: 0/(\d+)$`),
		regexp.MustCompile(`Link flapping detected on port 0/(\d+), flapped 1 time\(s\)`))
	for _, idx := range []int{1, 2} {
		p := ports[idx]
		if !(p.flap > p.down) || p.logFlap != 1 || p.logDown != 0 {
			t.Errorf("USXG port %d: link_down_count %v, link_flap_count %v, log drops %d, detections %d; want flaps above drops, one detection with no logged drop", idx, p.down, p.flap, p.logDown, p.logFlap)
		}
	}
	if p := ports[12]; !(p.down > 10*p.flap) {
		t.Errorf("USXG port 12: link_down_count %v, link_flap_count %v; want drops far above flaps", p.down, p.flap)
	}
}

// The bridge sends link_down_count on every port and link_flap_count on none.
func TestBridgeSendsNoLinkFlapCount(t *testing.T) {
	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
	c := aristaeos.NewCollector(ft)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := DescriptorFor("UDC48X6", snap, Identity{MAC: snap.System.MAC, IP: "192.0.2.9", Hostname: "arista", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetCapabilities(c.Capabilities())
	for i := range snap.Ports {
		snap.Ports[i].Health.LinkChanges = 15 // seven drops, now up
	}
	sess.SetSnapshot(snap)
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	for _, e := range m["port_table"].([]any) {
		p := e.(map[string]any)
		if _, ok := p["link_flap_count"]; ok {
			t.Errorf("port %v: carries link_flap_count", p["port_idx"])
		}
		if n, _ := p["link_down_count"].(float64); n != 7 {
			t.Errorf("port %v: link_down_count %v, want 7", p["port_idx"], p["link_down_count"])
		}
	}
}
