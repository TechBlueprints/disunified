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

// SerialFor is the serial a device reports: the operator's (-serial) when
// one is named and fits in MaxSerialLen, else the MAC's 12 hex digits in
// upper case, as every real UniFi device reports. A device's own serial is
// never reported (Clint, 2026-11-12: the MAC, fleet-wide): a DMI UUID or a
// chassis serial can be longer than the controller keeps. The controller
// replaces a stored serial from the next inform (podman, 2026-11-12: 36
// characters to 12 on the first inform, still adopted).
func SerialFor(serial, mac string) string {
	if serial != "" && len(serial) <= MaxSerialLen {
		return serial
	}
	return strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(mac))
}

// MaxVersionLen is the longest version a real UniFi device reports: four
// groups of digits and dots, such as 7.2.123.16565 (the site's 42 real
// switches on Network 10.6.109, docs/wire-shapes.md §2). The controller keeps
// the reported version, and the last one as previous_firmware_version, and
// derives displayable_version from it.
const MaxVersionLen = 13

// maxVersionGroups is how many dot-separated groups a real version has.
const maxVersionGroups = 4

// WireVersion is v as it goes on the wire: digits and dots only
// (docs/wire-shapes.md §5.1). v is split on every character that is not a
// letter or digit; leading tokens that do not start with a digit are
// skipped; from each following token its leading digits are taken, until
// the first token that does not start with a digit; at most four groups are
// kept, and trailing ones dropped until the result is at most MaxVersionLen
// characters. "4.26.14M" is "4.26.14", "9.1.6" stays, a SONiC image name
// ("igmp-202511.20261106-f2e25f54c", "202505.1238526-ff3c0c9f6") is its
// branch ("202511", "202505"). Nothing is padded. A version with no digits
// to take gives "": the caller falls back to the model profile's.
func WireVersion(v string) string {
	tokens := strings.FieldsFunc(v, func(r rune) bool { return !isASCIIDigit(r) && !isASCIILetter(r) })
	var groups []string
	for _, tok := range tokens {
		n := 0
		for n < len(tok) && isASCIIDigit(rune(tok[n])) {
			n++
		}
		if n == 0 {
			if len(groups) == 0 {
				continue // a leading word ("igmp-"): skipped
			}
			break // a word after the numbers (a build hash): the end
		}
		groups = append(groups, tok[:n])
		if len(groups) == maxVersionGroups {
			break
		}
	}
	for len(groups) > 0 && len(strings.Join(groups, ".")) > MaxVersionLen {
		groups = groups[:len(groups)-1]
	}
	return strings.Join(groups, ".")
}

// VersionFits reports whether v goes on the wire as it is: an operator's
// -version must, or it is refused at start (as an over-long -serial is).
func VersionFits(v string) bool {
	return v != "" && WireVersion(v) == v
}

// reportedVersion is the version the payload carries for a device tracked
// at v (its own, an emulated upgrade, or -version): WireVersion(v), or the
// model profile's when v has no digits.
func reportedVersion(v, model string) string {
	if w := WireVersion(v); w != "" {
		return w
	}
	if p, ok := emu.Profile(model); ok {
		return WireVersion(p.Version)
	}
	return ""
}

// reportsOwnVersion says whether a device claimed as model reports its own
// version (reduced by WireVersion). Only the USW Leaf does: it has no need
// for real firmware numbers. Every other model is a UniFi product on that
// product's firmware line, so it reports UniFi firmware numbers only -- the
// model's catalogue version, then whatever the controller "upgrades" it to
// -- and never presents the device's own (Clint, 2026-11-13: a SONiC switch
// claimed as an ECS showed its image's branch, 202511, as its firmware).
// An operator's -version / firmware: still overrides either.
func reportsOwnVersion(model string) bool { return model == "UDC48X6" }

// ReportsOwnVersion is reportsOwnVersion for the command: a model that
// reports UniFi firmware numbers takes the updates the controller offers
// it without an operator's click (cmd/disunified, upgradeAsker).
func ReportsOwnVersion(model string) bool { return reportsOwnVersion(model) }

func isASCIIDigit(r rune) bool  { return r >= '0' && r <= '9' }
func isASCIILetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

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
	// The firmware version: an operator's -version first. Then, for a device
	// claimed as the USW Leaf only, the device's own ("4.26.14M", "9.1.6"),
	// so the UI's Version column says what is really running (Clint,
	// 2026-09-20). Every other model reports UniFi firmware numbers only:
	// the model profile's version (reportsOwnVersion), which is also the
	// Leaf's fallback when its driver reports none. The descriptor keeps
	// the device's own string, which the emulated-upgrade logic compares;
	// the payload carries it reduced to digits and dots (WireVersion,
	// applied in Session.buildPayload).
	version := id.Version
	if version == "" && snap != nil && reportsOwnVersion(profile.Model) {
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
