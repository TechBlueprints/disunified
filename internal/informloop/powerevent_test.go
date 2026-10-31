package informloop

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

// powerCollector is a power device without a log of its own: it asks for
// power-event lines.
type powerCollector struct {
	fail      bool
	onBattery bool
}

func (p *powerCollector) Collect(context.Context) (*devicemodel.Snapshot, error) {
	if p.fail {
		return nil, errors.New("dial tcp 192.0.2.30:502: i/o timeout")
	}
	return &devicemodel.Snapshot{System: devicemodel.System{Model: "Smart-UPS 1500", SKU: "SMTL1500RM3UC", Battery: &devicemodel.Battery{
		OnBattery: p.onBattery, ChargePct: 100, Runtime: 376 * time.Second, RealPowerRatingW: 1350,
		HasOutput: true, RealPowerW: 1067, LoadPct: 79,
	}}}, nil
}

func (p *powerCollector) PowerEvents() devicemodel.PowerEventSettings {
	return devicemodel.PowerEventSettings{}
}

// The loop logs a PowerEventSource's transitions under the config entry's
// name and the controller's name for the device, which it reads from the
// applied push (persisted, so known on the first cycle after a restart):
// here the UPS's real push, whose resolv.host.1.name the scrub left as
// the unit's default UniFi name.
func TestLoopLogsPowerEvents(t *testing.T) {
	cfgText, err := os.ReadFile("../../docs/fixtures/controller-10.6.106/system_cfg-ups-nutserver.txt")
	if err != nil {
		t.Fatal(err)
	}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", CfgVersion: "0123456789abcdef", SystemCfg: string(cfgText)}
	desc := inform.Descriptor{MAC: "02:00:00:00:00:03", Model: "USPDA2B"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	pc := &powerCollector{}
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), Collector: pc, CollectTimeout: time.Second, DeviceName: "ups"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	l.collect(ctx)
	l.collect(ctx)
	pc.onBattery = true
	l.collect(ctx)
	pc.fail = true
	for i := 0; i < 3; i++ {
		l.collect(ctx)
	}
	var events []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "power-event ") {
			events = append(events, line)
		}
	}
	if len(events) != 3 {
		t.Fatalf("want initial_state, on_battery, comm_lost; got:\n%s", buf.String())
	}
	for i, want := range []string{
		"power-event level=info device=ups name=APCSmart-UPS1500 model=SMTL1500RM3UC event=initial_state ",
		"power-event level=error device=ups name=APCSmart-UPS1500 model=SMTL1500RM3UC event=on_battery ",
		"power-event level=error device=ups name=APCSmart-UPS1500 model=SMTL1500RM3UC event=comm_lost ",
	} {
		if !strings.HasPrefix(events[i], want) {
			t.Errorf("line %d:\n got %s\nwant %s...", i, events[i], want)
		}
	}

	// A device that does not ask (an APC card that syslogs for itself)
	// gets no power-event lines.
	buf.Reset()
	l2, _ := New(desc, sess, Config{Logger: log.New(&buf, "", 0), Collector: &failingCollector{}, CollectTimeout: time.Second})
	l2.collect(ctx)
	if strings.Contains(buf.String(), "power-event") {
		t.Errorf("no PowerEventSource, no lines: %s", buf.String())
	}
}
