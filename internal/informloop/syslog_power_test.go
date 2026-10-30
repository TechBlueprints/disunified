package informloop

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

type fakeSyslog struct{ got [][]string }

func (f *fakeSyslog) ApplySyslog(_ context.Context, hosts []string) (bool, error) {
	f.got = append(f.got, hosts)
	return true, nil
}

// A power device (outlet control, no port control) is handed the remote
// syslog host from the controller's push -- captured from the rack PDU on
// Network 10.6.109 the day after remote logging was switched on -- both when
// the push is applied and on every reconcile; before 2026-10-30 only the
// switch path read it, so no APC card was ever given the site's log host. A
// push without the syslog.remote keys (remote logging off) arrives as no
// host.
func TestPowerDeviceIsGivenTheSyslogHost(t *testing.T) {
	b, err := os.ReadFile("../../docs/fixtures/controller-10.6.109/system_cfg-pdu-syslog-remote.txt")
	if err != nil {
		t.Fatal(err)
	}
	sl := &fakeSyslog{}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: string(b)}
	desc := inform.Descriptor{MAC: "02:00:00:00:00:02", Model: "USPPDUP"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), OutletController: &planningOutlets{}, SyslogController: sl})
	if err != nil {
		t.Fatal(err)
	}
	if !l.applyPending(context.Background()) {
		t.Fatal("pending push not consumed")
	}
	if len(sl.got) != 1 || len(sl.got[0]) != 1 || sl.got[0][0] != "192.0.2.3" {
		t.Fatalf("SyslogController got %q, want [[192.0.2.3]]", sl.got)
	}
	if !strings.Contains(buf.String(), "applied syslog: the device was given 192.0.2.3") {
		t.Errorf("log: %s", buf.String())
	}
	l.reconcile(context.Background())
	if len(sl.got) != 2 {
		t.Errorf("reconcile did not reach the SyslogController: %d calls", len(sl.got))
	}

	// Remote logging off: the same device, a push without the keys.
	sl.got = nil
	st.PendingSystemCfg = readFixture(t, "system_cfg-pdu-static-ip.txt")
	sess = device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, _ = New(desc, sess, Config{Logger: log.New(&buf, "", 0), OutletController: &planningOutlets{}, SyslogController: sl})
	l.applyPending(context.Background())
	if len(sl.got) != 1 || sl.got[0] != nil {
		t.Errorf("SyslogController got %q, want one call with no hosts", sl.got)
	}

	// A device with neither outlet nor port control still gets it.
	sl.got = nil
	st.PendingSystemCfg = string(b)
	sess = device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	l, _ = New(desc, sess, Config{Logger: log.New(&buf, "", 0), SyslogController: sl})
	l.applyPending(context.Background())
	if len(sl.got) != 1 || len(sl.got[0]) != 1 {
		t.Errorf("read-only device: SyslogController got %q", sl.got)
	}
}

var _ devicemodel.SyslogController = (*fakeSyslog)(nil)
