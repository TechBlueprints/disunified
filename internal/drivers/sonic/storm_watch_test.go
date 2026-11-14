package sonic

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// §11.21: disunified-l2d watches the ports' storm-control thresholds, so a
// push with storm control installs and runs it even with spanning tree and
// loop protection off; without storm control nothing is installed.
func TestL2RunsForStormControlAlone(t *testing.T) {
	off := devicemodel.DeviceDesired{STPSet: true, STPEnabled: false}
	withMstpd := func(fr *FixtureRunner) {
		in := fr.l2Inst()
		in.Mstpd = mstpdVersion + "-"
		fr.setL2Inst(in)
	}

	r := newL2Rig(t, withMstpd)
	if _, err := r.push(off, r.ports(nil)); err != nil {
		t.Fatal(err)
	}
	if len(r.fr.L2Scripts) != 0 {
		t.Fatalf("STP off, no loop protection, no storm control: l2d installed (%d scripts)", len(r.fr.L2Scripts))
	}

	five := 5.0
	r = newL2Rig(t, withMstpd)
	if _, err := r.push(off, r.ports(func(d *devicemodel.PortDesired) {
		if d.Index == 3 {
			d.StormCtrl = &devicemodel.StormControlSpec{BroadcastPct: &five}
		}
	})); err != nil {
		t.Fatal(err)
	}
	if len(r.fr.L2Scripts) != 1 || r.inst().Service != "active" {
		t.Fatalf("storm control alone: %d l2 scripts, service %q; want l2d installed and running", len(r.fr.L2Scripts), r.inst().Service)
	}
	if c := r.conf(); c.STP.Enabled || c.LoopGuard.Enabled {
		t.Errorf("storm control turned on STP or loop protection: %+v", c)
	}

	// Unknown unicast alone is not watched (no counter tells it apart).
	r = newL2Rig(t, withMstpd)
	if _, err := r.push(off, r.ports(func(d *devicemodel.PortDesired) {
		if d.Index == 3 {
			d.StormCtrl = &devicemodel.StormControlSpec{UnknownUnicastPct: &five}
		}
	})); err != nil {
		t.Fatal(err)
	}
	if len(r.fr.L2Scripts) != 0 {
		t.Errorf("unknown-unicast storm control alone installed l2d")
	}
}

// l2d's StormWatch over the DX010's own counters (storm-counters.ndjson,
// live 2026-11-14, every 0.5 s for 95 s): cage 5 (Ethernet16, MAC loopback,
// a 1000 kbps broadcast and unknown-multicast limit = 1953 frames/s at 64
// bytes) took in 74100 broadcast frames over 15 s, then 22290 over 15 s
// (1486/s, under the limit), then 74150 multicast frames over 15 s; the
// uplink (Ethernet0) carried its ordinary traffic throughout. The replay
// finds the two floods above the limit and not the one under it, counts a
// lower bound of the frames above it, and finds nothing on the uplink even
// at that limit (a thousandth of UniFi's lowest, 1 % of 100G).
func TestL2dStormWatchOverLiveCounters(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	capture := filepath.Join("..", "..", "..", "docs", "fixtures", "sonic-igmp-202511.20261106-f2e25f54c", "storm-counters.ndjson")
	replay := func(port string) (logs []string, row map[string]string) {
		out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_storm.py"), "l2d.py", capture, port, "1000", "1000").CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if l, ok := strings.CutPrefix(line, "LOG "); ok {
				logs = append(logs, l)
			}
			if r, ok := strings.CutPrefix(line, "ROW "); ok {
				if err := json.Unmarshal([]byte(r), &row); err != nil {
					t.Fatal(err)
				}
			}
		}
		return logs, row
	}

	logs, row := replay("Ethernet16")
	var starts []string
	for _, l := range logs {
		if strings.HasSuffix(l, "(1953 frames/s at 64 bytes)") {
			starts = append(starts, strings.Fields(l)[3]) // "storm control: Ethernet16 <type> storm, ..."
		}
	}
	if strings.Join(starts, ",") != "broadcast,multicast" {
		t.Errorf("storms found = %q, want the broadcast flood then the multicast one (not the one under the limit):\n%s", starts, strings.Join(logs, "\n"))
	}
	// Above the limit: at most the frames sent minus 15 s at the limit;
	// the detection's 5 s window starts the count late, so well above half.
	for field, sent := range map[string]float64{"broadcast": 74100, "multicast": 74150} {
		n, _ := strconv.ParseFloat(row[field], 64)
		if most := sent - 1953.125*15; n <= most/2 || n > most {
			t.Errorf("%s frames above the limit = %v, want in (%.0f, %.0f]", field, row[field], most/2, most)
		}
	}

	if logs, row := replay("Ethernet0"); len(logs) != 0 || row["broadcast"] != "0" || row["multicast"] != "0" {
		t.Errorf("the uplink's ordinary traffic read as a storm: %q %v", logs, row)
	}
}
