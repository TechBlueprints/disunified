package sonic

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// maclockFixture is the DX010's live MAC locking runs (docs/drivers/sonic.md
// §11.18.2): the learn-mode probe, the chip's learn flag per mode, and the
// frames a watched port's netdev delivered. collect-maclock.txt is the
// collector's capture during the second run, with the count it produced.
const (
	maclockFixture = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/maclock-live.txt"
	maclockCollect = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-maclock.txt"
)

// l2d's MAC locking against fakes fed with the captured frames: the kernel
// drop goes in before the port's learning moves to cpu_trap and comes out
// after it moves back; the switch's own BPDU, a listed source and the
// switch's own transmissions are not counted; every frame from a source not
// on the list is, the count reaches STATE_DB once a pass, and the port is
// never taken down; a restart carries the count on (it is since boot), and
// unwatching keeps it.
func TestL2dMacLocking(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	out, err := exec.Command(py, "-I", filepath.Join("testdata", "l2d_maclock.py"), "l2d.py", maclockFixture, t.TempDir()).CombinedOutput()
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
	has := func(step, line string) bool { return index(step, line) >= 0 && steps[step][index(step, line)] == line }
	show := func(step string) string { return strings.Join(steps[step], "\n") }

	chain := index("watch", "RUN /usr/sbin/ebtables -A DUI_MACLOCK -i Ethernet36 -j DROP")
	trap := index("watch", "PST PORT_TABLE Ethernet36 learn_mode=cpu_trap")
	if chain < 0 || trap < 0 || chain > trap {
		t.Errorf("watch: the kernel drop (%d) must be in place before cpu_trap (%d):\n%s", chain, trap, show("watch"))
	}
	if !has("watch", "RUN /usr/sbin/ebtables -A DUI_MACLOCK -i Ethernet36 -s 02:00:00:00:00:91 -j RETURN") ||
		!has("watch", "RUN /usr/sbin/ebtables -A INPUT -j DUI_MACLOCK") || !has("watch", "RUN /usr/sbin/ebtables -A FORWARD -j DUI_MACLOCK") {
		t.Errorf("watch: the listed source passes, the chain is reached from INPUT and FORWARD:\n%s", show("watch"))
	}
	for _, l := range steps["second sync"] {
		if strings.HasPrefix(l, "PST ") || strings.HasPrefix(l, "SET ") || strings.Contains(l, "ebtables -A") || strings.Contains(l, "ebtables -F") {
			t.Errorf("a second sync wrote %q", l)
		}
	}
	if !has("own BPDU and listed source", "STATE row null") {
		t.Errorf("the switch's own BPDU, its own transmissions or a listed source were counted:\n%s", show("own BPDU and listed source"))
	}
	s := "source not on the list"
	if !has(s, "STATE before publish null") || !has(s, "SET DUI_MACLOCK_TABLE|Ethernet36 drops=5 last_mac=02:00:00:00:00:91") ||
		!has(s, `STATE row {"drops": "5", "last_mac": "02:00:00:00:00:91"}`) {
		t.Errorf("five frames from an unlisted source: one count of 5, written by the next pass:\n%s", show(s))
	}
	if n := strings.Count(show(s), "is not on its list"); n != 1 {
		t.Errorf("%d log lines for one source's burst, want 1", n)
	}
	for _, step := range []string{s, "restart"} {
		for _, l := range steps[step] {
			if strings.Contains(l, "admin_status") || strings.Contains(l, "DUI_LOOP_GUARD_TABLE") {
				t.Errorf("%s: the port must stay up, wrote %q", step, l)
			}
		}
	}
	if !has(s, "STATE admin up") {
		t.Errorf("the port's admin status changed:\n%s", show(s))
	}
	if !has("restart", `STATE row {"drops": "6", "last_mac": "02:00:00:00:00:91"}`) {
		t.Errorf("a restart must carry the count on:\n%s", show("restart"))
	}
	w := "unwatch"
	back := index(w, "PST PORT_TABLE Ethernet36 learn_mode=drop")
	gone := index(w, "RUN /usr/sbin/ebtables -X DUI_MACLOCK")
	if back < 0 || gone < 0 || back > gone {
		t.Errorf("unwatch: the CONFIG_DB learn mode back before the chain goes:\n%s", show(w))
	}
	if !has(w, `STATE learn_mode drop chains ['FORWARD', 'INPUT', 'OUTPUT'] row {"drops": "6", "last_mac": "02:00:00:00:00:91"}`) {
		t.Errorf("unwatch must keep the count:\n%s", show(w))
	}
	if !has("teardown", "STATE learn_mode drop chains ['FORWARD', 'INPUT', 'OUTPUT']") {
		t.Errorf("teardown leaves the port trapping or the chain behind:\n%s", show("teardown"))
	}
}

// The live count reaches the port it was counted on: cage 10 (Ethernet36)
// counted 22 frames from sources not on its list; every other port reads 0,
// and no port carries a Fault for it (the port stayed up).
func TestMacLockingDropsFromCapture(t *testing.T) {
	r := rig20261106(t, maclockCollect)
	snap, err := r.c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snap.Ports {
		want := uint64(0)
		if p.Index == 10 {
			want = 22
		}
		if p.Health.MACLockingDrops != want {
			t.Errorf("port %d: %d MAC locking drops, want %d", p.Index, p.Health.MACLockingDrops, want)
		}
		if strings.Contains(p.Fault, "mac-locking") {
			t.Errorf("port %d: Fault %q for a port that stayed up", p.Index, p.Fault)
		}
	}
}

// A port's MAC Address Filter puts the port (or its PortChannel) in l2d's
// mac_lock section with its list, lower case and sorted; no secured port, no
// section, so a config written before the section existed reads the same.
func TestL2MacLockConfig(t *testing.T) {
	r := enabled(t)
	ds := r.ports(func(d *devicemodel.PortDesired) {
		switch d.Index {
		case 2:
			d.PortSecurity, d.AllowedMACs = true, []string{"02:00:00:00:00:B2", "02:00:00:00:00:a1", "02:00:00:00:00:b2"}
		case 3:
			d.PortSecurity = true // an empty list allows nothing
		}
	})
	if _, err := r.push(rstp, ds); err != nil {
		t.Fatal(err)
	}
	c := r.conf()
	if c.MacLock == nil {
		t.Fatalf("no mac_lock section: %s", r.fr.L2Files[l2ConfPath])
	}
	if got := c.MacLock.Ports["Ethernet4"]; strings.Join(got, ",") != "02:00:00:00:00:a1,02:00:00:00:00:b2" {
		t.Errorf("Ethernet4's list = %v", got)
	}
	if got, ok := c.MacLock.Ports["Ethernet8"]; !ok || got == nil || len(got) != 0 {
		t.Errorf("Ethernet8's list = %#v, want present and empty", got)
	}
	if len(c.MacLock.Ports) != 2 {
		t.Errorf("mac_lock = %+v", *c.MacLock)
	}
	if !strings.Contains(r.log.String(), "MAC locking 2 ports") {
		t.Errorf("the apply log does not say so:\n%s", r.log.String())
	}
	if cmds, _ := r.push(rstp, ds); len(cmds) != 0 {
		t.Errorf("a second push wrote %q", cmds)
	}
	if _, err := r.push(rstp, r.ports(nil)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.fr.L2Files[l2ConfPath], "mac_lock") {
		t.Errorf("filter off, the file still names mac_lock:\n%s", r.fr.L2Files[l2ConfPath])
	}
}
