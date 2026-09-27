package apcbackups

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// FixtureRunner serves captured pages (docs/fixtures/apc-backups-*/
// <page>.htm and config.ini, the card's real output scrubbed by
// scripts/sanitize-apc-backups.py) instead of a card, and records every
// write. A control action moves the group's state in its copy of the
// Outlet Settings page, as the card would, so the next poll reads the new
// state back and the driver converges like it does live.
type FixtureRunner struct {
	Dir      string
	Pages_   map[string]string // page name -> HTML, mutable copies
	Config   string
	Controls []map[int]string // every Control call's actions
	Puts     []string         // every PutConfig body
	FailNext error            // returned by the next write, once
}

// NewFixtureRunner loads a fixture directory.
func NewFixtureRunner(dir string) (*FixtureRunner, error) {
	f := &FixtureRunner{Dir: dir, Pages_: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		switch {
		case strings.HasSuffix(e.Name(), ".htm"):
			f.Pages_[strings.TrimSuffix(e.Name(), ".htm")] = string(b)
		case e.Name() == "config.ini":
			f.Config = string(b)
		}
	}
	if len(f.Pages_) == 0 {
		return nil, fmt.Errorf("apc-backups: no pages in %s", dir)
	}
	return f, nil
}

// Pages implements Runner.
func (f *FixtureRunner) Pages(_ context.Context, names ...string) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range names {
		p, ok := f.Pages_[n]
		if !ok {
			return nil, fmt.Errorf("fixture: no page %q in %s", n, f.Dir)
		}
		out[n] = p
	}
	return out, nil
}

// Control implements Runner: records the actions, flips the switched
// groups' state in the Outlet Settings and On/Off/Reboot pages (a reboot
// is not a state: the group reads back on), and returns the SogControl
// value the card's confirmation would carry.
func (f *FixtureRunner) Control(_ context.Context, actions map[int]string) (string, error) {
	if f.FailNext != nil {
		err := f.FailNext
		f.FailNext = nil
		return "", err
	}
	f.Controls = append(f.Controls, actions)
	words := []string{"none", "none", "none", "none"}
	keys := make([]int, 0, len(actions))
	for n := range actions {
		keys = append(keys, n)
	}
	sort.Ints(keys)
	for _, n := range keys {
		state := ""
		switch actions[n] {
		case codeOn:
			state, words[n-1] = "On", "on"
		case codeOff:
			state, words[n-1] = "Off", "off"
		case codeReboot:
			state, words[n-1] = "On", "reboot"
		}
		if state != "" {
			f.setSOGState(n, state)
		}
	}
	return strings.Join(words, ",") + ",", nil
}

// setSOGState rewrites the state cell of switched group n in the pages
// that show it.
func (f *FixtureRunner) setSOGState(n int, state string) {
	cfgRe := regexp.MustCompile(fmt.Sprintf(`(?s)(name="SOG%dName"[^>]*/>\s*</td>\s*<td>)(On|Off)(</td>)`, n))
	f.Pages_["uloutcfg2"] = cfgRe.ReplaceAllString(f.Pages_["uloutcfg2"], "${1}"+state+"${3}")
	// The On/Off/Reboot page lists the switched groups in order, each as
	// a name cell followed by its state cell.
	ctl := f.Pages_["ulsogctl"]
	rowRe := regexp.MustCompile(`(<td class="dataName">[^<]*</td>\s*<td>)(On|Off)(</td>)`)
	i := 0
	f.Pages_["ulsogctl"] = rowRe.ReplaceAllStringFunc(ctl, func(m string) string {
		i++
		if i != n {
			return m
		}
		return rowRe.ReplaceAllString(m, "${1}"+state+"${3}")
	})
}

// PutConfig implements Runner: records the body and applies a BootMode
// change to the fixture's config, as the card would.
func (f *FixtureRunner) PutConfig(_ context.Context, body []byte) error {
	if f.FailNext != nil {
		err := f.FailNext
		f.FailNext = nil
		return err
	}
	f.Puts = append(f.Puts, string(body))
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "BootMode=") {
			re := regexp.MustCompile(`(?m)^BootMode=.*$`)
			f.Config = re.ReplaceAllString(f.Config, line)
		}
	}
	return nil
}

// GetConfig implements Runner.
func (f *FixtureRunner) GetConfig(_ context.Context) ([]byte, error) { return []byte(f.Config), nil }
