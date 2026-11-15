package sonic

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The DX010 on igmp-202511.20261115-6c8eb1cf7, 2026-11-15 (docs/drivers/
// sonic.md §6.4.1), managed in-band on Vlan4094 with the controller's
// static IP Settings (STATIC_ROUTE default via the gateway):
//
//   - collect-route-nhid.txt: as the image boots, zebra on kernel nexthop
//     objects (`zebra nexthop kernel enable`, the template's default), the
//     kernel's default through nhid 32;
//   - collect-route-missing.txt: the same after the kernel deleted that
//     nexthop object (`ip nexthop del id 32`, the deletion a carrier loss
//     makes silently): zebra re-created the object within 10 ms, never the
//     route -- the kernel has no default, FRR reports it selected,
//     installed, and its nexthop in the FIB, a minute later as at boot;
//   - collect-route-fixed.txt: after the vtysh call the driver makes (zebra
//     off nexthop objects, the static line out and back in) and
//     DEVICE_METADATA zebra_nexthop disabled: the default inline, proto 196.
const (
	fixtureRouteNHID    = ucastFixtures + "/collect-route-nhid.txt"
	fixtureRouteMissing = ucastFixtures + "/collect-route-missing.txt"
	fixtureRouteFixed   = ucastFixtures + "/collect-route-fixed.txt"
)

const (
	krMeta    = "sudo sonic-db-cli CONFIG_DB hset 'DEVICE_METADATA|localhost' zebra_nexthop disabled"
	krSave    = "sudo config save -y"
	krNoNH    = "docker exec bgp vtysh -c 'configure terminal' -c 'no zebra nexthop kernel enable' </dev/null"
	krReplay  = "docker exec bgp vtysh -c 'configure terminal' -c 'no ip route 0.0.0.0/0 192.0.2.5 tag 1' -c 'ip route 0.0.0.0/0 192.0.2.5 tag 1' </dev/null"
	krNoNHAll = "docker exec bgp vtysh -c 'configure terminal' -c 'no zebra nexthop kernel enable' -c 'no ip route 0.0.0.0/0 192.0.2.5 tag 1' -c 'ip route 0.0.0.0/0 192.0.2.5 tag 1' </dev/null"
)

// kroute runs the route apply once and returns its writes.
func (r *controlRig) kroute() []string {
	r.t.Helper()
	n := len(r.fr.Commands)
	unlock, err := r.c.lockApply(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	_, err = r.c.applyKernelRoute(context.Background())
	unlock()
	if err != nil {
		r.t.Fatalf("applyKernelRoute: %v", err)
	}
	return append([]string(nil), r.fr.Commands[n:]...)
}

func (r *controlRig) recollect() {
	r.t.Helper()
	if _, err := r.c.Collect(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func TestKernelRouteParse(t *testing.T) {
	for _, c := range []struct {
		path               string
		kernelNH, inKernel bool
		nhid               bool
	}{
		{fixtureRouteNHID, true, true, true},
		{fixtureRouteMissing, true, false, false},
		{fixtureRouteFixed, false, true, false},
	} {
		r := rig20261106(t, c.path)
		k := r.c.state.kr
		if !k.known || k.kernelNH != c.kernelNH || len(k.static) != 1 || k.static[0] != "ip route 0.0.0.0/0 192.0.2.5 tag 1" {
			t.Errorf("%s: known %v kernel nexthops %v static %q", c.path, k.known, k.kernelNH, k.static)
		}
		rt := k.installed()
		if rt == nil || rt.Protocol != "static" || len(rt.Nexthops) != 1 || rt.Nexthops[0].IP != "192.0.2.5" || rt.Nexthops[0].Interface != "Vlan4094" {
			t.Fatalf("%s: FRR's installed default = %+v", c.path, rt)
		}
		if found, nhid := k.kernelVia(rt); found != c.inKernel || nhid != c.nhid {
			t.Errorf("%s: in the kernel %v (nhid %v), want %v (%v)", c.path, found, nhid, c.inKernel, c.nhid)
		}
	}
	// A capture from before the section joined collect.py writes nothing.
	r := rig20261106(t, fixtureIPv6On)
	if r.c.state.kr.known {
		t.Error("a capture without frr_route reads as known")
	}
	if got := r.kroute(); len(got) != 0 {
		t.Errorf("without frr_route: wrote %q", got)
	}
}

// The DX010's state on 2026-11-15: no default in the kernel, FRR says
// installed. The first cycle takes zebra off nexthop objects (now and at
// the next bgp start); the route goes back in once two collects in a row
// have missed it; after that nothing more is written.
func TestKernelRouteMissingIsReinstalled(t *testing.T) {
	r := rig20261106(t, fixtureRouteMissing)
	sameCommands(t, r.kroute(), []string{krMeta, krSave, krNoNH})
	if got := r.kroute(); len(got) != 0 {
		t.Errorf("a second apply before the next collect wrote %q", got)
	}
	r.recollect()
	sameCommands(t, r.kroute(), []string{krReplay})
	if !strings.Contains(r.log.String(), "the kernel has no IPv4 default route while FRR reports 0.0.0.0/0 via 192.0.2.5 on Vlan4094 selected and installed (2 collects in a row); re-installing it through zebra") {
		t.Errorf("no loud log line:\n%s", r.log.String())
	}
	r.recollect()
	if k := r.c.state.kr; k.kernelNH {
		t.Error("zebra still on kernel nexthop objects")
	}
	if found, nhid := r.c.state.kr.kernelVia(r.c.state.kr.installed()); !found || nhid {
		t.Errorf("after the re-install: in the kernel %v, nhid %v", found, nhid)
	}
	for i := 0; i < 3; i++ {
		if got := r.kroute(); len(got) != 0 {
			t.Fatalf("cycle %d after the repair wrote %q", i, got)
		}
		r.recollect()
	}
}

// One collect without the route is not enough: zebra's own re-install
// after a slow flap leaves the kernel without it for milliseconds (live,
// the docker0 flaps of 2026-11-15), and a collect can land there.
func TestKernelRouteOneMissIsNotReinstalled(t *testing.T) {
	r := rig20261106(t, fixtureRouteMissing)
	r.kroute() // the zebra switch
	fx, err := NewFixtureRunner(fixtureRouteFixed)
	if err != nil {
		t.Fatal(err)
	}
	fixed := sections(fx.Fixture)
	r.fr.UseSection("route", fixed["route"])
	r.recollect()
	if got := r.kroute(); len(got) != 0 {
		t.Errorf("the route is back: wrote %q", got)
	}
	r.fr.UseSection("route", "[]")
	r.recollect()
	if got := r.kroute(); len(got) != 0 {
		t.Errorf("one collect without the route: wrote %q", got)
	}
	r.recollect()
	sameCommands(t, r.kroute(), []string{krReplay})
}

// How every 202511 boot leaves it: the default in the kernel through a
// nexthop object, which the next carrier loss deletes. The driver switches
// zebra over and re-installs the route inline at once.
func TestKernelRouteThroughNexthopObjectIsMovedInline(t *testing.T) {
	r := rig20261106(t, fixtureRouteNHID)
	sameCommands(t, r.kroute(), []string{krMeta, krSave, krNoNHAll})
	if !strings.Contains(r.log.String(), "uses a nexthop object") {
		t.Errorf("no log line for the move:\n%s", r.log.String())
	}
	r.recollect()
	if got := r.kroute(); len(got) != 0 {
		t.Errorf("after the move: wrote %q", got)
	}
}

// The fixed state writes nothing, through ApplyDevice too.
func TestKernelRouteFixedWritesNothing(t *testing.T) {
	r := rig20261106(t, fixtureRouteFixed)
	if got := r.kroute(); len(got) != 0 {
		t.Errorf("wrote %q", got)
	}
}

// ApplyDevice runs the route apply every cycle, whatever else the push says.
func TestApplyDeviceRepairsTheKernelRoute(t *testing.T) {
	r := rig20261106(t, fixtureRouteNHID)
	n := len(r.fr.Commands)
	if _, err := r.c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{}); err != nil {
		t.Fatal(err)
	}
	got := r.fr.Commands[n:]
	if !containsStr(got, krMeta) || !containsStr(got, krNoNHAll) {
		t.Errorf("ApplyDevice did not reach the route apply: %q", got)
	}
}
