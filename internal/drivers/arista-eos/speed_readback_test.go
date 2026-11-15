package aristaeos

import (
	"context"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// A speed the driver wrote is read back on the next collect: the admin
// speed lives in `show interfaces transceiver properties`, which the
// collector reads with the startup commands, so a forced speed has to
// refresh them or every reconcile until the media refresh (30 polls)
// writes it again.
func TestForcedSpeedIsReadBackOnTheNextCollect(t *testing.T) {
	c, ft := startedCollector(t)
	ctx := context.Background()
	var desired []devicemodel.PortDesired
	for _, p := range c.last.Ports {
		if p.Index == 2 {
			desired = append(desired, devicemodel.PortDesired{Index: 2, Enabled: p.Enabled, SpeedMbps: 1000})
		}
	}
	if n, err := c.ApplyPorts(ctx, desired); err != nil || n != 1 {
		t.Fatalf("the speed change: n=%d err=%v batches %q", n, err, ft.Configured)
	}
	if _, err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.PlanPorts(desired); len(got) != 0 {
		t.Errorf("after the collect the same push still changes ports %v", got)
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("unmodelled: %q", ft.Unmodelled)
	}
}
