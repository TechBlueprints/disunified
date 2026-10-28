package informloop

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

// deadlineController records how long each write was given.
type deadlineController struct{ budgets []time.Duration }

func (d *deadlineController) ApplyPorts(ctx context.Context, _ []devicemodel.PortDesired) (int, error) {
	dl, _ := ctx.Deadline()
	d.budgets = append(d.budgets, time.Until(dl))
	return 0, nil
}

// Writes get the apply budget, not the collect one: a SONiC push of dozens
// of ~250 ms CLI calls was cut at the 20 s collect budget live.
func TestWritesUseTheApplyBudget(t *testing.T) {
	for _, tc := range []struct {
		set, want time.Duration
	}{{0, DefaultApplyTimeout}, {3 * time.Minute, 3 * time.Minute}} {
		ctl := &deadlineController{}
		st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: "switch.port.1.status=enabled\n"}
		sess := device.NewSession(inform.Descriptor{MAC: "02:00:00:00:00:01"}, "http://192.0.2.1:8080/inform", st, nil, time.Now())
		var buf strings.Builder
		l, err := New(inform.Descriptor{MAC: "02:00:00:00:00:01"}, sess, Config{Logger: log.New(&buf, "", 0), Controller: ctl, AllowInitialChanges: true,
			CollectTimeout: time.Second, ApplyTimeout: tc.set})
		if err != nil {
			t.Fatal(err)
		}
		l.applyPending(context.Background())
		l.reconcile(context.Background())
		if len(ctl.budgets) != 2 {
			t.Fatalf("ApplyPorts called %d times, want 2", len(ctl.budgets))
		}
		for _, b := range ctl.budgets {
			if b < tc.want-5*time.Second || b > tc.want {
				t.Errorf("ApplyTimeout %s: a write got %s, want about %s (the collect budget is 1s)", tc.set, b, tc.want)
			}
		}
	}
}
