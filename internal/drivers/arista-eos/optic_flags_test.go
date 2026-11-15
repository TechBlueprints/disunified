package aristaeos

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The module flags as `show interfaces X phy detail` prints them on EOS
// 4.26.14M (docs/drivers/arista-eos.md §4, optic fault flags). optic-flags/
// is one read of 2026-11-15: a 100G CWDM4 seated in cage 49 with no fibre,
// a 100G DAC linked in cage 53, cage 54 empty. The 2026-09-19 capture is the
// same CWDM4 linked, with light.
const (
	opticFlagsDir = "optic-flags"
	darkPHY       = opticFlagsDir + "/show-interfaces-Ethernet49_1-phy-detail.txt"
)

func TestParsePHYDetailModuleFlags(t *testing.T) {
	for _, tc := range []struct {
		file        string
		rxLOS, txFt bool
	}{
		{darkPHY, true, false}, // seated, no light
		{opticFlagsDir + "/show-interfaces-Ethernet49_2-phy-detail.txt", true, false},  // the same module's inactive second lane
		{opticFlagsDir + "/show-interfaces-Ethernet53_1-phy-detail.txt", false, false}, // DAC, linked
		{opticFlagsDir + "/show-interfaces-Ethernet54_1-phy-detail.txt", false, false}, // empty cage
		{"show-interfaces-Ethernet49_1-phy-detail.txt", false, false},                  // the CWDM4 with light (2026-09-19)
	} {
		b, err := os.ReadFile(filepath.Join(aclFixtureDir, tc.file))
		if err != nil {
			t.Fatal(err)
		}
		d := parsePHYDetail(string(b))
		if !d.HasXcvrFlags || d.RxLOS != tc.rxLOS || d.TxFault != tc.txFt {
			t.Errorf("%s: flags %v rx_los %v tx_fault %v, want rx_los %v tx_fault %v", tc.file, d.HasXcvrFlags, d.RxLOS, d.TxFault, tc.rxLOS, tc.txFt)
		}
	}
}

// eAPI's `| json` of phy detail is the same text in a string, nothing
// structured: the driver keeps the text form.
func TestPHYDetailJSONIsText(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(aclFixtureDir, opticFlagsDir, "show-interfaces-Ethernet49_1-phy-detail.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		InterfacePhyStatuses map[string]struct {
			PhyStatuses []map[string]json.RawMessage `json:"phyStatuses"`
		} `json:"interfacePhyStatuses"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	st := r.InterfacePhyStatuses["Ethernet49/1"].PhyStatuses
	if len(st) != 1 {
		t.Fatalf("phyStatuses = %d entries", len(st))
	}
	var text string
	if err := json.Unmarshal(st[0]["text"], &text); err != nil {
		t.Fatal(err)
	}
	for k := range st[0] {
		if k != "text" && k != "description" {
			t.Errorf("phy detail JSON has a structured key %q: the driver could read it instead of the text", k)
		}
	}
	if d := parsePHYDetail(text); !d.HasXcvrFlags || !d.RxLOS {
		t.Errorf("the JSON's text: %+v", d)
	}
}

// With eAPI a reading beyond its alarm threshold sets no fault: the dark
// thresholds capture against the 2026-09-19 phy detail (RX_LOS False).
func TestThresholdsAloneSetNoFault(t *testing.T) {
	ft := NewFixtureTransport(aclFixtureDir)
	ft.Overrides = map[string]string{"show interfaces transceiver dom thresholds": domNoLight}
	snap, err := NewCollector(ft).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := portByIndex(t, snap, 49); p.Health.OpticRxAlarm || p.Health.OpticTxAlarm {
		t.Errorf("port 49: rx %v tx %v from the thresholds alone", p.Health.OpticRxAlarm, p.Health.OpticTxAlarm)
	}
}

// jsonOnly is a transport that cannot run text, as the SSH transport.
type jsonOnly struct{ Transport }

// Without text the thresholds are the fallback: the dark readings (-30 dBm,
// a low alarm of -10, aggregate -Infinity) fault the cage.
func TestThresholdFallbackWithoutText(t *testing.T) {
	ft := NewFixtureTransport(aclFixtureDir)
	ft.Overrides = map[string]string{"show interfaces transceiver dom thresholds": domNoLight}
	snap, err := NewCollector(jsonOnly{ft}).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := portByIndex(t, snap, 49); !p.Health.OpticRxAlarm || p.Health.OpticTxAlarm {
		t.Errorf("port 49: rx %v tx %v, want true and false", p.Health.OpticRxAlarm, p.Health.OpticTxAlarm)
	}
	if len(ft.TextRuns) != 0 {
		t.Errorf("text ran on a JSON-only transport: %v", ft.TextRuns)
	}
}

// A seated cage that is down is read in a batch of its own: cage 50 of the
// 2026-09-19 capture (a 4x25G DAC, every lane down) has no phy detail
// capture, so its lanes are served the 2026-11-15 reads, the dark module on
// lane 2 and the DAC on the others. The cage takes its lane's fault, the
// lane port its own; the up ports' batch (port 49's FEC) is untouched, and
// the empty cages are not read.
func TestSeatedDownCageReadAlone(t *testing.T) {
	ft := NewFixtureTransport(aclFixtureDir)
	dac := opticFlagsDir + "/show-interfaces-Ethernet53_1-phy-detail.txt"
	ft.Overrides = map[string]string{
		"show interfaces Ethernet50/1 phy detail": dac,
		"show interfaces Ethernet50/2 phy detail": darkPHY,
		"show interfaces Ethernet50/3 phy detail": dac,
		"show interfaces Ethernet50/4 phy detail": dac,
	}
	c := NewCollector(ft)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p := portByIndex(t, snap, 50); !p.Health.OpticRxAlarm || p.Health.OpticTxAlarm {
		t.Errorf("cage 50: rx %v tx %v, want true and false", p.Health.OpticRxAlarm, p.Health.OpticTxAlarm)
	}
	if p := portByIndex(t, snap, 49); !p.Health.HasFECCounters || p.Health.FECUncorrected != 5 || p.Health.OpticRxAlarm {
		t.Errorf("port 49 (up, lit): %+v", p.Health)
	}
	v, err := c.Lanes(50)
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range v.Lanes {
		if want := i == 1; l.Health.OpticRxAlarm != want || l.Health.OpticTxAlarm {
			t.Errorf("lane %d: rx %v tx %v, want rx %v", l.Index, l.Health.OpticRxAlarm, l.Health.OpticTxAlarm, want)
		}
	}
	var read []string
	for _, b := range ft.TextRuns {
		read = append(read, b[1:]...)
	}
	for _, cmd := range read {
		for _, empty := range []string{"Ethernet51/1", "Ethernet52/1", "Ethernet54/1"} {
			if cmd == "show interfaces "+empty+" phy detail" {
				t.Errorf("read the empty cage %s", empty)
			}
		}
	}
	if len(ft.TextRuns) != 2 {
		t.Errorf("text batches = %d, want 2 (up ports, then the seated down cage): %v", len(ft.TextRuns), ft.TextRuns)
	}
}
