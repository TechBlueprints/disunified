package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	emu "github.com/jamesbraid/unifi-emu"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	apcbackups "github.com/TechBlueprints/disunified/internal/drivers/apc-backups"
	apcpdu "github.com/TechBlueprints/disunified/internal/drivers/apc-pdu"
	apcups "github.com/TechBlueprints/disunified/internal/drivers/apc-ups"
	aristaeos "github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
	"github.com/TechBlueprints/disunified/internal/drivers/podman"
	"github.com/TechBlueprints/disunified/internal/drivers/proxmox"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// The version on the wire (docs/wire-shapes.md §5): a real UniFi device
// reports digits and dots, at most four groups and 13 characters. A device
// claimed as the USW Leaf reports its own version reduced to that, and keeps
// its own string for everything the emulated upgrade compares; every other
// model reports UniFi firmware numbers only (reportsOwnVersion).

func fixtureDir(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "docs", "fixtures"}, parts...)...)
}

// versionCase is one driver's snapshot, collected by the real driver from
// its committed capture, and the model it is claimed as.
type versionCase struct {
	name, model string
	snap        *devicemodel.Snapshot
}

func startCollector(t *testing.T, name string, start func() (*devicemodel.Snapshot, error)) *devicemodel.Snapshot {
	t.Helper()
	snap, err := start()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return snap
}

// driverVersionCases collects every driver's snapshot from its real capture,
// the way the contract_*_test.go builders do.
func driverVersionCases(t *testing.T) []versionCase {
	t.Helper()
	ctx := context.Background()
	var out []versionCase

	arista := startCollector(t, "arista-eos", func() (*devicemodel.Snapshot, error) {
		return aristaeos.NewCollector(aristaeos.NewFixtureTransport(fixtureDir("arista-eos-4.26.14M"))).Start(ctx)
	})
	out = append(out, versionCase{"arista-eos", "UDC48X6", arista})

	pve := startCollector(t, "proxmox", func() (*devicemodel.Snapshot, error) {
		fr, err := proxmox.NewFixtureRunner(fixtureDir("proxmox-9.1.6", "collect-node2.txt"))
		if err != nil {
			return nil, err
		}
		c := proxmox.NewCollector(fr)
		c.ManageLLDP = false
		return c.Start(ctx)
	})
	out = append(out, versionCase{"proxmox", "USWF07D", pve})

	pod := startCollector(t, "podman", func() (*devicemodel.Snapshot, error) {
		fr, err := podman.NewFixtureRunner(fixtureDir("podman-5.8.2", "collect.txt"))
		if err != nil {
			return nil, err
		}
		return podman.NewCollector(fr).Start(ctx)
	})
	out = append(out, versionCase{"podman", "UDC48X6", pod})

	pdu := startCollector(t, "apc-pdu", func() (*devicemodel.Snapshot, error) {
		fr, err := apcpdu.NewFixtureRunner(fixtureDir("apc-aos-3.9.2"))
		if err != nil {
			return nil, err
		}
		return apcpdu.NewCollector(fr).Start(ctx)
	})
	out = append(out, versionCase{"apc-pdu", "USPPDUP", pdu})

	ups := startCollector(t, "apc-ups", func() (*devicemodel.Snapshot, error) {
		fr, err := apcups.NewFixtureRunner(fixtureDir("apc-smtl-15.5"))
		if err != nil {
			return nil, err
		}
		c := apcups.NewCollector(fr)
		c.Addr, c.MAC, c.Netmask, c.GatewayMAC = "192.0.2.30:502", "02:00:00:00:00:02", "255.255.255.0", "02:00:00:00:00:fe"
		return c.Start(ctx)
	})
	out = append(out, versionCase{"apc-ups", "USPDA2B", ups})

	backups := startCollector(t, "apc-backups", func() (*devicemodel.Snapshot, error) {
		fr, err := apcbackups.NewFixtureRunner(fixtureDir("apc-backups-05.3"))
		if err != nil {
			return nil, err
		}
		c := apcbackups.NewCollector(fr)
		c.Addr, c.MAC, c.Netmask, c.GatewayMAC = "192.0.2.30", "02:00:00:00:00:01", "255.255.255.0", "02:00:00:00:00:fe"
		return c.Start(ctx)
	})
	out = append(out, versionCase{"apc-backups", "USPDA2B", backups})

	// The SONiC captures: the virtual-switch guest, the DX010 on its first
	// image (the guest's commit two builds later, so the guest's help texts
	// stand in, as in contract_sonic_hw_test.go), and the DX010 on the repo's
	// 202511 image, whose capture directory is complete on its own.
	for _, capture := range []string{viaVS + "/collect.txt", "sonic-202505.1238526-ff3c0c9f6/collect.txt"} {
		_, snap := sonicCaptureSnapshot(t, capture)
		out = append(out, versionCase{"sonic " + capture, "USWF07D", snap})
	}
	igmp := startCollector(t, "sonic 202511", func() (*devicemodel.Snapshot, error) {
		fr, err := sonic.NewFixtureRunner(fixtureDir("sonic-igmp-202511.20261106-f2e25f54c", "collect.txt"))
		if err != nil {
			return nil, err
		}
		return sonic.NewCollector(fr).Start(ctx)
	})
	out = append(out, versionCase{"sonic sonic-igmp-202511.20261106-f2e25f54c/collect.txt", "USWF07D", igmp})
	return out
}

// wantWire is what each driver's captured version becomes on the wire. The
// device's own strings are the captures'; the results are the rule's
// (docs/wire-shapes.md §5.1 gives the EOS, PVE and both SONiC ones).
var wantWire = map[string][2]string{ // name -> {device's own, on the wire}
	"arista-eos":                      {"4.26.14M", "4.26.14"},
	"proxmox":                         {"9.1.6", "9.1.6"},
	"podman":                          {"5.8.2", "5.8.2"},
	"apc-pdu":                         {"3.9.2", "3.9.2"},
	"apc-ups":                         {"15.5", "15.5"},
	"apc-backups":                     {"05.3", "05.3"},
	"sonic " + viaVS + "/collect.txt": {"202505.1238524-ff3c0c9f6", "202505"},
	"sonic sonic-202505.1238526-ff3c0c9f6/collect.txt":       {"202505.1238526-ff3c0c9f6", "202505"},
	"sonic sonic-igmp-202511.20261106-f2e25f54c/collect.txt": {"igmp-202511.20261106-f2e25f54c", "202511"},
}

// payloadFor is the payload a fresh adopted session sends for snap.
func payloadFor(t *testing.T, s *Session) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(s.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func sessionFor(t *testing.T, vc versionCase, st State) *Session {
	t.Helper()
	desc, err := DescriptorFor(vc.model, vc.snap, Identity{MAC: vc.snap.System.MAC, IP: "192.0.2.9", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Key == "" {
		st.Key, st.Adopted, st.UseAESGCM, st.CfgVersion = "0123456789abcdef0123456789abcdef", true, true, "abc"
	}
	s := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	s.SetSnapshot(vc.snap)
	return s
}

func TestWireVersionPerDriver(t *testing.T) {
	cases := driverVersionCases(t)
	if len(cases) != len(wantWire) {
		t.Fatalf("%d driver captures, %d expectations", len(cases), len(wantWire))
	}
	for _, vc := range cases {
		want, ok := wantWire[vc.name]
		if !ok {
			t.Errorf("%s: no expectation", vc.name)
			continue
		}
		own := vc.snap.System.Version
		if own != want[0] {
			t.Errorf("%s: the capture's version is %q, want %q", vc.name, own, want[0])
		}
		if got := WireVersion(own); got != want[1] {
			t.Errorf("%s: WireVersion(%q) = %q, want %q", vc.name, own, got, want[1])
		}
		// Only a device claimed as the USW Leaf sends its own version; every
		// other model sends its catalogue's, and never the device's own.
		wantSent, wantTracked := want[1], own
		if !reportsOwnVersion(vc.model) {
			wantSent = mustProfileVersion(t, vc.model)
			wantTracked = wantSent
		}
		s := sessionFor(t, vc, State{})
		m := payloadFor(t, s)
		v, _ := m["version"].(string)
		if v != wantSent || len(v) > MaxVersionLen || !VersionFits(v) {
			t.Errorf("%s (%s): payload version %q, want %q", vc.name, vc.model, v, wantSent)
		}
		if dv, _ := m["displayable_version"].(string); dv != displayableVersion(v) {
			t.Errorf("%s: displayable_version %q, want it derived from %q", vc.name, dv, v)
		}
		// The Leaf's own string stays where the emulated upgrade compares it.
		if s.Version() != wantTracked {
			t.Errorf("%s (%s): tracked version %q, want %q", vc.name, vc.model, s.Version(), wantTracked)
		}
		if s.ReportedVersion() != v {
			t.Errorf("%s: ReportedVersion %q, payload %q", vc.name, s.ReportedVersion(), v)
		}
	}
}

// The rule as docs/wire-shapes.md §5.1 states it, on the spec's examples
// (each a capture's string) and on the edges it names.
func TestWireVersionRule(t *testing.T) {
	for in, want := range map[string]string{
		"4.26.14M":                       "4.26.14",
		"9.1.6":                          "9.1.6",
		"igmp-202511.20261106-f2e25f54c": "202511", // with the date, 15 characters
		"202505.1238526-ff3c0c9f6":       "202505", // with the build, 14
		"7.2.123.16565":                  "7.2.123.16565",
		"4.0.1.995.1":                    "4.0.1.995", // at most four groups
		"":                               "",
		"unknown":                        "", // no digits: the profile's, below
	} {
		if got := WireVersion(in); got != want {
			t.Errorf("WireVersion(%q) = %q, want %q", in, got, want)
		}
	}
	p, _ := emu.Profile("UDC48X6")
	if got := reportedVersion("unknown", "UDC48X6"); got != p.Version {
		t.Errorf("a version with no digits = %q, want the profile's %q", got, p.Version)
	}
	for v, fits := range map[string]bool{
		"7.3.109.16640": true, "4.26.14": true, "4.26.14M": false, "9.9.9": true,
		"202505.1238526-ff3c0c9f6": false, "": false, "1.2.3.4.5": false, "12345.12345.12": false,
	} {
		if VersionFits(v) != fits {
			t.Errorf("VersionFits(%q) = %v", v, !fits)
		}
	}
}

// An adopted device that the controller "upgraded" under the old build has
// State.Firmware (the target) and State.FirmwareBase (the device's own string
// at the time, "4.26.14M" -- not its wire form). Restarted on this build with
// the same device version, it must still report the target; restarted with
// no emulated upgrade, it must not be thrown into one; and the device's own
// version moving still drops the fiction.
func TestEmulatedUpgradeAcrossTheWireVersion(t *testing.T) {
	cases := driverVersionCases(t)
	byName := map[string]versionCase{}
	for _, vc := range cases {
		byName[vc.name] = vc
	}
	arista := byName["arista-eos"]
	target := mustProfileVersion(t, arista.model) // the controller offers the catalogue's firmware

	dir := t.TempDir()
	store := &Store{Path: filepath.Join(dir, "device.json")}
	old := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc",
		Firmware: target, FirmwareBase: arista.snap.System.Version}
	if err := store.Save(old); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := sessionFor(t, arista, st)
	if v := payloadFor(t, s)["version"]; v != target {
		t.Errorf("restart with a persisted emulated upgrade reports %v, want the target %q", v, target)
	}
	if s.Version() != target {
		t.Errorf("tracked version %q, want %q", s.Version(), target)
	}

	// No emulated upgrade: the device's own version, reduced; no fiction.
	fresh := sessionFor(t, arista, State{})
	if v := payloadFor(t, fresh)["version"]; v != "4.26.14" {
		t.Errorf("no emulated upgrade: version %v", v)
	}
	if fresh.st.Firmware != "" || fresh.st.FirmwareBase != "" {
		t.Errorf("no emulated upgrade, yet State has %q over %q", fresh.st.Firmware, fresh.st.FirmwareBase)
	}

	// An upgrade the controller requests now records the device's own string
	// as the base, as the old build did, so the two builds' state agrees.
	up := sessionFor(t, arista, State{})
	up.Apply(time.Now(), []byte(`{"_type":"upgrade","version":"`+target+`"}`))
	if up.st.Firmware != target || up.st.FirmwareBase != "4.26.14M" {
		t.Errorf("after an upgrade: Firmware %q over %q, want %q over the device's own", up.st.Firmware, up.st.FirmwareBase, target)
	}

	// The Leaf's own version moving drops the fiction, while running and
	// across a restart.
	moved := *arista.snap
	moved.System.Version = "4.26.15M"
	running := sessionFor(t, arista, st)
	running.SetSnapshot(&moved)
	if v := payloadFor(t, running)["version"]; v != "4.26.15" || running.st.Firmware != "" {
		t.Errorf("leaf, version moved while running: %v (Firmware %q), want 4.26.15 and no fiction", v, running.st.Firmware)
	}
	movedCase := arista
	movedCase.snap = &moved
	restartedLeaf := sessionFor(t, movedCase, st)
	if v := payloadFor(t, restartedLeaf)["version"]; v != "4.26.15" || restartedLeaf.st.Firmware != "" {
		t.Errorf("leaf, version moved across a restart: %v (Firmware %q), want 4.26.15 and no fiction", v, restartedLeaf.st.Firmware)
	}
}

// A device claimed as anything but the USW Leaf presents UniFi firmware
// numbers only (Clint, 2026-11-13): its catalogue's version until the
// controller "upgrades" it, the target from then on, and never its own --
// whatever image it runs, and whatever base an older build recorded.
func TestUniFiVersionsOnlyOffTheLeaf(t *testing.T) {
	byName := map[string]versionCase{}
	for _, vc := range driverVersionCases(t) {
		byName[vc.name] = vc
	}
	vs := byName["sonic "+viaVS+"/collect.txt"]
	dx := byName["sonic sonic-202505.1238526-ff3c0c9f6/collect.txt"]
	igmp := byName["sonic sonic-igmp-202511.20261106-f2e25f54c/collect.txt"]
	catalogue := mustProfileVersion(t, vs.model)

	// No upgrade yet: the catalogue's version, on every image.
	for _, vc := range []versionCase{vs, dx, igmp} {
		s := sessionFor(t, vc, State{})
		if v := payloadFor(t, s)["version"]; v != catalogue {
			t.Errorf("%s: version %v, want the catalogue's %q", vc.name, v, catalogue)
		}
	}

	// The state the deployed DX010 holds: an emulated upgrade recorded by an
	// older build over the image's own name. It stands, and the base goes.
	const target = "4.0.1.995"
	old := State{Firmware: target, FirmwareBase: igmp.snap.System.Version}
	s := sessionFor(t, igmp, old)
	if v := payloadFor(t, s)["version"]; v != target || s.st.Firmware != target || s.st.FirmwareBase != "" {
		t.Errorf("an older build's upgrade: version %v, Firmware %q over %q; want %q over nothing", v, s.st.Firmware, s.st.FirmwareBase, target)
	}
	// The image moving under a running bridge, or across a restart, changes
	// nothing.
	s.SetSnapshot(dx.snap)
	if v := payloadFor(t, s)["version"]; v != target || s.st.Firmware != target {
		t.Errorf("image moved while running: version %v (Firmware %q), want %q kept", v, s.st.Firmware, target)
	}
	if v := payloadFor(t, sessionFor(t, dx, old))["version"]; v != target {
		t.Errorf("image moved across a restart: version %v, want %q kept", v, target)
	}
	// An upgrade the controller requests now records no base.
	up := sessionFor(t, dx, State{})
	up.Apply(time.Now(), []byte(`{"_type":"upgrade","version":"`+target+`"}`))
	if v := payloadFor(t, up)["version"]; v != target || up.st.Firmware != target || up.st.FirmwareBase != "" {
		t.Errorf("after an upgrade: version %v, Firmware %q over %q; want %q over nothing", v, up.st.Firmware, up.st.FirmwareBase, target)
	}
	// Nothing in the payload carries the image's name, whole or reduced.
	m := payloadFor(t, up)
	if raw := string(up.BuildPayload(time.Now())); strings.Contains(raw, dx.snap.System.Version) {
		t.Errorf("the payload carries the device's own version %q", dx.snap.System.Version)
	}
	for _, k := range []string{"version", "displayable_version", "required_version"} {
		if v, _ := m[k].(string); strings.HasPrefix(v, WireVersion(dx.snap.System.Version)) {
			t.Errorf("%s is %q, the device's own version reduced", k, v)
		}
	}
}

func mustProfileVersion(t *testing.T, model string) string {
	t.Helper()
	p, ok := emu.Profile(model)
	if !ok || !VersionFits(p.Version) {
		t.Fatalf("model %s: profile version %q", model, p.Version)
	}
	return p.Version
}
