package sonic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/TechBlueprints/disunified/internal/sshrun"
)

// FixtureRunner serves a captured collector run (docs/fixtures/sonic-*/
// collect.txt, collect.py's output scrubbed by scripts/sanitize-sonic.py)
// instead of a switch. `sudo config <group> --help` is answered from the
// captured help texts (config-help.txt beside the capture). Every write --
// one per line of a `bash -s` batch -- is recorded in Commands and applied
// to the capture's CONFIG_DB sections, so the next Collect reads it back,
// and the refusals SONiC itself makes are modelled as errors (a second
// untagged VLAN, VLAN config on a LAG member or a mirror destination, a
// missing VLAN or PortChannel), so a wrong order fails a test.
type FixtureRunner struct {
	Fixture  string
	Help     map[string]string // "config interface --help" -> output
	Commands []string          // every write, in order, as sent (with sudo)
	Helps    []string          // the help commands Start ran

	KeyScripts []string // the authorized_keys rewrites, as sent
	// DialFails makes a runner from Dial fail its first command, as a
	// switch that does not answer at the new address would.
	DialFails bool
	Dials     []string // the addresses Dial was asked for

	// promote is net.ipv4.conf.<iface>.promote_secondaries as written by the
	// driver (0 on SONiC by default: removing a subnet's primary address
	// removes its secondaries too). IgnorePromote makes the write a no-op,
	// for the path where the switch loses the new address anyway.
	promote       map[string]bool
	IgnorePromote bool

	// Zones answers zonesCommand: the switch's zone list with each zone's
	// POSIX rule (zones.txt beside the capture, through sanitize-sonic.py).
	Zones     string
	ZoneReads int // how often the zone list was read
	// LEDWorks models a platform whose API sets the chassis status LED (the
	// DX010); otherwise LED writes fail as on the VS, whose platform API
	// raises FileNotFoundError for its chassis metadata. LED is the colour
	// last set.
	LEDWorks bool
	LED      string

	mu sync.Mutex // Run may be called from the locate blink's goroutine
}

// Recorded is a copy of Commands, safe while a blink is running.
func (f *FixtureRunner) Recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Commands...)
}

var (
	promoteRe = regexp.MustCompile(`^echo 1 \| sudo tee /proc/sys/net/ipv4/conf/(\S+)/promote_secondaries >/dev/null$`)
	verifyRe  = regexp.MustCompile(`^ip -4 -o addr show dev (\S+) \| grep -q ' (\S+)/(\d+) '$`)
	ledRe     = regexp.MustCompile(`^sudo python3 -c 'import sys; from sonic_platform\.platform import Platform; sys\.exit\(0 if Platform\(\)\.get_chassis\(\)\.set_status_led\("(green|off)"\) else 1\)'$`)
)

// Dial is the fixture's Collector.Redial: a runner for the switch at host,
// sharing this capture (it is the same switch).
func (f *FixtureRunner) Dial(host string) sshrun.Runner {
	f.Dials = append(f.Dials, host)
	return &dialled{f: f, host: host}
}

type dialled struct {
	f    *FixtureRunner
	host string
}

func (d *dialled) Run(ctx context.Context, command, stdin string) (string, error) {
	if command == "true" && d.f.DialFails {
		return "", fmt.Errorf("ssh dial %s:22: i/o timeout", d.host)
	}
	if command == "true" {
		return "", nil
	}
	return d.f.Run(ctx, command, stdin)
}

func (d *dialled) Close() error { return nil }

// NewFixtureRunner loads a capture file and, when present, the help texts
// beside it.
func NewFixtureRunner(path string) (*FixtureRunner, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := &FixtureRunner{Fixture: string(b), Help: map[string]string{}}
	if h, err := os.ReadFile(filepath.Join(filepath.Dir(path), "config-help.txt")); err == nil {
		for name, body := range sections(string(h)) {
			f.Help[name] = body
		}
	}
	if z, err := os.ReadFile(filepath.Join(filepath.Dir(path), "zones.txt")); err == nil {
		f.Zones = string(z)
	}
	return f, nil
}

// Run implements sshrun.Runner.
func (f *FixtureRunner) Run(_ context.Context, command, stdin string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case command == collectCommand:
		return f.Fixture, nil
	case command == zonesCommand:
		f.ZoneReads++
		if f.Zones == "" {
			return "", fmt.Errorf("timedatectl: no zones")
		}
		return f.Zones, nil
	case strings.HasPrefix(command, "sudo config ") && strings.HasSuffix(command, " --help"):
		f.Helps = append(f.Helps, command)
		body, ok := f.Help[strings.TrimPrefix(command, "sudo ")]
		if !ok {
			return "", fmt.Errorf("%s: no such command", command)
		}
		return body, nil
	case command == keysCommand:
		f.Commands = append(f.Commands, command)
		f.KeyScripts = append(f.KeyScripts, stdin)
		return "", f.applyKeys(stdin)
	case command == batchCommand:
		for _, line := range strings.Split(stdin, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || line == "set -e" {
				continue
			}
			if err := f.apply(line); err != nil {
				return "", fmt.Errorf("%s: %w", line, err) // set -e: the batch stops here
			}
		}
		return "", nil
	}
	return "", f.apply(command)
}

func (f *FixtureRunner) Close() error { return nil }

// --- the capture's CONFIG_DB ---

func (f *FixtureRunner) table(sec string) table {
	t, err := parseTable(sec, sections(f.Fixture)[sec])
	if err != nil || t == nil {
		return table{}
	}
	return t
}

// setTable re-renders a section in collect.py's layout.
func (f *FixtureRunner) setTable(sec string, t table) {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	if len(keys) == 0 {
		b.WriteString("{}\n")
	} else {
		b.WriteString("{\n")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(",\n")
			}
			kj, _ := json.Marshal(k)
			b.Write(kj)
			b.WriteString(": ")
			b.WriteString(pyDict(t[k]))
		}
		b.WriteString("\n}\n")
	}
	head := "@@@ " + sec + "\n"
	i := strings.Index(f.Fixture, head)
	if i < 0 {
		return
	}
	rest := f.Fixture[i+len(head):]
	j := strings.Index(rest, "@@@ ")
	if j < 0 {
		return
	}
	f.Fixture = f.Fixture[:i] + head + b.String() + rest[j:]
}

// pyDict renders a field map as Python's json.dumps(sort_keys=True) does.
func pyDict(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		kj, _ := json.Marshal(k)
		vj, _ := json.Marshal(m[k])
		parts = append(parts, string(kj)+": "+string(vj))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func (f *FixtureRunner) update(sec string, fn func(table) error) error {
	t := f.table(sec)
	if err := fn(t); err != nil {
		return err
	}
	f.setTable(sec, t)
	return nil
}

func (f *FixtureRunner) hasPort(p string) bool { _, ok := f.table("cfg_port")["PORT|"+p]; return ok }

func (f *FixtureRunner) hasVLANConfig(iface string) bool {
	for k := range f.table("cfg_vlan_member") {
		if strings.HasSuffix(k, "|"+iface) {
			return true
		}
	}
	return false
}

func (f *FixtureRunner) lagOf(iface string) string {
	for k := range f.table("cfg_lag_member") {
		parts := strings.Split(k, "|")
		if len(parts) == 3 && parts[2] == iface {
			return parts[1]
		}
	}
	return ""
}

// lagMTU is a PortChannel's MTU (SONiC's default 9100 when unset).
func (f *FixtureRunner) lagMTU(pc string) string {
	if m := f.table("cfg_lag")["PORTCHANNEL|"+pc]["mtu"]; m != "" {
		return m
	}
	return "9100"
}

// portMTU is a port's MTU: CONFIG_DB's, else APPL_DB's (the L2 preset sets none).
func (f *FixtureRunner) portMTU(p string) string {
	if m := f.table("cfg_port")["PORT|"+p]["mtu"]; m != "" {
		return m
	}
	if m := f.table("appl_port")["PORT_TABLE:"+p]["mtu"]; m != "" {
		return m
	}
	return "9100"
}

func (f *FixtureRunner) isMirrorDst(iface string) bool {
	for _, v := range f.table("cfg_mirror") {
		if v["dst_port"] == iface {
			return true
		}
	}
	return false
}

// apply records one write and applies it to the capture.
func (f *FixtureRunner) apply(line string) error {
	f.Commands = append(f.Commands, line)
	if m := promoteRe.FindStringSubmatch(line); m != nil {
		if !f.IgnorePromote {
			if f.promote == nil {
				f.promote = map[string]bool{}
			}
			f.promote[m[1]] = true
		}
		return nil
	}
	if m := ledRe.FindStringSubmatch(line); m != nil {
		if !f.LEDWorks {
			return fmt.Errorf("exit status 1: Traceback (most recent call last): ... FileNotFoundError: Metadata file /etc/sonic/vs_chassis_metadata.json not found")
		}
		f.LED = m[1]
		return nil
	}
	if m := verifyRe.FindStringSubmatch(line); m != nil {
		if !f.hasKernelAddress(m[1], m[2], m[3]) {
			return fmt.Errorf("exit status 1") // grep -q found nothing
		}
		return nil
	}
	w := shellWords(line)
	if len(w) > 0 && w[0] == "sudo" {
		w = w[1:]
	}
	at := func(i int) string {
		if i < len(w) {
			return w[i]
		}
		return ""
	}
	cmd := strings.Join(w[:min(len(w), 3)], " ")
	switch {
	case len(w) >= 2 && w[0] == "config" && w[1] == "save":
		return nil
	case len(w) == 1 && (w[0] == "reboot" || w[0] == "true"):
		return nil
	case strings.HasPrefix(cmd, "config ntp") || strings.HasPrefix(cmd, "config syslog") || cmd == "config snmp community" ||
		cmd == "config dns nameserver" || strings.HasPrefix(cmd, "config route") || cmd == "config interface ip" ||
		(len(w) >= 3 && w[0] == "config" && w[1] == "spanning-tree" && w[2] != "interface"):
		return f.applyDevice(w)

	case len(w) >= 5 && w[0] == "sonic-db-cli" && w[1] == "CONFIG_DB" && (w[2] == "hset" || w[2] == "hdel"):
		key := w[3]
		if pc, ok := strings.CutPrefix(key, "PORTCHANNEL|"); ok {
			// The probed way to set a PortChannel's MTU, and the LACP fields
			// the driver brings in line on a PortChannel made without them.
			fv := w[4:]
			if w[2] != "hset" || len(fv)%2 != 0 {
				return fmt.Errorf("unmodelled PORTCHANNEL write")
			}
			for i := 0; i < len(fv); i += 2 {
				if fv[i] != "mtu" && fv[i] != "fast_rate" && fv[i] != "min_links" {
					return fmt.Errorf("unmodelled PORTCHANNEL field %s", fv[i])
				}
			}
			return f.update("cfg_lag", func(t table) error {
				if t[key] == nil {
					return fmt.Errorf("%s does not exist", pc)
				}
				for i := 0; i < len(fv); i += 2 {
					t[key][fv[i]] = fv[i+1]
				}
				return nil
			})
		}
		if !strings.HasPrefix(key, "PORT|") || !f.hasPort(strings.TrimPrefix(key, "PORT|")) {
			return fmt.Errorf("unexpected key %q", key)
		}
		return f.update("cfg_port", func(t table) error {
			if w[2] == "hset" {
				t[key][w[4]] = at(5)
			} else {
				delete(t[key], w[4])
			}
			return nil
		})

	case cmd == "config interface startup" || cmd == "config interface shutdown":
		return f.portField(at(3), "admin_status", map[string]string{"startup": "up", "shutdown": "down"}[w[2]])
	case cmd == "config interface speed":
		if err := f.laneSpeedOK(at(3), at(4)); err != nil {
			return err
		}
		return f.portField(at(3), "speed", at(4))
	case cmd == "config interface breakout":
		return f.breakout(w[3:])
	case cmd == "config clock timezone":
		return f.timezone(at(3))
	case len(w) == 3 && w[0] == "config" && w[1] == "hostname":
		return f.hostname(w[2])
	case cmd == "config interface fec":
		return f.portField(at(3), "fec", at(4))
	case cmd == "config interface autoneg":
		return f.portField(at(3), "autoneg", map[string]string{"enabled": "on", "disabled": "off"}[at(4)])
	case cmd == "config interface mtu":
		if strings.HasPrefix(at(3), "PortChannel") {
			return fmt.Errorf("Invalid port %s", at(3)) // probed: the PortChannel's MTU is a CONFIG_DB write
		}
		if f.lagOf(at(3)) != "" {
			return fmt.Errorf("Error: 'interface_name' is in portchannel!") // probed: any value, any member
		}
		return f.portField(at(3), "mtu", at(4))

	case cmd == "config vlan add":
		ids := []string{at(3)}
		if at(3) == "-m" {
			ids = strings.Split(at(4), ",")
		}
		return f.update("cfg_vlan", func(t table) error {
			for _, id := range ids {
				k := "VLAN|Vlan" + id
				if _, ok := t[k]; ok {
					return fmt.Errorf("Vlan%s already exists", id)
				}
				t[k] = map[string]string{"vlanid": id}
			}
			return nil
		})

	case cmd == "config vlan member":
		return f.vlanMember(w[3:])

	case cmd == "config portchannel add":
		// sonic-utilities 202505 add_portchannel: --min-links defaults to
		// 1 (range 1..1024), --fast-rate to false (true|false), both written.
		fields := map[string]string{"admin_status": "up", "lacp_key": "auto", "fast_rate": "false", "min_links": "1", "mtu": "9100"}
		for rest := w[4:]; len(rest) > 0; rest = rest[2:] {
			if len(rest) < 2 {
				return fmt.Errorf("option %s requires an argument", rest[0])
			}
			switch v := rest[1]; rest[0] {
			case "--fast-rate":
				if v = strings.ToLower(v); v != "true" && v != "false" {
					return fmt.Errorf("invalid value for --fast-rate: %s", rest[1])
				}
				fields["fast_rate"] = v
			case "--min-links":
				if n := atoi(v); n < 1 || n > 1024 {
					return fmt.Errorf("invalid value for --min-links: %s", v)
				}
				fields["min_links"] = v
			default:
				return fmt.Errorf("no such option: %s", rest[0])
			}
		}
		return f.update("cfg_lag", func(t table) error {
			k := "PORTCHANNEL|" + at(3)
			if _, ok := t[k]; ok {
				return fmt.Errorf("%s already exists", at(3))
			}
			t[k] = fields
			return nil
		})
	case cmd == "config portchannel del":
		pc := at(3)
		for k := range f.table("cfg_lag_member") {
			if strings.Contains(k, "|"+pc+"|") {
				return fmt.Errorf("%s has members", pc)
			}
		}
		if f.hasVLANConfig(pc) {
			return fmt.Errorf("%s has vlan config", pc)
		}
		return f.update("cfg_lag", func(t table) error { delete(t, "PORTCHANNEL|"+pc); return nil })
	case cmd == "config portchannel member":
		pc, p := at(4), at(5)
		if _, ok := f.table("cfg_lag")["PORTCHANNEL|"+pc]; !ok {
			return fmt.Errorf("%s does not exist", pc)
		}
		k := "PORTCHANNEL_MEMBER|" + pc + "|" + p
		switch at(3) {
		case "add":
			if !f.hasPort(p) {
				return fmt.Errorf("%s is not a port", p)
			}
			if f.hasVLANConfig(p) {
				return fmt.Errorf("%s Interface configured as VLAN_MEMBER under vlan", p)
			}
			if cur := f.lagOf(p); cur != "" {
				return fmt.Errorf("%s is already a member of %s", p, cur)
			}
			if f.portMTU(p) != f.lagMTU(pc) {
				return fmt.Errorf("Port MTU of %s is different than the %s MTU size", p, pc) // seen live, 2026-10-27
			}
			return f.update("cfg_lag_member", func(t table) error { t[k] = map[string]string{"NULL": "NULL"}; return nil })
		case "del":
			if _, ok := f.table("cfg_lag_member")[k]; !ok {
				return fmt.Errorf("%s is not a member of %s", p, pc)
			}
			return f.update("cfg_lag_member", func(t table) error { delete(t, k); return nil })
		}

	case len(w) >= 7 && cmd == "config mirror_session span" && w[3] == "add":
		name, dst, src, dir := w[4], w[5], w[6], at(7)
		if _, ok := f.table("cfg_mirror")["MIRROR_SESSION|"+name]; ok {
			return fmt.Errorf("session %s already exists", name)
		}
		if f.hasVLANConfig(dst) {
			return fmt.Errorf("Error: Destination Interface %s has vlan config", dst)
		}
		if f.lagOf(dst) != "" {
			return fmt.Errorf("Error: Destination Interface %s is a portchannel member", dst)
		}
		return f.update("cfg_mirror", func(t table) error {
			t["MIRROR_SESSION|"+name] = map[string]string{"type": "SPAN", "dst_port": dst, "src_port": src, "direction": strings.ToUpper(dir)}
			return nil
		})
	case cmd == "config mirror_session remove":
		k := "MIRROR_SESSION|" + at(3)
		if _, ok := f.table("cfg_mirror")[k]; !ok {
			return fmt.Errorf("session %s does not exist", at(3))
		}
		return f.update("cfg_mirror", func(t table) error { delete(t, k); return nil })

	case cmd == "config interface storm-control":
		p, typ := at(4), at(5)
		if !f.hasPort(p) {
			return fmt.Errorf("%s is not a port", p)
		}
		k := "PORT_STORM_CONTROL|" + p + "|" + typ
		return f.update("cfg_storm", func(t table) error {
			switch at(3) {
			case "add":
				if _, err := strconv.Atoi(at(6)); err != nil {
					return fmt.Errorf("bad kbps %q", at(6))
				}
				t[k] = map[string]string{"kbps": at(6)}
			case "del":
				if _, ok := t[k]; !ok {
					return fmt.Errorf("no %s storm control on %s", typ, p)
				}
				delete(t, k)
			}
			return nil
		})

	case cmd == "config spanning-tree interface":
		if _, ok := f.table("cfg_feature")["FEATURE|stp"]; !ok {
			return fmt.Errorf("STP is not available on this image")
		}
		var field, value, p string
		switch at(3) {
		case "bpdu_guard", "portfast":
			field, value, p = at(3), strconv.FormatBool(at(4) == "enable"), at(5)
		case "enable", "disable":
			field, value, p = "enabled", strconv.FormatBool(at(3) == "enable"), at(4)
		default:
			return fmt.Errorf("unmodelled: %s", line)
		}
		return f.update("cfg_stp", func(t table) error {
			k := "STP_PORT|" + p
			if t[k] == nil {
				t[k] = map[string]string{}
			}
			t[k][field] = value
			return nil
		})
	}
	return fmt.Errorf("unmodelled write: %s", line)
}

func (f *FixtureRunner) portField(p, field, value string) error {
	if !f.hasPort(p) {
		return fmt.Errorf("Interface name %s is invalid", p)
	}
	if value == "" {
		return fmt.Errorf("bad value for %s", field)
	}
	return f.update("cfg_port", func(t table) error { t["PORT|"+p][field] = value; return nil })
}

// vlanMember: add|del [-u] [-m] [-e] <vid or list> <port>. -m takes a
// comma list of VLANs for the one port; -e turns the list into "every
// existing VLAN except these". Each VLAN is applied as the single form
// would be, so the same refusals hold.
func (f *FixtureRunner) vlanMember(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage")
	}
	op := args[0]
	untagged, multi, except := false, false, false
	rest := args[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		switch rest[0] {
		case "-u":
			untagged = true
		case "-m":
			multi = true
		case "-e":
			except = true
		default:
			return fmt.Errorf("unknown flag %s", rest[0])
		}
		rest = rest[1:]
	}
	if len(rest) != 2 {
		return fmt.Errorf("usage")
	}
	port := rest[1]
	ids := []string{rest[0]}
	if multi {
		ids = strings.Split(rest[0], ",")
	} else if strings.Contains(rest[0], ",") {
		return fmt.Errorf("a VLAN list needs -m")
	}
	if except {
		if !multi {
			return fmt.Errorf("-e needs -m")
		}
		skip := map[string]bool{}
		for _, id := range ids {
			skip[id] = true
		}
		ids = nil
		for k := range f.table("cfg_vlan") {
			if id := strings.TrimPrefix(k, "VLAN|Vlan"); !skip[id] {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
	}
	for _, vid := range ids {
		if err := f.vlanMemberOne(op, vid, port, untagged); err != nil {
			return err
		}
	}
	return nil
}

func (f *FixtureRunner) vlanMemberOne(op, vid, port string, untagged bool) error {
	if _, ok := f.table("cfg_vlan")["VLAN|Vlan"+vid]; !ok {
		return fmt.Errorf("Vlan%s does not exist", vid)
	}
	k := "VLAN_MEMBER|Vlan" + vid + "|" + port
	members := f.table("cfg_vlan_member")
	switch op {
	case "add":
		if !f.hasPort(port) && !strings.HasPrefix(port, "PortChannel") {
			return fmt.Errorf("%s does not exist", port)
		}
		if strings.HasPrefix(port, "PortChannel") {
			if _, ok := f.table("cfg_lag")["PORTCHANNEL|"+port]; !ok {
				return fmt.Errorf("%s does not exist", port)
			}
		}
		if f.lagOf(port) != "" {
			return fmt.Errorf("%s is part of portchannel!", port)
		}
		if f.isMirrorDst(port) {
			return fmt.Errorf("%s is configured as mirror destination port", port)
		}
		if _, ok := members[k]; ok {
			return fmt.Errorf("%s is already a member of Vlan%s", port, vid)
		}
		if untagged {
			for mk, mv := range members {
				if strings.HasSuffix(mk, "|"+port) && mv["tagging_mode"] == "untagged" {
					return fmt.Errorf("%s is already untagged member!", port)
				}
			}
		}
		mode := "tagged"
		if untagged {
			mode = "untagged"
		}
		return f.update("cfg_vlan_member", func(t table) error { t[k] = map[string]string{"tagging_mode": mode}; return nil })
	case "del":
		if _, ok := members[k]; !ok {
			return fmt.Errorf("%s is not a member of Vlan%s", port, vid)
		}
		return f.update("cfg_vlan_member", func(t table) error { delete(t, k); return nil })
	}
	return fmt.Errorf("usage")
}

// shellWords splits a command line the way the shell would for the forms
// the driver writes: plain words and single-quoted strings (sshrun.ShellQuote's
// '\” escape included).
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	inWord, inQuote := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case inQuote:
			if ch == '\'' {
				inQuote = false
			} else {
				cur.WriteByte(ch)
			}
		case ch == '\'':
			inQuote, inWord = true, true
		case ch == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case ch == ' ' || ch == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(ch)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}
