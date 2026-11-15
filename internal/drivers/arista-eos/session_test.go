package aristaeos

import (
	"context"
	"strings"
	"testing"
)

// The tests in this package expect fixed session names: the suffix is "t".
func init() { sessionSuffix = func() string { return "t" } }

// TestSessionNamesDifferPerChange: EOS keeps the last completed session by
// name and will not enter it again ("Cannot enter session dui-acl, it is
// already complete", the deployed bridge's log on 2026-11-14, when the
// controller removed the IP rule it had applied three minutes before). Two
// changes in a row must use two names, and each change's commit timer,
// confirm and abort must name its own session.
func TestSessionNamesDifferPerChange(t *testing.T) {
	saved := sessionSuffix
	defer func() { sessionSuffix = saved }()
	sessionSuffix = timeSessionSuffix // the real one

	a, b := sessionName(aclSession), sessionName(aclSession)
	if a == b || !strings.HasPrefix(a, aclSession+"-") || !strings.HasPrefix(b, aclSession+"-") {
		t.Fatalf("two changes got %q and %q", a, b)
	}

	// Both changes through the driver (the IP rules written, then removed,
	// as live): each confirm names the session its batch opened, and the
	// second change opens another.
	fresh := &answering{FixtureTransport: NewFixtureTransport(aclFixtureDir)}
	c, ft := aclCollector(t, func(string) Transport { return fresh })
	if _, err := c.applyACL(context.Background(), c.last, sampleWant()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.applyACL(context.Background(), c.last, aclWant{}); err != nil {
		t.Fatal(err)
	}
	var opened []string
	for _, batch := range ft.Configured {
		if len(batch) > 1 && strings.HasPrefix(batch[1], "configure session "+aclSession+"-") && batch[len(batch)-1] == "commit timer "+commitTimer {
			opened = append(opened, strings.TrimPrefix(batch[1], "configure session "))
		}
	}
	if len(opened) != 2 || opened[0] == opened[1] {
		t.Fatalf("sessions opened: %q, want two different names", opened)
	}
	var confirmed []string
	for _, batch := range fresh.Configured {
		if len(batch) == 3 && strings.HasSuffix(batch[1], " commit") {
			confirmed = append(confirmed, strings.TrimSuffix(strings.TrimPrefix(batch[1], "configure session "), " commit"))
		}
	}
	if strings.Join(confirmed, ",") != strings.Join(opened, ",") {
		t.Errorf("confirmed %q, opened %q", confirmed, opened)
	}
}
