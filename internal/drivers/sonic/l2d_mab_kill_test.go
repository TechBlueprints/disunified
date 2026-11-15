package sonic

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mabKillFixture is the DX010's run of MAC authentication over SIGKILLs of
// disunified-l2d (docs/drivers/sonic.md §11.26.12): base the l2d before
// mab_heard (main at the time), fix this one.
const mabKillFixture = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/mab-kill-live.txt"

// l2d killed with SIGKILL and restarted by systemd, replayed from the run
// with this l2d (fix): its frames, RADIUS answers and kills. Nothing is
// written as l2d dies, so the next l2d's idle timer can only start from the
// row's mab_heard (written as l2d runs, at most MAB_HEARD_STEP behind) plus
// that step: a silent host goes between 40 s and 40 s + the step + a pass
// after its last frame -- after one kill, after two in a row, and when its
// one frame came just before the kill -- and a talking host is never
// removed or written again.
func TestL2dMABKill(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_mab_kill.py"), "l2d.py", mabKillFixture, "fix").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	steps := map[string][]string{}
	var cur string
	var all []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s, ok := strings.CutPrefix(line, "STEP "); ok {
			cur = s
			continue
		}
		steps[cur] = append(steps[cur], line)
		all = append(all, line)
	}
	show := func(step string) string { return strings.Join(steps[step], "\n") }
	num := func(step, prefix string) []float64 {
		var v []float64
		for _, l := range steps[step] {
			if s, ok := strings.CutPrefix(l, prefix); ok {
				f, err := strconv.ParseFloat(strings.Fields(s)[0], 64)
				if err != nil {
					t.Fatalf("%s: %q: %v", step, l, err)
				}
				v = append(v, f)
			}
		}
		return v
	}
	const port = "Ethernet116"
	a, c := "02:00:00:00:0a:01", "02:00:00:00:0a:03"
	const idle, step, pass = 40.0, 5.0, 2.1

	for _, l := range all {
		if strings.HasPrefix(l, "GAP ") {
			t.Errorf("a restart left an admitted host without its RETURN: %s", l)
		}
		if strings.HasPrefix(l, "SEND ") && !strings.HasSuffix(l, " as captured true") {
			t.Errorf("a request not as the switch sent it: %s", l)
		}
		if strings.Contains(l, "FDB_TABLE Vlan4094:"+a) && strings.HasPrefix(l, "PSTDEL") ||
			strings.HasPrefix(l, "LOG 802.1X: "+a+" no longer authorised") {
			t.Errorf("the talking host lost its admission: %s", l)
		}
	}
	if !strings.Contains(show("end"), `STATE admitted ["`+a+`"]`) {
		t.Errorf("end: the talking host is still admitted:\n%s", show("end"))
	}

	for _, name := range []string{"kill, C silent 8 s", "two kills, C silent 12 s", "kill just after C's frame"} {
		lastC, kills, gone := num(name, "LASTC "), num(name, "KILL "), num(name, "CAPTURED GONE ")
		var revoked []float64
		for _, l := range steps[name] {
			if f := strings.Fields(l); len(f) == 3 && f[0] == "TIME" && f[2] == c {
				x, _ := strconv.ParseFloat(f[1], 64)
				revoked = append(revoked, x)
			}
		}
		if len(lastC) == 0 || len(kills) == 0 || len(gone) != 1 || len(revoked) != 1 {
			t.Errorf("%s: a kill, C's last frame, its removal here and on the switch:\n%s", name, show(name))
			continue
		}
		last := lastC[0]
		if revoked[0] < last+idle || revoked[0] > last+idle+step+pass {
			t.Errorf("%s: C (last frame %.2f, killed %v) went at %.2f; want %.0f-%.1f s after its last frame",
				name, last, kills, revoked[0], idle, idle+step+pass)
		}
		if d := revoked[0] - gone[0]; d < -pass || d > pass {
			t.Errorf("%s: C went at %.2f here, %.2f on the switch", name, revoked[0], gone[0])
		}
		if !strings.Contains(show(name), "LOG 802.1X: "+c+" authorised on "+port+" by its MAC address, kept over a restart") {
			t.Errorf("%s: C kept over the kill until its time:\n%s", name, show(name))
		}
		if strings.Contains(show(name), "HDEL DUI_DOT1X_TABLE|"+port+" mab_seen") {
			t.Errorf("%s: a killed l2d left no mab_seen to read:\n%s", name, show(name))
		}
	}
}
