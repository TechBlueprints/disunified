package device

import (
	"fmt"
	"strings"

	emu "github.com/jamesbraid/unifi-emu"
	"github.com/jamesbraid/unifi-emu/inform"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Identity is what the operator (or the switch) supplies for the device the
// bridge presents.
type Identity struct {
	MAC, Serial, IP, Hostname string
	Version                   string // "" = the switch's own version, else the model profile's
	UDAPIVersion              string
	UplinkPort                int // 0 = the snapshot's LLDP-chosen uplink
}

// MaxSerialLen is the longest serial the controller can keep. Network 11
// moves the controller's config from MongoDB to PostgreSQL, and its device
// table holds the serial in a varchar(12): one longer record halts the
// whole migration, and UniFi OS rolls the Network app back to 10.x
// (11.0.97 on 2026-11-12, four tries, each failing on the Podman bridge's
// 36-character DMI UUID; the DX010's 22-character chassis serial was
// next). Every real UniFi device reports its MAC's 12 hex digits.
const MaxSerialLen = 12

// SerialFor is the serial a device reports: its own when it has one that
// fits in MaxSerialLen, else the MAC's 12 hex digits in upper case, as a
// real UniFi device reports.
func SerialFor(serial, mac string) string {
	if serial != "" && len(serial) <= MaxSerialLen {
		return serial
	}
	return strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(mac))
}

// DescriptorFor builds the inform.Descriptor for model (a UniFi model code
// from the catalogue) presenting snap. The port list comes from the model's
// profile; the uplink flag from id.UplinkPort or the snapshot.
func DescriptorFor(model string, snap *devicemodel.Snapshot, id Identity) (inform.Descriptor, error) {
	profile, ok := emu.Profile(model)
	if !ok {
		return inform.Descriptor{}, fmt.Errorf("unknown model %q", model)
	}
	// A switch, or a power device on the controller's power path: the
	// UPS-class models (UPS 2U Pro and kin) inform as "usp", the rack
	// PDUs and battery-backed UPS Tower/2U as "usw" (unifi-emu's
	// PROTOCOL.md, "There is no power device type"). Either way the
	// payload is the switch payload plus the power tables.
	typ := wireType(model, profile.Type)
	if typ != "usw" && typ != "usp" {
		return inform.Descriptor{}, fmt.Errorf("model %q is a %q, not a switch or power device", model, typ)
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
	// The firmware version is the switch's own ("4.26.14M", "9.1.6"): the
	// UI's Version column should say what is really running (Clint,
	// 2026-09-20). The model profile's UniFi version is the fallback for a
	// driver that reports none, and -version overrides both.
	version := id.Version
	if version == "" && snap != nil {
		version = snap.System.Version
	}
	if version == "" {
		version = profile.Version
	}
	return inform.Descriptor{
		MAC:          id.MAC,
		Serial:       SerialFor(id.Serial, id.MAC),
		Model:        profile.Model,
		ModelDisplay: profile.ModelDisplay,
		Version:      version,
		IP:           id.IP,
		Hostname:     id.Hostname,
		Type:         typ,
		FWCaps:       FWCaps,
		UDAPIVersion: id.UDAPIVersion,
		Ports:        ports,
	}, nil
}

// wireType is the type a model informs as. The pinned catalogue (unifi-emu
// v0.5.5) labels the UPS 2U Pro "usw"; Ubiquiti's public fingerprint DB and
// the newer catalogue say "usp", the controller's power path, and the
// controller resolves the family from the model string regardless -- so
// the wire must carry what the controller expects. Everything else is the
// catalogue's word.
func wireType(model, catalogue string) string {
	switch model {
	case "USPDA2B", "USPDA2C":
		return "usp"
	}
	return catalogue
}
