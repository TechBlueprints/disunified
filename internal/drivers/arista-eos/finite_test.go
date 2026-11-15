package aristaeos

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const domNoLight = "dom-no-light/show-interfaces-transceiver-dom-thresholds.json"

// The switch's own reply for a module with no receive signal is not JSON
// (its aggrRxPwr is -Infinity); finiteJSON makes it so, and leaves the same
// text alone inside a string.
func TestFiniteJSONOnTheNoLightCapture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(aclFixtureDir, domNoLight))
	if err != nil {
		t.Fatal(err)
	}
	if json.Valid(raw) {
		t.Fatal("the capture parses as it is: it no longer holds the token this test is about")
	}
	if !bytes.Contains(raw, []byte("-Infinity")) {
		t.Fatal("the capture has no -Infinity")
	}
	clean := finiteJSON(raw)
	if !json.Valid(clean) {
		t.Fatal("finiteJSON left the reply unparseable")
	}
	var th showTransceiverThresholds
	if err := json.Unmarshal(clean, &th); err != nil {
		t.Fatal(err)
	}
	aggr := th.Interfaces["Ethernet49/1"].Parameters["aggrRxPwr"].Channels
	if v, ok := aggr["-"]; !ok || v != nil {
		t.Errorf("aggrRxPwr channel = %v (present %v), want a nil reading", v, ok)
	}
	if v := th.Interfaces["Ethernet49/1"].Parameters["rxPower"].Channels["1"]; v == nil || *v != -30 {
		t.Errorf("rxPower lane 1 = %v, want -30", v)
	}

	// The whole capture as one JSON string: the token is text there.
	quoted, err := json.Marshal(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := finiteJSON(quoted); !bytes.Equal(got, quoted) {
		t.Error("finiteJSON rewrote a token inside a string")
	}
}

// A poll whose thresholds reply carries the token collects, and the cage
// with the dark optic reports a receive fault (its lanes read -30 dBm
// against a low alarm of -10) and no transmit fault.
func TestCollectWithANoLightOptic(t *testing.T) {
	ft := NewFixtureTransport(aclFixtureDir)
	ft.Overrides = map[string]string{"show interfaces transceiver dom thresholds": domNoLight}
	snap, err := NewCollector(ft).Start(context.Background())
	if err != nil {
		t.Fatalf("collect with the no-light capture: %v", err)
	}
	for _, p := range snap.Ports {
		if p.Index != 49 {
			continue
		}
		if !p.Health.OpticRxAlarm || p.Health.OpticTxAlarm {
			t.Errorf("port 49: rx alarm %v tx alarm %v, want true and false", p.Health.OpticRxAlarm, p.Health.OpticTxAlarm)
		}
		return
	}
	t.Fatal("no port 49 in the snapshot")
}
