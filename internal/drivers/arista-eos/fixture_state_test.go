package aristaeos

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The fixture transport's writes, held to the switch: the commands of the
// write-forms capture (docs/fixtures/arista-eos-4.26.14M/write-forms/),
// written to the poll capture, must read back as the switch printed them.

func readJSON(t *testing.T, ft *FixtureTransport, cmd string) map[string]any {
	t.Helper()
	out, err := ft.Run(context.Background(), []string{cmd})
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(out[0], &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func captured(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(aclFixtureDir, "write-forms", name))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func path(v map[string]any, keys ...string) any {
	var cur any = v
	for _, k := range keys {
		m, _ := cur.(map[string]any)
		cur = m[k]
	}
	return cur
}

func configureAll(t *testing.T, ft *FixtureTransport, lines ...string) {
	t.Helper()
	if err := ft.Configure(context.Background(), append(append([]string{"enable", "configure"}, lines...), "end", "write memory")); err != nil {
		t.Fatal(err)
	}
}

func rcKeys(v map[string]any, section string) []string {
	var keys []string
	for k := range path(v, "cmds", section, "cmds").(map[string]any) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestFixtureWritesReadBackAsCaptured(t *testing.T) {
	ft := NewFixtureTransport(aclFixtureDir)
	configureAll(t, ft, "interface Ethernet2",
		"description dui-fixture-test",
		"switchport trunk native vlan 2",
		"switchport trunk allowed vlan 1-2,8-10,1000",
		"spanning-tree portfast",
		"spanning-tree bpdufilter enable",
		"speed forced 1000full")

	// The description as written (the capture's value is scrubbed; its
	// running config carries the text).
	if d := path(readJSON(t, ft, "show interfaces"), "interfaces", "Ethernet2", "description"); d != "dui-fixture-test" {
		t.Errorf("description %v", d)
	}
	if _, ok := path(captured(t, "show-running-config-written.json"), "cmds", "interface Ethernet2", "cmds").(map[string]any)["description dui-fixture-test"]; !ok {
		t.Error("the capture's running config has no `description dui-fixture-test`")
	}
	sw := func(v map[string]any) map[string]any {
		info := path(v, "switchports", "Ethernet2", "switchportInfo").(map[string]any)
		out := map[string]any{}
		for _, k := range []string{"mode", "accessVlanId", "accessVlanName", "trunkingNativeVlanId", "trunkingNativeVlanName", "trunkAllowedVlans"} {
			out[k] = info[k]
		}
		return out
	}
	if got, want := sw(readJSON(t, ft, "show interfaces switchport")), sw(captured(t, "show-interfaces-Ethernet2-switchport-trunk.json")); !reflect.DeepEqual(got, want) {
		t.Errorf("trunk: %v, captured %v", got, want)
	}
	as := func(v map[string]any) any { return path(v, "interfaces", "Ethernet2", "adminSpeed") }
	if got, want := as(readJSON(t, ft, "show interfaces transceiver properties")), as(captured(t, "show-interfaces-Ethernet2-transceiver-properties-1g.json")); !reflect.DeepEqual(got, want) {
		t.Errorf("1G: %v, captured %v", got, want)
	}

	configureAll(t, ft, "interface Ethernet2", "switchport mode access", "switchport access vlan 2")
	if got, want := sw(readJSON(t, ft, "show interfaces switchport")), sw(captured(t, "show-interfaces-Ethernet2-switchport-access.json")); !reflect.DeepEqual(got, want) {
		t.Errorf("access: %v, captured %v", got, want)
	}

	configureAll(t, ft, "interface Ethernet54/1", "error-correction encoding fire-code")
	rc := readJSON(t, ft, "show running-config")
	if _, ok := path(rc, "cmds", "interface Ethernet54/1", "cmds").(map[string]any)["error-correction encoding fire-code"]; !ok {
		t.Errorf("fire-code: %v", rcKeys(rc, "interface Ethernet54/1"))
	}
	if !reflect.DeepEqual(rcKeys(captured(t, "show-running-config-fire-code.json"), "interface Ethernet54/1"), []string{"error-correction encoding fire-code", "mtu 1500", "speed forced 100gfull", "switchport mode trunk"}) {
		t.Error("the fire-code capture changed")
	}

	configureAll(t, ft, "interface Ethernet54/1", "no error-correction encoding",
		"ip access-list DUI_FIXTURE_PROBE",
		"10 deny tcp 203.0.113.0/24 198.51.100.0/24 eq 9",
		"20 deny udp 203.0.113.0/24 198.51.100.0/24 eq 9",
		"30 deny tcp 203.0.113.0/24 198.51.100.0/24 eq 1001",
		"40 permit tcp any host 192.0.2.1 eq 443",
		"50 permit tcp any host 192.0.2.1 eq 22",
		"60 deny tcp 203.0.113.0/24 eq 9 192.0.2.0/24",
		"70 deny udp 198.51.100.0/24 host 192.0.2.1 eq 68",
		"80 permit ip 192.0.2.0/24 198.51.100.0/24",
		"90 deny ip any 198.51.100.0/24",
		"exit",
		"ntp server 0.ubnt.pool.ntp.org")
	rc = readJSON(t, ft, "show running-config")
	want := captured(t, "show-running-config-written.json")
	if got, w := rcKeys(rc, "ip access-list DUI_FIXTURE_PROBE"), rcKeys(want, "ip access-list DUI_FIXTURE_PROBE"); !reflect.DeepEqual(got, w) {
		t.Errorf("probe list:\n%q\ncaptured:\n%q", got, w)
	}
	for _, sec := range []string{"interface Ethernet2", "interface Ethernet54/1"} {
		have := map[string]bool{}
		for _, k := range rcKeys(rc, sec) {
			have[k] = true
		}
		for _, k := range rcKeys(want, sec) {
			// Lines the transport does not keep: the driver reads the
			// description and the VLANs from their own replies.
			if strings.HasPrefix(k, "description ") || strings.HasPrefix(k, "switchport ") {
				continue
			}
			if !have[k] {
				t.Errorf("%s: no %q (have %v)", sec, k, rcKeys(rc, sec))
			}
		}
	}
	if _, ok := path(rc, "cmds").(map[string]any)["ntp server 0.ubnt.pool.ntp.org"]; !ok {
		t.Error("ntp server by name")
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("unmodelled: %q", ft.Unmodelled)
	}

	// Undone as the run undid them.
	configureAll(t, ft, "no ip access-list DUI_FIXTURE_PROBE", "no ntp server 0.ubnt.pool.ntp.org",
		"interface Ethernet2", "no description", "switchport mode trunk", "no switchport trunk native vlan",
		"switchport trunk allowed vlan all", "no spanning-tree portfast", "no spanning-tree bpdufilter", "speed auto")
	if got, w := sw(readJSON(t, ft, "show interfaces switchport")), sw(captured(t, "show-interfaces-Ethernet2-switchport-undone.json")); got["trunkAllowedVlans"] != w["trunkAllowedVlans"] || got["trunkingNativeVlanId"] != w["trunkingNativeVlanId"] || got["mode"] != w["mode"] {
		t.Errorf("undone: %v, captured %v", got, w)
	}
	if got, w := as(readJSON(t, ft, "show interfaces transceiver properties")), as(captured(t, "show-interfaces-Ethernet2-transceiver-properties-undone.json")); !reflect.DeepEqual(got, w) {
		t.Errorf("auto: %v, captured %v", got, w)
	}
	rc = readJSON(t, ft, "show running-config")
	if _, ok := path(rc, "cmds").(map[string]any)["ip access-list DUI_FIXTURE_PROBE"]; ok {
		t.Error("the probe list is still there")
	}
	if got := rcKeys(rc, "interface Ethernet2"); !reflect.DeepEqual(got, []string{"speed auto", "switchport mode trunk"}) {
		t.Errorf("Ethernet2 after the undo: %q", got)
	}
}

// A session is applied at its timed commit, kept by the confirm, and
// reverted by an abort; one never committed changes nothing.
func TestFixtureSessions(t *testing.T) {
	ft := NewFixtureTransport(aclFixtureDir)
	ctx := context.Background()
	bound := func() bool {
		_, ok := path(readJSON(t, ft, "show running-config"), "cmds", "interface Ethernet2", "cmds").(map[string]any)["ip access-group DUI_IP_ACL_Ethernet2 in"]
		return ok
	}
	body := []string{"ip access-list DUI_IP_ACL_Ethernet2", "10 remark disunified default", "20 permit ip any any", "exit", "interface Ethernet2", "ip access-group DUI_IP_ACL_Ethernet2 in", "exit"}
	_ = ft.Configure(ctx, append([]string{"enable", "configure session a"}, body...))
	if bound() {
		t.Fatal("an uncommitted session changed the running config")
	}
	_ = ft.Configure(ctx, []string{"enable", "configure session a abort"})
	_ = ft.Configure(ctx, append(append([]string{"enable", "configure session b"}, body...), "commit timer 00:02:00"))
	if !bound() {
		t.Fatal("the timed commit is not applied")
	}
	_ = ft.Configure(ctx, []string{"enable", "configure session b abort"})
	if bound() {
		t.Fatal("the abort did not revert the pending session")
	}
	_ = ft.Configure(ctx, append(append([]string{"enable", "configure session c"}, body...), "commit timer 00:02:00"))
	_ = ft.Configure(ctx, []string{"enable", "configure session c commit", "write memory"})
	_ = ft.Configure(ctx, []string{"enable", "configure session c abort"})
	if !bound() {
		t.Fatal("a confirmed session was reverted")
	}
}

// Stateless serves the captures whatever is written; a reply nothing wrote
// to is the capture byte for byte.
func TestFixtureUntouchedRepliesAreTheCapture(t *testing.T) {
	ft := NewFixtureTransport(aclFixtureDir)
	configureAll(t, ft, "interface Ethernet2", "description x")
	out, err := ft.Run(context.Background(), []string{"show vlan"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(aclFixtureDir, "show-vlan.json"))
	if string(out[0]) != string(raw) {
		t.Error("show vlan, never written to, is not the capture")
	}
	st := NewFixtureTransport(aclFixtureDir)
	st.Stateless = true
	configureAll(t, st, "interface Ethernet2", "description x")
	if d := path(readJSON(t, st, "show interfaces"), "interfaces", "Ethernet2", "description"); d != "" {
		t.Errorf("stateless: description %v", d)
	}
}
