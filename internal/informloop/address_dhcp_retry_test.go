package informloop

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/jamesbraid/unifi-emu/inform"
)

// scriptedAddress answers ApplyAddress from a list of errors, one per call
// (nil once the list is used up), and records what it was asked.
type scriptedAddress struct {
	errs []error
	got  []devicemodel.AddressDesired
}

func (f *scriptedAddress) ApplyAddress(_ context.Context, d devicemodel.AddressDesired) (bool, error) {
	f.got = append(f.got, d)
	if len(f.errs) == 0 {
		return true, nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err == nil, err
}

// saved reads back what the session last persisted to its state file.
func saved(t *testing.T, store *device.Store) device.State {
	t.Helper()
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// dhcpRetryLoop is a device following the captured static push, with the
// captured "Using DHCP" push pending: the transition someone made by hand.
func dhcpRetryLoop(t *testing.T, addr devicemodel.AddressController, st device.State) (*Loop, *device.Session, *device.Store, *strings.Builder) {
	t.Helper()
	desc := inform.Descriptor{MAC: "02:00:00:00:00:02", Model: "USPPDUP"}
	store := &device.Store{Path: filepath.Join(t.TempDir(), "device.json")}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, store, time.Now())
	var buf strings.Builder
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), AddressController: addr, CollectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return l, sess, store, &buf
}

func staticThenDHCP(t *testing.T) device.State {
	return device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef",
		CfgVersion: "v1", SystemCfg: readFixture(t, "system_cfg-pdu-static-ip.txt"),
		PendingCfgVersion: "v2", PendingSystemCfg: readFixture(t, "system_cfg-pdu-outlets.txt")}
}

// A switch to DHCP whose write fails is not lost when the push is marked
// followed: the reconcile asks again until the driver takes it, the intent
// is in the saved state for a restart, and after the write nothing more is
// asked (a later DHCP push is the default again).
func TestSwitchToDHCPIsAskedAgainUntilItIsWritten(t *testing.T) {
	addr := &scriptedAddress{errs: []error{errors.New("card busy"), errors.New("card busy")}}
	l, sess, store, buf := dhcpRetryLoop(t, addr, staticThenDHCP(t))
	ctx := context.Background()

	if !l.applyPending(ctx) {
		t.Fatal("pending push not consumed")
	}
	if len(addr.got) != 1 || !addr.got[0].DHCP {
		t.Fatalf("the push: AddressController got %+v, want DHCP once", addr.got)
	}
	if !sess.DHCPPending() || !saved(t, store).DHCPPending {
		t.Fatal("a failed switch to DHCP must be kept, in the saved state too")
	}
	if !strings.Contains(buf.String(), "asked again") {
		t.Errorf("log does not say the write is retried: %s", buf.String())
	}

	l.reconcile(ctx) // fails again
	if len(addr.got) != 2 || !addr.got[1].DHCP || !sess.DHCPPending() {
		t.Fatalf("reconcile after a failure: got %+v pending=%v; want DHCP asked again", addr.got, sess.DHCPPending())
	}
	l.reconcile(ctx) // succeeds
	if len(addr.got) != 3 || !addr.got[2].DHCP {
		t.Fatalf("second reconcile: got %+v; want DHCP asked a third time", addr.got)
	}
	if sess.DHCPPending() || saved(t, store).DHCPPending {
		t.Fatal("the written switch to DHCP is still pending")
	}
	if !strings.Contains(buf.String(), "now takes its address from DHCP") {
		t.Errorf("log: %s", buf.String())
	}
	l.reconcile(ctx)
	if len(addr.got) != 3 {
		t.Errorf("DHCP asked again after it was written: %+v", addr.got)
	}
}

// The pending intent survives a restart: a new session over the saved state
// asks for DHCP on its first reconcile.
func TestPendingSwitchToDHCPSurvivesARestart(t *testing.T) {
	addr := &scriptedAddress{errs: []error{errors.New("ftp: connection refused")}}
	l, _, store, _ := dhcpRetryLoop(t, addr, staticThenDHCP(t))
	l.applyPending(context.Background())
	if !saved(t, store).DHCPPending || saved(t, store).CfgVersion != "v2" {
		t.Fatalf("saved state: pending=%v cfgversion=%q", saved(t, store).DHCPPending, saved(t, store).CfgVersion)
	}

	after := &scriptedAddress{}
	l2, sess2, _, _ := dhcpRetryLoop(t, after, saved(t, store))
	l2.reconcile(context.Background())
	if len(after.got) != 1 || !after.got[0].DHCP || sess2.DHCPPending() {
		t.Fatalf("after the restart: got %+v pending=%v; want DHCP asked once and settled", after.got, sess2.DHCPPending())
	}
}

// A static setting pushed while the switch to DHCP is still pending replaces
// it: the static address is applied and DHCP is never asked again.
func TestStaticPushCancelsAPendingSwitchToDHCP(t *testing.T) {
	addr := &scriptedAddress{errs: []error{errors.New("card busy")}}
	l, sess, store, _ := dhcpRetryLoop(t, addr, staticThenDHCP(t))
	ctx := context.Background()
	l.applyPending(ctx)
	if !sess.DHCPPending() {
		t.Fatal("setup: the failed switch to DHCP is not pending")
	}

	st := saved(t, store)
	st.PendingCfgVersion, st.PendingSystemCfg = "v3", readFixture(t, "system_cfg-pdu-static-ip.txt")
	l3, sess3, _, _ := dhcpRetryLoop(t, addr, st)
	l3.applyPending(ctx)
	last := addr.got[len(addr.got)-1]
	if last.DHCP || last.IP != "192.0.2.1" || sess3.DHCPPending() {
		t.Fatalf("the static push: asked %+v pending=%v; want the static address and no pending DHCP", last, sess3.DHCPPending())
	}
	n := len(addr.got)
	l3.reconcile(ctx)
	if len(addr.got) != n+1 || addr.got[n].DHCP {
		t.Errorf("reconcile after the static push asked %+v; want the static address only", addr.got[n:])
	}
}

// A driver that declines DHCP by design is asked once, not every cycle.
func TestDeclinedDHCPIsNotAskedAgain(t *testing.T) {
	addr := &scriptedAddress{errs: []error{devicemodel.AddressDeclined(errors.New("this device keeps its static address"))}}
	l, sess, _, buf := dhcpRetryLoop(t, addr, staticThenDHCP(t))
	ctx := context.Background()
	l.applyPending(ctx)
	l.reconcile(ctx)
	if len(addr.got) != 1 || sess.DHCPPending() {
		t.Fatalf("declined DHCP: asked %d times, pending=%v; want once and nothing kept", len(addr.got), sess.DHCPPending())
	}
	if !strings.Contains(buf.String(), "keeps its static address") {
		t.Errorf("the driver's reason is not logged: %s", buf.String())
	}
}

// The default DHCP push, never preceded by a static one, still applies
// nothing, on the push and on every reconcile.
func TestDefaultDHCPIsStillNeverApplied(t *testing.T) {
	addr := &scriptedAddress{}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef",
		PendingCfgVersion: "v1", PendingSystemCfg: readFixture(t, "system_cfg-pdu-outlets.txt")}
	l, sess, _, _ := dhcpRetryLoop(t, addr, st)
	ctx := context.Background()
	l.applyPending(ctx)
	l.reconcile(ctx)
	if len(addr.got) != 0 || sess.DHCPPending() {
		t.Fatalf("the DHCP default: asked %+v pending=%v", addr.got, sess.DHCPPending())
	}
}
