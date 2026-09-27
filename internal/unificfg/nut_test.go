package unificfg

import (
	"os"
	"path/filepath"
	"testing"
)

// The controller's NUT Server switch, captured 2026-09-27 on a UPS 2U Pro
// claiming smart_power_caps bit 1: name "ups", port 3493, no credential.
func TestParseNUTServerCapture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106/system_cfg-ups-nutserver.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	if c.NUTServer == nil {
		t.Fatal("nutserver block not parsed")
	}
	want := NUTServer{Enabled: true, ID: "ups", Port: 3493}
	if *c.NUTServer != want {
		t.Errorf("NUTServer = %+v, want %+v", *c.NUTServer, want)
	}
	if Parse("switch.mtu=1500\n").NUTServer != nil {
		t.Error("a push without the block must leave NUTServer nil")
	}
	c = Parse("nutserver.status=enabled\nnutserver.id=rack\nnutserver.port=3494\nnutserver.credential=enabled\nnutserver.username=monuser\nnutserver.password=secretpass\n")
	if *c.NUTServer != (NUTServer{Enabled: true, ID: "rack", Port: 3494, CredentialRequired: true, Username: "monuser", Password: "secretpass"}) {
		t.Errorf("credentialed block = %+v", *c.NUTServer)
	}
}
