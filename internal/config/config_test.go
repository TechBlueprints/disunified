package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExample(t *testing.T) {
	t.Setenv("STU_SWITCH_USER", "stu")
	t.Setenv("STU_SWITCH_PASS", "x")
	f, err := Load(filepath.Join("..", "..", "deploy", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Controller.Host != "192.0.2.1" || f.Controller.APIKeyEnv != "STU_UNIFI_API_KEY" || f.Controller.Site != "default" {
		t.Errorf("controller = %+v", f.Controller)
	}
	if len(f.Switches) != 2 || f.Switches[1].Driver != "proxmox" || f.Switches[1].SSH == "" || f.Switches[0].Name != "arista" || f.Switches[0].Driver != "arista-eos" || f.Switches[0].Username != "stu" || f.Switches[0].Password != "x" {
		t.Errorf("switches = %+v", f.Switches)
	}
	if f.Switches[0].Model != "auto" || f.Switches[0].UDAPIVersion != "1.0.0" || f.Switches[0].Control.Ports != "all" || !f.Switches[0].Control.IGMP {
		t.Errorf("switch defaults = %+v", f.Switches[0])
	}
}

func TestLoadRejectsMissingCredentials(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("controller:\n  host: 192.0.2.1\nswitches:\n  - name: s\n    driver: arista-eos\n    url: https://x/command-api\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("url without credentials must be rejected")
	}
}
