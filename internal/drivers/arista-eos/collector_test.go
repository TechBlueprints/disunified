package aristaeos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/switchmodel"
)

// fixtureTransport serves docs/fixtures/arista-eos-4.26.14M/<cmd>.json, where the
// file name is the command with spaces -> "-" and "/" -> "_" (the naming the
// capture script used).
type fixtureTransport struct {
	dir        string
	runs       [][]string
	textRuns   [][]string // RunText calls (text-only commands), counted separately
	configured [][]string
	overrides  map[string]string // command -> fixture file name, for scenario variants
}

func (f *fixtureTransport) Run(_ context.Context, cmds []string) ([]json.RawMessage, error) {
	f.runs = append(f.runs, cmds)
	out := make([]json.RawMessage, 0, len(cmds))
	for _, c := range cmds {
		if c == "enable" {
			out = append(out, json.RawMessage("{}"))
			continue
		}
		name := strings.NewReplacer(" ", "-", "/", "_").Replace(c) + ".json"
		if o, ok := f.overrides[c]; ok {
			name = o
		}
		b, err := os.ReadFile(filepath.Join(f.dir, name))
		if err != nil {
			return nil, fmt.Errorf("no fixture for %q: %w", c, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// RunText serves <cmd>.txt fixtures (text-only commands).
func (f *fixtureTransport) RunText(_ context.Context, cmds []string) ([]string, error) {
	f.textRuns = append(f.textRuns, cmds)
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		if c == "enable" {
			out = append(out, "")
			continue
		}
		name := strings.NewReplacer(" ", "-", "/", "_").Replace(c) + ".txt"
		b, err := os.ReadFile(filepath.Join(f.dir, name))
		if err != nil {
			return nil, fmt.Errorf("no text fixture for %q: %w", c, err)
		}
		out = append(out, string(b))
	}
	return out, nil
}

func (f *fixtureTransport) Configure(_ context.Context, cmds []string) error {
	f.configured = append(f.configured, cmds)
	return nil
}

func (f *fixtureTransport) Close() error { return nil }

func newFixtureCollector(t *testing.T) (*Collector, *fixtureTransport) {
	t.Helper()
	ft := &fixtureTransport{dir: filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M")}
	return NewCollector(ft), ft
}

func portByIndex(t *testing.T, snap *switchmodel.Snapshot, idx int) switchmodel.Port {
	t.Helper()
	for _, p := range snap.Ports {
		if p.Index == idx {
			return p
		}
	}
	t.Fatalf("no port with index %d", idx)
	return switchmodel.Port{}
}

func TestStartRunsEveryCommandOnce(t *testing.T) {
	c, ft := newFixtureCollector(t)
	if _, err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ft.runs) != 2 {
		t.Fatalf("Start made %d transport calls, want 2 (startup + first poll)", len(ft.runs))
	}
}

func TestSystem(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := snap.System
	if s.Vendor != "Arista" || s.Model != "DCS-7160-48TC6-F" || s.Version != "4.26.14M" {
		t.Errorf("identity = %+v", s)
	}
	if s.MAC != "02:00:00:00:00:3c" {
		t.Errorf("MAC = %q (scrubbed fixture value expected)", s.MAC)
	}
	if s.Hostname != "localhost" {
		t.Errorf("hostname = %q", s.Hostname)
	}
	if got := s.Uptime.Hours(); got < 1604 || got > 1605 {
		t.Errorf("uptime = %v", s.Uptime)
	}
	if s.CPUPercent < 19.3 || s.CPUPercent > 19.5 { // idle 80.6
		t.Errorf("cpu = %v", s.CPUPercent)
	}
	if s.MemTotalKB != 8099020 || s.MemUsedKB != 930304 || s.MemBufferKB != 3209318 {
		t.Errorf("mem = %d/%d/%d", s.MemTotalKB, s.MemUsedKB, s.MemBufferKB)
	}
	if !s.HasTemperature || s.TemperatureC < 40 || s.TemperatureC > 100 {
		t.Errorf("temperature = %v (%v)", s.TemperatureC, s.HasTemperature)
	}
}

func TestPortsLayout(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Ports) != 54 {
		t.Fatalf("got %d ports, want 54 (48 copper + 6 QSFP slots)", len(snap.Ports))
	}
	for i, p := range snap.Ports {
		if p.Index != i+1 {
			t.Fatalf("ports[%d].Index = %d, want %d", i, p.Index, i+1)
		}
	}
	for i := 1; i <= 48; i++ {
		if p := portByIndex(t, snap, i); p.Media != switchmodel.MediaCopper10G {
			t.Errorf("port %d media = %q, want 10G-T", i, p.Media)
		}
	}
	for i := 49; i <= 54; i++ {
		if p := portByIndex(t, snap, i); p.Media != switchmodel.MediaQSFP28 {
			t.Errorf("port %d media = %q, want QSFP28", i, p.Media)
		}
	}
}

func TestDownCopperPort(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, _ := c.Start(context.Background())
	p := portByIndex(t, snap, 1)
	if p.Up || !p.Enabled || !p.Present || p.SpeedMbps != 0 || p.FullDuplex {
		t.Errorf("port 1 = %+v", p)
	}
	if p.MTU != 9214 || p.IfName != "Ethernet1" || p.Name != "Ethernet1" || p.Lanes != 1 {
		t.Errorf("port 1 = %+v", p)
	}
}

func TestUplinkPort(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, _ := c.Start(context.Background())
	p := portByIndex(t, snap, 49)
	if !p.Up || p.SpeedMbps != 100000 || !p.FullDuplex || p.MTU != 9214 {
		t.Errorf("port 49 = %+v", p)
	}
	if p.Counters.RxBytes != 18015430154652 || p.Counters.TxBytes != 48636293789 {
		t.Errorf("port 49 bytes = %+v", p.Counters)
	}
	if p.Counters.RxPackets != 11984958269+291599364+100201842 {
		t.Errorf("port 49 rx packets = %d", p.Counters.RxPackets)
	}
	if p.Counters.RxErrors != 10 || p.Counters.TxErrors != 0 {
		t.Errorf("port 49 errors = %+v", p.Counters)
	}
	if p.STPState != "forwarding" {
		t.Errorf("port 49 stp = %q", p.STPState)
	}
	if p.Neighbor == nil {
		t.Fatal("port 49 has no LLDP neighbour")
	}
	n := p.Neighbor
	if n.SystemName != "neighbor-1" || n.PortID != "one00GigE48" || n.ChassisID != "02:00:00:00:00:3d" || n.ManagementIP != "192.0.2.2" {
		t.Errorf("port 49 neighbour = %+v", n)
	}
	if !n.IsBridge || !n.IsRouter {
		t.Errorf("port 49 neighbour caps = %+v", n)
	}
}

func TestBreakoutFolded(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, _ := c.Start(context.Background())
	p := portByIndex(t, snap, 50)
	if p.Lanes != 4 || p.IfName != "Ethernet50" {
		t.Errorf("port 50 = %+v", p)
	}
	if p.Up {
		t.Errorf("port 50 should be down (all lanes notconnect): %+v", p)
	}
	if p.Media != switchmodel.MediaQSFP28 {
		t.Errorf("port 50 media = %q", p.Media)
	}
}

func TestEmptyCageNotPresent(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, _ := c.Start(context.Background())
	p := portByIndex(t, snap, 52)
	if p.Present || p.Up {
		t.Errorf("port 52 (no optic) = %+v", p)
	}
	if p.Media != switchmodel.MediaQSFP28 {
		t.Errorf("port 52 media = %q, want QSFP28 inferred from 100G bandwidth", p.Media)
	}
}

func TestNonFrontPanelInterfacesIgnored(t *testing.T) {
	c, _ := newFixtureCollector(t)
	snap, _ := c.Start(context.Background())
	for _, p := range snap.Ports {
		if strings.HasPrefix(p.IfName, "Management") || strings.HasPrefix(p.IfName, "Port-Channel") || strings.HasPrefix(p.IfName, "Vlan") {
			t.Errorf("non-front-panel interface leaked into ports: %s", p.IfName)
		}
	}
}

func TestMediaFor(t *testing.T) {
	cases := []struct {
		typ  string
		bw   int64
		want switchmodel.Media
	}{
		{"10GBASE-T", 0, switchmodel.MediaCopper10G},
		{"10/100/1000", 1e9, switchmodel.MediaCopper1G},
		{"100GBASE-CR4", 1e11, switchmodel.MediaQSFP28},
		{"100GBASE-CWDM4", 1e11, switchmodel.MediaQSFP28},
		{"Not Present", 1e11, switchmodel.MediaQSFP28},
		{"N/A", 25e9, switchmodel.MediaSFP28},
		{"10GBASE-SR", 1e10, switchmodel.MediaSFPPlus},
		{"", 0, switchmodel.MediaUnknown},
	}
	for _, tc := range cases {
		if got := mediaFor(tc.typ, tc.bw); got != tc.want {
			t.Errorf("mediaFor(%q, %d) = %q, want %q", tc.typ, tc.bw, got, tc.want)
		}
	}
}
