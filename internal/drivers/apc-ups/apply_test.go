package apcups

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The command word is what NUT's apc_modbus sends for load.on / load.off /
// load.cycle on a group: command bit | target bit, high word first.
func TestCommandWordsMatchTheReferenceEncoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  uint32
		g    int
		want []uint16
	}{
		{"group 1 off", cmdOutputOff, 1, []uint16{0x0000, 0x0204}},
		{"group 1 on", cmdOutputOn, 1, []uint16{0x0000, 0x0202}},
		{"group 1 reboot", cmdOutputReboot, 1, []uint16{0x0000, 0x0210}},
		{"group 2 off", cmdOutputOff, 2, []uint16{0x0000, 0x0404}},
	} {
		got := commandWords(tc.cmd, groupTarget(tc.g))
		if len(got) != 2 || got[0] != tc.want[0] || got[1] != tc.want[1] {
			t.Errorf("%s: words = %04x, want %04x", tc.name, got, tc.want)
		}
	}
	if groupTarget(0) != 1<<8 {
		t.Error("the Main group's target bit is bit 8 (never set by this driver)")
	}
}

// On the wire: Write Multiple Registers (16) at 1538, two registers, four
// data bytes -- byte for byte.
func TestWriteRegistersFrameIsByteExact(t *testing.T) {
	addr, got := serve(t, []byte{0x10, 0x06, 0x02, 0x00, 0x02}) // the echo reply
	m := NewModbus(addr)
	m.Timeout = 2 * time.Second
	if err := m.WriteRegisters(context.Background(), regOutletCommand, commandWords(cmdOutputOff, groupTarget(1))); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x10, 0x06, 0x02, 0x00, 0x02, 0x04, 0x00, 0x00, 0x02, 0x04}
	if req := <-got; !bytes.Equal(req, want) {
		t.Errorf("request body = % x, want % x (fc 16, start 1538, count 2, 4 bytes, 0000 0204)", req, want)
	}
}

// An exception to a write is the device refusing; surfaced, not retried.
func TestWriteRegistersSurfacesAnException(t *testing.T) {
	addr, _ := serve(t, []byte{0x90, 0x02})
	m := NewModbus(addr)
	m.Timeout = 2 * time.Second
	err := m.WriteRegisters(context.Background(), regOutletCommand, commandWords(cmdOutputOn, groupTarget(1)))
	var ex *Exception
	if !errors.As(err, &ex) || ex.Code != 0x02 || ex.Function != 16 {
		t.Fatalf("err = %v, want a modbus exception 0x02 on function 16", err)
	}
}

func desired(idx int, on bool) []devicemodel.OutletDesired {
	return []devicemodel.OutletDesired{{Index: idx, On: on}}
}

// A push that differs from the device writes one command; the same push
// again writes nothing; the opposite push writes the other command.
func TestApplyOutletsIsAStrictDiff(t *testing.T) {
	c, fr := startFixture(t)
	ctx := context.Background()
	n, err := c.ApplyOutlets(ctx, desired(2, false))
	if err != nil || n != 1 {
		t.Fatalf("switch off: changed=%d err=%v", n, err)
	}
	if strings.Join(fr.Writes, ",") != "1538=00000204" {
		t.Fatalf("writes = %v, want exactly the group-1 off word", fr.Writes)
	}
	snap, _ := c.Collect(ctx)
	if snap.Outlets[1].On {
		t.Fatal("after the off command the fixture still reads group 1 on")
	}
	if n, _ := c.ApplyOutlets(ctx, desired(2, false)); n != 0 || len(fr.Writes) != 1 {
		t.Errorf("re-applying the same intent wrote again: changed=%d writes=%v", n, fr.Writes)
	}
	if n, _ := c.ApplyOutlets(ctx, desired(2, true)); n != 1 || fr.Writes[len(fr.Writes)-1] != "1538=00000202" {
		t.Errorf("switch on: changed=%d writes=%v", n, fr.Writes)
	}
	if snap, _ := c.Collect(ctx); !snap.Outlets[1].On {
		t.Error("after the on command the fixture still reads group 1 off")
	}
}

// A push that matches the device writes nothing at all.
func TestApplyOutletsWritesNothingWhenNothingChanges(t *testing.T) {
	c, fr := startFixture(t)
	n, err := c.ApplyOutlets(context.Background(), []devicemodel.OutletDesired{{Index: 1, On: true}, {Index: 2, On: true}})
	if err != nil || n != 0 || len(fr.Writes) != 0 {
		t.Errorf("changed=%d err=%v writes=%v, want nothing written", n, err, fr.Writes)
	}
}

// The Main group is never commanded, whatever the controller asks.
func TestApplyOutletsNeverCommandsTheMainGroup(t *testing.T) {
	c, fr := startFixture(t)
	n, err := c.ApplyOutlets(context.Background(), desired(1, false))
	if err != nil || n != 0 || len(fr.Writes) != 0 {
		t.Errorf("main group off: changed=%d err=%v writes=%v, want refused with nothing written", n, err, fr.Writes)
	}
	if err := c.CycleOutlet(context.Background(), 1); err == nil || len(fr.Writes) != 0 {
		t.Errorf("cycling the main group: err=%v writes=%v, want refused with nothing written", err, fr.Writes)
	}
	if got := c.PlanOutlets(desired(1, false)); len(got) != 0 {
		t.Errorf("plan lists the main group %v", got)
	}
}

// An outlet the unit does not have is ignored, never mapped onto a relay.
func TestApplyOutletsIgnoresUnknownOutlets(t *testing.T) {
	c, fr := startFixture(t)
	n, err := c.ApplyOutlets(context.Background(), desired(7, false))
	if err != nil || n != 0 || len(fr.Writes) != 0 {
		t.Errorf("unknown outlet: changed=%d err=%v writes=%v", n, err, fr.Writes)
	}
	if err := c.CycleOutlet(context.Background(), 7); err == nil {
		t.Error("cycling an unknown outlet did not fail")
	}
}

func TestPlanOutletsReportsOnlyRealChanges(t *testing.T) {
	c, _ := startFixture(t)
	if got := c.PlanOutlets(desired(2, false)); len(got) != 1 || got[0] != 2 {
		t.Errorf("plan = %v, want [2]", got)
	}
	if got := c.PlanOutlets(desired(2, true)); len(got) != 0 {
		t.Errorf("plan for a matching state = %v, want none", got)
	}
}

// A power cycle is the unit's own reboot command; the group reads back on.
func TestCycleOutletSendsTheRebootWord(t *testing.T) {
	c, fr := startFixture(t)
	if err := c.CycleOutlet(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fr.Writes, ",") != "1538=00000210" {
		t.Errorf("writes = %v", fr.Writes)
	}
	if snap, _ := c.Collect(context.Background()); !snap.Outlets[1].On {
		t.Error("a reboot is not a state: the group must read back on")
	}
}

func TestApplyOutletsSurfacesAWriteFailure(t *testing.T) {
	c, fr := startFixture(t)
	fr.FailWrite = errors.New("boom")
	n, err := c.ApplyOutlets(context.Background(), desired(2, false))
	if n != 0 || err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("changed=%d err=%v", n, err)
	}
}

func TestApplyOutletsBeforeAnySnapshotFails(t *testing.T) {
	fr, _ := NewFixtureRunner(fixtureDir)
	c := NewCollector(fr)
	if _, err := c.ApplyOutlets(context.Background(), desired(2, false)); err == nil {
		t.Error("apply with no snapshot succeeded")
	}
}
