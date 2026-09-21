package aristaeos

import (
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/switchmodel"
)

// Shapes for the VLAN/speed state commands (EOS 4.26.14M, fixtures captured
// 2026-09-19).

type showInterfacesSwitchport struct {
	Switchports map[string]struct {
		Enabled        bool `json:"enabled"`
		SwitchportInfo struct {
			Mode                 string `json:"mode"` // trunk | access | ...
			AccessVlanID         int    `json:"accessVlanId"`
			TrunkingNativeVlanID int    `json:"trunkingNativeVlanId"`
			TrunkAllowedVlans    string `json:"trunkAllowedVlans"` // "ALL" | "NONE" | "1-2,10"
		} `json:"switchportInfo"`
	} `json:"switchports"`
}

type showVLAN struct {
	VLANs map[string]struct {
		Name    string `json:"name"`
		Dynamic bool   `json:"dynamic"`
		Status  string `json:"status"`
	} `json:"vlans"`
}

type showInterfacesTransceiverProperties struct {
	Interfaces map[string]struct {
		MediaType  string `json:"mediaType"`
		AdminSpeed struct {
			Speed  string `json:"speed"`  // "auto" | "10Gbps" | "100Gbps" | "1Gbps" ...
			Duplex string `json:"duplex"` // "auto" | "full"
		} `json:"adminSpeed"`
		XcvrCapabilities []struct {
			SpeedCapabilities struct {
				Speed string `json:"speed"`
			} `json:"speedCapabilities"`
			FECCapabilities struct {
				HostErrorCorrection string `json:"hostErrorCorrection"` // unsupported | reedSolomon | fireCode
			} `json:"fecCapabilities"`
		} `json:"xcvrCapabilities"`
	} `json:"interfaces"`
}

// speedCapsFor derives the speeds a port can run. Optics list them
// explicitly; copper and empty cages fall back to the media type.
func speedCapsFor(media switchmodel.Media, mediaType string, xcvr []struct {
	SpeedCapabilities struct {
		Speed string `json:"speed"`
	} `json:"speedCapabilities"`
	FECCapabilities struct {
		HostErrorCorrection string `json:"hostErrorCorrection"`
	} `json:"fecCapabilities"`
}) (caps []int, fec bool) {
	for _, x := range xcvr {
		if mbps := adminSpeedMbps(x.SpeedCapabilities.Speed); mbps > 0 {
			caps = append(caps, mbps)
		}
		if x.FECCapabilities.HostErrorCorrection != "" && x.FECCapabilities.HostErrorCorrection != "unsupported" {
			fec = true
		}
	}
	if len(caps) == 0 {
		switch media {
		case switchmodel.MediaCopper10G:
			caps = []int{100, 1000, 10000}
		case switchmodel.MediaCopper2G5:
			caps = []int{100, 1000, 2500}
		case switchmodel.MediaCopper1G:
			caps = []int{10, 100, 1000}
		case switchmodel.MediaQSFP28:
			caps, fec = []int{10000, 25000, 40000, 50000, 100000}, true
		case switchmodel.MediaQSFPPlus:
			caps = []int{10000, 40000}
		case switchmodel.MediaSFP28:
			caps, fec = []int{1000, 10000, 25000}, true
		case switchmodel.MediaSFPPlus:
			caps = []int{1000, 10000}
		case switchmodel.MediaSFP:
			caps = []int{1000}
		}
	}
	return uniqueSorted(caps), fec
}

// parseVLANList expands EOS's "1-2,10,1000" form. "ALL"/"NONE" return nil
// with the matching flag.
func parseVLANList(s string) (ids []int, all bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "ALL":
		return nil, true
	case "NONE", "":
		return nil, false
	}
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		for i := a; i <= b; i++ {
			ids = append(ids, i)
		}
	}
	sort.Ints(ids)
	return ids, false
}

// formatVLANList renders ids in EOS's compact form ("1-2,10").
func formatVLANList(ids []int) string {
	ids = append([]int(nil), ids...)
	sort.Ints(ids)
	var parts []string
	for i := 0; i < len(ids); {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, strconv.Itoa(ids[i])+"-"+strconv.Itoa(ids[j]))
		} else {
			parts = append(parts, strconv.Itoa(ids[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// adminSpeedMbps converts EOS's adminSpeed.speed ("10Gbps", "auto") to Mbps.
func adminSpeedMbps(s string) int {
	s = strings.TrimSuffix(strings.ToLower(s), "bps")
	switch {
	case s == "auto" || s == "":
		return 0
	case strings.HasSuffix(s, "g"):
		f, err := strconv.ParseFloat(strings.TrimSuffix(s, "g"), 64)
		if err != nil {
			return 0
		}
		return int(f * 1000)
	case strings.HasSuffix(s, "m"):
		n, _ := strconv.Atoi(strings.TrimSuffix(s, "m"))
		return n
	}
	return 0
}

// speedKeyword is the EOS `speed forced <kw>` keyword for a UniFi speed.
func speedKeyword(mbps int) string {
	switch mbps {
	case 100:
		return "100full"
	case 1000:
		return "1000full"
	case 2500:
		return "2.5gfull"
	case 5000:
		return "5gfull"
	case 10000:
		return "10gfull"
	case 25000:
		return "25gfull"
	case 40000:
		return "40gfull"
	case 50000:
		return "50gfull"
	case 100000:
		return "100gfull"
	}
	return ""
}

func applyVLANState(ports []switchmodel.Port, sp showInterfacesSwitchport, props showInterfacesTransceiverProperties) {
	byIndex := map[int]*switchmodel.Port{}
	for i := range ports {
		byIndex[ports[i].Index] = &ports[i]
	}
	laneState := map[int]map[int]switchmodel.PortVLAN{} // slot -> lane -> state
	for name, e := range sp.Switchports {
		slot, lane, ok := parseEthName(name)
		if !ok {
			continue
		}
		info := e.SwitchportInfo
		v := switchmodel.PortVLAN{Mode: info.Mode}
		switch info.Mode {
		case "access":
			v.NativeVLAN = info.AccessVlanID
		default:
			v.NativeVLAN = info.TrunkingNativeVlanID
			v.Allowed, v.AllowAll = parseVLANList(info.TrunkAllowedVlans)
		}
		if laneState[slot] == nil {
			laneState[slot] = map[int]switchmodel.PortVLAN{}
		}
		laneState[slot][lane] = v
	}
	for slot, lanes := range laneState {
		p := byIndex[slot]
		if p == nil {
			continue
		}
		// Lane 1 (or the unsplit port, lane 0) represents the cage; any
		// other lane that differs marks the cage as diverged so the next
		// apply rewrites every lane.
		first, ok := lanes[1]
		if !ok {
			first = lanes[0]
		}
		p.VLAN = first
		for lane, v := range lanes {
			if lane <= 1 {
				continue
			}
			if v.Mode != first.Mode || v.NativeVLAN != first.NativeVLAN || v.AllowAll != first.AllowAll || !equalInts(v.Allowed, first.Allowed) {
				p.LanesDiverge = true
			}
		}
	}
	for name, e := range props.Interfaces {
		slot, lane, ok := parseEthName(name)
		if !ok {
			continue
		}
		p := byIndex[slot]
		if p == nil {
			continue
		}
		if lane <= 1 {
			p.AdminSpeed = adminSpeedMbps(e.AdminSpeed.Speed)
			p.SpeedCaps, p.FECCapable = speedCapsFor(p.Media, e.MediaType, e.XcvrCapabilities)
		}
	}
	// A split cage whose lanes run different admin speeds is misconfigured
	// (EOS errdisables the odd lanes); mark it so the next apply rewrites
	// the speed on every lane.
	for name, e := range props.Interfaces {
		slot, lane, ok := parseEthName(name)
		if !ok || lane <= 1 {
			continue
		}
		if p := byIndex[slot]; p != nil && adminSpeedMbps(e.AdminSpeed.Speed) != p.AdminSpeed {
			p.LanesDiverge = true
		}
	}
	for i := range ports {
		if ports[i].SpeedCaps == nil {
			ports[i].SpeedCaps, ports[i].FECCapable = speedCapsFor(ports[i].Media, "", nil)
		}
	}
}

func vlanIDs(v showVLAN) []int {
	out := make([]int, 0, len(v.VLANs))
	for k := range v.VLANs {
		if n, err := strconv.Atoi(k); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// portChannelVLANs extracts Port-ChannelN entries from `show interfaces switchport`.
func portChannelVLANs(sp showInterfacesSwitchport) map[string]switchmodel.PortVLAN {
	out := map[string]switchmodel.PortVLAN{}
	for name, e := range sp.Switchports {
		if !strings.HasPrefix(name, "Port-Channel") {
			continue
		}
		info := e.SwitchportInfo
		v := switchmodel.PortVLAN{Mode: info.Mode}
		switch info.Mode {
		case "access":
			v.NativeVLAN = info.AccessVlanID
		default:
			v.NativeVLAN = info.TrunkingNativeVlanID
			v.Allowed, v.AllowAll = parseVLANList(info.TrunkAllowedVlans)
		}
		out[name] = v
	}
	return out
}
