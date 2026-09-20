package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechBlueprints/switch-to-unifi/internal/drivers/arista-eos"
)

// The Arista EOS driver's payload, built from the real EOS 4.26.14M
// captures (docs/fixtures/arista-eos-4.26.14M), against the contract.
func buildContractPayload(t *testing.T) map[string]any {
	t.Helper()
	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
	c := aristaeos.NewCollector(ft)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ip := "192.0.2.9"
	if len(snap.System.Addresses) > 0 {
		ip = snap.System.Addresses[0].IP // the switch's own address, so netmask resolves
	}
	desc, err := DescriptorFor("UDC48X6", snap, Identity{MAC: snap.System.MAC, Serial: snap.System.Serial, IP: ip, Hostname: "arista", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	desc.FWCaps = FWCapsFor(c.Capabilities())
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetCapabilities(c.Capabilities())
	sess.SetSnapshot(snap)
	sess.SetGatewayIP("192.0.2.1")
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWireContractArista(t *testing.T) {
	runContract(t, "arista-eos", buildContractPayload(t), nil)
}
