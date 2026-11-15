package sonic

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// l2dLinkFlapJournal is disunified-l2d's own journal while an empty cage of
// the DX010 was looped and unlooped with the chip's MAC loopback
// (docs/drivers/sonic.md §11.23, 2026-11-15): each cycle the kernel bridge
// port goes disabled -> blocking on link up (RSTP makes the looped port
// Backup, discarding) and blocking -> disabled on link down. mstpd's own
// counters (num_transition_fwd/blk) did not move once in the whole run.
const l2dLinkFlapJournal = "../../../docs/fixtures/sonic-igmp-202511.20261114-42cccc87a/l2d-linkflap-20261115.txt"

// l2d counts every change of a port's kernel state, which is what a UniFi
// switch's stp_state_change_count counts (2 per drop on the real switch's
// flapping ports, docs/fixtures/controller-11.0.99/; 0 on a port never up):
// the replayed journal leaves one count per logged change, the poll that
// re-reads the same state adds none, the count carries over an l2d restart,
// and a port that never changed publishes 0.
func TestL2dCountsStateChanges(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	f, err := os.Open(l2dLinkFlapJournal)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	re := regexp.MustCompile(`disunified-l2d\[\d+\]: (\S+): (\w+) -> (\w+)$`)
	var changes, ups, downs int
	cur := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := re.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		if cur == "" {
			cur = m[2]
		}
		if m[3] != cur {
			changes++
		}
		cur = m[3]
		switch m[2] + ">" + m[3] {
		case "disabled>blocking":
			ups++
		case "blocking>disabled":
			downs++
		default:
			t.Errorf("journal holds %s -> %s: the looped cage was expected to go only disabled <-> blocking", m[2], m[3])
		}
	}
	if ups < 30 || downs < 30 {
		t.Fatalf("journal: %d link-ups, %d drops; the capture is the whole run", ups, downs)
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_state_changes.py"), "l2d.py", l2dLinkFlapJournal).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(line, " ")
		if strings.HasPrefix(k, "STEP") {
			continue
		}
		got[k] = v
	}
	if want := "Ethernet64 " + strconv.Itoa(changes); got["PUBLISH"] != want {
		t.Errorf("published %q, want %q: one per change of state (2 per drop), none for the poll", got["PUBLISH"], want)
	}
	if want := strconv.Itoa(changes + 1); got["RESTART"] != want {
		t.Errorf("after a restart and one more change: %q, want %q (the count is since boot)", got["RESTART"], want)
	}
	if got["NEVER"] != "0" {
		t.Errorf("a port that never changed: %q, want 0 (mstpd's own counters read 1 there)", got["NEVER"])
	}
}
