package aristaeos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The driver's session on the test ports (session-dui-acl-test-ports.txt),
// written to the poll capture, reads back as the switch printed both lists
// and the summary after it: show-ip-access-lists-DUI_IP_ACL_*.json (less
// each rule's ruleFilter, fixture_acl.go) and
// show-ip-access-lists-summary-dui-acl.json. The session's text and the
// JSON were scrubbed apart, so their addresses differ: they are masked.
func TestFixtureACLReadBackAsCaptured(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(aclFixtureDir, "session-dui-acl-test-ports.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var body []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		body = append(body, strings.TrimSpace(l))
	}
	ft := NewFixtureTransport(aclFixtureDir)
	if err := ft.Configure(t.Context(), append(append([]string{"enable", "configure session dui-acl-t"}, body...), "commit timer 00:02:00")); err != nil {
		t.Fatal(err)
	}
	if err := ft.Configure(t.Context(), []string{"enable", "configure session dui-acl-t commit", "write memory"}); err != nil {
		t.Fatal(err)
	}
	ip := regexp.MustCompile(`\b\d+\.\d+\.\d+\.\d+\b`)
	norm := func(v any) string {
		b, _ := json.Marshal(v)
		return ip.ReplaceAllString(string(b), "IP")
	}
	for _, name := range []string{"DUI_IP_ACL_Ethernet2", "DUI_IP_ACL_Ethernet54-1"} {
		got := readJSON(t, ft, "show ip access-lists "+name)
		var want map[string]any
		raw, err := os.ReadFile(filepath.Join(aclFixtureDir, "show-ip-access-lists-"+name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		for _, e := range path(want, "aclList").([]any)[0].(map[string]any)["sequence"].([]any) {
			delete(e.(map[string]any), "ruleFilter")
		}
		if norm(got) != norm(want) {
			t.Errorf("%s\n%s\ncaptured\n%s", name, norm(got), norm(want))
		}
	}
	got := readJSON(t, ft, "show ip access-lists summary")
	var want map[string]any
	readACLFixture(t, "show-ip-access-lists-summary-dui-acl.json", &want)
	if norm(got) != norm(want) {
		t.Errorf("summary\n%s\ncaptured\n%s", norm(got), norm(want))
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("unmodelled: %q", ft.Unmodelled)
	}

	// Unbound and removed: gone from the summary, the capture's own lists kept.
	configureAll(t, ft, "interface Ethernet2", "no ip access-group DUI_IP_ACL_Ethernet2 in", "exit", "no ip access-list DUI_IP_ACL_Ethernet2")
	var sum showIPACLSummary
	b, _ = json.Marshal(readJSON(t, ft, "show ip access-lists summary"))
	_ = json.Unmarshal(b, &sum)
	var names []string
	for _, a := range sum.ACLList {
		names = append(names, a.Name)
	}
	if strings.Join(names, " ") != "DUI_IP_ACL_Ethernet54-1 default-control-plane-acl mDNS-block" {
		t.Errorf("after the removal the summary lists %v", names)
	}
}
