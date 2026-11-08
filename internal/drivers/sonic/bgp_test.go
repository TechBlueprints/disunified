package sonic

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The DX010 as captured before the BGP run (2026-11-07): bgpd runs only
// SONiC's own LOCAL_VLAN_IPV4_PREFIX.
const fixtureBGP = "../../../docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/collect-bgp.txt"

// bgpDesired is a captured push's BGP router as the loop hands it over (the
// loop's own translation is tested in internal/informloop).
func bgpDesired(t *testing.T, name string) devicemodel.DeviceDesired {
	t.Helper()
	b := ecsPush(t, name).BGP
	d := devicemodel.DeviceDesired{BGPSet: true}
	if b != nil && b.Enabled {
		d.BGPConfig = b.Config
	}
	return d
}

// The operator's file (system_cfg-dx010-bgp-1-enabled.txt) opens three of
// bgpd's objects; every line of it is loaded.
func TestSplitBGPCapturedFile(t *testing.T) {
	cfg := bgpDesired(t, "system_cfg-dx010-bgp-1-enabled.txt").BGPConfig
	f := splitBGP(cfg, map[string]bool{"ip prefix-list LOCAL_VLAN_IPV4_PREFIX": true}, nil)
	if got := strings.Join(f.objects, "|"); got != "router bgp 65010|ip prefix-list DUI-TEST-PL|route-map DUI-TEST-IN" {
		t.Errorf("objects = %s", got)
	}
	if len(f.notes) != 0 || f.sum == "" {
		t.Errorf("notes %v, sum %q", f.notes, f.sum)
	}
	for _, line := range strings.Split(strings.TrimSpace(cfg), "\n") {
		if !strings.Contains(f.text, line+"\n") {
			t.Errorf("the loaded file lacks %q", line)
		}
	}

	// The same file naming SONiC's own prefix-list: that stanza is left
	// out, the rest is loaded, and the switch's object is never owned.
	own := strings.ReplaceAll(cfg, "DUI-TEST-PL", "LOCAL_VLAN_IPV4_PREFIX")
	f = splitBGP(own, map[string]bool{"ip prefix-list LOCAL_VLAN_IPV4_PREFIX": true}, nil)
	if strings.Contains(strings.Join(f.objects, "|"), "LOCAL_VLAN") || strings.Contains(f.text, "ip prefix-list LOCAL_VLAN_IPV4_PREFIX seq") {
		t.Errorf("SONiC's prefix-list was taken: %v\n%s", f.objects, f.text)
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "the switch's own") {
		t.Errorf("notes = %v", f.notes)
	}

	// A stanza for another daemon or for FRR itself is left out with a
	// note naming its first words only (a password line never reaches the
	// log).
	other := "hostname dx\npassword s3cret\ninterface Vlan3999\n ip ospf cost 5\n" + cfg
	f = splitBGP(other, nil, nil)
	if strings.Contains(f.text, "s3cret") || strings.Contains(f.text, "interface") || strings.Contains(f.text, "ospf") {
		t.Errorf("a non-BGP stanza was loaded:\n%s", f.text)
	}
	if got := strings.Join(f.notes, "; "); strings.Contains(got, "s3cret") || !strings.Contains(got, `"interface Vlan3999"`) {
		t.Errorf("notes = %s", got)
	}
}

// The live run of 2026-11-07, step by step: the file loaded, nothing on the
// next cycle, loaded again (no removal) once bgpd lost it, replaced whole
// when the operator changed it, removed when the router was switched off.
func TestBGPApplyReconcileReplaceRemove(t *testing.T) {
	r := rig20261106(t, fixtureBGP)
	ctx := context.Background()
	apply := func(step string, d devicemodel.DeviceDesired) []string {
		t.Helper()
		if _, err := r.c.Collect(ctx); err != nil {
			t.Fatal(err)
		}
		r.fr.Commands, r.fr.BGPFiles = nil, nil
		if _, err := r.c.ApplyDevice(ctx, d); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		return r.fr.BGPFiles
	}
	quiet := func(step string, d devicemodel.DeviceDesired) {
		t.Helper()
		if got := apply(step, d); len(got) != 0 || len(r.fr.Commands) != 0 {
			t.Errorf("the cycle after %s wrote %q", step, r.fr.Commands)
		}
	}
	on := bgpDesired(t, "system_cfg-dx010-bgp-1-enabled.txt")
	quiet("a push without BGP", devicemodel.DeviceDesired{BGPSet: true})

	got := apply("the file", on)
	if len(got) != 1 || !strings.HasPrefix(got[0], "router bgp 65010\n bgp router-id 198.51.100.1\n") || strings.Contains(got[0], "\nno ") {
		t.Fatalf("the first load sent %q", got)
	}
	if len(r.fr.Commands) != 1 || r.fr.Commands[0] != bgpCommand {
		t.Errorf("commands = %q", r.fr.Commands)
	}
	quiet("the file", on)

	// The bgp container restarted: bgpd runs SONiC's own objects only.
	r.fr.UseSection("frr_bgp", "ip prefix-list LOCAL_VLAN_IPV4_PREFIX")
	got = apply("the restart", on)
	if len(got) != 1 || strings.HasPrefix(got[0], "no ") || !strings.HasPrefix(got[0], "router bgp 65010\n") {
		t.Fatalf("the restore sent %q", got)
	}
	quiet("the restore", on)

	got = apply("the changed file", bgpDesired(t, "system_cfg-dx010-bgp-3-changed.txt"))
	if len(got) != 1 || !strings.HasPrefix(got[0], "no router bgp 65010\nno ip prefix-list DUI-TEST-PL\nno route-map DUI-TEST-IN\nrouter bgp 65010\n") ||
		!strings.Contains(got[0], "\n neighbor 198.51.100.2 timers 10 30\n") {
		t.Fatalf("the replace sent %q", got)
	}
	quiet("the changed file", bgpDesired(t, "system_cfg-dx010-bgp-3-changed.txt"))

	got = apply("the router switched off", bgpDesired(t, "system_cfg-dx010-bgp-2-disabled.txt"))
	if len(got) != 1 || got[0] != "no router bgp 65010\nno ip prefix-list DUI-TEST-PL\nno route-map DUI-TEST-IN\n" {
		t.Fatalf("the removal sent %q", got)
	}
	if !r.c.managedBGP.empty() {
		t.Errorf("the record kept %+v", r.c.managedBGP)
	}
	quiet("the removal", bgpDesired(t, "system_cfg-dx010-bgp-4-removed.txt"))
	if strings.Contains(r.log.String(), "LOCAL_VLAN") {
		t.Errorf("SONiC's own prefix-list was touched:\n%s", r.log.String())
	}
}

// vtysh refused part of the file: the record keeps it, the log names the
// line by number only, and the reconcile does not load it again every
// cycle.
func TestBGPRefusedFileIsNotRetried(t *testing.T) {
	r := rig20261106(t, fixtureBGP)
	ctx := context.Background()
	r.fr.BGPRefuse = "line 5: % Unknown command[4]:  neighbor 198.51.100.2 password hunter2"
	on := bgpDesired(t, "system_cfg-dx010-bgp-1-enabled.txt")
	if _, err := r.c.ApplyDevice(ctx, on); err != nil {
		t.Fatal(err)
	}
	if len(r.fr.BGPFiles) != 1 || r.c.managedBGP.Failed == "" {
		t.Fatalf("files %d, record %+v", len(r.fr.BGPFiles), r.c.managedBGP)
	}
	if s := r.log.String(); !strings.Contains(s, "at line 5") || strings.Contains(s, "hunter2") {
		t.Errorf("log:\n%s", s)
	}
	if _, err := r.c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	r.fr.BGPFiles = nil
	if _, err := r.c.ApplyDevice(ctx, on); err != nil || len(r.fr.BGPFiles) != 0 {
		t.Errorf("the refused file was loaded again (%v)", err)
	}
}
