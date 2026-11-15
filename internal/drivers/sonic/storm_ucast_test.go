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

const ucastFixtures = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7"

// §11.24: l2d's unknown-unicast watch over the DX010's own psample samples
// (ucast-samples.ndjson, live 2026-11-15 on igmp-202511.20261115-6c8eb1cf7):
// cage 15 (Ethernet56, MAC loopback, alone in a VLAN of its own) under a
// 1000 kbps unknown-unicast limit, sampled 1:256, took in from the CPU
// A) 60000 frames to an unknown MAC over 15 s (4000/s, 2x the limit),
// B) 22000 to it over 15 s (1450/s, under the limit), C) 60000 to a MAC
// in the FDB on the port itself (forwarded: OIFINDEX the port), D) 60000
// to the switch's own MAC (flooded in a VLAN without an interface, but
// never counted), E) 20000 broadcast. Only A is a storm; its count is a
// lower bound of the frames above the limit. The sFlow configuration is
// written in order and taken out again, the feature last.
func TestL2dUcastStormOverLiveSamples(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_ucast.py"), "l2d.py",
		filepath.Join(ucastFixtures, "ucast-samples.ndjson"), "Ethernet56", "1000").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var starts, writes []string
	var row map[string]string
	after := ""
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "LOG storm control: Ethernet56 unicast storm,"):
			starts = append(starts, line)
		case strings.HasPrefix(line, "SET SFLOW"), strings.HasPrefix(line, "DEL SFLOW"), strings.HasPrefix(line, "HSET FEATURE"):
			writes = append(writes, line)
		case strings.HasPrefix(line, "ROW "):
			if err := json.Unmarshal([]byte(line[4:]), &row); err != nil {
				t.Fatal(err)
			}
		case strings.HasPrefix(line, "AFTER "):
			after = line
		}
	}
	if len(starts) != 1 {
		t.Fatalf("unicast storms found = %d, want 1 (A only; not B under the limit, the known MAC, the switch's MAC or broadcast):\n%s", len(starts), out)
	}
	// Above the limit: at most A's frames minus 15 s at the limit
	// (125000 bytes/s = 1953 frames/s of 64 bytes); the 5 s window starts
	// the count late, so more than half of that.
	n, _ := strconv.ParseFloat(row["unicast"], 64)
	if most := 60000 - 1953.125*15; n <= most/2 || n > most {
		t.Errorf("unicast frames above the limit = %v, want in (%.0f, %.0f]", row["unicast"], most/2, most)
	}
	want := []string{
		"SET SFLOW_SESSION|all admin_state=down",
		"SET SFLOW_SESSION|Ethernet56 admin_state=up sample_rate=256",
		"SET SFLOW|global admin_state=up",
		"HSET FEATURE|sflow state=enabled",
		"DEL SFLOW_SESSION|all",
		"DEL SFLOW_SESSION|Ethernet56",
		"DEL SFLOW|global",
		"HSET FEATURE|sflow state=disabled",
	}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Errorf("sFlow configuration writes:\n%s\nwant:\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	if after != "AFTER [] disabled" {
		t.Errorf("left behind: %q", after)
	}
}

// §11.24: unknown-unicast storm control alone runs l2d where the image can
// sample (collect.py's genl_psample yes, the image with sonic-buildimage
// 0014), and the driver reports the row's count as the port's
// StormDropsUnicast; on an image without it nothing is installed and the
// driver says so once.
func TestL2RunsForUnicastStormControlWithPsample(t *testing.T) {
	withMstpd := func(fr *FixtureRunner) {
		in := fr.l2Inst()
		in.Mstpd = mstpdVersion + "-"
		fr.setL2Inst(in)
	}
	off := devicemodel.DeviceDesired{STPSet: true, STPEnabled: false}
	five := 5.0
	unicast := func(d *devicemodel.PortDesired) {
		if d.Index == 3 {
			d.StormCtrl = &devicemodel.StormControlSpec{UnknownUnicastPct: &five}
		}
	}

	r := newL2RigAt(t, filepath.Join(ucastFixtures, "collect.txt"), false, withMstpd)
	if _, err := r.push(off, r.ports(unicast)); err != nil {
		t.Fatal(err)
	}
	if len(r.fr.L2Scripts) != 1 || r.inst().Service != "active" {
		t.Fatalf("unknown-unicast storm control alone on an image with genl_psample: %d l2 scripts, service %q; want l2d installed and running", len(r.fr.L2Scripts), r.inst().Service)
	}
	if strings.Contains(r.log.String(), "cannot detect an unknown-unicast storm") {
		t.Errorf("the no-genl note on an image that has it:\n%s", r.log.String())
	}

	r = newL2Rig(t, withMstpd) // 202505: no genl_psample section
	if _, err := r.push(off, r.ports(unicast)); err != nil {
		t.Fatal(err)
	}
	if len(r.fr.L2Scripts) != 0 {
		t.Errorf("unknown-unicast storm control alone installed l2d on an image that cannot sample")
	}
	if n := strings.Count(r.log.String(), "cannot detect an unknown-unicast storm"); n != 1 {
		t.Errorf("the no-genl note logged %d times, want once:\n%s", n, r.log.String())
	}
}

// The capture taken while the storm row held 15745 frames (live, during the
// run above): port 15 reports them as StormDropsUnicast, and no other port
// any.
func TestStormDropsUnicastFromCapture(t *testing.T) {
	fr, err := NewFixtureRunner(filepath.Join(ucastFixtures, "collect-ucast-storm.txt"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := NewCollector(fr).Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports {
		want := uint64(0)
		if p.Index == 15 {
			want = 15745
		}
		if p.Health.StormDropsUnicast != want {
			t.Errorf("port %d StormDropsUnicast = %d, want %d", p.Index, p.Health.StormDropsUnicast, want)
		}
	}
}
