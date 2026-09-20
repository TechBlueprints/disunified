package unifimodel

import "testing"

func TestSuggestArista7160(t *testing.T) {
	// 48x 10GBASE-T + 6x QSFP28: UDC48X6 (48 SFP28 + 6 QSFP28) is the only
	// 54-port model with six QSFP28 cages, so it must win despite the
	// copper-vs-SFP28 front (cosmetic: the device's media report wins).
	c, err := Suggest(Layout{Copper: 48, QSFP28: 6})
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "UDC48X6" {
		t.Errorf("got %s (%+v), want UDC48X6", c.Model, c)
	}
}

func TestSuggestFallsBackToLeaf(t *testing.T) {
	c, err := Suggest(Layout{Copper: 47, QSFP28: 6}) // 53 ports: no model
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != Fallback || c.Note == "" {
		t.Errorf("got %s note %q, want the Leaf fallback with a note", c.Model, c.Note)
	}
}
