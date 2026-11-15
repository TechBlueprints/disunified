package sonic

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stpStrayCapture is the DX010's STP tables while Ethernet0, a member of
// PortChannel1 since the LACP push, held a bridge port and a BLOCKING STP
// port in Vlan4094's instance and PortChannel1 had none there; the syslog
// excerpt is how it came about and what every l2d restart logged since
// (docs/drivers/sonic.md §11.34).
const (
	stpStrayCapture = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/stp-stray-20261115.json"
	stpStraySyslog  = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/lag-stp-stray-syslog-20261115.txt"
)

// l2d writes no state for a port leaving Bridge (the write StpOrch turned
// into a new bridge port and STP ports for Ethernet0 and Ethernet48), still
// writes one for a link drop, and repairs the captured stray: the restart's
// first pass holds PortChannel1's states back (live the chip refused them at
// every restart), the second removes
// Ethernet0's STP ports through StpOrch and writes PortChannel1's again,
// the third writes nothing.
func TestL2dLagMemberStrayStpPorts(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_strays.py"), "l2d.py", stpStrayCapture, stpStraySyslog).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"LEAVE Ethernet48 0 False", "DROP Ethernet48 1 True",
		"LEAVE Ethernet0 0 False", "DROP Ethernet0 1 True",
		"CHIP Ethernet0 -", "CHIP PortChannel1 4094=FORWARDING", "STALE -",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %q", want)
		}
	}
	passes := strings.SplitAfter(got, "\nPASS ")
	if len(passes) < 4 {
		t.Fatalf("expected three passes:\n%s", got)
	}
	if strings.Contains(passes[0], "REFUSED") || strings.Contains(passes[0], "SET STP_PORT_STATE_TABLE:PortChannel1:") {
		t.Errorf("first pass: PortChannel1's states must wait while its member Ethernet0 holds the instance's STP port (the chip refused them live)")
	}
	two := passes[1]
	if !strings.Contains(two, "DEL STP_PORT_STATE_TABLE:Ethernet0:4094") || !strings.Contains(two, "SET STP_PORT_STATE_TABLE:PortChannel1:4094 4") {
		t.Errorf("second pass: Ethernet0's STP port removed through StpOrch and PortChannel1's state written again")
	}
	if strings.Index(two, "DEL STP_PORT_STATE_TABLE:Ethernet0:4094") > strings.Index(two, "SET STP_PORT_STATE_TABLE:PortChannel1:4094") {
		t.Errorf("second pass: the member's DEL must go before the PortChannel's SET")
	}
	if strings.Contains(two, "REFUSED") || strings.Contains(passes[2], "REFUSED") {
		t.Errorf("refused after the repair:\n%s", got)
	}
	if !strings.HasPrefix(passes[3], "3 0\n") {
		t.Errorf("third pass wrote something: %q", strings.SplitN(passes[3], "\n", 2)[0])
	}
	if !strings.Contains(got, "LOG Ethernet0 is no bridge port (a member of PortChannel1) but held 1 STP ports in the chip") {
		t.Errorf("no log line for the repair:\n%s", got)
	}
}
