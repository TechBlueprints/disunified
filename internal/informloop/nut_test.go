package informloop

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/unificfg"
	"github.com/jamesbraid/unifi-emu/inform"
)

type fakeNUT struct{ got []*unificfg.NUTServer }

func (f *fakeNUT) ApplyNUT(spec *unificfg.NUTServer) error { f.got = append(f.got, spec); return nil }

// The controller's nutserver block (captured 2026-09-27) reaches the NUT
// applier on apply; a push without the block hands it nil, which is "off".
func TestNUTServerBlockReachesTheApplier(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106/system_cfg-ups-nutserver.txt"))
	if err != nil {
		t.Fatal(err)
	}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: string(b)}
	desc := inform.Descriptor{MAC: "02:00:00:00:00:02", Model: "USPDA2B"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	nut := &fakeNUT{}
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), NUT: nut})
	if err != nil {
		t.Fatal(err)
	}
	l.applyNUT(string(b))
	if len(nut.got) != 1 || nut.got[0] == nil || *nut.got[0] != (unificfg.NUTServer{Enabled: true, ID: "ups", Port: 3493}) {
		t.Fatalf("applier got %+v", nut.got)
	}
	l.applyNUT("switch.mtu=1500\n")
	if len(nut.got) != 2 || nut.got[1] != nil {
		t.Errorf("a push without the block must hand the applier nil (off): %+v", nut.got)
	}
}
