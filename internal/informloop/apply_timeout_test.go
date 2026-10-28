package informloop

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// deadlineController records how long each write was given.
type deadlineController struct{ budgets []time.Duration }

func (d *deadlineController) got() []time.Duration { return d.budgets }

func (d *deadlineController) ApplyPorts(ctx context.Context, _ []devicemodel.PortDesired) (int, error) {
	dl, _ := ctx.Deadline()
	d.budgets = append(d.budgets, time.Until(dl))
	return 0, nil
}

// budgetController is a driver that asks for a longer write budget.
type budgetController struct{ deadlineController }

func (*budgetController) ApplyTimeout() time.Duration { return 90 * time.Second }

// A write gets the collect budget unless the driver asks for more
// (devicemodel.ApplyBudget: a SONiC push of dozens of ~250 ms CLI calls was
// cut at the 20 s collect budget live) or control.apply_timeout is set,
// which wins over both.
func TestWritesUseTheApplyBudget(t *testing.T) {
	for _, tc := range []struct {
		set, want time.Duration
		asks      bool
	}{{0, time.Second, false}, {0, 90 * time.Second, true}, {3 * time.Minute, 3 * time.Minute, false}, {3 * time.Minute, 3 * time.Minute, true}} {
		var ctl interface {
			devicemodel.Controller
			got() []time.Duration
		} = &deadlineController{}
		if tc.asks {
			ctl = &budgetController{}
		}
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
		if len(ctl.got()) != 2 {
			t.Fatalf("ApplyPorts called %d times, want 2", len(ctl.got()))
		}
		for _, b := range ctl.got() {
			if b < tc.want-500*time.Millisecond || b > tc.want {
				t.Errorf("ApplyTimeout %s, driver asks %v: a write got %s, want about %s (the collect budget is 1s)", tc.set, tc.asks, b, tc.want)
			}
		}
	}
}

// The sonic driver asks for 90 s.
func TestSonicAsksForItsWriteBudget(t *testing.T) {
	var c devicemodel.ApplyBudget = sonic.NewCollector(nil)
	if c.ApplyTimeout() != 90*time.Second {
		t.Errorf("sonic write budget = %s", c.ApplyTimeout())
	}
}
