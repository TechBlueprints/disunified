package sonic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
}

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
	return f, nil
}

// Run implements sshrun.Runner.
func (f *FixtureRunner) Run(_ context.Context, command, stdin string) (string, error) {
	switch {
	case command == collectCommand:
		return f.Fixture, nil
	case strings.HasPrefix(command, "sudo config ") && strings.HasSuffix(command, " --help"):
		f.Helps = append(f.Helps, command)
		body, ok := f.Help[strings.TrimPrefix(command, "sudo ")]
		if !ok {
			return "", fmt.Errorf("%s: no such command", command)
		}
		return body, nil
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

	case len(w) >= 5 && w[0] == "sonic-db-cli" && w[1] == "CONFIG_DB" && (w[2] == "hset" || w[2] == "hdel"):
		key := w[3]
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
		return f.portField(at(3), "speed", at(4))
	case cmd == "config interface fec":
		return f.portField(at(3), "fec", at(4))
	case cmd == "config interface autoneg":
		return f.portField(at(3), "autoneg", map[string]string{"enabled": "on", "disabled": "off"}[at(4)])
	case cmd == "config interface mtu":
		if f.lagOf(at(3)) != "" {
			return fmt.Errorf("Interface %s is a portchannel member", at(3))
		}
		return f.portField(at(3), "mtu", at(4))

	case cmd == "config vlan add":
		return f.update("cfg_vlan", func(t table) error {
			k := "VLAN|Vlan" + at(3)
			if _, ok := t[k]; ok {
				return fmt.Errorf("Vlan%s already exists", at(3))
			}
			t[k] = map[string]string{"vlanid": at(3)}
			return nil
		})

	case cmd == "config vlan member":
		return f.vlanMember(w[3:])

	case cmd == "config portchannel add":
		return f.update("cfg_lag", func(t table) error {
			k := "PORTCHANNEL|" + at(3)
			if _, ok := t[k]; ok {
				return fmt.Errorf("%s already exists", at(3))
			}
			t[k] = map[string]string{"admin_status": "up", "lacp_key": "auto", "min_links": "1", "mtu": "9100"}
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

// vlanMember: add [-u] <vid> <port> | del <vid> <port>.
func (f *FixtureRunner) vlanMember(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage")
	}
	op := args[0]
	untagged := false
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "-u" {
		untagged, rest = true, rest[1:]
	}
	if len(rest) != 2 {
		return fmt.Errorf("usage")
	}
	vid, port := rest[0], rest[1]
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
