package aristaeos

import (
	"strings"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// showInterfacesHardware is `show interfaces hardware`: per port, every link
// mode the port can run with the error corrections each mode allows, and
// whether the port auto-negotiates. This is the authoritative source for the
// speed_caps the bridge claims (verified on 4.26.14M: the 10GBASE-T ports
// list only 1G and 10G, and EOS rejects `speed forced 100full` on them).
type showInterfacesHardware struct {
	Interfaces map[string]struct {
		HasAutoneg bool `json:"hasAutoneg"`
		Modes      []struct {
			Link struct {
				Speed string `json:"speed"` // "1Gbps", "10Gbps", ... ; absent for the auto mode
				Auto  bool   `json:"auto"`
			} `json:"link"`
			ErrorCorrections []string `json:"errorCorrections"` // "Disabled", "ReedSolomon", "FireCode"
		} `json:"modes"`
	} `json:"interfaces"`
}

// portHardware is what the hardware table says about one EOS interface.
type portHardware struct {
	speeds  []int
	fec     bool
	autoneg bool
}

func hardwareByInterface(h showInterfacesHardware) map[string]portHardware {
	out := make(map[string]portHardware, len(h.Interfaces))
	for name, e := range h.Interfaces {
		var ph portHardware
		ph.autoneg = e.HasAutoneg
		for _, m := range e.Modes {
			if mbps := adminSpeedMbps(m.Link.Speed); mbps > 0 {
				ph.speeds = append(ph.speeds, mbps)
			}
			for _, ec := range m.ErrorCorrections {
				if strings.EqualFold(ec, "ReedSolomon") || strings.EqualFold(ec, "FireCode") {
					ph.fec = true
				}
			}
		}
		ph.speeds = uniqueSorted(ph.speeds)
		out[name] = ph
	}
	return out
}

// applyHardwareCaps sets SpeedCaps/FECCapable from the hardware table. An
// empty cage lists only the auto mode, so it keeps the media-based fallback
// that applyVLANState already filled in. A split cage's lanes list only lane
// speeds (a 25G lane cannot do 100G), but the *cage* still can — and the
// controller validates a requested speed against what we claim, so a split
// cage must keep claiming the cage speeds or it could never be joined back
// from the UI. For a folded port the claim is the union of the lanes' caps
// and the medium's table.
func applyHardwareCaps(ports []switchmodel.Port, hw map[string]portHardware) {
	for i := range ports {
		p := &ports[i]
		ph, ok := hw[p.IfName]
		if !ok && len(p.Interfaces) > 0 {
			ph, ok = hw[p.Interfaces[0]]
		}
		if !ok || len(ph.speeds) == 0 {
			continue
		}
		p.SpeedCaps = ph.speeds
		p.FECCapable = ph.fec
		if p.Lanes > 1 {
			cage, cageFEC := speedCapsFor(p.Media, "", nil)
			p.SpeedCaps = uniqueSorted(append(append([]int(nil), ph.speeds...), cage...))
			p.FECCapable = p.FECCapable || cageFEC
		}
	}
}
