package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const laneHead = "controller:\n  host: 192.0.2.1\ndevices:\n  - name: core-1\n    driver: sonic\n    ssh: admin@192.0.2.10\n"

func loadString(t *testing.T, body string) (*File, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLaneSwitchEntry(t *testing.T) {
	f, err := loadString(t, laneHead+"  - name: core-1-cage2\n    driver: breakout\n    options:\n      parent: core-1\n      cage: \"2\"\n      mac: \"02:00:00:00:00:AA\"\n")
	if err != nil {
		t.Fatal(err)
	}
	d := f.Devices[1]
	if d.Model != LaneModel || d.URL != "" || d.SSH != "" {
		t.Errorf("lane switch defaults: %+v", d)
	}
	ls := f.LaneSwitchesOf("core-1")
	if len(ls) != 1 || ls[0] != (LaneSwitch{Name: "core-1-cage2", Cage: 2, MAC: "02:00:00:00:00:aa"}) {
		t.Errorf("LaneSwitchesOf: %+v", ls)
	}
	if ls := f.LaneSwitchesOf("core-1-cage2"); len(ls) != 0 {
		t.Errorf("a lane switch has lane switches: %+v", ls)
	}
}

func TestLaneSwitchRules(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{"  - name: l\n    driver: breakout\n    options: {cage: \"2\"}\n", "needs options.parent"},
		{"  - name: l\n    driver: breakout\n    options: {parent: nobody, cage: \"2\"}\n", "is not an entry of this file"},
		{"  - name: l\n    driver: breakout\n    options: {parent: core-1, cage: \"0\"}\n", "options.cage"},
		{"  - name: l\n    driver: breakout\n    options: {parent: core-1, cage: \"100\"}\n", "options.cage"},
		{"  - name: l\n    driver: breakout\n    options: {parent: core-1, cage: \"2\", mac: x}\n", "not a 6-byte MAC"},
		{"  - name: l\n    driver: breakout\n    ssh: admin@192.0.2.10\n    options: {parent: core-1, cage: \"2\"}\n", "no transport of its own"},
		{"  - name: l\n    driver: breakout\n    options: {parent: core-1, cage: \"2\"}\n  - name: m\n    driver: breakout\n    options: {parent: core-1, cage: \"2\"}\n", "already has a lane switch (l)"},
		{"  - name: l\n    driver: breakout\n    options: {parent: core-1, cage: \"2\"}\n  - name: m\n    driver: breakout\n    options: {parent: l, cage: \"3\"}\n", "is a lane switch itself"},
	} {
		if _, err := loadString(t, laneHead+c.body); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v (want %q)", c.body, err, c.want)
		}
	}
	// A children: entry on the same port is the old experiment's knob.
	body := "controller:\n  host: 192.0.2.1\ndevices:\n  - name: core-1\n    driver: sonic\n    ssh: admin@192.0.2.10\n    children: [{port: 2, mac: \"02:00:00:00:00:aa\"}]\n  - name: l\n    driver: breakout\n    options: {parent: core-1, cage: \"2\"}\n"
	if _, err := loadString(t, body); err == nil || !strings.Contains(err.Error(), "already declares a child on port 2") {
		t.Errorf("children: on the cage: %v", err)
	}
}
