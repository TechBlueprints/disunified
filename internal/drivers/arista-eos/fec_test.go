package aristaeos

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/fecpolicy"
)

// fec-auto/ is the switch on 2026-11-17: cage 49 a 100G CWDM4 optic with
// reed-solomon configured, cage 50 a CA-25G-S breakout cable split 4x25G at
// EOS's default (fire-code running, no link), cages 51-53 CA-25G-L 100G
// cables (51 and 53 with reed-solomon configured, 52 at the default and
// running it), cage 54 empty. The -fc, -fc-rs, -none and -rs captures are
// the same switch with Ethernet50/4 written by hand under a commit timer
// that was aborted.
func fecAutoCollector(t *testing.T, variant string) (*Collector, *FixtureTransport, *bytes.Buffer, *devicemodel.Snapshot) {
	t.Helper()
	ft := NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M", "fec-auto"))
	if variant != "" {
		ft.Overrides = map[string]string{
			"show interfaces error-correction": "show-interfaces-error-correction-" + variant + ".json",
			"show running-config":              "show-running-config-" + variant + ".json",
		}
	}
	c := NewCollector(ft)
	var buf bytes.Buffer
	c.Log = log.New(&buf, "", 0)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, ft, &buf, snap
}

// auto is the ports as they are with FEC left to the driver (no FEC in the
// push: the controller's "auto").
func auto(ports []devicemodel.Port) []devicemodel.PortDesired {
	ds := desiredFrom(ports)
	for i := range ds {
		ds[i].FEC = nil
	}
	return ds
}

func fecLines(ft *FixtureTransport) []string {
	var out []string
	for _, batch := range ft.Configured {
		iface := ""
		for _, c := range batch {
			if strings.HasPrefix(c, "interface ") {
				iface = strings.TrimPrefix(c, "interface ")
			}
			if strings.Contains(c, "error-correction") {
				out = append(out, iface+": "+c)
			}
		}
	}
	return out
}

// The switch as it is follows the standard already: cage 49's and the CA-L
// cables' reed-solomon, cage 52's default (which runs it), the CA-S lanes'
// default fire-code. Auto writes no FEC, for the device and for a lane
// switch on cage 50, and logs no FEC note.
func TestFECAutoWritesNothingOnTheSwitchAsItIs(t *testing.T) {
	c, ft, buf, snap := fecAutoCollector(t, "")
	c.mu.Lock()
	codes := c.eeprom
	c.mu.Unlock()
	for cage, want := range map[int]fecpolicy.Medium{49: fecpolicy.CWDM4, 50: fecpolicy.CopperCAS, 51: fecpolicy.CopperCAL, 52: fecpolicy.CopperCAL, 53: fecpolicy.CopperCAL} {
		if got := moduleMedium(codes[cage], ""); got != want {
			t.Errorf("cage %d: medium %v (codes %+v), want %v", cage, got, codes[cage], want)
		}
	}
	if _, ok := codes[54]; ok {
		t.Errorf("the empty cage has codes: %+v", codes[54])
	}
	if _, err := c.ApplyPorts(context.Background(), auto(snap.Ports)); err != nil {
		t.Fatal(err)
	}
	v := lanePorts(t, c, 50)
	if _, err := c.ApplyLanes(context.Background(), 50, auto(v.Lanes)); err != nil {
		t.Fatal(err)
	}
	if got := fecLines(ft); len(got) != 0 {
		t.Errorf("FEC written: %q", got)
	}
	if strings.Contains(buf.String(), "FEC") {
		t.Errorf("a FEC note: %s", buf.String())
	}
}

// A hand-set encoding on a lane that has no link: auto ends on the CA-S
// cable's fire-code alone, written to that lane only, counted by neither
// PlanPorts nor PlanLanes, and a reconcile after it writes nothing.
func TestFECAutoCorrectsAStaleEncoding(t *testing.T) {
	for variant, want := range map[string][]string{
		"none":  {"Ethernet50/4: error-correction encoding fire-code"},                                                            // disabled: outside the cable's set
		"rs":    {"Ethernet50/4: error-correction encoding fire-code", "Ethernet50/4: no error-correction encoding reed-solomon"}, // one encoding, not the chosen one
		"fc-rs": {"Ethernet50/4: no error-correction encoding reed-solomon"},                                                      // two encodings: exactly one stays
		"fc":    nil,                                                                                                              // already the choice
	} {
		t.Run(variant, func(t *testing.T) {
			c, ft, _, snap := fecAutoCollector(t, variant)
			ds := auto(snap.Ports)
			if got := c.PlanPorts(ds); len(got) != 0 {
				t.Errorf("PlanPorts = %v: the module's FEC is not the controller's change", got)
			}
			if _, err := c.ApplyPorts(context.Background(), ds); err != nil {
				t.Fatal(err)
			}
			if got := fecLines(ft); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("wrote %q, want %q", got, want)
			}
			if len(ft.Unmodelled) != 0 {
				t.Errorf("unmodelled: %q", ft.Unmodelled)
			}
			// Before the next collect, and after it.
			for _, step := range []string{"again", "after a collect"} {
				n := len(ft.Configured)
				if _, err := c.ApplyPorts(context.Background(), ds); err != nil {
					t.Fatal(err)
				}
				if len(ft.Configured) != n {
					t.Errorf("%s: wrote %q", step, ft.Configured[n:])
				}
				if _, err := c.Collect(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			c.mu.Lock()
			row := c.fecRows["Ethernet50/4"]
			c.mu.Unlock()
			if !row.only(devicemodel.FECFC) || row.Status != "fireCode" {
				t.Errorf("Ethernet50/4 after: %+v", row)
			}
		})
	}
}

// The same through a lane switch's plan (cage 50's lanes as its ports).
func TestFECAutoOnLanes(t *testing.T) {
	c, ft, _, _ := fecAutoCollector(t, "none")
	v := lanePorts(t, c, 50)
	ds := auto(v.Lanes)
	if got := c.PlanLanes(50, ds); len(got) != 0 {
		t.Errorf("PlanLanes = %v", got)
	}
	if n, err := c.ApplyLanes(context.Background(), 50, ds); err != nil || n != 1 {
		t.Fatalf("ApplyLanes = %d, %v", n, err)
	}
	if got := fecLines(ft); len(got) != 1 || got[0] != "Ethernet50/4: error-correction encoding fire-code" {
		t.Errorf("wrote %q", got)
	}
	n := len(ft.Configured)
	if _, err := c.ApplyLanes(context.Background(), 50, ds); err != nil || len(ft.Configured) != n {
		t.Errorf("a second apply wrote %q (%v)", ft.Configured[n:], err)
	}
}

// A pushed FEC ends with exactly that mode configured: the encodings are
// additive on EOS, so the other one is taken out.
func TestPushedFECReplacesTheOtherEncoding(t *testing.T) {
	c, ft, _, _ := fecAutoCollector(t, "rs")
	v := lanePorts(t, c, 50)
	ds := auto(v.Lanes)
	fc := devicemodel.FECFC
	ds[3].FEC = &fc
	if got := c.PlanLanes(50, ds); len(got) != 1 || got[0] != 4 {
		t.Errorf("PlanLanes = %v, want [4]: a pushed FEC is the controller's", got)
	}
	if _, err := c.ApplyLanes(context.Background(), 50, ds); err != nil {
		t.Fatal(err)
	}
	want := []string{"Ethernet50/4: error-correction encoding fire-code", "Ethernet50/4: no error-correction encoding reed-solomon"}
	if got := fecLines(ft); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrote %q, want %q", got, want)
	}
}

// fecPlan against the captured rows, for the modules the switch does not
// hold in that position: the row of a 25G lane (reed-solomon and fire-code
// available, fire-code running at the default) and of a linked 100G cage
// (reed-solomon only, running it).
func TestFECPlanByMedium(t *testing.T) {
	c, _, _, snap := fecAutoCollector(t, "")
	c.mu.Lock()
	rows := c.fecRows
	c.mu.Unlock()
	lane := portByIndex(t, snap, 50)
	lane.Interfaces, lane.IfName = []string{"Ethernet50/4"}, "Ethernet50/4"
	cage := portByIndex(t, snap, 52)
	if lane.Up || !cage.Up || lane.AdminSpeed != 25000 || cage.AdminSpeed != 100000 {
		t.Fatalf("the capture changed: lane %+v cage %+v", lane, cage)
	}
	ext := func(code int) moduleCodes { return moduleCodes{ethernet: 0x80, extended: code, ok: true} }
	for _, tc := range []struct {
		name  string
		p     devicemodel.Port
		codes moduleCodes
		media string
		want  string
		note  string
	}{
		{"CA-L on a 25G lane: RS (this switch's lanes run it)", lane, ext(0x0B), "100GBASE-CR4", "error-correction encoding reed-solomon", ""},
		{"a 25G LR optic on a lane: RS", lane, ext(0x03), "", "error-correction encoding reed-solomon", ""},
		{"CA-N on a lane: none", lane, ext(0x0D), "", "no error-correction encoding", ""},
		{"CA-S on a lane: the default's fire-code, nothing written", lane, ext(0x0C), "", "", ""},
		{"an active cable at 1e-12, linked on the default's RS: permitted but not preferred, rewritten to none", cage, ext(0x18), "", "no error-correction encoding", ""},
		{"a CA-L cable linked on the default's RS: already the preferred mode, nothing written", cage, ext(0x0B), "", "", ""},
		{"LR4 linked on RS: outside the standard, corrected", cage, ext(0x03), "", "no error-correction encoding", ""},
		{"no codes, a CR4 cable at 100G: RS, already running", cage, moduleCodes{}, "100GBASE-CR4", "", ""},
		{"no codes, a CR4 cable on a lane: the class is not known", lane, moduleCodes{}, "100GBASE-CR4", "", "no FEC known for the module \"100GBASE-CR4\" (100GBASE-CR4 copper (25G class unknown)) at 25000 Mb/s on Ethernet50/4"},
		{"a media type no capture shows", cage, moduleCodes{}, "100GBASE-XYZ", "", "the module \"100GBASE-XYZ\" is not one the FEC table knows"},
		{"a 100G-only optic on a lane", lane, ext(0x06), "", "", "no FEC known"},
	} {
		p := tc.p
		o := *p.Optic
		o.MediaType = tc.media
		p.Optic = &o
		var notes []string
		note := func(f string, a ...any) {
			notes = append(notes, strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf(f, a...), "\n", " ")))
		}
		cmds, _ := fecPlan(fecInputs{rows: rows, codes: map[int]moduleCodes{p.Index: tc.codes}}, p, devicemodel.PortDesired{Index: p.Index}, p.Index, false, note)
		if got := strings.Join(cmds[laneNames(p)[0]], "; "); got != tc.want {
			t.Errorf("%s: wrote %q, want %q", tc.name, got, tc.want)
		}
		if joined := strings.Join(notes, " | "); (tc.note == "") != (joined == "") || !strings.Contains(joined, tc.note) {
			t.Errorf("%s: notes %q, want %q", tc.name, joined, tc.note)
		}
		if got, _ := fecPlan(fecInputs{rows: rows, codes: map[int]moduleCodes{p.Index: tc.codes}}, p, devicemodel.PortDesired{Index: p.Index}, p.Index, true, func(string, ...any) {}); len(got) != 0 {
			t.Errorf("%s: the controller-only plan wrote %q", tc.name, got)
		}
	}
}

// What EOS printed for each step, in a session that was aborted: the
// encodings are additive, `no ... <x>` removes one, the last one out
// leaves nothing, the bare `no` replaces them all (session-config-diffs.txt).
func TestFixtureFECFollowsTheSessionDiffs(t *testing.T) {
	ft := NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M", "fec-auto"))
	lines := func() []string {
		var out []string
		for _, k := range rcKeys(readJSON(t, ft, "show running-config"), "interface Ethernet54/1") {
			if strings.Contains(k, "error-correction") {
				out = append(out, k)
			}
		}
		return out
	}
	for _, step := range []struct {
		cmd  string
		want string
	}{
		{"error-correction encoding reed-solomon", "error-correction encoding reed-solomon"},
		{"error-correction encoding fire-code", "error-correction encoding fire-code|error-correction encoding reed-solomon"},
		{"no error-correction encoding reed-solomon", "error-correction encoding fire-code"},
		{"no error-correction encoding fire-code", ""},
		{"no error-correction encoding", "no error-correction encoding"},
		{"error-correction encoding fire-code", "error-correction encoding fire-code"},
		{"default error-correction encoding", ""},
	} {
		configureAll(t, ft, "interface Ethernet54/1", step.cmd)
		if got := strings.Join(lines(), "|"); got != step.want {
			t.Errorf("after %q: %q, want %q", step.cmd, got, step.want)
		}
	}
}
