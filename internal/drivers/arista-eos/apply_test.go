package aristaeos

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/switchmodel"
)

func startedCollector(t *testing.T) (*Collector, *fixtureTransport) {
	t.Helper()
	c, ft := newFixtureCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, ft
}

func TestApplyDisablesOnePort(t *testing.T) {
	c, ft := startedCollector(t)
	// Fixture: every port enabled, no descriptions (port 54 carries storm
	// control and BPDU guard, so it is left out here). Ask for port 2 down
	// and everything else unchanged (enabled, no description).
	var desired []switchmodel.PortDesired
	for i := 1; i <= 53; i++ {
		desired = append(desired, switchmodel.PortDesired{Index: i, Enabled: i != 2})
	}
	n, err := c.ApplyPorts(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("changed %d ports, want 1", n)
	}
	if len(ft.configured) != 1 {
		t.Fatalf("Configure called %d times", len(ft.configured))
	}
	got := strings.Join(ft.configured[0], "\n")
	want := "enable\nconfigure\ninterface Ethernet2\nshutdown\nend\nwrite memory"
	if got != want {
		t.Errorf("commands:\n%s\nwant:\n%s", got, want)
	}

	// Same request again: the collector remembers what it wrote, so nothing
	// is sent until the next poll proves otherwise.
	n, err = c.ApplyPorts(context.Background(), desired)
	if err != nil || n != 0 || len(ft.configured) != 1 {
		t.Errorf("second apply: n=%d err=%v configure calls=%d (want idempotent)", n, err, len(ft.configured))
	}
}

func TestApplyBreakoutWritesEveryLane(t *testing.T) {
	c, ft := startedCollector(t)
	n, err := c.ApplyPorts(context.Background(), []switchmodel.PortDesired{{Index: 50, Enabled: false}})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := strings.Join(ft.configured[0], "\n")
	for _, lane := range []string{"Ethernet50/1", "Ethernet50/2", "Ethernet50/3", "Ethernet50/4"} {
		if !strings.Contains(got, "interface "+lane+"\nshutdown") {
			t.Errorf("lane %s not shut down in:\n%s", lane, got)
		}
	}
}

func TestApplyDescription(t *testing.T) {
	c, ft := startedCollector(t)
	n, err := c.ApplyPorts(context.Background(), []switchmodel.PortDesired{
		{Index: 2, Enabled: true, Description: "  Julie desk\nno shutdown  "}, // control chars stripped
		{Index: 3, Enabled: true, Description: ""},                            // already empty: no-op
	})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := strings.Join(ft.configured[0], "\n")
	if !strings.Contains(got, "interface Ethernet2\ndescription Julie deskno shutdown\nend") {
		t.Errorf("commands:\n%s", got)
	}
}

func TestApplyNothingToDo(t *testing.T) {
	c, ft := startedCollector(t)
	n, err := c.ApplyPorts(context.Background(), []switchmodel.PortDesired{{Index: 1, Enabled: true}, {Index: 99, Enabled: false}})
	if err != nil || n != 0 || len(ft.configured) != 0 {
		t.Errorf("n=%d err=%v configure calls=%d", n, err, len(ft.configured))
	}
}

func TestApplyNeedsSnapshot(t *testing.T) {
	c, _ := newFixtureCollector(t)
	if _, err := c.ApplyPorts(context.Background(), []switchmodel.PortDesired{{Index: 2}}); err == nil {
		t.Error("apply before any poll must fail")
	}
}
