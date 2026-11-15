package device

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The link-flap and STP-flap alerts on bridged ports (Network 11.0.99,
// 2026-11-15, docs/drivers/sonic.md §11.23): the controller raised each at
// the first inform whose count had grown faster than a rate since the inform
// before -- link_down_count above 0.3 per second, stp_state_change_count
// above 0.2 per second -- and its COUNT is the count that inform carried.
// The capture brackets the two rates: every interval before the alert grew
// slower than the rate, the alert's interval faster. Raising either alert
// needs nothing from the bridge but the raw counts (tables.go).
func TestFlapAlertRates(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99")
	var rates struct {
		LinkFlap flapRun `json:"link_flap"`
		STPFlap  flapRun `json:"stp_flap"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "alert-rates-20261115.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &rates); err != nil {
		t.Fatal(err)
	}
	var alerts struct {
		Data []struct {
			Key        string
			Timestamp  int64
			Parameters struct {
				COUNT struct{ Name string }
				PORT  struct{ ID string }
			}
		}
	}
	b, err = os.ReadFile(filepath.Join(dir, "system-log-device-alert-bridged.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &alerts); err != nil {
		t.Fatal(err)
	}
	alertCount := map[string]string{}
	for _, a := range alerts.Data {
		alertCount[a.Key] = a.Parameters.COUNT.Name
	}
	for _, c := range []struct {
		name, key string
		rate      float64
		exp       flapRun
	}{
		{"link flap", "LOG_SWITCH_PORT_LINK_FLAP", 0.3, rates.LinkFlap},
		{"STP flap", "LOG_SWITCH_PORT_STP_FLAP", 0.2, rates.STPFlap},
	} {
		if got := alertCount[c.key]; got == "" || got != itoaAny(c.exp.AlertCount) {
			t.Errorf("%s: alert COUNT %q, want %d (the count the alert's inform carried)", c.name, got, c.exp.AlertCount)
		}
		var prevT time.Time
		var prevN float64
		alerted := false
		for i, in := range c.exp.Informs {
			at, err := time.Parse("15:04:05", in[0].(string))
			if err != nil {
				t.Fatal(err)
			}
			n := in[1].(float64)
			if i > 0 && !alerted {
				r := (n - prevN) / at.Sub(prevT).Seconds()
				if in[0] == c.exp.AlertAt {
					alerted = true
					if int(n) != c.exp.AlertCount || r <= c.rate {
						t.Errorf("%s: the alert's inform %s: count %v rate %.3f/s, want %d above %.1f/s", c.name, in[0], n, r, c.exp.AlertCount, c.rate)
					}
				} else if r >= c.rate {
					t.Errorf("%s: inform %s grew %.3f/s, at or above %.1f/s, and raised nothing", c.name, in[0], r, c.rate)
				}
			}
			prevT, prevN = at, n
		}
		if !alerted {
			t.Errorf("%s: no inform at %s in the capture", c.name, c.exp.AlertAt)
		}
	}
}

// flapRun is one experiment of alert-rates-20261115.json: the counts per
// inform and the inform the alert came with.
type flapRun struct {
	Port       int
	AlertAt    string `json:"alert_at"`
	AlertCount int    `json:"alert_count"`
	Informs    [][2]any
}

func itoaAny(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
