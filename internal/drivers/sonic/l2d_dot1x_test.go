package sonic

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dot1xFixture is the DX010's live 802.1X runs (docs/drivers/sonic.md
// §11.25): hostapd's events and station replies on a looped empty cage.
const dot1xFixture = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/dot1x-live.txt"

// l2d's 802.1X against fakes replaying what the DX010's hostapd said: the
// port's kernel drop is in place before its authenticator starts, the
// config file holds the RADIUS secret and is root's alone; the host
// hostapd authorised gets a static FDB entry and a kernel RETURN, the
// wrong password counts one failure, the switch's own looped station
// nothing; a restart adopts the running hostapd and keeps the admission
// without removing it first; a logoff removes it and reaches STATE_DB; an
// unwatched port loses its authenticator, chain and trap, and keeps its
// count.
func TestL2dDot1X(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_dot1x.py"), "l2d.py", dot1xFixture).CombinedOutput()
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
	show := func(step string) string { return strings.Join(steps[step], "\n") }

	drop := index("start", "RUN /usr/sbin/ebtables -A DUI_DOT1X -i Ethernet120 -j DROP")
	eapol := index("start", "RUN /usr/sbin/ebtables -A DUI_DOT1X -i Ethernet120 -p 0x888e -j ACCEPT")
	jump := index("start", "RUN /usr/sbin/ebtables -I INPUT 1 -j DUI_DOT1X")
	trap := index("start", "SET COPP_TRAP|dui_eapol trap_ids=eapol trap_group=queue4_group1 always_enabled=true")
	group := index("start", "SET COPP_GROUP|queue4_group1 ")
	hostapd := -1
	for i, l := range steps["start"] {
		if strings.Contains(l, "/hostapd -B -P ") {
			hostapd = i
		}
	}
	if eapol < 0 || drop < eapol || jump < drop || group < 0 || trap < group || hostapd < trap || hostapd < jump {
		t.Errorf("start: EAPOL accepted before the port's drop, the chain jumped to, the trap's group before the trap, and only then hostapd:\n%s", show("start"))
	}
	for _, l := range steps["start"] {
		if strings.Contains(l, "-i Ethernet0") || strings.Contains(l, "Ethernet0.conf") {
			t.Errorf("start: the force-authorised port got %q", l)
		}
	}
	if !strings.Contains(show("start"), `"auth_server_addr=192.0.2.1", "auth_server_port=1812", "auth_server_shared_secret=test-secret"`) ||
		!strings.Contains(show("start"), `"own_ip_addr=192.0.2.200"`) || !strings.Contains(show("start"), `"driver=wired"`) ||
		!has("start", `STATE conf mode "0o600"`) {
		t.Errorf("start: hostapd's config (wired driver, the NAS address, the server and its secret) is root's alone:\n%s", show("start"))
	}
	if len(steps["second sync"]) != 0 {
		t.Errorf("a second sync wrote:\n%s", show("second sync"))
	}

	ev := "captured events"
	if !has(ev, "PST FDB_TABLE Vlan4094:02:00:00:00:00:03 port=Ethernet120 type=static") ||
		!has(ev, "RUN /usr/sbin/ebtables -A DUI_DOT1X -i Ethernet120 -s 02:00:00:00:00:03 -j RETURN") ||
		!has(ev, `STATE row {"authorized": "02:00:00:00:00:03", "failures": "1", "last_failed_mac": "02:00:00:00:00:01", "mode": "auto"}`) {
		t.Errorf("the authorised host admitted, the refused one counted once:\n%s", show(ev))
	}
	if strings.Contains(show(ev), "02:00:00:00:00:02") {
		t.Errorf("the switch's own looped station was admitted or counted:\n%s", show(ev))
	}

	if strings.Contains(show("restart"), "PSTDEL") || strings.Contains(show("restart"), "/hostapd -B") ||
		!has("restart", `STATE row {"authorized": "02:00:00:00:00:03", "failures": "1", "last_failed_mac": "02:00:00:00:00:01", "mode": "auto"}`) {
		t.Errorf("restart: the running hostapd is adopted, its authorised host kept without a gap, the count carried on:\n%s", show("restart"))
	}

	if !has("logoff", "PSTDEL FDB_TABLE Vlan4094:02:00:00:00:00:03") ||
		!has("logoff", `STATE row {"authorized": "", "failures": "1", "last_failed_mac": "02:00:00:00:00:01", "mode": "auto"}`) ||
		strings.Contains(show("logoff"), "-s 02:00:00:00:00:03") {
		t.Errorf("logoff: the admission goes, from the ASIC, the kernel and STATE_DB:\n%s", show("logoff"))
	}

	u := "unwatched"
	if !has(u, "KILL hostapd Ethernet120") || !has(u, "STATE chain null") || !has(u, "STATE input []") ||
		!has(u, "STATE copp null") || !has(u, "STATE hostapd []") ||
		!has(u, `STATE row {"authorized": "", "failures": "1", "last_failed_mac": "02:00:00:00:00:01", "mode": ""}`) {
		t.Errorf("unwatched: authenticator, chain and trap gone, the count kept:\n%s", show(u))
	}
}
