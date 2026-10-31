package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExample(t *testing.T) {
	t.Setenv("DUI_DEVICE_USER", "stu")
	t.Setenv("DUI_DEVICE_PASS", "x")
	t.Setenv("DUI_APC_USER", "apc")
	t.Setenv("DUI_APC_PASS", "x")
	f, err := Load(filepath.Join("..", "..", "deploy", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Controller.Host != "192.0.2.1" || f.Controller.APIKeyEnv != "DUI_UNIFI_API_KEY" || f.Controller.Site != "default" {
		t.Errorf("controller = %+v", f.Controller)
	}
	if len(f.Devices) != 6 || f.Devices[5].Driver != "apc-backups" || f.Devices[3].Driver != "apc-ups" || f.Devices[3].Auth != "none" || f.Devices[4].Driver != "podman" || f.Devices[1].Driver != "proxmox" || f.Devices[1].SSH == "" || f.Devices[0].Name != "arista" || f.Devices[0].Driver != "arista-eos" || f.Devices[0].Username != "stu" || f.Devices[0].Password != "x" {
		t.Errorf("devices = %+v", f.Devices)
	}
	if f.Devices[0].Model != "auto" || f.Devices[0].UDAPIVersion != "1.0.0" || f.Devices[0].Control.Ports != "all" {
		t.Errorf("device defaults = %+v", f.Devices[0])
	}
	pdu := f.Devices[2]
	if pdu.Driver != "apc-pdu" || pdu.URL != "192.0.2.20" || pdu.Control.Outlets != "all" || pdu.Username != "apc" {
		t.Errorf("pdu-1 = %+v", pdu)
	}
}

func TestLoadRejectsMissingCredentials(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("controller:\n  host: 192.0.2.1\ndevices:\n  - name: s\n    driver: arista-eos\n    url: https://x/command-api\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("url without credentials must be rejected")
	}
}

// The pre-rename "switches:" key still loads and lands in Devices.
func TestLoadAcceptsLegacySwitchesKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("controller:\n  host: 192.0.2.1\nswitches:\n  - name: s\n    driver: proxmox\n    ssh: root@node\n"), 0o644)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Devices) != 1 || f.Devices[0].Name != "s" || f.Devices[0].Driver != "proxmox" {
		t.Errorf("legacy switches: not folded into Devices: %+v", f.Devices)
	}
	if f.Switches != nil {
		t.Errorf("Switches should be cleared after folding, got %+v", f.Switches)
	}
}

// Using both keys at once is a config error, not a silent merge.
func TestLoadRejectsBothDevicesAndSwitches(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("controller:\n  host: 192.0.2.1\ndevices:\n  - name: a\n    driver: proxmox\n    ssh: root@a\nswitches:\n  - name: b\n    driver: proxmox\n    ssh: root@b\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("devices: and switches: together must be rejected")
	}
}

// A device with no credentials at all (Modbus TCP) says so explicitly; the
// guard for API endpoints that forgot theirs stays.
func TestURLWithAuthNoneNeedsNoCredentials(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("controller:\n  host: 192.0.2.1\ndevices:\n  - name: u\n    driver: apc-ups\n    url: 192.0.2.30\n    auth: none\n"), 0o644)
	if _, err := Load(p); err != nil {
		t.Errorf("auth: none with a url must be accepted, got %v", err)
	}
}

func TestApplyTimeout(t *testing.T) {
	load := func(v string) (*File, error) {
		p := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(p, []byte("controller:\n  host: 192.0.2.1\ndevices:\n  - name: s\n    driver: sonic\n    ssh: admin@192.0.2.40\n    control:\n      ports: all\n      apply_timeout: \""+v+"\"\n"), 0o644)
		return Load(p)
	}
	if f, err := load("2m"); err != nil || f.Devices[0].Control.ApplyTimeout != "2m" {
		t.Errorf("apply_timeout 2m: %v", err)
	}
	for _, bad := range []string{"abc", "-5s", "0s", "90"} {
		if _, err := load(bad); err == nil {
			t.Errorf("apply_timeout %q accepted", bad)
		}
	}
}

func TestAuthRejectsUnknownValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("controller:\n  host: 192.0.2.1\ndevices:\n  - name: u\n    driver: apc-ups\n    url: 192.0.2.30\n    auth: maybe\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("an unknown auth value must be rejected")
	}
}

// A config written when IGMP, NTP, syslog, the hostname, Restart, SSH keys
// and SNMP each had an opt-in still loads: those keys are ignored now that
// UniFi owns all of them on a device it controls.
func TestRetiredControlKeysStillLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("controller:\n  host: 192.0.2.1\ndevices:\n  - name: s\n    driver: sonic\n    ssh: admin@192.0.2.40\n    control:\n      ports: all\n      address: true\n      igmp: true\n      ntp: false\n      syslog: true\n      hostname: false\n      reboot: true\n      ssh_keys: true\n      snmp: false\n"), 0o644)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Devices[0].Control.Ports != "all" {
		t.Errorf("control = %+v", f.Devices[0].Control)
	}
}
