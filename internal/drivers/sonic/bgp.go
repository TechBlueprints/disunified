package sonic

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// UniFi's BGP on a switch (docs/drivers/sonic.md §11.14). The controller
// has no BGP form of its own: under Settings -> Routing -> BGP the operator
// picks a device (the gateway or a BGP-capable switch) and uploads an FRR
// bgpd configuration file, which a switch receives whole as
// switch.bgp.1.raw_config_base64. Ubiquiti's SONiC stores it in a table of
// its own for its FRR manager; community SONiC has no such table, so the
// driver loads the file into the bgp container's FRR through vtysh.
//
// The driver owns the top-level objects the file adds ("router bgp <asn>",
// route-maps, prefix-lists, community and as-path lists, access-lists) and
// never one that was on the switch before (SONiC's own
// LOCAL_VLAN_IPV4_PREFIX): such a stanza is left out with a note. A changed
// file replaces the old one whole -- every owned object is removed, then the
// new file is loaded -- so a changed file restarts its sessions. SONiC
// renders bgpd's configuration from its templates when the bgp container
// starts, so a restart (config reload, reboot) loses the file; the next
// reconcile sees an owned object missing and loads the same file again.
// Anything else in the file (interfaces, static routes, other daemons) is
// left out with a note: those belong to the controller's other settings.

// bgpCommand loads a file into FRR. The file arrives on stdin (so a
// neighbor's password never sits on a command line) and is written to a
// private temporary file in the container first: FRR 10's `vtysh -f` forks
// one child per daemon and each reads the file itself, so with /dev/stdin
// only the first child gets it -- live, 2026-11-07, the second load left
// bgpd untouched with exit 0 (mgmtd had read the file). Run on its own,
// never inside a `bash -s` batch (it would read the batch).
const bgpCommand = `docker exec -i bgp sh -c 'umask 077 && f=$(mktemp /tmp/disunified-bgp.XXXXXX) && cat > "$f" && vtysh -f "$f"; rc=$?; rm -f "$f"; exit $rc'`

// bgpManaged is what the driver loaded: the file's sha256 (of the stanzas
// it applied) and the objects it added. Failed is the sum of a file vtysh
// refused part of: not loaded again until the file changes.
type bgpManaged struct {
	Sum     string   `json:"sum,omitempty"`
	Objects []string `json:"objects,omitempty"`
	Failed  string   `json:"failed,omitempty"`
}

func (m bgpManaged) empty() bool { return m.Sum == "" && len(m.Objects) == 0 && m.Failed == "" }

func (m bgpManaged) clone() bgpManaged {
	return bgpManaged{Sum: m.Sum, Objects: append([]string(nil), m.Objects...), Failed: m.Failed}
}

// frrObject is the object a top-level FRR line opens, as collect.py's
// frr_bgp names it ("" for a line that opens none of bgpd's objects).
func frrObject(line string) string {
	if line == "" || line[0] == ' ' || line[0] == '\t' {
		return ""
	}
	t := strings.Fields(line)
	var k []string
	switch {
	case len(t) >= 3 && t[0] == "router" && t[1] == "bgp":
		k = t[:3]
		if len(t) >= 5 && t[3] == "vrf" {
			k = t[:5]
		}
	case len(t) >= 2 && t[0] == "route-map":
		k = t[:2]
	case len(t) >= 3 && (t[0] == "ip" || t[0] == "ipv6") && (t[1] == "prefix-list" || t[1] == "access-list"):
		k = t[:3]
	case len(t) >= 2 && t[0] == "access-list":
		k = t[:2]
	case len(t) >= 4 && t[0] == "bgp" && t[1] == "as-path" && t[2] == "access-list":
		k = t[:4]
	case len(t) >= 3 && t[0] == "bgp" && (t[1] == "community-list" || t[1] == "extcommunity-list" || t[1] == "large-community-list"):
		k = t[:3]
		if (t[2] == "standard" || t[2] == "expanded") && len(t) >= 4 {
			k = t[:4]
		}
	}
	return strings.Join(k, " ")
}

// frrGlobal: top-level lines of a configuration file that set FRR itself,
// not an object: left out quietly (the switch's own) or with a note.
var frrQuiet = regexp.MustCompile(`^(frr (version|defaults)\b|end$|exit$|!)`)

// bgpFile is the part of the operator's file the driver loads.
type bgpFile struct {
	text    string   // the accepted stanzas, in the file's order
	objects []string // the objects they open, in order
	sum     string   // sha256 of text; "" when nothing is accepted
	notes   []string
}

// splitBGP reads the operator's file stanza by stanza. A stanza opening an
// object the switch already runs and the driver does not own is left out.
func splitBGP(cfg string, running, owned map[string]bool) bgpFile {
	var f bgpFile
	var text []string
	seen := map[string]bool{}
	keep := false
	for _, raw := range strings.Split(strings.ReplaceAll(cfg, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if keep {
				text = append(text, line)
			}
			continue
		}
		if frrQuiet.MatchString(line) {
			if keep && (line == "exit" || line == "!") {
				text = append(text, line)
			}
			continue
		}
		key := frrObject(line)
		switch {
		case key == "":
			keep = false
			f.notes = append(f.notes, fmt.Sprintf("%q left out (not a BGP object)", firstWords(line)))
			continue
		case running[key] && !owned[key]:
			keep = false
			f.notes = append(f.notes, fmt.Sprintf("%q is the switch's own; left out", key))
			continue
		}
		keep = true
		text = append(text, line)
		if !seen[key] {
			seen[key] = true
			f.objects = append(f.objects, key)
		}
	}
	if len(f.objects) > 0 {
		f.text = strings.Join(text, "\n") + "\n"
		f.sum = sha256Hex(f.text)
	}
	return f
}

// firstWords is a refused line for a note: its first two words, so a
// password line never reaches the log.
func firstWords(line string) string {
	t := strings.Fields(line)
	if len(t) > 2 {
		t = t[:2]
	}
	return strings.Join(t, " ")
}

// bgpRemoveOrder: routers first (they name the lists), then the rest.
func bgpRemoveOrder(keys []string) []string {
	out := append([]string(nil), keys...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := strings.HasPrefix(out[i], "router bgp "), strings.HasPrefix(out[j], "router bgp ")
		if ri != rj {
			return ri
		}
		return out[i] < out[j]
	})
	return out
}

// bgpPlan is planBGP's result: the vtysh file to load ("" = nothing to
// do), what the log says about it, the record and state after it.
type bgpPlan struct {
	script  string
	logged  string
	managed bgpManaged
	sim     map[string]bool
	notes   []string
}

func planBGP(st *swState, d devicemodel.DeviceDesired, m bgpManaged) (pl bgpPlan) {
	pl.managed = m.clone()
	if !d.BGPSet {
		return pl
	}
	if !st.frrKnown {
		pl.notes = append(pl.notes, "bgpd could not be read (vtysh); BGP not written")
		return pl
	}
	owned := map[string]bool{}
	for _, o := range m.Objects {
		owned[o] = true
	}
	f := splitBGP(d.BGPConfig, st.frrBGP, owned)
	pl.notes = f.notes
	pl.sim = copySet(st.frrBGP)
	var lines, logged []string
	switch {
	case f.sum != m.Sum:
		var gone []string
		for _, o := range bgpRemoveOrder(m.Objects) {
			if st.frrBGP[o] {
				lines = append(lines, "no "+o)
				gone = append(gone, o)
				delete(pl.sim, o)
			}
		}
		if len(gone) > 0 {
			logged = append(logged, "remove "+strings.Join(gone, ", "))
		}
		if f.text != "" {
			lines = append(lines, strings.TrimRight(f.text, "\n"))
			logged = append(logged, fmt.Sprintf("load the controller's file (%s)", strings.Join(f.objects, ", ")))
		}
		pl.managed = bgpManaged{Sum: f.sum, Objects: f.objects}
	case f.text != "" && m.Failed != f.sum:
		var missing []string
		for _, o := range f.objects {
			if !st.frrBGP[o] {
				missing = append(missing, o)
			}
		}
		if len(missing) == 0 {
			return pl
		}
		lines = append(lines, strings.TrimRight(f.text, "\n"))
		logged = append(logged, fmt.Sprintf("load the controller's file again (bgpd lacks %s)", strings.Join(missing, ", ")))
	default:
		return pl
	}
	if len(lines) == 0 {
		return pl
	}
	for _, o := range f.objects {
		pl.sim[o] = true
	}
	pl.script = strings.Join(lines, "\n") + "\n"
	pl.logged = strings.Join(logged, "; ")
	return pl
}

// bgpSet records the BGP router the push asked for, the target of every
// reconcile after it.
func (c *Collector) bgpSet(d devicemodel.DeviceDesired) {
	if !d.BGPSet {
		return
	}
	dd := d
	c.mu.Lock()
	c.bgp = &dd
	c.mu.Unlock()
}

// vtyshLineRe finds the line numbers vtysh names in its complaints; the
// lines themselves are not logged (a password may be on one).
var vtyshLineRe = regexp.MustCompile(`line (\d+)`)

// applyBGP loads the controller's BGP file into FRR when it changed or the
// bgp container lost it. Returns how many writes ran (one per load).
func (c *Collector) applyBGP(ctx context.Context) (int, error) {
	c.mu.Lock()
	st, d, m := c.state, c.bgp, c.managedBGP.clone()
	c.mu.Unlock()
	if st == nil || d == nil {
		return 0, nil
	}
	if !st.bgpOn() {
		if d.BGPConfig != "" {
			c.warnOnce("bgp no frr", "sonic: BGP requested, but FEATURE|bgp is not enabled (no FRR); not written")
		}
		return 0, nil
	}
	pl := planBGP(st, *d, m)
	for _, n := range pl.notes {
		c.warnOnce("bgp: "+n, "sonic: BGP: %s", n)
	}
	if pl.script == "" {
		if !reflect.DeepEqual(m, pl.managed) {
			c.keepBGP(pl.managed)
		}
		return 0, nil
	}
	c.Log.Printf("sonic apply BGP: %s", pl.logged)
	out, err := c.run(ctx, bgpCommand, pl.script)
	if err != nil {
		// vtysh applies what it can and fails on the rest: the record keeps
		// the file so a reconcile does not load it again every cycle.
		var at []string
		for _, mm := range vtyshLineRe.FindAllStringSubmatch(out+" "+err.Error(), -1) {
			at = append(at, mm[1])
		}
		pl.managed.Failed = pl.managed.Sum
		c.keepBGP(pl.managed)
		where := ""
		if len(at) > 0 {
			where = " at line " + strings.Join(at, ", ")
		}
		c.Log.Printf("sonic: BGP: FRR refused part of the controller's file%s; the rest is loaded, the file is not loaded again until it changes", where)
		return 1, nil
	}
	c.mu.Lock()
	if c.state == st {
		sim := st.clone()
		sim.frrBGP = pl.sim
		c.state = sim
	}
	c.mu.Unlock()
	pl.managed.Failed = ""
	c.keepBGP(pl.managed)
	return 1, nil
}

func (c *Collector) keepBGP(m bgpManaged) {
	c.mu.Lock()
	before := c.managedBGP
	c.managedBGP = m
	c.mu.Unlock()
	if !reflect.DeepEqual(before, m) {
		if err := c.saveManaged(); err != nil {
			c.Log.Printf("sonic: keeping %s: %v (the bridge's BGP ownership is held in memory)", managedFile, err)
		}
	}
}
