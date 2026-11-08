package device

import (
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// BGP and OSPF are claimed only beside L3 routing, with the bits Network
// 10.6.109's UI tests (SWITCH_CAP_BGP in feature_caps, SWITCH_CAP_OSPF in
// feature_caps2); feature_caps2 is sent only when OSPF is claimed, so no
// other driver's switch_caps changes. The DX010 was offered as a BGP and
// an OSPF device with exactly these (2026-11-07, docs/drivers/sonic.md
// §11.14).
func TestSwitchCapsRouting(t *testing.T) {
	l3 := devicemodel.Capabilities{L3Routing: true, L3Interfaces: 64, StaticRoutes: 256}
	if sc := switchCaps(l3); sc["feature_caps"].(int)&swCapBGP != 0 || sc["feature_caps2"] != nil {
		t.Errorf("L3 alone claims %v", sc)
	}
	bgp := l3
	bgp.BGP = true
	if sc := switchCaps(bgp); sc["feature_caps"].(int)&swCapBGP != 33554432 || sc["feature_caps2"] != nil {
		t.Errorf("BGP claims %v", sc)
	}
	ospf := l3
	ospf.OSPF = true
	if sc := switchCaps(ospf); sc["feature_caps2"] != 4 {
		t.Errorf("OSPF claims %v", sc)
	}
	if sc := switchCaps(devicemodel.Capabilities{BGP: true, OSPF: true}); sc["feature_caps"].(int)&swCapBGP != 0 || sc["feature_caps2"] != nil {
		t.Errorf("BGP/OSPF without L3 routing claim %v", sc)
	}
}
