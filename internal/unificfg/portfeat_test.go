package unificfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The DX010's push after the UI set port 30 to isolation, a two-MAC filter,
// a 100 Mb/s egress limit and LLDP-MED off, and port 31 to isolation
// (Network 10.6.109, 2026-11-07, docs/drivers/sonic.md 11.12).
func TestParsePortFeatures(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-dx010-portfeat-1-set.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	p := c.Ports[30]
	if !p.Isolation || !p.PortSecurity || strings.Join(p.SecurityMACs, ",") != "02:00:00:00:00:01,02:00:00:00:00:02" || p.EgressKbps != 100000 || p.LLDPMED == nil || *p.LLDPMED || p.LLDPMEDNotify {
		t.Errorf("port 30 = isolation %v security %v %q egress %d lldpmed %v notify %v", p.Isolation, p.PortSecurity, p.SecurityMACs, p.EgressKbps, p.LLDPMED, p.LLDPMEDNotify)
	}
	p = c.Ports[31]
	if !p.Isolation || p.PortSecurity || len(p.SecurityMACs) != 0 || p.EgressKbps != 0 || p.LLDPMED == nil || !*p.LLDPMED {
		t.Errorf("port 31 = %+v", p)
	}
	p = c.Ports[29]
	if p.Isolation || p.PortSecurity || p.EgressKbps != 0 {
		t.Errorf("port 29 = %+v", p)
	}
	// The baseline before the claims carries none of the keys.
	b, err = os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-dx010-portfeat-0-baseline.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if p := Parse(string(b)).Ports[30]; p.LLDPMED != nil || p.Isolation || p.PortSecurity {
		t.Errorf("baseline port 30 = %+v", p)
	}
}

// Port 29 with VLAN 69 tagged and set as its Voice VLAN: the push marks the
// membership switch.vlan.<slot>.port.29.voice=enabled.
func TestParseVoiceVLAN(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-dx010-portfeat-4-voice.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	if p := c.Ports[29]; p.VoiceVLAN != 69 || len(p.TaggedVLANs) != 1 || p.TaggedVLANs[0] != 69 || p.NativeVLAN != 1 {
		t.Errorf("port 29 = voice %d tagged %v native %d", p.VoiceVLAN, p.TaggedVLANs, p.NativeVLAN)
	}
	if p := c.Ports[30]; p.VoiceVLAN != 0 {
		t.Errorf("port 30 voice %d", p.VoiceVLAN)
	}
}

// Port 28 made a Multicast Router Port (all IGMP VLANs): the push adds
// switch.vlan.<slot>.igmp_mroute_port.K=28 to each snooping VLAN.
func TestParseMulticastRouterPort(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "system_cfg-dx010-portfeat-6-mrouter.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	if g := c.IGMP[1]; len(g.RouterPorts) != 1 || g.RouterPorts[0] != 28 {
		t.Errorf("VLAN 1 router ports %v", g.RouterPorts)
	}
}
