package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/drivers/podman"
)

// The Podman driver's payload (built by the real driver from a real host
// capture, docs/fixtures/podman-5.8.2/collect.txt) must meet the same
// contract as the Proxmox node's: the same model, the same gaps a Linux
// host has against a switch ASIC, plus none of the bridge's FDB.
var podmanContractOmissions = map[string]string{
	"psu_table":            "a VM/host has no PSU sensors",
	"total_max_power":      "no PSU capacity known",
	"root_switch":          "no spanning tree on a container host; no root bridge to name",
	"mac_table_capability": "no hardware FDB",
	"fan_level":            "no fans (has_fan is sent as false)",
	"fan_table":            "no fans",
	"general_temperature":  "no temperature sensor in a VM (has_temperature is sent as false)",
	"stp_priority":         "no spanning tree on a container host",
}

var podmanPortOmissions = map[string]string{
	"fec": "a virtio NIC has no forward error correction",
}

func buildPodmanContractPayload(t *testing.T) map[string]any {
	t.Helper()
	fr, err := podman.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "podman-5.8.2", "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := podman.NewCollector(fr)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ip := snap.System.Addresses[0].IP
	desc, err := DescriptorFor("UDC48X6", snap, Identity{MAC: snap.System.MAC, Serial: snap.System.Serial, IP: ip, Hostname: snap.System.Hostname, UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	desc.FWCaps = FWCapsFor(c.Capabilities())
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetCapabilities(c.Capabilities())
	sess.SetSnapshot(snap)
	sess.SetGatewayIP("192.0.2.2")
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWireContractPodman(t *testing.T) {
	ours := buildPodmanContractPayload(t)
	runContractPorts(t, "podman", ours, podmanContractOmissions, podmanPortOmissions)
	// The host is the switch: the device MAC is the uplink NIC's, and the
	// uplink is that NIC's port.
	if mac, _ := ours["mac"].(string); mac == "" || mac != ours["mac"] {
		t.Errorf("device MAC missing: %v", ours["mac"])
	}
	if up, _ := ours["uplink"].(string); up == "" {
		t.Errorf("uplink must be the interface name string, got %v", ours["uplink"])
	}
	ports, _ := ours["port_table"].([]any)
	if len(ports) != 54 {
		t.Errorf("port_table has %d rows, want 54 (the USW Leaf)", len(ports))
	}
}
