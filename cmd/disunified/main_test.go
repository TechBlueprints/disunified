package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/config"
)

// -device-ssh-key / DUI_DEVICE_SSH_KEY is the config file's options.ssh_key
// for a device run from the flags (docs/drivers/sonic.md §10.5): the drivers
// read the same option, so a .pub selects that one agent identity there too.
func TestFlagDriverOptions(t *testing.T) {
	if got := flagDriverOptions(""); got != nil {
		t.Errorf("no key: %v, want nil (every agent key, one at a time)", got)
	}
	if got := flagDriverOptions("  "); got != nil {
		t.Errorf("blank key: %v, want nil", got)
	}
	want := map[string]string{"ssh_key": "/home/admin/.ssh/id_ed25519.pub"}
	if got := flagDriverOptions(" /home/admin/.ssh/id_ed25519.pub "); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// options.probe defaults to the inform URL's host:port (docs/drivers/
// sonic-on-switch.md §2): the controller's address and port, or the port of
// the URL's scheme when it names none.
func TestProbeAddr(t *testing.T) {
	for in, want := range map[string]string{
		"http://192.0.2.1:8080/inform": "192.0.2.1:8080",
		"http://192.0.2.1/inform":      "192.0.2.1:80",
		"https://192.0.2.1/inform":     "192.0.2.1:443",
		"http://[2001:db8::1]:8080/":   "[2001:db8::1]:8080",
		"not a url":                    "",
	} {
		if got := probeAddr(in); got != want {
			t.Errorf("probeAddr(%q) = %q, want %q", in, got, want)
		}
	}
	u, err := informURLFor(options{controller: "192.0.2.1"})
	if err != nil || probeAddr(u) != "192.0.2.1:8080" {
		t.Errorf("controller.host 192.0.2.1: %q %v", u, err)
	}
	if _, err := informURLFor(options{}); err == nil {
		t.Error("no controller: want an error")
	}
}

// A config-file device's firmware: reaches the run as -version does: the
// key was parsed and dropped until 2026-11-13 (docs/wire-shapes.md §8).
func TestConfigFirmwareIsTheVersion(t *testing.T) {
	f := &config.File{StateDir: t.TempDir()}
	if got := configOptions(f, config.Device{Name: "sw", Firmware: "7.3.109.16640"}).version; got != "7.3.109.16640" {
		t.Errorf("firmware: gave version %q", got)
	}
	if got := configOptions(f, config.Device{Name: "sw"}).version; got != "" {
		t.Errorf("no firmware: gave version %q, want none (the model's rule decides)", got)
	}
}

// log_dir moves the reply log off the state directory (a switch's flash);
// the state stays where state_dir says.
func TestConfigLogDir(t *testing.T) {
	state, logs := t.TempDir(), t.TempDir()
	o := configOptions(&config.File{StateDir: state}, config.Device{Name: "sw"})
	if o.recordDir != filepath.Join(state, "inform-log", "sw") {
		t.Errorf("no log_dir: record dir %q, want it under the state directory", o.recordDir)
	}
	o = configOptions(&config.File{StateDir: state, LogDir: logs}, config.Device{Name: "sw"})
	if o.recordDir != filepath.Join(logs, "inform-log", "sw") || o.stateFile != filepath.Join(state, "state", "sw", "device.json") {
		t.Errorf("log_dir: record dir %q, state file %q", o.recordDir, o.stateFile)
	}
}

// A failed controller API step is retried at 15 s, doubling to five minutes;
// only the first failure of a run is worth a log line, and a success resets
// the interval.
func TestAPIRetryIntervals(t *testing.T) {
	var r apiRetry
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		got, first := r.failed()
		if got != w || first != (i == 0) {
			t.Errorf("failure %d: wait %v first %v, want %v and %v", i+1, got, first, w, i == 0)
		}
	}
	if !r.succeeded() {
		t.Error("a success after failures did not report a recovery")
	}
	if r.succeeded() {
		t.Error("a second success reported a recovery")
	}
	if got, first := r.failed(); got != 15*time.Second || !first {
		t.Errorf("after a success: wait %v first %v, want 15s and true", got, first)
	}
}
