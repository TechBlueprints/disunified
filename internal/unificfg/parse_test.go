package unificfg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCapturedSystemCfg(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106-system_cfg.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	if len(c.Ports) != 54 {
		t.Fatalf("parsed %d ports, want 54", len(c.Ports))
	}
	if p := c.Ports[2]; p.Enabled || p.Name != "SFP28 2" || p.OpMode != "switch" {
		t.Errorf("port 2 = %+v (the capture has it disabled)", p)
	}
	if p := c.Ports[1]; !p.Enabled || p.Name != "SFP28 1" {
		t.Errorf("port 1 = %+v", p)
	}
	if p := c.Ports[49]; !p.Enabled || p.Name != "QSFP28 1" {
		t.Errorf("port 49 = %+v", p)
	}
	if len(c.VLANs) != 10 || c.VLANs[0].ID != 1 || c.VLANs[0].Mode != "untagged" || c.VLANs[9].ID != 4000 || c.VLANs[9].Mode != "tagged" {
		t.Errorf("vlans = %+v", c.VLANs)
	}
	if c.MTU != 9216 {
		t.Errorf("mtu = %d", c.MTU)
	}
	if c.Raw["unifi.version"] != "10.6.106" {
		t.Errorf("raw unifi.version = %q", c.Raw["unifi.version"])
	}
	if idx := c.PortIndexes(); idx[0] != 1 || idx[53] != 54 {
		t.Errorf("port indexes = %v", idx)
	}
}

func TestParseDefaults(t *testing.T) {
	c := Parse("switch.port.7.name=x\r\n# comment\nbogus line\nswitch.port.7.status=enabled\n")
	if p := c.Ports[7]; !p.Enabled || p.Name != "x" {
		t.Errorf("port 7 = %+v", p)
	}
}

func TestParsePort2Capture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106-system_cfg-port2-name-speed-vlans.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	p := c.Ports[2]
	if p.Name != "stu capture" || p.AutoNeg || p.SpeedMbps != 10000 || !p.FullDuplex {
		t.Errorf("port 2 = %+v", p)
	}
	if !p.VLANExplicit || p.NativeVLAN != 1 || len(p.TaggedVLANs) != 2 || p.TaggedVLANs[0] != 2 || p.TaggedVLANs[1] != 10 {
		t.Errorf("port 2 vlans = %+v", p)
	}
	if q := c.Ports[1]; q.VLANExplicit || !q.AutoNeg {
		t.Errorf("port 1 (untouched) = %+v", q)
	}
	if ids := c.VLANIDs(); len(ids) != 10 || ids[0] != 1 || ids[9] != 4000 {
		t.Errorf("vlan ids = %v", ids)
	}
}

func TestParsePort54FeatureCapture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106-system_cfg-port54-fec-storm-bpdu-caps.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := Parse(string(b))
	p := c.Ports[54]
	if p.FEC != "cl-91" || p.AutoNeg || p.SpeedMbps != 100000 || !p.BPDUGuard || !p.STPPortMode {
		t.Errorf("port 54 = %+v", p)
	}
	if !p.StormCtrl.Enabled || p.StormCtrl.Type != "level" || p.StormCtrl.Bcast != 5 || p.StormCtrl.Mcast != 1 || p.StormCtrl.Ucast != -1 {
		t.Errorf("port 54 storm = %+v", p.StormCtrl)
	}
	if p.LLDPMED == nil || *p.LLDPMED {
		t.Errorf("port 54 lldpmed = %v", p.LLDPMED)
	}
	if q := c.Ports[1]; q.FEC != "" || q.BPDUGuard || q.StormCtrl.Enabled || q.LLDPMED == nil || !*q.LLDPMED {
		t.Errorf("port 1 = %+v", q)
	}
	if !c.STP.Set || !c.STP.Enabled || c.STP.Version != "rstp" || c.STP.Priority != 32768 {
		t.Errorf("stp = %+v", c.STP)
	}
	if len(c.IGMPSnooping) != 3 || !c.IGMPSnooping[1] || !c.IGMPSnooping[2] || !c.IGMPSnooping[69] {
		t.Errorf("igmp = %v", c.IGMPSnooping)
	}
}
