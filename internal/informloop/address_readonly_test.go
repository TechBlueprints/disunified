package informloop

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

type fakeAddress struct{ got []devicemodel.AddressDesired }

func (f *fakeAddress) ApplyAddress(_ context.Context, d devicemodel.AddressDesired) (bool, error) {
	f.got = append(f.got, d)
	return true, nil
}

// A device with no port or outlet control (the Podman host) still has the
// controller's static IP Settings handed to its AddressController -- the
// read-only branch used to skip it, so the Podman host's static push was
// accepted and never applied (2026-09-27).
func TestReadOnlyDeviceStillAppliesItsAddress(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.106", "system_cfg-pdu-static-ip.txt"))
	if err != nil {
		t.Fatal(err)
	}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: string(b)}
	desc := inform.Descriptor{MAC: "02:00:00:00:00:02", Model: "UDC48X6"}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	addr := &fakeAddress{}
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), AddressController: addr, CollectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !l.applyPending(context.Background()) {
		t.Fatal("pending push not consumed")
	}
	if len(addr.got) != 1 || addr.got[0].DHCP || addr.got[0].IP == "" {
		t.Fatalf("AddressController got %+v, want the static address from the push", addr.got)
	}
	if !strings.Contains(buf.String(), "applied IP Settings") {
		t.Errorf("log: %s", buf.String())
	}
	// The reconcile of the now-applied push hands it over again (the
	// driver's idempotence makes that a no-op on the device).
	l.reconcile(context.Background())
	if len(addr.got) != 2 {
		t.Errorf("reconcile did not reach the AddressController: %d calls", len(addr.got))
	}
}
