package device

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Advanced snooping is claimed as the ECS Aggregation claims it in its own
// inform: IGMP_SNOOPING and IGMP_SNOOPING_ADVANCED in feature_caps and the
// same igmp_snoop_caps. Plain snooping (the Arista) keeps igmp_snoop_caps 0
// and no advanced bit, so its payload does not change.
func TestSwitchCapsIGMPAdvancedAsTheECS(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", "inform-ecs-aggregation-uswf066.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ecs struct {
		SwitchCaps struct {
			FeatureCaps   int `json:"feature_caps"`
			IGMPSnoopCaps int `json:"igmp_snoop_caps"`
		} `json:"switch_caps"`
	}
	if err := json.Unmarshal(b, &ecs); err != nil {
		t.Fatal(err)
	}
	sc := switchCaps(devicemodel.Capabilities{IGMPSnooping: true, IGMPSnoopingAdvanced: true})
	bits := swCapIGMPSnooping | swCapIGMPAdvanced
	if ecs.SwitchCaps.FeatureCaps&bits != bits || sc["feature_caps"].(int)&bits != bits {
		t.Errorf("feature_caps = %d, the ECS's %d; both need %d", sc["feature_caps"], ecs.SwitchCaps.FeatureCaps, bits)
	}
	if sc["igmp_snoop_caps"] != ecs.SwitchCaps.IGMPSnoopCaps {
		t.Errorf("igmp_snoop_caps = %v, the ECS claims %d", sc["igmp_snoop_caps"], ecs.SwitchCaps.IGMPSnoopCaps)
	}
	plain := switchCaps(devicemodel.Capabilities{IGMPSnooping: true})
	if plain["igmp_snoop_caps"] != 0 || plain["feature_caps"].(int)&swCapIGMPAdvanced != 0 {
		t.Errorf("plain snooping claims the advanced set: %v", plain)
	}
}
