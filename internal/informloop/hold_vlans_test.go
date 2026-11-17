package informloop

import (
	"context"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

// vlanPlanningController sees a change only with the push's VLANs (what a
// SONiC switch that lacks them does: docs/drivers/breakout.md §16.10).
type vlanPlanningController struct {
	planned [][]int
	ensured int
	applied int
}

func (v *vlanPlanningController) ApplyPorts(context.Context, []devicemodel.PortDesired) (int, error) {
	v.applied++
	return 1, nil
}
func (v *vlanPlanningController) PlanPorts([]devicemodel.PortDesired) []int { return nil }
func (v *vlanPlanningController) PlanPortsWithVLANs(_ []devicemodel.PortDesired, ids []int) []int {
	v.planned = append(v.planned, append([]int{}, ids...))
	return []int{18}
}
func (v *vlanPlanningController) EnsureVLANs(context.Context, []int) (int, error) {
	v.ensured++
	return 0, nil
}

// The ordinary device's path: the first push is planned with its VLANs
// before anything is created, and held. The push is the recorded first
// push of the cage-3 adoption (the controller's defaults, eleven VLANs).
func TestFirstPushIsPlannedWithItsVLANs(t *testing.T) {
	ctl := &vlanPlanningController{}
	push := laneSwitchPushes(t, "replies-lane-switch-dx010-cage3.ndjson")[0].cfg
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: push}
	desc := inform.Descriptor{MAC: "02:00:00:00:00:01"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), Controller: ctl})
	if err != nil {
		t.Fatal(err)
	}
	l.applyPending(context.Background())
	if !strings.Contains(buf.String(), "HOLDING the first push") {
		t.Errorf("not held: %s", buf.String())
	}
	if ctl.ensured != 0 || ctl.applied != 0 {
		t.Errorf("a held push created VLANs (%d) or applied (%d)", ctl.ensured, ctl.applied)
	}
	if !reflect.DeepEqual(ctl.planned, [][]int{{1, 2, 8, 9, 10, 69, 1000, 2000, 3000, 4000, 4010}}) {
		t.Errorf("planned with %v, want the push's eleven", ctl.planned)
	}
}
