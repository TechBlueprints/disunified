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
	if len(f.Devices) != 3 || f.Devices[1].Driver != "proxmox" || f.Devices[1].SSH == "" || f.Devices[0].Name != "arista" || f.Devices[0].Driver != "arista-eos" || f.Devices[0].Username != "stu" || f.Devices[0].Password != "x" {
		t.Errorf("devices = %+v", f.Devices)
	}
	if f.Devices[0].Model != "auto" || f.Devices[0].UDAPIVersion != "1.0.0" || f.Devices[0].Control.Ports != "all" || !f.Devices[0].Control.IGMP {
		t.Errorf("device defaults = %+v", f.Devices[0])
	}
	pdu := f.Devices[2]
	if pdu.Driver != "apc-pdu" || pdu.URL != "192.0.2.20" || pdu.Control.Outlets != "all" || !pdu.Control.Address || pdu.Username != "apc" {
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
