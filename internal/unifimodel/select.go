// Package unifimodel chooses which UniFi switch model the bridge claims.
//
// The controller renders a device from *its own* hardware profile for the
// claimed model string, so the claim must exist in the controller's
// database and should match the switch's port layout as closely as
// possible. The catalogue here is unifi-emu's model_profiles.json;
// docs/unifi-models.md explains how it is refreshed for a newer controller.
package unifimodel

import (
	"fmt"
	"sort"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
	emu "github.com/jamesbraid/unifi-emu"
)

// Layout is a switch's port mix in the controller's own vocabulary.
type Layout struct {
	Copper int // RJ45 of any speed ("standard" in the controller DB; media GE/2.5GbE/10GbE)
	SFP    int // 1G SFP
	SFPP   int // SFP+ (10G)
	SFP28  int
	QSFP28 int // includes QSFP+ cages
}

// Total is the port count.
func (l Layout) Total() int { return l.Copper + l.SFP + l.SFPP + l.SFP28 + l.QSFP28 }

// LayoutOf classifies a snapshot's ports.
func LayoutOf(snap *switchmodel.Snapshot) Layout {
	var l Layout
	for _, p := range snap.Ports {
		switch p.Media {
		case switchmodel.MediaSFP:
			l.SFP++
		case switchmodel.MediaSFPPlus:
			l.SFPP++
		case switchmodel.MediaSFP28:
			l.SFP28++
		case switchmodel.MediaQSFP28, switchmodel.MediaQSFPPlus:
			l.QSFP28++
		default:
			l.Copper++
		}
	}
	return l
}

func layoutOfProfile(p emu.ModelProfile) Layout {
	var l Layout
	for _, port := range p.Ports {
		switch port.Media {
		case "SFP":
			l.SFP++
		case "SFP+":
			l.SFPP++
		case "SFP28":
			l.SFP28++
		case "QSFP28", "QSFP+":
			l.QSFP28++
		default:
			l.Copper++
		}
	}
	return l
}

// Candidate is one catalogue model scored against a layout.
type Candidate struct {
	Model   string
	Display string
	Ports   int
	Layout  Layout
	Score   int    // lower is better; 0 = identical layout
	Note    string // set when the candidate is a fallback rather than a match
}

// Rank scores every switch model in the catalogue against l; lower is
// better. Weights, from what was observed on Network 10.6.106:
//
//   - port count ×100: the controller draws exactly the profile's ports,
//     so a count mismatch leaves ports undrawn or draws phantoms;
//   - QSFP28 cages ×20: the uplinks carry the switch's role, and a cage
//     drawn as SFP+ loses its 40G/100G identity in the topology;
//   - every other class ×1: cosmetic, because the device's own per-port
//     media report replaces the profile's icons once its capabilities are
//     accepted (see docs/feature-map.md §5);
//   - internal model codes (display name == code) +50: they are real
//     products (USWF066 is the ECS Aggregation) but may be renamed or
//     dropped by a later controller, which would strand the adoption.
func Rank(l Layout) []Candidate {
	var out []Candidate
	for _, m := range emu.Models() {
		p, ok := emu.Profile(m)
		if !ok || p.Type != "usw" {
			continue
		}
		pl := layoutOfProfile(p)
		score := 100 * abs(len(p.Ports)-l.Total())
		score += 20 * abs(pl.QSFP28-l.QSFP28)
		score += abs(pl.SFP28-l.SFP28) + abs(pl.SFPP-l.SFPP) + abs(pl.SFP-l.SFP) + abs(pl.Copper-l.Copper)
		if p.ModelDisplay == "" || p.ModelDisplay == m {
			score += 50
		}
		out = append(out, Candidate{Model: m, Display: p.ModelDisplay, Ports: len(p.Ports), Layout: pl, Score: score})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Fallback is claimed when no catalogue model has the switch's port
// count: the USW Leaf. It was a limited Early Access product with no
// firmware releases, so the controller has no expectations about it and
// never offers a firmware update (docs/unifi-models.md, "Choosing").
const Fallback = "UDC48X6"

// Suggest returns the best-scoring model with the switch's exact port
// count; when none exists it returns the Fallback with Note set, so the
// caller can say why. err is only for an unusable catalogue.
func Suggest(l Layout) (Candidate, error) {
	ranked := Rank(l)
	if len(ranked) == 0 {
		return Candidate{}, fmt.Errorf("empty model catalogue")
	}
	best := ranked[0]
	if best.Ports == l.Total() {
		return best, nil
	}
	for _, c := range ranked {
		if c.Model == Fallback {
			c.Note = fmt.Sprintf("no catalogue model has %d ports (closest by layout: %s with %d); claiming the USW Leaf, which has no firmware releases and so never triggers upgrade offers", l.Total(), best.Model, best.Ports)
			return c, nil
		}
	}
	return best, fmt.Errorf("no catalogue model has %d ports and the fallback %s is missing from the catalogue", l.Total(), Fallback)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
