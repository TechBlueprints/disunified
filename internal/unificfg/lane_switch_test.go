package unificfg

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The pushes of the first adopted lane switch (a split cage of the DX010 as
// a USFXG; Network 11.0.99, 2026-11-16; docs/drivers/breakout.md 16.10),
// and its parent's during the same run. The lane's drawer saves: 25 GbE
// with FC-FEC, then 10 GbE (no FEC key), then 25 GbE with FC-FEC again; on
// the parent, the join's forced 100 GbE with RS-FEC and the fold's forced
// 25 GbE with FC-FEC that the hand-over then set to Auto.
func pushes(t *testing.T, name string) map[string]*Config {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "docs", "fixtures", "controller-11.0.99", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]*Config{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var rec struct {
			Payload struct {
				Type       string `json:"_type"`
				SystemCfg  string `json:"system_cfg"`
				Cfgversion string `json:"cfgversion"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Payload.Type == "setparam" && rec.Payload.SystemCfg != "" {
			out[rec.Payload.Cfgversion] = Parse(rec.Payload.SystemCfg)
		}
	}
	return out
}

func TestLaneSwitchPushes(t *testing.T) {
	lane := pushes(t, "replies-lane-switch-dx010-cage3.ndjson")
	for _, tc := range []struct {
		ver   string
		speed int
		fec   string
	}{
		{"228f12a85c3ec0d9", 25000, "cl-74"},
		{"e4b863f2a87e553f", 10000, ""},
		{"1b6b4168f1319261", 25000, "cl-74"},
		{"c09c23495493524e", 0, ""}, // the seeded push: the lane on Auto
	} {
		c := lane[tc.ver]
		if c == nil {
			t.Fatalf("no push %s in the lane switch's log", tc.ver)
		}
		p := c.Ports[1]
		if p.SpeedMbps != tc.speed || p.FEC != tc.fec {
			t.Errorf("push %s lane 1: speed %d fec %q, want %d %q", tc.ver, p.SpeedMbps, p.FEC, tc.speed, tc.fec)
		}
		if len(c.Ports) != 5 {
			t.Errorf("push %s names %d ports, want the Flex XG's 5", tc.ver, len(c.Ports))
		}
	}
	parent := pushes(t, "replies-dx010-lane-handover.ndjson")
	for _, tc := range []struct {
		ver   string
		speed int
		fec   string
	}{
		{"36598feef70985f7", 0, ""},           // port 3 on Auto, the lane switch attached
		{"6a829ea1254a9d99", 100000, "cl-91"}, // the join from the drawer
		{"c2aee4ed47bfae93", 0, ""},           // Auto again: the module rule splits
		{"48ee765ca8d55f32", 25000, "cl-74"},  // the fold's forced 25G, lane switch forgotten
		{"6ffd82a23c583f4b", 0, ""},           // the hand-over set it to Auto
	} {
		c := parent[tc.ver]
		if c == nil {
			t.Fatalf("no push %s in the parent's log", tc.ver)
		}
		if p := c.Ports[3]; p.SpeedMbps != tc.speed || p.FEC != tc.fec {
			t.Errorf("push %s port 3: speed %d fec %q, want %d %q", tc.ver, p.SpeedMbps, p.FEC, tc.speed, tc.fec)
		}
	}
}
