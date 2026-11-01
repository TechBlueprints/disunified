package device

import (
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The custom-ACL counts are claimed only by a driver that honours rules,
// so no other driver's switch_caps changes; MAC 0 is sent beside a non-zero
// IP count, as the ECS sends both.
func TestSwitchCapsACLCounts(t *testing.T) {
	sc := switchCaps(devicemodel.Capabilities{DHCPSnooping: true, IPACLs: 128})
	if sc["max_custom_ip_acls"] != 128 || sc["max_custom_mac_acls"] != 0 {
		t.Errorf("switch_caps = %v", sc)
	}
	if sc["feature_caps"].(int)&swCapDHCPSnooping == 0 {
		t.Errorf("DHCP snooping not claimed: %v", sc["feature_caps"])
	}
	for _, k := range []string{"max_custom_ip_acls", "max_custom_mac_acls", "max_global_acls", "max_acl_port_range"} {
		if _, ok := switchCaps(devicemodel.Capabilities{FEC: true})[k]; ok {
			t.Errorf("%s sent by a driver that claims no ACLs", k)
		}
	}
}
