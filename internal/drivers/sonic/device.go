package sonic

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// The device controller (docs/drivers/sonic.md §6.3), the SSH key installer
// and the rebooter (§6.4).

// managedFile keeps, beside device.json, which NTP servers, syslog hosts and
// SNMP community the bridge itself put on the switch, so a later push
// removes only those and never the operator's own.
const managedFile = "sonic-managed.json"

type managedSet struct {
	NTP    []string `json:"ntp,omitempty"`
	Syslog []string `json:"syslog,omitempty"`
	SNMP   string   `json:"snmp,omitempty"`
	// Pinned is each NTP/syslog hostname's resolution, keyed "ntp|<host>"
	// or "syslog|<host>": a pool answers a different address on every
	// lookup, and re-resolving each push churned the switch's NTP list
	// forever (seen live). Kept while the hostname stays in the push and
	// its address stays configured on the switch.
	Pinned map[string]string `json:"pinned,omitempty"`
}

func (c *Collector) loadManaged() error {
	if c.StateDir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(c.StateDir, managedFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var m managedSet
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("sonic: %s: %w", managedFile, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range m.NTP {
		c.managedNTP[s] = true
	}
	for _, s := range m.Syslog {
		c.managedSyslog[s] = true
	}
	c.managedSNMP = m.SNMP
	for k, v := range m.Pinned {
		c.pinned[k] = v
	}
	return nil
}

func (c *Collector) saveManaged() error {
	c.mu.Lock()
	m := c.managedLocked()
	dir := c.StateDir
	c.mu.Unlock()
	if dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, managedFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, managedFile))
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func copySet(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// managedLocked is what the driver owns, as stored (c.mu held).
func (c *Collector) managedLocked() managedSet {
	m := managedSet{NTP: sortedSet(c.managedNTP), Syslog: sortedSet(c.managedSyslog), SNMP: c.managedSNMP}
	if len(c.pinned) > 0 {
		m.Pinned = copyPins(c.pinned)
	}
	return m
}

func copyPins(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// pinnedIP is the address an NTP/syslog server is configured by: itself
// for an IP; for a hostname, the address it resolved to when it first
// appeared in the push, kept while that address is still configured on
// the switch. Re-resolved only when the pinned address is gone from the
// switch (removed by hand, or absent at a bridge start) or when the
// hostname had left the push and came back (its pin was dropped then).
func (c *Collector) pinnedIP(kind, host string, pins map[string]string, configured func(string) bool) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	key := kind + "|" + host
	if ip, ok := pins[key]; ok && configured(ip) {
		return ip, nil
	}
	ip, err := c.resolve(host)
	if err != nil {
		delete(pins, key)
		return "", err
	}
	pins[key] = ip
	return ip, nil
}

// dropPins forgets the pins of hostnames no longer in the push.
func dropPins(kind string, hosts []string, pins map[string]string) {
	in := map[string]bool{}
	for _, h := range hosts {
		in[kind+"|"+h] = true
	}
	for k := range pins {
		if strings.HasPrefix(k, kind+"|") && !in[k] {
			delete(pins, k)
		}
	}
}

// resolve uses the Resolve hook (tests) or resolveIP.
func (c *Collector) resolve(host string) (string, error) {
	if c.Resolve != nil {
		return c.Resolve(host)
	}
	return resolveIP(host)
}

// resolveIP: SONiC takes addresses only, so a hostname is resolved by the
// bridge (IPv4 preferred).
func resolveIP(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	ips, err := net.LookupHost(host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("cannot resolve %q: %v", host, err)
	}
	for _, ip := range ips {
		if p := net.ParseIP(ip); p != nil && p.To4() != nil {
			return ip, nil
		}
	}
	return ips[0], nil
}

// deviceOp is one device-level write; logged is what the log shows (an SNMP
// community is a credential and is not logged).
type deviceOp struct {
	cmd, logged string
	effect      func(*swState)
}

// ApplyDevice implements devicemodel.DeviceController.
func (c *Collector) ApplyDevice(ctx context.Context, d devicemodel.DeviceDesired) (int, error) {
	c.mu.Lock()
	st := c.state
	mNTP, mSyslog, mSNMP := copySet(c.managedNTP), copySet(c.managedSyslog), c.managedSNMP
	pins := copyPins(c.pinned)
	before := c.managedLocked()
	c.mu.Unlock()
	if st == nil {
		return 0, fmt.Errorf("sonic apply switch: no snapshot yet")
	}
	sim := st.clone()
	var ops []deviceOp
	var notes []string
	add := func(cmd, logged string, eff func(*swState)) {
		if logged == "" {
			logged = cmd
		}
		ops = append(ops, deviceOp{cmd: "sudo " + cmd, logged: "sudo " + logged, effect: eff})
		if eff != nil {
			eff(sim)
		}
	}
	note := func(format string, args ...any) { notes = append(notes, fmt.Sprintf(format, args...)) }

	// STP: PVST only, and only where the image has the daemon.
	if d.STPSet {
		switch {
		case !sim.stpFeature:
			if d.STPEnabled {
				note("spanning tree requested, but this image has no STP daemon (no FEATURE|stp); not written")
			}
		case d.STPEnabled && d.STPMode == "mstp":
			note("MSTP requested; SONiC runs PVST (per-VLAN 802.1D) only; not written")
		default:
			enabled := sim.stpGlobal != nil
			if d.STPEnabled && !enabled {
				add("config spanning-tree enable pvst", "", func(s *swState) {
					s.stpGlobal = map[string]string{"mode": "pvst", "priority": "32768", "hello_time": "2", "forward_delay": "15", "max_age": "20"}
				})
			}
			if !d.STPEnabled && enabled {
				add("config spanning-tree disable pvst", "", func(s *swState) { s.stpGlobal = nil })
			}
			if d.STPEnabled && d.STPPriority > 0 && atoi(sim.stpGlobal["priority"]) != d.STPPriority {
				p := strconv.Itoa(d.STPPriority)
				add("config spanning-tree priority "+p, "", func(s *swState) { s.stpGlobal["priority"] = p })
			}
		}
	}
	for _, on := range d.IGMPSnooping {
		if on {
			note("IGMP snooping requested; community SONiC has none; not written")
			break
		}
	}
	if d.DHCPSnooping != nil && *d.DHCPSnooping {
		note("DHCP snooping requested; community SONiC has none; not written")
	}

	// NTP and syslog: add the controller's, remove only what the bridge
	// added earlier, leave the rest.
	if d.ManageNTP {
		want := map[string]bool{}
		for _, h := range d.NTPServers {
			ip, err := c.pinnedIP("ntp", h, pins, func(ip string) bool { return sim.ntp[ip] })
			if err != nil {
				note("NTP server %s; skipped", err)
				continue
			}
			want[ip] = true
			if !sim.ntp[ip] {
				a := ip
				add("config ntp add "+a, "", func(s *swState) { s.ntp[a] = true })
				mNTP[a] = true
			}
		}
		for _, ip := range sortedSet(mNTP) {
			if want[ip] {
				continue
			}
			if sim.ntp[ip] {
				a := ip
				add("config ntp del "+a, "", func(s *swState) { delete(s.ntp, a) })
			}
			delete(mNTP, ip)
		}
		dropPins("ntp", d.NTPServers, pins)
	}
	if d.ManageSyslog {
		want := map[string]bool{}
		var hosts []string
		for _, h := range d.SyslogHosts {
			host, port := h, "514"
			if hh, pp, err := net.SplitHostPort(h); err == nil {
				host, port = hh, pp
			}
			hosts = append(hosts, host)
			ip, err := c.pinnedIP("syslog", host, pins, func(ip string) bool { _, ok := sim.syslog[ip]; return ok })
			if err != nil {
				note("syslog host %s; skipped", err)
				continue
			}
			want[ip] = true
			cur, ok := sim.syslog[ip]
			if cur == "" {
				cur = "514"
			}
			if ok && cur == port {
				continue
			}
			a, p := ip, port
			if ok {
				add("config syslog del "+a, "", func(s *swState) { delete(s.syslog, a) })
			}
			add("config syslog add "+a+" --port "+p, "", func(s *swState) { s.syslog[a] = p })
			mSyslog[a] = true
		}
		for _, ip := range sortedSet(mSyslog) {
			if want[ip] {
				continue
			}
			if _, ok := sim.syslog[ip]; ok {
				a := ip
				add("config syslog del "+a, "", func(s *swState) { delete(s.syslog, a) })
			}
			delete(mSyslog, ip)
		}
		dropPins("syslog", hosts, pins)
	}

	// SNMP: one managed read-only community; the previous managed one is
	// replaced, any other (the image's `public`) is left alone.
	if d.SNMPSet {
		want := d.SNMPCommunity
		if want != "" && !sim.snmp[want] {
			w := want
			add("config snmp community add "+sshrun.ShellQuote(w)+" RO", "config snmp community add <community> RO", func(s *swState) { s.snmp[w] = true })
		}
		if mSNMP != "" && mSNMP != want && sim.snmp[mSNMP] {
			old := mSNMP
			add("config snmp community del "+sshrun.ShellQuote(old), "config snmp community del <community>", func(s *swState) { delete(s.snmp, old) })
		}
		mSNMP = want
	}

	for _, n := range notes {
		c.warnOnce("device: "+n, "%s", n)
	}
	commitManaged := func() {
		c.mu.Lock()
		c.managedNTP, c.managedSyslog, c.managedSNMP, c.pinned = mNTP, mSyslog, mSNMP, pins
		after := c.managedLocked()
		c.mu.Unlock()
		if reflect.DeepEqual(before, after) {
			return
		}
		if err := c.saveManaged(); err != nil {
			c.Log.Printf("sonic: keeping %s: %v (the bridge's NTP/syslog/SNMP ownership is held in memory)", managedFile, err)
		}
	}
	if len(ops) == 0 {
		commitManaged()
		return 0, nil
	}
	cmds := make([]string, 0, len(ops)+1)
	logged := make([]string, 0, len(ops)+1)
	for _, o := range ops {
		cmds = append(cmds, o.cmd)
		logged = append(logged, o.logged)
	}
	cmds = append(cmds, "sudo config save -y")
	logged = append(logged, "sudo config save -y")
	c.Log.Printf("sonic apply switch: %s", strings.Join(logged, " ; "))
	if err := c.runBatch(ctx, cmds); err != nil {
		return 0, fmt.Errorf("sonic apply switch settings: %s", redact(err.Error(), d.SNMPCommunity, mSNMP))
	}
	c.mu.Lock()
	c.state = sim
	c.mu.Unlock()
	commitManaged()
	return len(ops), nil
}

func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// redact keeps SNMP communities out of an error that echoes a command.
func redact(s string, secrets ...string) string {
	for _, x := range secrets {
		if x != "" {
			s = strings.ReplaceAll(s, x, "<community>")
		}
	}
	return s
}

// --- SSH keys ---

const (
	keysBegin = "# disunified-managed-begin"
	keysEnd   = "# disunified-managed-end"
	// keysCommand rewrites the login user's authorized_keys: the script on
	// stdin keeps every line outside the bridge's block and replaces the
	// block, through a temporary file moved into place.
	keysCommand = "bash -s -- disunified-authorized-keys"
)

var (
	keyTypeRe  = regexp.MustCompile(`^[a-z0-9@.-]+$`)
	keyValueRe = regexp.MustCompile(`^[A-Za-z0-9+/]+=*$`)
)

// InstallSSHKeys implements devicemodel.SSHKeyInstaller: every key the
// controller pushes, in the bridge's marked block of the login user's
// ~/.ssh/authorized_keys. Lines outside the block -- the operator's own key,
// the bridge's own -- are never touched. Compared by "<type> <value>".
func (c *Collector) InstallSSHKeys(ctx context.Context, keys []devicemodel.SSHKey) (int, error) {
	c.mu.Lock()
	st := c.state
	c.mu.Unlock()
	if st == nil {
		return 0, fmt.Errorf("sonic install ssh keys: no snapshot yet")
	}
	var lines []string
	want := map[string]bool{}
	for _, k := range keys {
		typ, val := strings.TrimSpace(k.Type), strings.TrimSpace(k.Value)
		if !keyTypeRe.MatchString(typ) || !keyValueRe.MatchString(val) {
			c.warnOnce("key "+typ+" "+val, "a pushed SSH key is not a public key line (%q); skipped", typ)
			continue
		}
		id := typ + " " + val
		if want[id] {
			continue
		}
		want[id] = true
		line := id
		if cm := sanitizeComment(k.Comment); cm != "" {
			line += " " + cm
		}
		lines = append(lines, line)
	}
	have := map[string]bool{}
	for _, l := range st.keys {
		if f := strings.Fields(l); len(f) >= 2 {
			have[f[0]+" "+f[1]] = true
		}
	}
	if equalSets(have, want) {
		return 0, nil
	}
	if _, err := c.runner().Run(ctx, keysCommand, keysScript(lines)); err != nil {
		return 0, fmt.Errorf("sonic install ssh keys: %w", err)
	}
	c.mu.Lock()
	if c.state == st {
		next := st.clone()
		next.keys = append([]string(nil), lines...)
		c.state = next
	}
	c.mu.Unlock()
	return len(lines), nil
}

// sanitizeComment keeps a key comment to printable characters on one line.
func sanitizeComment(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// keysScript rewrites ~/.ssh/authorized_keys with the block replaced (or
// removed, for no keys), atomically.
func keysScript(lines []string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(`f="$HOME/.ssh/authorized_keys"` + "\n")
	b.WriteString(`mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh" && touch "$f"` + "\n")
	b.WriteString(`tmp=$(mktemp "$HOME/.ssh/.authorized_keys.XXXXXX")` + "\n")
	b.WriteString(`awk '$0=="` + keysBegin + `"{skip=1;next} $0=="` + keysEnd + `"{skip=0;next} !skip' "$f" > "$tmp"` + "\n")
	if len(lines) > 0 {
		b.WriteString(`cat >> "$tmp" <<'DISUNIFIED_KEYS'` + "\n")
		b.WriteString(keysBegin + "\n")
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
		b.WriteString(keysEnd + "\n")
		b.WriteString("DISUNIFIED_KEYS\n")
	}
	b.WriteString(`chmod 600 "$tmp" && mv -f "$tmp" "$f"` + "\n")
	return b.String()
}

// Reboot implements devicemodel.Rebooter (only with control.reboot).
func (c *Collector) Reboot(ctx context.Context) error {
	c.Log.Printf("sonic: rebooting the switch at the controller's request")
	_, err := c.runner().Run(ctx, "sudo reboot", "")
	return err
}
