package informloop

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// dx010Snapshot collects the DX010's capture (docs/drivers/sonic.md §10.7).
func dx010Snapshot(t *testing.T) *devicemodel.Snapshot {
	t.Helper()
	return dx010SnapshotFrom(t, "collect.txt")
}

func dx010SnapshotFrom(t *testing.T, name string) *devicemodel.Snapshot {
	t.Helper()
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238526-ff3c0c9f6", name))
	if err != nil {
		t.Fatal(err)
	}
	coll := sonic.NewCollector(fr)
	coll.Log = log.New(&strings.Builder{}, "", 0)
	snap, err := coll.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// The DX010's eth0 is cabled but taken down on purpose and keeps its last
// lease: no "carries address" warning (§10.6). The same address on a port
// that is up -- with or without carrier -- is warned about as before.
func TestOOBWarningSkipsAdminDownPort(t *testing.T) {
	snap := dx010Snapshot(t)
	oob := snap.System.OOBInterfaces
	if len(oob) != 1 || !oob[0].AdminDown || oob[0].IP == "" {
		t.Fatalf("capture's OOB interfaces %+v, want eth0 admin down with an address", oob)
	}
	warn := func(o devicemodel.OOBInterface, dial string) string {
		var buf strings.Builder
		l := &Loop{cfg: Config{Logger: log.New(&buf, "", 0), DeviceHost: dial}}
		s := *snap
		s.System.OOBInterfaces = []devicemodel.OOBInterface{o}
		l.warnOOB(&s)
		return buf.String()
	}
	inband := snap.System.Addresses[0].IP
	if got := warn(oob[0], inband); got != "" {
		t.Errorf("admin-down eth0 warned: %s", got)
	}
	up := oob[0]
	up.AdminDown = false
	if got := warn(up, inband); !strings.Contains(got, "carries address") {
		t.Errorf("admin-up eth0 without carrier: %q, want the carries-address warning", got)
	}
	cabled := up
	cabled.Up = true
	if got := warn(cabled, inband); !strings.Contains(got, "carries address") {
		t.Errorf("eth0 with carrier: %q, want the carries-address warning", got)
	}
	if got := warn(oob[0], oob[0].IP); !strings.Contains(got, "reaches this switch through its out-of-band") {
		t.Errorf("dialled on eth0's address: %q, want the dialled warning", got)
	}
}

// After the reboot trap (§10.6) eth0 holds a static 169.254.1.1/16 in
// MGMT_INTERFACE, admin up, cabled to a disabled switch port: no warning
// (collect-20261101.txt). The same address with carrier is the milder
// "connected" note, not "carries address"; a routable address on an eth0
// that is up without carrier -- the trap itself -- still warns.
func TestOOBWarningSkipsLinkLocalWithoutCarrier(t *testing.T) {
	snap := dx010SnapshotFrom(t, "collect-20261101.txt")
	oob := snap.System.OOBInterfaces
	if len(oob) != 1 || oob[0].IP != "169.254.1.1" || oob[0].Up || oob[0].AdminDown {
		t.Fatalf("capture's OOB interfaces %+v, want eth0 admin up, no carrier, 169.254.1.1", oob)
	}
	warn := func(o devicemodel.OOBInterface) string {
		var buf strings.Builder
		l := &Loop{cfg: Config{Logger: log.New(&buf, "", 0), DeviceHost: snap.System.Addresses[0].IP}}
		s := *snap
		s.System.OOBInterfaces = []devicemodel.OOBInterface{o}
		l.warnOOB(&s)
		return buf.String()
	}
	if got := warn(oob[0]); got != "" {
		t.Errorf("link-local eth0 without carrier warned: %s", got)
	}
	cabled := oob[0]
	cabled.Up = true
	if got := warn(cabled); !strings.Contains(got, "NOTE: out-of-band management port eth0 is connected, with only a link-local address") {
		t.Errorf("link-local eth0 with carrier: %q", got)
	}
	lease := oob[0]
	lease.IP = dx010Snapshot(t).System.OOBInterfaces[0].IP // the recorded lease of the first capture
	if got := warn(lease); !strings.Contains(got, "carries address") {
		t.Errorf("a lease on an eth0 that is up without carrier: %q, want the carries-address warning", got)
	}
}
