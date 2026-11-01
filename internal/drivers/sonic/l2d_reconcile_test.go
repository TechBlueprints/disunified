package sonic

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The bug found live on the DX010 (2026-11-01, docs/drivers/sonic.md §11.5):
// from the controller, bridge priority 61440 and, on cage 3 (Ethernet8,
// empty), BPDU guard and loop protection. The driver wrote the config right,
// in two writes with a reload each (ApplyDevice, then ApplyPorts); mstpd took
// the priority, but Ethernet8's BPDU guard never reached it -- the second
// reload arrived during the sync the first started and was cleared at its
// end. l2d now sets what differs from mstpd's own report on every pass. Here
// the config is the one the driver writes for that push, and mstpd's state
// is the live capture's (collect-l2-running.txt).
func TestL2dReconcilesFromMstpdsReport(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	r := enabled(t)
	prio := devicemodel.DeviceDesired{STPSet: true, STPEnabled: true, STPMode: "rstp", STPPriority: 61440}
	ds := r.ports(func(d *devicemodel.PortDesired) {
		if d.Index == 3 {
			d.BPDUGuard, d.LoopProtect = true, true
		}
	})
	n := len(r.fr.L2Scripts)
	if _, err := r.push(prio, ds); err != nil {
		t.Fatal(err)
	}
	if got := len(r.fr.L2Scripts) - n; got != 2 {
		t.Errorf("%d config writes for the push, want 2 (the bridge, then the ports): the live sequence", got)
	}
	c := r.conf()
	if c.STP.Priority != 61440 || !c.STP.Ports["Ethernet8"].BPDUGuard || len(c.LoopGuard.Ports) != 1 || c.LoopGuard.Ports[0] != "Ethernet8" {
		t.Fatalf("config = %+v", c)
	}
	conf := filepath.Join(t.TempDir(), "l2.json")
	if err := os.WriteFile(conf, []byte(r.fr.L2Files[l2ConfPath]), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_reconcile.py"), "l2d.py", l2RunningFixture, conf).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	first, second, _ := strings.Cut(strings.TrimPrefix(string(out), "PASS 1\n"), "PASS 2\n")
	var calls []string
	for _, line := range strings.Split(first, "\n") {
		if c, ok := strings.CutPrefix(line, "CALL "); ok {
			calls = append(calls, c)
		}
	}
	want := []string{"settreeprio Bridge 0 15", "setbpduguard Bridge Ethernet8 yes"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("mstpctl calls = %q, want exactly %q (nothing for the ports already right)", calls, want)
	}
	if !strings.Contains(first, "LOG settings: priority 32768 -> 61440; Ethernet8 bpdu guard on") {
		t.Errorf("the change must be logged:\n%s", first)
	}
	if strings.TrimSpace(second) != "" {
		t.Errorf("a second pass on the state the first produced set something:\n%s", second)
	}
}
