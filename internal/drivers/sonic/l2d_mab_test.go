package sonic

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mabFixture is the DX010's MAC authentication run by hand
// (docs/drivers/sonic.md §11.26.8): the requests decoded and the RADIUS
// server's answers as received.
const mabFixture = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/mab-live.txt"

// l2d's MAC authentication against fakes and the captured answers: a
// mac_based port is trapped to the CPU only behind the kernel's drop; a
// frame from an unknown source sends one request, built as the hand run's
// (the MAC upper case without separators as user name and password, the
// usual MAB attributes, a Message-Authenticator), and the captured
// Access-Accept admits the host (static FDB in every VLAN, a kernel
// RETURN, STATE_DB at once); the captured Access-Reject counts a failure and
// leaves the source quiet; EAPOL, the switch's own frames and a quiet source
// send nothing; an idle host loses its admission and authenticates again on
// its next frame; an answer not signed with the request's authenticator is
// ignored; a port leaving the config gets its learn mode back, and a
// force-unauthorised port's row loses its mode (seen live: it kept it).
func TestL2dMAB(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_mab.py"), "l2d.py", mabFixture).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	steps := map[string][]string{}
	var cur string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s, ok := strings.CutPrefix(line, "STEP "); ok {
			cur = s
			continue
		}
		steps[cur] = append(steps[cur], line)
	}
	index := func(step, prefix string) int {
		for i, l := range steps[step] {
			if strings.HasPrefix(l, prefix) {
				return i
			}
		}
		return -1
	}
	has := func(step, line string) bool { i := index(step, line); return i >= 0 && steps[step][i] == line }
	count := func(step, prefix string) int {
		n := 0
		for _, l := range steps[step] {
			if strings.HasPrefix(l, prefix) {
				n++
			}
		}
		return n
	}
	show := func(step string) string { return strings.Join(steps[step], "\n") }

	drop := index("start", "RUN /usr/sbin/ebtables -A DUI_DOT1X -i Ethernet120 -j DROP")
	trap := index("start", "PST PORT_TABLE Ethernet120 learn_mode=cpu_trap")
	if drop < 0 || trap < drop || !has("start", `STATE learn 112 "drop"`) {
		t.Errorf("start: the mac_based port trapped to the CPU only after its kernel drop, the force-unauthorised one left closed:\n%s", show("start"))
	}

	a := "accept"
	if count(a, "SEND ") != 1 || count(a, "STATE as captured true") != 1 || !has(a, `STATE password "020000000A01"`) ||
		!has(a, "STATE message-authenticator true") ||
		!strings.Contains(show(a), "User-Name=020000000A01 ") || !strings.Contains(show(a), "Service-Type=10 ") ||
		!strings.Contains(show(a), "NAS-Port-Type=15 ") || !strings.Contains(show(a), "Calling-Station-Id=02-00-00-00-0A-01 ") {
		t.Errorf("accept: one request for two frames, as the hand run sent it:\n%s", show(a))
	}
	if !has(a, "PST FDB_TABLE Vlan3997:02:00:00:00:0a:01 port=Ethernet120 type=static") ||
		!has(a, "PST FDB_TABLE Vlan4094:02:00:00:00:0a:01 port=Ethernet120 type=static") ||
		!has(a, "RUN /usr/sbin/ebtables -A DUI_DOT1X -i Ethernet120 -s 02:00:00:00:0a:01 -j RETURN") ||
		!has(a, `STATE row {"authorized": "02:00:00:00:0a:01", "failures": "0", "last_failed_mac": "", "mab": "02:00:00:00:0a:01", "mode": "mac_based"}`) {
		t.Errorf("accept: the captured Access-Accept admits the host, published at once:\n%s", show(a))
	}

	r := "reject"
	if count(r, "STATE as captured true") != 1 || strings.Contains(show(r), "PST FDB_TABLE") ||
		!has(r, `STATE row {"authorized": "02:00:00:00:0a:01", "failures": "1", "last_failed_mac": "02:00:00:00:0a:02", "mab": "02:00:00:00:0a:01", "mode": "mac_based"}`) {
		t.Errorf("reject: the captured Access-Reject counts one failure and admits nothing:\n%s", show(r))
	}
	if !has("quiet", "STATE requests 0") {
		t.Errorf("quiet: a refused source, an 802.1X host or the switch itself sent a request:\n%s", show("quiet"))
	}

	i := "idle"
	if !has(i, "PSTDEL FDB_TABLE Vlan4094:02:00:00:00:0a:01") || !has(i, "LOG 802.1X: 02:00:00:00:0a:01 no longer authorised on Ethernet120 (idle for 20s)") ||
		!has(i, `STATE row {"authorized": "", "failures": "1", "last_failed_mac": "02:00:00:00:0a:02", "mab": "", "mode": "mac_based"}`) {
		t.Errorf("idle: the host loses its admission after the push's idle timeout:\n%s", show(i))
	}
	if count("return", "STATE as captured true") != 1 || !has("return", "PST FDB_TABLE Vlan4094:02:00:00:00:0a:01 port=Ethernet120 type=static") {
		t.Errorf("return: the host's next frame authenticates it again:\n%s", show("return"))
	}

	f := "forged"
	if count(f, "SEND ") != 1 || strings.Contains(show(f), "02:00:00:00:0a:03 authorised") || strings.Contains(show(f), "PST FDB_TABLE") ||
		index(f, "LOG 802.1X: a reply from 127.0.0.1 that is not a signed answer") < 0 {
		t.Errorf("forged: an accept not signed for the request admitted the host:\n%s", show(f))
	}

	u := "unwatched"
	if !has(u, "PST PORT_TABLE Ethernet120 learn_mode=drop") || !has(u, "STATE chain null") || !has(u, "STATE fdb []") ||
		index(u, "PST PORT_TABLE Ethernet120") > index(u, "LOG 802.1X: kernel bridge rules removed") ||
		!has(u, `STATE row {"authorized": "", "failures": "1", "last_failed_mac": "02:00:00:00:0a:02", "mab": "", "mode": ""}`) ||
		!has(u, `STATE row 112 {"authorized": "", "failures": "0", "last_failed_mac": "", "mab": "", "mode": ""}`) {
		t.Errorf("unwatched: the learn mode back before the chain goes, the admissions gone, the counts kept, no mode left:\n%s", show(u))
	}
}
