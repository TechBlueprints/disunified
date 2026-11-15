package aristaeos

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const warnFixtures = aclFixtureDir + "/acl-commit-warning"

func readWarnFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(warnFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The commit's warnings as eAPI returns them (in each command's result)
// and as the CLI prints them (`! ` lines over SSH) are the same texts.
func TestCommitWarningsFromBothTransports(t *testing.T) {
	eapi := resultWarnings([]json.RawMessage{
		json.RawMessage("{}"),
		readWarnFixture(t, "eapi-result-ip-access-group.json"),
		json.RawMessage("{}"),
		readWarnFixture(t, "eapi-result-commit-timer.json"),
	})
	cli := cliWarnings(string(readWarnFixture(t, "ssh-session.txt")))
	if len(eapi) != 2 || strings.Join(eapi, "\n--\n") != strings.Join(cli, "\n--\n") {
		t.Fatalf("eAPI warnings %q\nCLI warnings %q", eapi, cli)
	}
	want := "Error in ACL commit, configuration may differ from hardware (Insufficient HW resources on Chip-0,dscp not supported)"
	if got := aclCommitError(eapi); got != want {
		t.Errorf("commit error %q", got)
	}
	if aclCommitError(eapi[:1]) != "" {
		t.Error("the bind's data-plane warning is not a commit error")
	}
}

// A commit EOS answers with the warning is aborted at once, never
// confirmed, and the same change is held with the reason, naming the lists
// it wrote; other rules are sent again.
func TestApplyACLAbortsAnUnprogrammedCommit(t *testing.T) {
	c, ft := aclCollector(t, nil)
	var res struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(readWarnFixture(t, "eapi-result-commit-timer.json"), &res); err != nil {
		t.Fatal(err)
	}
	ft.Warnings = map[string][]string{"commit timer " + commitTimer: res.Warnings}
	ctx := context.Background()
	before := len(ft.Configured)
	_, err := c.applyACL(ctx, c.last, sampleWant())
	if err == nil || !strings.Contains(err.Error(), "EOS could not program the ACL change (Error in ACL commit") ||
		!strings.Contains(err.Error(), "lists written: DUI_IP_ACL_Ethernet1, ") || !strings.Contains(err.Error(), " aborted") {
		t.Fatalf("err = %v", err)
	}
	got := ft.Configured[before:]
	if len(got) != 2 || strings.Join(got[1], " / ") != "enable / configure session "+aclSession+"-t abort" {
		t.Fatalf("batches: %d, last %v; want the session then its abort", len(got), got[len(got)-1])
	}
	snap, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.acl.lists) != 0 {
		t.Errorf("after the abort the switch has %d of the driver's lists", len(c.acl.lists))
	}
	before = len(ft.Configured)
	_, err = c.applyACL(ctx, snap, sampleWant())
	if err == nil || !strings.Contains(err.Error(), "holding the ACL change (EOS could not program") || len(ft.Configured) != before {
		t.Errorf("the same rules again: err=%v, batches %d", err, len(ft.Configured)-before)
	}
	ft.Warnings = nil
	other := sampleWant()
	other.rules = []string{"deny tcp 203.0.113.0/24 192.0.2.0/24 eq 22"}
	if n, err := c.applyACL(ctx, snap, other); err != nil || n == 0 || c.aclHeld != "" {
		t.Errorf("other rules: n=%d err=%v held=%q", n, err, c.aclHeld)
	}
}

// The summary's switch-wide warning reaches the read-back status: the list
// itself still reads active there.
func TestACLStatusCarriesTheSummaryWarning(t *testing.T) {
	var sum showIPACLSummary
	readACLFixture(t, "acl-commit-warning/show-ip-access-lists-summary-not-programmed.json", &sum)
	st := newACLState()
	st.lists["DUI_IP_ACL_Ethernet2"] = aclList{}
	if got := aclStatus(sum, st); !strings.Contains(got, "EOS: Not all ACLs are programmed in the hardware!") {
		t.Errorf("status %q", got)
	}
	if got := aclStatus(sum, newACLState()); got != "" {
		t.Errorf("without lists of the driver's: %q", got)
	}
}
