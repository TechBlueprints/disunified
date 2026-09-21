package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/drivers/proxmox"
)

// The Proxmox driver's payload (built by the real driver from a real node
// capture, docs/fixtures/proxmox-9.1.6/collect-node2.txt) must meet the same
// contract. A Linux bridge lacks some of what a switch ASIC reports; each
// gap is listed with its reason.
var proxmoxContractOmissions = map[string]string{
	"psu_table":            "a workstation-class node has no PSU sensors (no BMC)",
	"total_max_power":      "no PSU capacity known",
	"root_switch":          "STP is off on a Proxmox bridge; no root bridge to name",
	"mac_table_capability": "a Linux bridge's FDB is a hash table with no fixed capacity to report",
}

func buildProxmoxContractPayload(t *testing.T) map[string]any {
	t.Helper()
	fr, err := proxmox.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "proxmox-9.1.6", "collect-node2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := proxmox.NewCollector(fr)
	c.ManageLLDP = false
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

func TestWireContractProxmox(t *testing.T) {
	ours := buildProxmoxContractPayload(t)
	runContract(t, "proxmox", ours, proxmoxContractOmissions)
	// The node is the switch: the device MAC is the bridge's.
	if mac, _ := ours["mac"].(string); mac != "02:00:00:00:00:01" {
		t.Errorf("the device MAC must be the node's bridge MAC, got %s", mac)
	}
}
