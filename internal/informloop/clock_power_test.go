package informloop

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

type fakeClock struct{ got []devicemodel.Clock }

func (f *fakeClock) ApplyClock(_ context.Context, c devicemodel.Clock) (bool, error) {
	f.got = append(f.got, c)
	return true, nil
}

var _ devicemodel.ClockController = (*fakeClock)(nil)

// A power device is handed the controller's time zone and NTP servers from
// the push captured from the rack PDU on Network 10.6.109 (the POSIX string
// with its US rule, two servers, one a host name), when the push is applied
// and on every reconcile, with outlet control or none.
func TestPowerDeviceIsGivenTheClock(t *testing.T) {
	b, err := os.ReadFile("../../docs/fixtures/controller-10.6.109/system_cfg-pdu-syslog-remote.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := "{MST7MDT,M3.2.0,M11.1.0 [192.0.2.4 0.ubnt.pool.ntp.org]}"
	for _, outlets := range []bool{true, false} {
		ck := &fakeClock{}
		st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: string(b)}
		desc := inform.Descriptor{MAC: "02:00:00:00:00:02", Model: "USPPDUP"}
		sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
		var buf strings.Builder
		cfg := Config{Logger: log.New(&buf, "", 0), ClockController: ck}
		if outlets {
			cfg.OutletController = &planningOutlets{}
		}
		l, err := New(desc, sess, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !l.applyPending(context.Background()) {
			t.Fatal("pending push not consumed")
		}
		if len(ck.got) != 1 || fmt.Sprint(ck.got[0]) != want {
			t.Fatalf("outlets %v: ClockController got %v, want [%s]", outlets, ck.got, want)
		}
		if !strings.Contains(buf.String(), `applied the clock: the device was given time zone "MST7MDT,M3.2.0,M11.1.0"`) {
			t.Errorf("log: %s", buf.String())
		}
		l.reconcile(context.Background())
		if len(ck.got) != 2 {
			t.Errorf("outlets %v: reconcile did not reach the ClockController: %d calls", outlets, len(ck.got))
		}
	}
}
