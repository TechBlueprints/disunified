package sonic

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The DHCP rate detector (docs/drivers/sonic.md §11.17). collect-dhcprate.txt
// is the DX010 on igmp-202511.20261106-f2e25f54c (2026-11-14) right after the
// live check: with DHCP's trap in its own CoPP group, 400 requests a second
// for 5 s from one host to the switch's own address on the uplink left
// DUI_DHCP_RATE_TABLE|Ethernet0 at 397 over the threshold, peak 403.
const (
	fixtureDHCPRate   = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-dhcprate.txt"
	fixtureDHCPFrames = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/dhcp-frames-20261114.txt"
)

func TestDHCPRateCountFromTheLiveCapture(t *testing.T) {
	r := rig20261106(t, fixtureDHCPRate)
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports {
		want := uint64(0)
		if p.Index == 1 {
			want = 397
		}
		if p.Health.DHCPRateDrops != want {
			t.Errorf("port %d: DHCP rate count %d, want %d", p.Index, p.Health.DHCPRateDrops, want)
		}
		if p.Fault != "" {
			t.Errorf("port %d: fault %q: the DHCP rate is a detection, the port keeps running", p.Index, p.Fault)
		}
	}
	// An older capture has no section: nothing is counted.
	r = rig20261106(t, fixtureRDTest)
	snap, err = r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports {
		if p.Health.DHCPRateDrops != 0 {
			t.Errorf("port %d: %d from a capture without the section", p.Index, p.Health.DHCPRateDrops)
		}
	}
}

// The l2d side, without a switch: the packet filter (through a classic-BPF
// interpreter, so its hand-computed jumps are checked) and the classifier on
// the DX010's real frames, the per-second accounting, and the CoPP rows in
// the order sonic-yang and coppmgr need (testdata/l2d_dhcp_rate.py).
func TestL2dDHCPRate(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_dhcp_rate.py"), "l2d.py", fixtureDHCPFrames).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasPrefix(line, "ok ") {
			t.Error(line)
		}
		n++
	}
	if n < 20 {
		t.Errorf("%d checks ran:\n%s", n, out)
	}
}

// The detector rides in l2d's config wherever l2d is written.
func TestL2ConfCarriesTheDHCPRateDetector(t *testing.T) {
	c := (&l2Want{}).conf()
	if !c.DHCPRate.Enabled || c.DHCPRate.Threshold != 300 || c.DHCPRate.Rate <= c.DHCPRate.Threshold {
		t.Errorf("dhcp_rate %+v: on, UniFi's 300/s, a CoPP rate above it", c.DHCPRate)
	}
}
