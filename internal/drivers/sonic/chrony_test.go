package sonic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// chrony left offline by its boot hook (docs/drivers/sonic.md §10.6), on the
// DX010 after its hand fix (collect-20261101.txt: NTP from Vlan4094, which
// has carrier). The chrony sections are read from the unit (scrubbed):
// chrony-synced.txt; chrony-offline.txt with every source offline (`sudo
// chronyc offline`, the state the boot hook leaves); and
// chrony-after-onoffline.txt right after `sudo chronyc onoffline` -- the
// sources still read `?` with reach 0, `activity` already 2 online.

func chronyFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(hwFixture), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// chronyAs puts a chrony fixture's sections in place; after, when set, is
// what `chronyc onoffline` leads to.
func chronyAs(t *testing.T, name, after string) func(*FixtureRunner) {
	return func(fr *FixtureRunner) {
		sec := sections(chronyFile(t, name))
		fr.UseSection("chrony_sources", sec["chrony_sources"])
		fr.UseSection("chrony_activity", sec["chrony_activity"])
		if after != "" {
			fr.ChronyAfterOnoffline = chronyFile(t, after)
		}
	}
}

const onoffline = "sudo chronyc onoffline"

func (r *controlRig) deviceCmds(d devicemodel.DeviceDesired) []string {
	r.t.Helper()
	n := len(r.fr.Commands)
	changed, err := r.c.ApplyDevice(context.Background(), d)
	if err != nil {
		r.t.Fatal(err)
	}
	got := append([]string(nil), r.fr.Commands[n:]...)
	if containsStr(got, onoffline) && changed != 0 {
		r.t.Errorf("the nudge counted as %d changed settings", changed)
	}
	return got
}

func TestChronyParse(t *testing.T) {
	src, ok := parseChronySources(sections(chronyFile(t, "chrony-synced.txt"))["chrony_sources"])
	if !ok || len(src) != 2 || src[0].Reach != 0o177 || src[1].State != "*" {
		t.Errorf("synced sources %+v %v", src, ok)
	}
	for name, want := range map[string][2]int{"chrony-synced.txt": {2, 0}, "chrony-offline.txt": {0, 2}, "chrony-after-onoffline.txt": {2, 0}} {
		on, off, ok := chronyActivity(sections(chronyFile(t, name))["chrony_activity"])
		if !ok || on != want[0] || off != want[1] {
			t.Errorf("%s: activity online %d offline %d (%v), want %v", name, on, off, ok, want)
		}
	}
	// What `sources` cannot tell apart: offline (before and after the
	// nudge) reads like unreachable.
	for _, name := range []string{"chrony-offline.txt", "chrony-after-onoffline.txt"} {
		src, _ := parseChronySources(sections(chronyFile(t, name))["chrony_sources"])
		for _, s := range src {
			if s.State != "?" || s.Reach != 0 {
				t.Errorf("%s: %+v", name, s)
			}
		}
	}
	if _, _, ok := chronyActivity(""); ok {
		t.Error("no section must read as unknown")
	}
}

// Every source offline, Vlan4094 up: one nudge per collect, logged once
// for the episode, never counted; a second apply on the same snapshot (a
// push and the reconcile in one cycle) does not nudge again.
func TestChronyNudgedWhileEverySourceIsOffline(t *testing.T) {
	r := hwRigAt(t, filepath.Join(filepath.Dir(hwFixture), "collect-20261101.txt"), chronyAs(t, "chrony-offline.txt", ""))
	d := ntpAsCaptured(r)
	if got := r.deviceCmds(d); len(got) != 1 || got[0] != onoffline {
		t.Fatalf("first apply wrote %q, want the nudge alone", got)
	}
	if got := r.deviceCmds(d); len(got) != 0 {
		t.Errorf("same snapshot: wrote %q, want nothing (once per collect)", got)
	}
	r.system2()
	if got := r.deviceCmds(d); len(got) != 1 || got[0] != onoffline {
		t.Errorf("next cycle, still offline: wrote %q, want the nudge again", got)
	}
	if n := strings.Count(r.log.String(), "every NTP source offline (2: 192.0.2.2, 192.0.2.3) while Vlan4094 is up"); n != 1 {
		t.Errorf("episode logged %d times, want once:\n%s", n, r.log.String())
	}
}

// The nudge works (chrony-after-onoffline.txt after it: online, not yet
// answered): the next cycle writes nothing and says they are online.
func TestChronyNudgeRecovers(t *testing.T) {
	r := hwRigAt(t, filepath.Join(filepath.Dir(hwFixture), "collect-20261101.txt"), chronyAs(t, "chrony-offline.txt", "chrony-after-onoffline.txt"))
	d := ntpAsCaptured(r)
	if got := r.deviceCmds(d); len(got) != 1 || got[0] != onoffline {
		t.Fatalf("wrote %q", got)
	}
	r.system2()
	if got := r.deviceCmds(d); len(got) != 0 {
		t.Errorf("after the nudge: wrote %q", got)
	}
	if !strings.Contains(r.log.String(), "sonic: chrony: NTP sources online again (2)") {
		t.Errorf("recovery not logged:\n%s", r.log.String())
	}
}

// No nudge: a synced chrony, or one whose sources are online but have not
// answered yet (`?`, reach 0 -- what `sources` alone would mistake for the
// boot hook's state); no chrony sections (an older capture); NTP not
// managed; an apply that writes NTP itself (it restarts chrony); and
// the in-band interface without carrier (the capture's Vlan4094 entry
// with its link taken away, as ip -j addr prints a down interface).
func TestChronyNotNudged(t *testing.T) {
	at := filepath.Join(filepath.Dir(hwFixture), "collect-20261101.txt")
	for _, name := range []string{"chrony-synced.txt", "chrony-after-onoffline.txt"} {
		r := hwRigAt(t, at, chronyAs(t, name, ""))
		if got := r.deviceCmds(ntpAsCaptured(r)); len(got) != 0 {
			t.Errorf("%s: wrote %q", name, got)
		}
	}
	var r *controlRig
	r = hwRigAt(t, at, nil)
	if got := r.deviceCmds(ntpAsCaptured(r)); len(got) != 0 {
		t.Errorf("no chrony sections: wrote %q", got)
	}
	r = hwRigAt(t, at, chronyAs(t, "chrony-offline.txt", ""))
	if got := r.deviceCmds(devicemodel.DeviceDesired{}); len(got) != 0 {
		t.Errorf("NTP not managed: wrote %q", got)
	}
	d := ntpAsCaptured(r)
	d.NTPServers = append(d.NTPServers, "192.0.2.99")
	if got := r.deviceCmds(d); containsStr(got, onoffline) || !containsStr(got, "sudo config ntp add 192.0.2.99") {
		t.Errorf("an NTP write: wrote %q, want the add without the nudge", got)
	}
	r = hwRigAt(t, at, func(fr *FixtureRunner) {
		chronyAs(t, "chrony-offline.txt", "")(fr)
		var addrs []map[string]any
		if err := json.Unmarshal([]byte(sections(fr.Fixture)["addr"]), &addrs); err != nil {
			t.Fatal(err)
		}
		for _, a := range addrs {
			if a["ifname"] == "Vlan4094" {
				a["operstate"], a["flags"] = "DOWN", []string{"NO-CARRIER", "BROADCAST", "MULTICAST", "UP"}
			}
		}
		b, _ := json.Marshal(addrs)
		fr.UseSection("addr", string(b))
	})
	if got := r.deviceCmds(ntpAsCaptured(r)); len(got) != 0 {
		t.Errorf("Vlan4094 without carrier: wrote %q", got)
	}
}
