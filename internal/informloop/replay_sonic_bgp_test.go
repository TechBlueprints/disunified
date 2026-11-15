package informloop

import (
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// The DX010's BGP captures of 2026-11-07 (docs/drivers/sonic.md §11.14),
// replayed over the switch as captured before the run (collect-bgp.txt:
// bgpd runs only SONiC's own LOCAL_VLAN_IPV4_PREFIX), in the order the
// controller sent them: a BGP router made for the switch under Settings ->
// Routing -> BGP with an uploaded FRR file, the router switched off, on
// again, its file changed, the router deleted. Each push is followed by a
// cycle that must write nothing.
func TestReplaySonicDX010BGP(t *testing.T) {
	r, _ := newDX010Rig(t, "collect-bgp.txt")
	r.settle("the capture under its own push")
	const load = `docker exec -i bgp sh -c 'umask 077 && f=$(mktemp /tmp/disunified-bgp.XXXXXX) && cat > "$f" && vtysh -f "$f"; rc=$?; rm -f "$f"; exit $rc'`
	file := readECSPush(t, "system_cfg-dx010-bgp-1-enabled.txt")
	conf := unificfg.Parse(file).BGP.Config
	files := func() []string {
		out := r.fr.BGPFiles
		r.fr.BGPFiles = nil
		return out
	}
	const gone = "no router bgp 65010\nno ip prefix-list DUI-TEST-PL\nno route-map DUI-TEST-IN\n"

	wantWrites(t, "the router made", r.push("system_cfg-dx010-bgp-1-enabled.txt", "76bad7192044651e"), []string{load})
	if got := files(); len(got) != 1 || got[0] != conf {
		t.Errorf("the first load sent %q\nwant the operator's file %q", got, conf)
	}
	r.cycle("the router made")

	wantWrites(t, "the router off", r.push("system_cfg-dx010-bgp-2-disabled.txt", "39768b14b6abf1cf"), []string{load})
	if got := files(); len(got) != 1 || got[0] != gone {
		t.Errorf("switching the router off sent %q", got)
	}
	r.cycle("the router off")

	wantWrites(t, "the router on again", r.push("system_cfg-dx010-bgp-1-enabled.txt", "7f67f7f328676460"), []string{load})
	files()
	r.cycle("the router on again")

	wantWrites(t, "the file changed", r.push("system_cfg-dx010-bgp-3-changed.txt", "5d9f6b48280af4ae"), []string{load})
	if got := files(); len(got) != 1 || !strings.HasPrefix(got[0], gone+"router bgp 65010\n") || !strings.Contains(got[0], " neighbor 198.51.100.2 timers 10 30\n") {
		t.Errorf("the changed file sent %q", got)
	}
	r.cycle("the file changed")

	wantWrites(t, "the router deleted", r.push("system_cfg-dx010-bgp-4-removed.txt", "58e3978e90e18a49"), []string{load})
	if got := files(); len(got) != 1 || got[0] != gone {
		t.Errorf("deleting the router sent %q", got)
	}
	r.cycle("the router deleted")

	if s := r.log.String(); strings.Contains(s, "UNSUPPORTED") || strings.Contains(s, "sonic: BGP:") {
		t.Errorf("a BGP key was left unhandled or refused:\n%s", s)
	}
}
