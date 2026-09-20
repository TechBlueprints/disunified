package aristaeos

import (
	"context"
	"testing"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

func TestSecondWaveData(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sys := snap.System
	if sys.STPMode != "rstp" || sys.STPPriority != 32768 {
		t.Errorf("stp = %q %d", sys.STPMode, sys.STPPriority)
	}
	if sys.Overheating {
		t.Error("overheating reported on a temperatureOk system")
	}
	if len(sys.Fans) == 0 || !sys.Fans[0].OK || sys.Fans[0].SpeedPct == 0 {
		t.Errorf("fans = %+v", sys.Fans)
	}
	if len(sys.PSUs) != 2 || sys.PSUs[0].CapacityW != 500 || !sys.PSUs[0].OK {
		t.Errorf("psus = %+v", sys.PSUs)
	}
	if sys.PSUs[0].Slot != "1" || sys.PSUs[1].Slot != "2" || !sys.PSUs[0].Present || len(sys.PSUs[0].TempsC) != 3 {
		t.Errorf("psus not sorted/present/with temps = %+v", sys.PSUs)
	}
	if len(snap.MACTable) < 100 {
		t.Errorf("mac table has %d entries", len(snap.MACTable))
	}
	p49 := portByIndex(t, snap, 49)
	if p49.FEC != switchmodel.FECRS {
		t.Errorf("port 49 fec = %q, want rs-fec", p49.FEC)
	}
	if p49.Optic == nil || !p49.Optic.HasDOM || p49.Optic.MediaType != "100GBASE-CWDM4" || p49.Optic.Part == "" {
		t.Errorf("port 49 optic = %+v", p49.Optic)
	}
	if len(p49.MACs) < 100 {
		t.Errorf("port 49 (uplink) has %d MACs, want the whole house", len(p49.MACs))
	}
	if p49.STPPathCost != 200 {
		t.Errorf("port 49 path cost = %d", p49.STPPathCost)
	}
	if p49.AutoNeg {
		t.Errorf("port 49 autoneg = %v, want false (forced 100G)", p49.AutoNeg)
	}
	if p49.Counters.RxMulticast != 291599364 || p49.Counters.RxBroadcast != 100201842 {
		t.Errorf("port 49 mcast/bcast = %d/%d", p49.Counters.RxMulticast, p49.Counters.RxBroadcast)
	}
	p50 := portByIndex(t, snap, 50)
	if p50.FEC != switchmodel.FECFC {
		t.Errorf("port 50 (breakout lanes) fec = %q, want fc-fec", p50.FEC)
	}
	p53 := portByIndex(t, snap, 53)
	if len(p53.MACs) != 1 {
		t.Errorf("port 53 has %d MACs, want 1 (the NAS)", len(p53.MACs))
	}
	if p1 := portByIndex(t, snap, 1); p1.Optic != nil || p1.FEC != switchmodel.FECUnknown || !p1.AutoNeg {
		t.Errorf("copper port 1 = optic %v fec %q autoneg %v", p1.Optic, p1.FEC, p1.AutoNeg)
	}
	if p52 := portByIndex(t, snap, 52); p52.Optic != nil {
		t.Errorf("empty cage 52 has an optic: %+v", p52.Optic)
	}
	if up := snap.UplinkPort(); up != 49 {
		t.Errorf("uplink port = %d, want 49", up)
	}
	// Speed capabilities come from `show interfaces hardware`: copper is 1G+10G only.
	if p2 := portByIndex(t, snap, 2); len(p2.SpeedCaps) != 2 || p2.SpeedCaps[0] != 1000 || p2.SpeedCaps[1] != 10000 || p2.FECCapable {
		t.Errorf("port 2 caps = %v fec %v", p2.SpeedCaps, p2.FECCapable)
	}
	if p49.SpeedCaps == nil || p49.SpeedCaps[0] != 10000 || p49.SpeedCaps[len(p49.SpeedCaps)-1] != 100000 || !p49.FECCapable {
		t.Errorf("port 49 caps = %v fec %v", p49.SpeedCaps, p49.FECCapable)
	}
	if p52 := portByIndex(t, snap, 52); len(p52.SpeedCaps) == 0 {
		t.Errorf("empty cage 52 lost its media fallback caps: %v", p52.SpeedCaps)
	}
	// Split cage (port 50, 4x25G lanes) must still claim the cage speeds so
	// the controller lets the operator join it back to 100G.
	if p50 := portByIndex(t, snap, 50); p50.SpeedCaps[len(p50.SpeedCaps)-1] != 100000 || !p50.FECCapable {
		t.Errorf("split cage 50 caps = %v fec %v", p50.SpeedCaps, p50.FECCapable)
	}
	// Health signals: link changes (show interfaces), STP topology changes
	// (topology status detail), optic thresholds (dom thresholds), FEC
	// codewords (phy detail text).
	h := p49.Health
	if h.LinkChanges != 18 || h.STPChanges != 3 || h.STPInconsistent || h.OpticRxAlarm || h.OpticTxAlarm {
		t.Errorf("port 49 health = %+v", h)
	}
	if !h.HasFECCounters || h.FECCorrected != 1 || h.FECUncorrected != 5 {
		t.Errorf("port 49 fec counters = %+v", h)
	}
	if !h.HasPCSCounters || h.PCSErrBlocks != 309 || h.PCSHighBER {
		t.Errorf("port 49 pcs counters = %+v", h)
	}
	if p53 := portByIndex(t, snap, 53); p53.Health.STPChanges != 3 || p53.Health.LinkChanges != 4 {
		t.Errorf("port 53 health = %+v", p53.Health)
	}
	if snap.System.MgmtMAC != "02:00:00:00:00:3a" {
		t.Errorf("management MAC = %q", snap.System.MgmtMAC)
	}
	if oob := snap.System.OOBInterfaces; len(oob) != 1 || oob[0].Name != "Management1" || !oob[0].Up || oob[0].IP == "" {
		t.Errorf("oob interfaces = %+v (fixture: Management1 up with an address)", oob)
	}
}
