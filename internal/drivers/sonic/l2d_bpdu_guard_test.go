package sonic

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// l2BPDUGuardFixture is the DX010 with BPDU guard on cage 17 (Ethernet64)
// and the cage looped (2026-11-15, §11.23.3): mstpd reports the port
// bpdu-guard-error yes, its netdev shut, while APPL_DB's oper_status still
// reads up -- the chip's port never went down.
const l2BPDUGuardFixture = "../../../docs/fixtures/sonic-igmp-202511.20261114-42cccc87a/collect-bpduguard.txt"

// l2dSteps runs testdata/l2d_bpdu_guard.py and returns each step's lines.
func l2dSteps(t *testing.T) map[string][]string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_bpdu_guard.py"), "l2d.py", l2BPDUGuardFixture).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	steps := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(string(out), "\n") {
		if s, ok := strings.CutPrefix(line, "STEP "); ok {
			cur = s
			continue
		}
		if line != "" && !strings.Contains(line, "DUI_STP_PORT_TABLE") && !strings.Contains(line, "DUI_STP_TABLE") {
			steps[cur] = append(steps[cur], line)
		}
	}
	return steps
}

func has(lines []string, prefix string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
}

// §11.28: a tripped BPDU guard holds the port down in the chip (APPL_DB
// admin_status) until the operator's Re-enable, and the hold outlives what
// would otherwise undo it -- portmgrd copying CONFIG_DB's admin_status up
// after any write of the port's row, an l2d restart. A port released with
// no link keeps mstpd's old error and is not held again until its link
// comes up (a new trip); BPDU guard switched off releases the port.
func TestL2dHoldsATrippedBPDUGuard(t *testing.T) {
	s := l2dSteps(t)
	for _, c := range []struct {
		step, held string
		want, not  []string
	}{
		{"trip", "yes", []string{"PUT APPL_DB PORT_TABLE Ethernet64 admin_status=down", "PUT STATE_DB DUI_BPDU_GUARD_TABLE Ethernet64 state=held"}, nil},
		{"again", "yes", nil, []string{"PUT APPL_DB"}},
		{"portmgrd", "yes", []string{"PUT APPL_DB PORT_TABLE Ethernet64 admin_status=down", "LOG Ethernet64: admin_status was rewritten under a hold", "RUN ip link set dev Ethernet64 down"}, nil},
		{"restart", "yes", nil, []string{"PUT APPL_DB", "RUN "}},
		{"recover", "no", []string{"DEL STATE_DB DUI_PORT_RECOVERY Ethernet64", "PUT APPL_DB PORT_TABLE Ethernet64 admin_status=up", "RUN ip link set dev Ethernet64 up", "LOG bpdu guard: Ethernet64 released (re-enabled by the controller)"}, nil},
		{"nolink", "no", nil, []string{"PUT APPL_DB"}},
		{"restart-released", "no", nil, []string{"PUT APPL_DB"}},
		{"linkup", "yes", []string{"PUT APPL_DB PORT_TABLE Ethernet64 admin_status=down"}, nil},
		{"guardoff", "no", []string{"PUT APPL_DB PORT_TABLE Ethernet64 admin_status=up", "RUN ip link set dev Ethernet64 up", "LOG bpdu guard: Ethernet64 released (BPDU guard off)"}, []string{"DEL STATE_DB DUI_BPDU_GUARD_TABLE"}},
		{"guardon-stale", "no", nil, []string{"PUT APPL_DB"}},
		{"errorclears", "no", []string{"DEL STATE_DB DUI_BPDU_GUARD_TABLE Ethernet64"}, nil},
		{"nothing", "no", []string{"LOG port recovery for Ethernet64: nothing holds it"}, []string{"PUT APPL_DB"}},
	} {
		lines := s[c.step]
		if !has(lines, "HELD Ethernet64 "+c.held) {
			t.Errorf("%s: bpdu_guard_held should read %s:\n%s", c.step, c.held, strings.Join(lines, "\n"))
		}
		for _, w := range c.want {
			if !has(lines, w) {
				t.Errorf("%s: missing %q:\n%s", c.step, w, strings.Join(lines, "\n"))
			}
		}
		for _, n := range c.not {
			if has(lines, n) {
				t.Errorf("%s: unexpected %q:\n%s", c.step, n, strings.Join(lines, "\n"))
			}
		}
	}
}

// The same checks for loop protection's 150 s hold: put down again after a
// CONFIG_DB write under it, ended by the operator's Re-enable, and released
// when the port's loop protection is switched off.
func TestL2dLoopProtectionHoldSurvivesAndReleases(t *testing.T) {
	s := l2dSteps(t)
	if l := s["loop-block"]; !has(l, "LOG Ethernet64: admin_status was rewritten under a hold") {
		t.Errorf("loop hold not re-asserted:\n%s", strings.Join(l, "\n"))
	}
	if l := s["loop-recover"]; !has(l, "LOG loop protection: Ethernet64 re-enabled by the controller") || !has(l, "PUT APPL_DB PORT_TABLE Ethernet64 admin_status=up") {
		t.Errorf("Re-enable did not end the loop hold:\n%s", strings.Join(l, "\n"))
	}
	if l := s["loop-off"]; !has(l, "LOG loop protection: Ethernet64 restored (loop protection off)") {
		t.Errorf("a hold outlived the port's loop protection:\n%s", strings.Join(l, "\n"))
	}
}

// The controller's port-recovery reaches l2d as one STATE_DB row per
// interface of the port.
func TestRecoverPortAsksL2d(t *testing.T) {
	r := newL2RigAt(t, l2BPDUGuardFixture, false, nil)
	if err := r.c.RecoverPort(context.Background(), 17); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.fr.Recoveries, []string{"Ethernet64"}) {
		t.Errorf("recoveries = %q, want Ethernet64", r.fr.Recoveries)
	}
	if err := r.c.RecoverPort(context.Background(), 99); err == nil {
		t.Error("a port the switch does not have must be refused")
	}
	r.c.L2 = false
	if err := r.c.RecoverPort(context.Background(), 17); err == nil {
		t.Error("without l2d nothing holds a port: refuse")
	}
}
