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

// §11.8: `config interface breakout` validates the whole CONFIG_DB with
// sonic-yang, and the STP trap's trap_group is a leafref into CONFIG_DB's
// COPP_GROUP, which the image keeps in copp_cfg.json only -- every breakout
// failed on the trap left behind. l2d writes the group before the trap: the
// image's definition when the file has one, the stock one otherwise.
func TestL2dWritesTheTrapGroupWithTheTrap(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	run := func(args ...string) []string {
		t.Helper()
		out, err := exec.Command(py, append([]string{"-I", filepath.Join("testdata", "l2d_copp.py"), "l2d.py"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}
	stock := "PUT COPP_GROUP|queue4_group1 cbs=6000 cir=6000 meter_type=packets mode=sr_tcm queue=4 red_action=drop trap_action=trap trap_priority=4"
	trap := "PUT COPP_TRAP|stp trap_ids=stp trap_group=queue4_group1 always_enabled=true"
	if got := run(); strings.Join(got, "\n") != stock+"\n"+trap {
		t.Errorf("without copp_cfg.json:\n%s", strings.Join(got, "\n"))
	}
	cfg := filepath.Join(t.TempDir(), "copp_cfg.json")
	if err := os.WriteFile(cfg, []byte(`{"COPP_GROUP": {"queue4_group1": {"trap_action": "trap", "trap_priority": "4", "queue": "4", "meter_type": "packets", "mode": "sr_tcm", "cir": "600", "cbs": "600", "red_action": "drop"}}, "COPP_TRAP": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(cfg); got[0] != strings.Replace(stock, "cbs=6000 cir=6000", "cbs=600 cir=600", 1) || got[1] != trap {
		t.Errorf("with the image's copp_cfg.json:\n%s", strings.Join(got, "\n"))
	}
}
