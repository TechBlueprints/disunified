package sonic

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// newHardwareRig is newRig over the DX010's capture (§10.7), with the VS
// capture's help texts and zone list (the same commit).
func newHardwareRig(t *testing.T, edit func(*FixtureRunner)) *controlRig {
	t.Helper()
	fr, err := NewFixtureRunner(hwFixture)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fr.Help, fr.Zones = vs.Help, vs.Zones
	if edit != nil {
		edit(fr)
	}
	var buf strings.Builder
	c := NewCollector(fr)
	c.Log = log.New(&buf, "", 0)
	c.Dial = ifaceAddr(t, fr.Fixture, "Vlan4094")
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &controlRig{t: t, c: c, fr: fr, log: &buf}
}

// factorySNMPYml puts the factory /etc/sonic/snmp.yml in place of the
// unit's (fixed by hand on the day): snmp_yml-factory.txt is collect.py's
// section over the unit's backup of the factory file, scrubbed
// (snmp_rocommunity: community-1, `public` before the scrub). With boot
// true the snmp container has started on it, as at every boot.
func factorySNMPYml(t *testing.T, boot bool) func(*FixtureRunner) {
	return func(fr *FixtureRunner) {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(hwFixture), "snmp_yml-factory.txt"))
		if err != nil {
			t.Fatal(err)
		}
		fr.UseSection("snmp_yml", sections(string(b))["snmp_yml"])
		if boot {
			fr.SNMPStart()
		}
	}
}

// §10.1, as seen live: the factory snmp.yml's community is in CONFIG_DB
// after boot, and the site's SNMP is off. The file is rewritten first, then
// the community deleted -- whose snmp restart imports the file again, now
// without it -- and the second apply writes nothing. Deleting from
// CONFIG_DB alone (the old plan) brought it straight back on the restart
// and deleted it again every cycle; the runner models that importer, so
// rig.device's idempotence check is the regression test.
func TestSNMPOffRewritesSNMPYml(t *testing.T) {
	r := newHardwareRig(t, factorySNMPYml(t, true))
	if got := r.system().SNMPCommunities; !reflect.DeepEqual(got, []string{"community-1"}) {
		t.Fatalf("communities before = %v", got)
	}
	r.device("off", devicemodel.DeviceDesired{SNMPSet: true}, []string{
		snmpYmlCommand, "sudo config snmp community del 'community-1'", save,
	})
	if got := r.system().SNMPCommunities; len(got) != 0 {
		t.Errorf("communities after = %v, want none", got)
	}
	if got := strings.TrimSpace(sections(r.fr.Fixture)["snmp_yml"]); got != "{}" {
		t.Errorf("snmp.yml after = %s, want no community", got)
	}
	if len(r.fr.SNMPYmlScripts) != 1 || !strings.Contains(r.fr.SNMPYmlScripts[0], `drop = set(["community-1"])`) {
		t.Errorf("snmp.yml script %q", r.fr.SNMPYmlScripts)
	}
	if strings.Contains(r.log.String(), "community-1") {
		t.Error("a community is a credential and must not be logged")
	}
}

// UniFi's community while the file still names the image's: the file loses
// the image's, CONFIG_DB gains UniFi's and loses the image's; UniFi's own
// never goes into the file.
func TestSNMPCommunityWithFactorySNMPYml(t *testing.T) {
	r := newHardwareRig(t, factorySNMPYml(t, true))
	r.device("set", devicemodel.DeviceDesired{SNMPSet: true, SNMPCommunity: "lab-ro"}, []string{
		snmpYmlCommand, "sudo config snmp community add 'lab-ro' RO", "sudo config snmp community del 'community-1'", save,
	})
	if got := r.system().SNMPCommunities; !reflect.DeepEqual(got, []string{"lab-ro"}) {
		t.Errorf("communities = %v", got)
	}
	if strings.Contains(strings.Join(r.fr.SNMPYmlScripts, ""), "lab-ro") {
		t.Error("UniFi's community must not be written to snmp.yml")
	}
}

// A community only in the file (CONFIG_DB already clean, the snmp
// container not restarted since) is still configured -- the next restart
// imports it -- so the read side reports it and the write drops it from the
// file alone.
func TestSNMPYmlOnlyCommunity(t *testing.T) {
	r := newHardwareRig(t, factorySNMPYml(t, false))
	if got := r.system().SNMPCommunities; !reflect.DeepEqual(got, []string{"community-1"}) {
		t.Fatalf("communities = %v, want the file's", got)
	}
	r.device("off", devicemodel.DeviceDesired{SNMPSet: true}, []string{snmpYmlCommand})
	if got := r.system().SNMPCommunities; len(got) != 0 {
		t.Errorf("communities after = %v", got)
	}
}

// The unit as captured (the file fixed by hand): nothing to write. A file
// collect.py could not parse is logged and left alone.
func TestSNMPYmlCleanOrUnreadable(t *testing.T) {
	r := newHardwareRig(t, nil)
	r.device("off", devicemodel.DeviceDesired{SNMPSet: true}, nil)
	r = newHardwareRig(t, func(fr *FixtureRunner) { fr.UseSection("snmp_yml", "error") })
	r.device("off, unreadable", devicemodel.DeviceDesired{SNMPSet: true}, nil)
	if !strings.Contains(r.log.String(), "snmp.yml could not be read") {
		t.Errorf("no note for an unreadable snmp.yml: %s", r.log.String())
	}
}
