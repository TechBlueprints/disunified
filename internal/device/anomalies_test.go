package device

import (
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

func TestPortAnomalies(t *testing.T) {
	up := devicemodel.Port{Up: true, SpeedMbps: 10000, SpeedCaps: []int{1000, 10000}}
	if bits, sat, reason := portAnomalies(up, false, nil); bits != 0 || sat != 100 || reason != 0 {
		t.Errorf("healthy port = %d/%d/%d, want 0/100/0", bits, sat, reason)
	}
	down := devicemodel.Port{Up: false, Fault: "errdisabled: bpduguard"}
	if bits, sat, _ := portAnomalies(down, false, nil); bits != anomBPDUGuard || sat != 100 { // down ports stay at 100 like real switches
		t.Errorf("bpduguard port = %d/%d", bits, sat)
	}
	slow := devicemodel.Port{Up: true, SpeedMbps: 1000, SpeedCaps: []int{1000, 10000}}
	if bits, sat, _ := portAnomalies(slow, true, nil); bits != anomLowUplinkSpeed || sat != 90 {
		t.Errorf("slow uplink = %d/%d", bits, sat)
	}
	if bits, _, _ := portAnomalies(slow, false, nil); bits != 0 {
		t.Errorf("slow non-uplink flagged: %d", bits)
	}

	// Lifetime drops/errors set the satisfaction reasons the way UniFi
	// switches do; the anomaly bits only when they grew since last inform.
	dropped := up
	dropped.Counters = devicemodel.Counters{TxDropped: 14}
	if bits, sat, reason := portAnomalies(dropped, false, &portHistory{Counters: dropped.Counters}); bits != 0 || sat != 90 || reason != 1 {
		t.Errorf("old drops = %d/%d/%d, want 0/90/1", bits, sat, reason)
	}
	if bits, sat, reason := portAnomalies(dropped, false, &portHistory{}); bits != anomDroppedTraffic || sat != 90 || reason != 1 {
		t.Errorf("new drops = %d/%d/%d", bits, sat, reason)
	}
	errs := up
	errs.Counters = devicemodel.Counters{RxErrors: 10}
	if bits, sat, reason := portAnomalies(errs, false, &portHistory{Counters: devicemodel.Counters{RxErrors: 1}}); bits != anomTransmissionError || sat != 85 || reason != 2 {
		t.Errorf("new errors = %d/%d/%d", bits, sat, reason)
	}
	both := up
	both.Counters = devicemodel.Counters{RxErrors: 1, RxDropped: 1}
	if _, sat, reason := portAnomalies(both, false, nil); sat != 75 || reason != 3 {
		t.Errorf("errors+drops = %d/%d", sat, reason)
	}

	// FEC uncorrected codewords count as transmission errors.
	fec := up
	fec.Health = devicemodel.PortHealth{HasFECCounters: true, FECUncorrected: 6}
	if bits, sat, reason := portAnomalies(fec, false, &portHistory{FECUncorrected: 5}); bits != anomTransmissionError || sat != 85 || reason != 2 {
		t.Errorf("fec uncorrected = %d/%d/%d", bits, sat, reason)
	}

	// PCS errored blocks growing, or a high-BER state, are transmission
	// errors at the PHY layer (copper ports have these; no FEC, no optic).
	pcs := up
	pcs.Health = devicemodel.PortHealth{HasPCSCounters: true, PCSErrBlocks: 310}
	if bits, sat, reason := portAnomalies(pcs, false, &portHistory{PCSErrBlocks: 309}); bits != anomTransmissionError || sat != 100 || reason != 0 {
		t.Errorf("pcs err blocks = %d/%d/%d", bits, sat, reason)
	}
	if bits, _, _ := portAnomalies(pcs, false, &portHistory{PCSErrBlocks: 310}); bits != 0 {
		t.Errorf("flat pcs err blocks = %d", bits)
	}
	pcs.Health.PCSHighBER = true
	if bits, _, _ := portAnomalies(pcs, false, &portHistory{PCSErrBlocks: 310}); bits != anomTransmissionError {
		t.Errorf("high BER = %d", bits)
	}

	// Flaps: link changes and STP topology changes since last inform.
	flap := up
	flap.Health.LinkChanges, flap.Health.STPChanges = 12, 7
	if bits, _, _ := portAnomalies(flap, false, &portHistory{LinkChanges: 10, STPChanges: 4}); bits != anomLinkFlap|anomTopologyFlap|anomSTPFlap {
		t.Errorf("flaps = %d", bits)
	}
	if bits, _, _ := portAnomalies(flap, false, &portHistory{LinkChanges: 11, STPChanges: 6}); bits != anomTopologyFlap {
		t.Errorf("single changes = %d, want topology only", bits)
	}

	// Optic alarms and STP guard inconsistency.
	optic := up
	optic.Health = devicemodel.PortHealth{OpticRxAlarm: true, OpticTxAlarm: true, STPInconsistent: true}
	if bits, _, _ := portAnomalies(optic, false, nil); bits != anomSFPRxFault|anomSFPTxFault|anomLoopSTP {
		t.Errorf("optic/stp = %d", bits)
	}
}
