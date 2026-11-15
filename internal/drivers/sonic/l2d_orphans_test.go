package sonic

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The DX010's STP tables with STP instances no VLAN uses
// (docs/drivers/sonic.md §11.35): stpOrphanCapture after the repro (ten:
// nine the 15:10 deploy's VLAN prune left, one the repro made),
// stpVlan3001Capture with the repro's test VLAN mapped (and the nine).
const (
	stpOrphanCapture   = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/stp-orphans-20261115.json"
	stpVlan3001Capture = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/stp-vlan3001-20261115.json"
)

func runL2dOrphans(t *testing.T, capture, part string) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_orphans.py"), "l2d.py", capture, part, t.TempDir()).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out)
}

// The ten orphans of the capture go, one per pass, each through a VLAN made
// in orchagent alone, mapped into the instance and out; Vlan4094's instance
// keeps all 31 STP ports and their states, nothing writes to it but the
// states it already had, and nothing is left behind.
func TestL2dRemovesOrphanedStpInstances(t *testing.T) {
	got := runL2dOrphans(t, stpOrphanCapture, "cleanup")
	for _, want := range []string{
		"INSTANCES 4094", "OBJECTS 2 OUTSIDE4094 0", "VLANS 4094", "STP4094 True 31 0",
		"APPL STP_VLAN_INSTANCE_TABLE:Vlan4094,VLAN_TABLE:Vlan4094", "STUCK -", "RETIRED -",
		"LOG STP instance 2 (its VLAN gone) was still in the chip: removed, 9 left",
		"LOG STP instance 4000 (its VLAN gone) was still in the chip: removed, 0 left",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %q\n%s", want, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "Vlan4094") && (strings.HasPrefix(line, "SET ") || strings.HasPrefix(line, "DEL ")) &&
			line != "SET STP_VLAN_INSTANCE_TABLE:Vlan4094 4094" {
			t.Errorf("a write to Vlan4094: %q", line)
		}
		if strings.HasPrefix(line, "DEL STP_PORT_STATE_TABLE:") && strings.HasSuffix(line, ":4094 ") {
			t.Errorf("a state of Vlan4094's instance removed: %q", line)
		}
		if strings.HasPrefix(line, "SET STP_PORT_STATE_TABLE:") && strings.Contains(line, ":4094 ") {
			t.Errorf("a changed state in Vlan4094's instance: %q", line)
		}
	}
	passes := strings.Split(got, "\nPASS ") // passes[i] ends with pass i+1's writes
	for i, p := range passes[:10] {
		if strings.Count(p, "SET VLAN_TABLE:") != 1 || strings.Count(p, "DEL VLAN_TABLE:") != 1 {
			t.Errorf("pass %d: one orphan per pass, its VLAN made and removed:\n%s", i+1, p)
		}
	}
	if strings.Contains(passes[10], "VLAN_TABLE:") {
		t.Errorf("pass 11 made a VLAN with no orphan left:\n%s", passes[10])
	}
}

// The 15:10 race: a restart's states queued while the VLAN goes; orchagent
// takes the VLAN's DEL first and then the queued states, which make the
// instance again (31 STP ports, no VLAN). The fixed l2d's DELs take its
// ports out at the next pass and the unmap the instance after
// RETIRE_SETTLE; the nine orphans whose ids it never had stay, logged.
func TestL2dVlanDeleteRace(t *testing.T) {
	got := runL2dOrphans(t, stpVlan3001Capture, "race")
	for _, want := range []string{
		"QUEUED 31", "AFTER-ORCH 3001 in ports=31",
		"INSTANCES 2,8,9,10,69,1000,2000,3000,4000,4094", "OBJECTS 11 OUTSIDE4094 21",
		"VLANS 4094", "STP4094 True 31 0", "STUCK -", "RETIRED -",
		"LOG Vlan3001 is gone: its STP instance keys dropped",
		"LOG STP instance 3001 (its VLAN gone) was still in the chip: removed, 9 left",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %q\n%s", want, got)
		}
	}
	if !strings.Contains(got, "DEL STP_PORT_STATE_TABLE:PortChannel1:3001 \n") {
		t.Errorf("the gone VLAN's port keys must go out through StpOrch's producer:\n%s", got)
	}
	if !strings.Contains(got, "LOG the chip holds 9 STP instances no VLAN uses, and no retired VLAN id to remove them by") {
		t.Errorf("no note for the orphans l2d has no id for:\n%s", got)
	}
}
