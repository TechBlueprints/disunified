package sonic

import (
	"context"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// A breakout with an egress limit and isolation on the cage, and a limit on
// another port, as live on the DX010's empty cage 26 (2026-11-17,
// docs/drivers/sonic.md 11.38). Live, the split was refused every cycle:
// sonic-yang's check before a breakout fails on the driver's SCHEDULER rows
// (pir without cir), so the batch now takes every shaper out of CONFIG_DB
// for the breakout and puts back those of the ports it leaves. And the
// batch the plan made wrote PORT_QOS_MAP on lanes 2-4 only: its copy kept
// the joined Ethernet100's row for lane 1, which keeps that name, as it had
// kept lane 1's LLDP-MED TLV (16.10 of breakout.md). And a breakout of an
// isolated port failed half done: the SAI keeps an isolated port's bridge
// port, so the cage now leaves the group first (port 5 stays in it). Over
// the DX010's capture, a split and a join, each followed by a reconcile
// that writes nothing.
func TestBreakoutKeepsShapersAndIsolationOnEveryLane(t *testing.T) {
	r := rig20261106(t, fixtureSplitDAC)
	base := r.baseline()
	r.apply(base)
	r.collect()
	if got, _ := r.apply(base); len(got) != 0 {
		t.Fatalf("the baseline does not settle: %q", got)
	}
	feat := edit(edit(base, 26, func(d *devicemodel.PortDesired) {
		d.EgressKbps = 100000
		d.Isolation = true
	}), 5, func(d *devicemodel.PortDesired) {
		d.EgressKbps = 100000
		d.Isolation = true
	})
	got, _ := r.apply(feat)
	if !containsStr(got, "sudo sonic-db-cli CONFIG_DB hset 'PORT_QOS_MAP|Ethernet100' scheduler DUI_EGRESS_100000") {
		t.Fatalf("the limit on port 26 was not written: %q", got)
	}
	r.collect()
	if again, _ := r.apply(feat); len(again) != 0 {
		t.Fatalf("the features do not settle: %q", again)
	}

	check := func(name string, ds []devicemodel.PortDesired, lanes ...string) {
		t.Helper()
		got, _ := r.apply(ds)
		all := strings.Join(got, "\n")
		br := strings.Index(all, "config interface breakout")
		if br < 0 {
			t.Fatalf("%s: no breakout in %q", name, got)
		}
		if !strings.Contains(all[:br], "sudo sonic-db-cli CONFIG_DB del 'SCHEDULER|DUI_EGRESS_100000'") {
			t.Errorf("%s: the shaper is not held out before the breakout: %q", name, got)
		}
		if !strings.Contains(all[:br], `"MEMBERS","Ethernet16"`) || !strings.Contains(all[:br], "SAI_OBJECT_TYPE_ISOLATION_GROUP_MEMBER") {
			t.Errorf("%s: the cage does not leave the isolation group (port 5 staying) before the breakout: %q", name, got)
		}
		after := all[br:]
		if !strings.Contains(after, "sudo sonic-db-cli CONFIG_DB hset 'PORT_QOS_MAP|Ethernet16' scheduler DUI_EGRESS_100000") {
			t.Errorf("%s: port 5's limit is not put back: %q", name, got)
		}
		for _, ln := range lanes {
			if !strings.Contains(after, "sudo sonic-db-cli CONFIG_DB hset 'PORT_QOS_MAP|"+ln+"' scheduler DUI_EGRESS_100000") {
				t.Errorf("%s: no limit on %s in %q", name, ln, got)
			}
		}
		iso := strings.Join(append(append([]string{}, lanes...), "Ethernet16"), ",")
		if !strings.Contains(after, `"MEMBERS","`+iso+`"`) {
			t.Errorf("%s: isolation is not %s: %q", name, iso, got)
		}
		r.collect()
		if again, n := r.apply(ds); len(again) != 0 || n != 0 {
			t.Errorf("%s: the next apply wrote %q (%d ports)", name, again, n)
		}
		snap, err := r.c.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if p := port(snap, 26); p.EgressKbps != 100000 || !p.Isolated {
			t.Errorf("%s: port 26 reads back %d kbps, isolated %v", name, p.EgressKbps, p.Isolated)
		}
		if p := port(snap, 5); p.EgressKbps != 100000 || !p.Isolated {
			t.Errorf("%s: port 5 reads back %d kbps", name, p.EgressKbps)
		}
	}
	split := speedOf(feat, 26, 25000)
	check("split", split, "Ethernet100", "Ethernet101", "Ethernet102", "Ethernet103")
	check("join", speedOf(feat, 26, 100000), "Ethernet100")
}
