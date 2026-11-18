package modulelayout

import (
	"reflect"
	"testing"

	"github.com/TechBlueprints/disunified/internal/fecpolicy"
)

// Every row of docs/module-layout.md §1, native layout first.
func TestRunsHoldsTheTable(t *testing.T) {
	ext := func(codes ...int) []Module {
		out := []Module{}
		for _, c := range codes {
			out = append(out, Extended(c))
		}
		return out
	}
	eth := func(bits ...int) []Module {
		out := []Module{}
		for _, b := range bits {
			out = append(out, Module{Ethernet: b})
		}
		return out
	}
	for _, tc := range []struct {
		row  string
		mods []Module
		want []Layout
	}{
		{"100G single-layout optics", ext(0x03, 0x04, 0x06, 0x17, 0x20, 0x21, 0x25, 0x26, 0x27), []Layout{L1x100}},
		{"SR4, PSM4, AOC, ACC", ext(0x02, 0x07, 0x01, 0x08, 0x18, 0x19), []Layout{L1x100, L4x25}},
		{"CR4 / CA-25G-L", ext(0x0B), []Layout{L1x100, L4x25, L2x50}},
		{"CA-25G-S, CA-25G-N", ext(0x0C, 0x0D), []Layout{L4x25, L2x50}},
		{"40G ER4, SWDM4", ext(0x10, 0x1F), []Layout{L1x40}},
		{"40G PSM4", ext(0x12), []Layout{L1x40, L4x10}},
		{"4 x 10GBASE-SR", ext(0x11), []Layout{L4x10}},
		{"40GBASE-LR4", eth(0x02), []Layout{L1x40}},
		{"40G SR4, CR4, XLPPI", eth(0x04, 0x08, 0x01), []Layout{L1x40, L4x10}},
		{"40GBASE-CR4,Extended with an unknown code", []Module{{Ethernet: 0x88, Extended: 0x00}}, []Layout{L1x40, L4x10}},
		{"40GBASE-CR4,Extended with a known code", []Module{{Ethernet: 0x88, Extended: 0x0B}}, []Layout{L1x100, L4x25, L2x50}},
	} {
		for _, m := range tc.mods {
			got, ok := Runs(m)
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: Runs(%#v) = %v %v, want %v", tc.row, m, got, ok, tc.want)
			}
		}
	}
	for _, m := range []Module{
		{},               // nothing
		Extended(0x00),   // Unspecified
		Extended(0x05),   // 100GBASE-SR10: not a QSFP28 layout
		Extended(0x0E),   // 10 Mb/s Single Pair Ethernet
		{Ethernet: 0x10}, // 10GBASE-SR in a QSFP
		{Extended: 0x06}, // a code without the Extended bit
		{Ethernet: 0x80, Extended: 0x30},
	} {
		if got, ok := Runs(m); ok {
			t.Errorf("Runs(%#v) = %v, want not known", m, got)
		}
	}
}

// The two tables speak for the same modules: every extended code the FEC
// policy names a medium for has a layout here.
func TestEveryFECModuleHasALayout(t *testing.T) {
	for code := 0; code < 0x100; code++ {
		switch fecpolicy.FromSFF8024(code) {
		case fecpolicy.Other, fecpolicy.Unknown:
			continue
		}
		if _, ok := Runs(Extended(code)); !ok {
			t.Errorf("code 0x%02X has a FEC medium but no layout", code)
		}
	}
}

// Runs hands out a copy: a caller that edits its slice changes no row.
func TestRunsIsACopy(t *testing.T) {
	a, _ := Runs(Extended(0x0B))
	a[0] = Layout{}
	if b, _ := Runs(Extended(0x0B)); b[0] != L1x100 {
		t.Fatal("the table was changed through a result")
	}
}
