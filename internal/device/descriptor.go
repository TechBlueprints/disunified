package device

import (
	"fmt"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/inform"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// Identity is what the operator (or the switch) supplies for the device the
// bridge presents.
type Identity struct {
	MAC, Serial, IP, Hostname string
	Version                   string // "" = the model profile's
	UDAPIVersion              string
	UplinkPort                int // 0 = the snapshot's LLDP-chosen uplink
}

// DescriptorFor builds the inform.Descriptor for model (a UniFi model code
// from the catalogue) presenting snap. The port list comes from the model's
// profile; the uplink flag from id.UplinkPort or the snapshot.
func DescriptorFor(model string, snap *switchmodel.Snapshot, id Identity) (inform.Descriptor, error) {
	profile, ok := emu.Profile(model)
	if !ok {
		return inform.Descriptor{}, fmt.Errorf("unknown model %q", model)
	}
	if profile.Type != "usw" {
		return inform.Descriptor{}, fmt.Errorf("model %q is a %q, not a switch", model, profile.Type)
	}
	ports := make([]inform.Port, len(profile.Ports))
	copy(ports, profile.Ports)
	uplink := id.UplinkPort
	if uplink == 0 && snap != nil {
		uplink = snap.UplinkPort()
	}
	if uplink > 0 {
		for i := range ports {
			ports[i].IsUplink = ports[i].PortIdx == uplink
		}
	}
	version := id.Version
	if version == "" {
		version = profile.Version
	}
	return inform.Descriptor{
		MAC:          id.MAC,
		Serial:       id.Serial,
		Model:        profile.Model,
		ModelDisplay: profile.ModelDisplay,
		Version:      version,
		IP:           id.IP,
		Hostname:     id.Hostname,
		Type:         profile.Type,
		FWCaps:       FWCaps,
		UDAPIVersion: id.UDAPIVersion,
		Ports:        ports,
	}, nil
}
