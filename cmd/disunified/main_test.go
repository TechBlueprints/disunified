package main

import (
	"reflect"
	"testing"

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
