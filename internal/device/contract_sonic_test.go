package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// The SONiC driver's payload (built by the real driver from a real capture
// of the virtual switch, docs/fixtures/sonic-202505.1238524-ff3c0c9f6/
// collect.txt) must meet the same contract, claimed as the ECS Core. The
// gaps are the VS's: pmon (fans, PSUs, temperatures) is empty without
// hardware, no STP is configured on the L2 preset, and the software ASIC
// has no FEC.
var sonicContractOmissions = map[string]string{
	"psu_table":            "pmon is empty on the virtual switch (hardware fills PSU_INFO)",
	"total_max_power":      "no PSU capacity known on the virtual switch",
	"root_switch":          "spanning tree is not configured on the L2 preset; no root bridge to name",
	"stp_priority":         "spanning tree is not configured on the L2 preset",
	"mac_table_capability": "SONiC publishes no FDB capacity in the tables the driver reads",
	"fan_level":            "pmon is empty on the virtual switch (has_fan is sent as false)",
	"fan_table":            "pmon is empty on the virtual switch",
	"general_temperature":  "no temperature sensor on the virtual switch (has_temperature is sent as false)",
}

var sonicPortOmissions = map[string]string{
	"fec": "the virtual switch reports FEC N/A (hardware reports rs/fc/none)",
}

// sonicDownPortOmissions: the references' down port has an optic seated;
// every down cage on the VS is empty, and the fault bits are sent only with
// an optic present (the up port carries them and is checked as the uplink).
var sonicDownPortOmissions = map[string]string{
	"sfp_rxfault": "every down cage on the virtual switch is empty; sent only with an optic seated",
	"sfp_txfault": "every down cage on the virtual switch is empty; sent only with an optic seated",
}

// runContractSonic is runContractPorts with omissions for the down port
// alone (an empty cage against the references' seated one).
func runContractSonic(t *testing.T, ours map[string]any) {
	t.Helper()
	merge := func(ms ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range ms {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	omit := merge(contractOmissions, switchOmissions, sonicContractOmissions)
	portOmit := merge(portOmissions, sonicPortOmissions)
	downOmit := merge(portOmit, sonicDownPortOmissions)
	for _, ref := range []string{"inform-ecs-aggregation-uswf066.json", "inform-usw-xg16-usxg.json"} {
		real := loadReference(t, ref)
		compareKeys(t, "sonic "+ref+" device", real, ours, omit)
		compareKeys(t, "sonic "+ref+" uplink port", uplinkPort(t, real), uplinkPort(t, ours), portOmit)
		rd := downPort(t, real, false)
		_, optical := rd["sfp_found"]
		compareKeys(t, "sonic "+ref+" down port", rd, downPort(t, ours, optical), downOmit)
	}
	if u, ok := ours["uplink"].(string); !ok || u == "" {
		t.Errorf("sonic: uplink must be the management interface name (a string), got %v", ours["uplink"])
	}
}

func buildSonicContractPayload(t *testing.T) map[string]any {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238524-ff3c0c9f6", "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	c := sonic.NewCollector(fr)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ip := snap.System.Addresses[0].IP
	desc, err := DescriptorFor("USWF07D", snap, Identity{MAC: snap.System.MAC, Serial: snap.System.Serial, IP: ip, Hostname: snap.System.Hostname, UDAPIVersion: "1.0.0"})
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

func TestWireContractSonic(t *testing.T) {
	ours := buildSonicContractPayload(t)
	runContractSonic(t, ours)
	// The device MAC is the switch's system MAC (DEVICE_METADATA), not eth0's.
	if mac, _ := ours["mac"].(string); mac != "02:00:00:00:00:01" {
		t.Errorf("the device MAC must be the system MAC, got %v", ours["mac"])
	}
	ports, _ := ours["port_table"].([]any)
	if len(ports) != 32 {
		t.Errorf("port_table has %d rows, want 32 (the ECS Core)", len(ports))
	}
	first, _ := ports[0].(map[string]any)
	if macs, _ := first["mac_table"].([]any); len(macs) != 132 {
		t.Errorf("port 1 reports %d MACs, want the 132 learned", len(macs))
	}
}
