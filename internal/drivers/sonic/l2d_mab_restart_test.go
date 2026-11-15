package sonic

import (
	"encoding/json"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mabRestartFixture is the DX010's run of MAC authentication and a
// Credentials port over restarts of disunified-l2d (docs/drivers/sonic.md
// §11.26.10): run1 the deployed l2d before the fix, run4 this l2d.
const mabRestartFixture = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/mab-restart-live.txt"

// l2d over a restart, replayed from the run with this l2d (run4): its
// frames, RADIUS answers, hostapd station replies and events, and when l2d
// was restarted, stopped and started and the link went. A host admitted
// by its MAC address is still admitted after a restart without having been
// removed or written again (no FDB delete or put, no moment in which the
// kernel chain drops it); its idle timer carries on from its last frame
// before the restart (the row's mab_seen, written as l2d stops and removed
// as the next one reads it), so a silent host goes 40 s after its last
// frame, not 40 s after the restart; one idle for longer than that by the
// time l2d starts again goes at once, and one whose link went during the
// stop is not admitted again; a refused host is refused again with the
// count carried over. A Credentials port its first host opened stays open
// over a restart (no learn-mode write, no flush, the chain never closes),
// closes on the host's logoff, and closes at start if its link went while
// l2d was stopped (the chip first, then the chain, as a running l2d does).
func TestL2dMABRestart(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_mab_restart.py"), "l2d.py", mabRestartFixture, "run4").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	kernelAtomic(t, string(out))
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
	has := func(step, line string) bool {
		for _, l := range steps[step] {
			if l == line {
				return true
			}
		}
		return false
	}
	// after returns a step's lines after its "AT ... l2d starts" line.
	after := func(step string) []string {
		for i, l := range steps[step] {
			if strings.HasPrefix(l, "AT ") && strings.HasSuffix(l, " l2d starts") {
				return steps[step][i+1:]
			}
		}
		return nil
	}
	hasPrefix := func(lines []string, prefix string) bool {
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return true
			}
		}
		return false
	}
	index := func(lines []string, prefix string) int {
		for i, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return i
			}
		}
		return -1
	}
	stateOf := func(step, what string, v interface{}) {
		for _, l := range steps[step] {
			if s, ok := strings.CutPrefix(l, "STATE "+what+" "); ok {
				if err := json.Unmarshal([]byte(s), v); err != nil {
					t.Fatalf("%s %s: %v", step, what, err)
				}
				return
			}
		}
		t.Fatalf("%s: no STATE %s:\n%s", step, what, show(step))
	}
	const port = "Ethernet116"
	a, b, c, h := "02:00:00:00:0a:01", "02:00:00:00:0a:02", "02:00:00:00:0a:03", "02:00:00:00:0b:01"

	for _, l := range all {
		if strings.HasPrefix(l, "GAP ") {
			t.Errorf("a restart left an admitted host (or an open port) without its RETURN: %s", l)
		}
		if strings.HasPrefix(l, "SEND ") && !strings.HasSuffix(l, " as captured true") {
			t.Errorf("a request not as the switch sent it: %s", l)
		}
	}

	if !has("mab start", `STATE learn "cpu_trap"`) {
		t.Errorf("mab start: the port traps unknown sources:\n%s", show("mab start"))
	}
	if !has("admit", `STATE fdb ["FDB_TABLE:Vlan4094:`+a+`", "FDB_TABLE:Vlan4094:`+c+`"]`) {
		t.Errorf("admit: both hosts admitted by their MAC address:\n%s", show("admit"))
	}

	// The restart: kept as they were.
	r := after("restart")
	if r == nil || hasPrefix(r, "PST FDB_TABLE") || hasPrefix(r, "PSTDEL") || hasPrefix(r, "PST PORT_TABLE") ||
		!has("restart", "HDEL DUI_DOT1X_TABLE|"+port+" mab_seen") ||
		!has("restart", "LOG 802.1X: "+a+" authorised on "+port+" by its MAC address, kept over a restart") ||
		!has("restart", "LOG 802.1X: "+c+" authorised on "+port+" by its MAC address, kept over a restart") {
		t.Errorf("restart: both hosts kept, nothing removed or written again, mab_seen read and removed:\n%s", show("restart"))
	}
	if !strings.Contains(show("restart"), "SET DUI_DOT1X_TABLE|"+port+" mab_seen="+a+"=") {
		t.Errorf("restart: the stopping l2d wrote no mab_seen:\n%s", show("restart"))
	}

	// The idle timer carries on from C's last frame.
	var seen map[string]float64
	stateOf("admit", "seen", &seen)
	var restart, revoked float64 = -1, -1
	for _, l := range all {
		f := strings.Fields(l)
		if len(f) == 3 && f[0] == "TIME" && f[2] == "restart" {
			restart, _ = strconv.ParseFloat(f[1], 64)
		}
		if len(f) == 3 && f[0] == "TIME" && f[2] == c && revoked < 0 && restart >= 0 {
			revoked, _ = strconv.ParseFloat(f[1], 64)
		}
	}
	if restart < 0 || revoked < seen[c]+40 || revoked > seen[c]+42.1 || revoked >= restart+40 {
		t.Errorf("idle: C (last frame %.2f, restart %.2f) went at %.2f; want 40-42 s after its last frame, before restart + 40 s", seen[c], restart, revoked)
	}

	if !strings.Contains(show("refused after restart"), "LOG 802.1X: "+b+" refused by its MAC address on "+port+" (2 failures on this port since boot)") {
		t.Errorf("refused after restart: the refused host is refused again, its count carried over:\n%s", show("refused after restart"))
	}

	// The stop: mab_seen as the switch's l2d wrote it.
	var mine, theirs map[string]float64
	for _, l := range steps["stop"] {
		if s, ok := strings.CutPrefix(l, "SET DUI_DOT1X_TABLE|"+port+" mab_seen="); ok {
			mine = map[string]float64{}
			for _, kv := range strings.Split(s, ",") {
				k, v, _ := strings.Cut(kv, "=")
				mine[k], _ = strconv.ParseFloat(v, 64)
			}
		}
	}
	stateOf("stop", "captured seen", &theirs)
	if len(mine) != 2 || len(theirs) != 2 {
		t.Errorf("stop: mab_seen for both hosts, here and on the switch: %v %v\n%s", mine, theirs, show("stop"))
	}
	for k, v := range theirs {
		if math.Abs(mine[k]-v) > 0.3 {
			t.Errorf("stop: %s last seen %.2f here, %.2f on the switch", k, mine[k], v)
		}
	}

	// Start after a 25 s stop: C (idle 45 s) goes at once, A stays.
	s := after("start, C idle over the stop")
	if !hasPrefix(s, "PSTDEL FDB_TABLE Vlan4094:"+c) || hasPrefix(s, "PSTDEL FDB_TABLE Vlan4094:"+a) || hasPrefix(s, "PST FDB_TABLE") ||
		!hasPrefix(s, "LOG 802.1X: "+c+" no longer authorised on "+port+" (idle for 40s, over a restart)") ||
		!hasPrefix(s, "LOG 802.1X: "+a+" authorised on "+port+" by its MAC address, kept over a restart") {
		t.Errorf("start: C, idle past its timeout during the stop, goes; A is kept:\n%s", show("start, C idle over the stop"))
	}
	var chain [][]string
	stateOf("start, C idle over the stop", "chain", &chain)
	for _, rl := range chain {
		if strings.Join(rl, " ") == "-i "+port+" -s "+c+" -j RETURN" {
			t.Errorf("start: C's RETURN outlived its admission: %v", chain)
		}
	}

	// The link went during the stop: A is not admitted again.
	l := after("link lost during a stop")
	if !hasPrefix(l, "PSTDEL FDB_TABLE Vlan4094:"+a) || hasPrefix(l, "PST FDB_TABLE") || hasPrefix(l, "LOG 802.1X: "+a+" authorised") ||
		!hasPrefix(l, "LOG 802.1X: "+a+" no longer authorised on "+port+" (link down)") {
		t.Errorf("link lost: A goes at start and is not admitted again:\n%s", show("link lost during a stop"))
	}
	if !strings.Contains(show("link back"), "PST FDB_TABLE Vlan4094:"+a+" port="+port+" type=static") {
		t.Errorf("link back: A authenticates again by its MAC:\n%s", show("link back"))
	}

	// Credentials.
	if !has("credentials", "PST PORT_TABLE "+port+" learn_mode=hardware") ||
		!has("credentials", `STATE chain [["-i", "`+port+`", "-p", "0x888e", "-j", "ACCEPT"], ["-i", "`+port+`", "-p", "802_1Q", "--vlan-encap", "0x888e", "-j", "ACCEPT"], ["-i", "`+port+`", "-j", "RETURN"]]`) {
		t.Errorf("credentials: the first host opens the port:\n%s", show("credentials"))
	}
	cr := after("credentials restart")
	if cr == nil || hasPrefix(cr, "PST ") || hasPrefix(cr, "PSTDEL") || hasPrefix(cr, "FLUSH") || !has("credentials restart", `STATE learn "hardware"`) ||
		!has("credentials restart", `STATE row {"authorized": "`+h+`", "failures": "2", "last_failed_mac": "`+b+`", "mab": "", "mode": "auto"}`) {
		t.Errorf("credentials restart: the open port stays open, nothing written:\n%s", show("credentials restart"))
	}
	if !has("logoff", "PSTDEL FDB_TABLE Vlan4094:"+h) || !has("logoff", "PST PORT_TABLE "+port+" learn_mode=drop") ||
		!has("logoff", "FLUSH FLUSHFDBREQUEST PORT "+port) || !has("logoff", `STATE learn "drop"`) {
		t.Errorf("logoff: the last host's logoff closes the port:\n%s", show("logoff"))
	}
	cl := after("credentials link lost during a stop")
	learn, drop := index(cl, "PST PORT_TABLE "+port+" learn_mode=drop"), -1
	for i, x := range cl {
		if x == "EB -A DUI_DOT1X -i "+port+" -j DROP" {
			drop = i
		}
	}
	if learn < 0 || index(cl, "FLUSH FLUSHFDBREQUEST PORT "+port) < learn || drop < learn ||
		!hasPrefix(cl, "PSTDEL FDB_TABLE Vlan4094:"+h) || !hasPrefix(cl, "CTRL DEAUTHENTICATE "+h) || hasPrefix(cl, "LOG 802.1X: "+h+" authorised") {
		t.Errorf("credentials link lost: closed at start, the chip before the chain, hostapd told:\n%s", show("credentials link lost during a stop"))
	}
}

// kernelAtomic holds a fake run's ebtables writes to one ebtables-restore
// each (docs/drivers/sonic.md §11.26.12): no other ebtables command but the
// listing, and no window (testdata/l2d_ebtables.py's WINDOW) in which a
// port that dropped a never-admitted source before and after a chain write
// passed it in between.
func kernelAtomic(t *testing.T, out string) {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "WINDOW ") {
			t.Errorf("a chain write let a closed port pass a never-admitted source: %s", l)
		}
		if strings.HasPrefix(l, "RUN /usr/sbin/ebtables ") && l != "RUN /usr/sbin/ebtables -L" {
			t.Errorf("an ebtables write outside one ebtables-restore: %s", l)
		}
	}
}
