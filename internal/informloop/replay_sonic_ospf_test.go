package informloop

import (
	"strings"
	"testing"
)

// The DX010's OSPF captures of 2026-11-07 (docs/drivers/sonic.md §11.14),
// replayed over the switch as captured during the run (collect-ospf.txt:
// the test network routed on VLAN 3999, ospfd running on the driver's
// file): the router made in the UI with the network in area 0, then MD5,
// plain text, a passive interface in a stub area, an NSSA, and the router
// deleted. Each push restarts ospfd on its file (or stops it); the cycle
// after it writes nothing.
func TestReplaySonicDX010OSPF(t *testing.T) {
	r, _ := newDX010Rig(t, "collect-ospf.txt")
	const start = `docker exec -i bgp sh -c 'f=/etc/frr/disunified-ospfd.conf && umask 077 && cat > "$f.tmp" && chown frr:frr "$f.tmp" && mv "$f.tmp" "$f" && pkill -x ospfd; for i in $(seq 1 50); do pgrep -x ospfd >/dev/null || break; sleep 0.1; done; /usr/lib/frr/ospfd -d -A 127.0.0.1 -P 0 -f "$f"'`
	const stop = `docker exec bgp sh -c 'pkill -x ospfd; rm -f /etc/frr/disunified-ospfd.conf'`
	ospfWrites := func(got []string) []string {
		var out []string
		for _, w := range got {
			if strings.Contains(w, "ospfd") {
				out = append(out, w)
			}
		}
		return out
	}
	files := func() string {
		f := r.fr.OSPFFiles
		r.fr.OSPFFiles = nil
		if len(f) != 1 {
			t.Fatalf("files sent: %d", len(f))
		}
		return f[0]
	}
	for _, step := range []struct {
		file, ver string
		want      []string // lines the ospfd file must hold
	}{
		{"system_cfg-dx010-ospf-1-defaults.txt", "9171c84478cba184", []string{"interface Vlan3999", " ip ospf area 0.0.0.0", " ip ospf cost 1", " ip ospf hello-interval 10", " ip ospf dead-interval 40", " ospf router-id 198.51.100.1", " redistribute connected metric-type 2", " redistribute static metric-type 1", " default-information originate"}},
		{"system_cfg-dx010-ospf-2-md5.txt", "712ac68973a6dd99", []string{" ip ospf authentication message-digest", " ip ospf message-digest-key 1 md5 <hash>", " ip ospf cost 10", " ip ospf priority 2"}},
		{"system_cfg-dx010-ospf-3-plaintext.txt", "a336f6ec66ea5e5e", []string{" ip ospf authentication", " ip ospf authentication-key <hash>"}},
		{"system_cfg-dx010-ospf-4-passive-stub.txt", "8e103ca3867d4876", []string{" ip ospf area 0.0.0.1", " ip ospf passive", " area 0.0.0.1 stub"}},
		{"system_cfg-dx010-ospf-5-nssa.txt", "52e4a7483b989bf4", []string{" ip ospf area 0.0.0.2", " area 0.0.0.2 nssa"}},
	} {
		got := ospfWrites(r.push(step.file, step.ver))
		if len(got) != 1 || got[0] != start {
			t.Fatalf("%s wrote %q", step.file, got)
		}
		f := files()
		for _, line := range step.want {
			if !strings.Contains(f, line+"\n") {
				t.Errorf("%s: the file lacks %q:\n%s", step.file, line, f)
			}
		}
		r.cycle(step.file)
	}
	if got := ospfWrites(r.push("system_cfg-dx010-ospf-6-removed.txt", "ccf8a20b61a9d215")); len(got) != 1 || got[0] != stop {
		t.Errorf("the router deleted wrote %q", got)
	}
	r.cycle("the router deleted")
	if s := r.log.String(); strings.Contains(s, "UNSUPPORTED") || strings.Contains(s, "not translated") || strings.Contains(s, "<hash>") {
		t.Errorf("log:\n%s", s)
	}
}
