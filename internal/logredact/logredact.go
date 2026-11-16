// Package logredact keeps the controller's pushed secrets out of the
// bridge's log. The inform loop hands it every secret value a push carries
// (unificfg.Secrets, unificfg.RedactJSON); every log line the process writes
// goes through Writer, which replaces each value it was handed with
// unificfg.Redacted. It is the net under the drivers' own care (the sonic
// driver never puts a RADIUS secret or an SNMP community in a message): an
// error that echoes a command, an eAPI reply naming the failing command, a
// driver's debug line.
//
// Values shorter than MinLen are not masked: a one-letter secret would
// mangle every line. Values are kept for the life of the process (a
// changed secret's old value may still be echoed by the change that
// removes it), up to MaxValues.
package logredact

import (
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/TechBlueprints/disunified/internal/unificfg"
)

const (
	// MinLen is the shortest value masked.
	MinLen = 4
	// MaxValues caps how many values are kept; the oldest go first.
	MaxValues = 1024
	// mask replaces a value, as in the reply log.
	mask = unificfg.Redacted
)

var (
	mu       sync.RWMutex
	order    []string
	known    = map[string]bool{}
	replacer *strings.Replacer
)

// Remember adds values to mask from now on.
func Remember(values ...string) {
	mu.Lock()
	defer mu.Unlock()
	added := false
	for _, v := range values {
		if len(v) < MinLen || v == mask || known[v] || strings.Trim(v, "0") == "" {
			continue
		}
		known[v] = true
		order = append(order, v)
		added = true
	}
	for len(order) > MaxValues {
		delete(known, order[0])
		order = order[1:]
		added = true
	}
	if added {
		replacer = build(order)
	}
}

// build masks the longest values first, so a secret that contains another
// is masked whole.
func build(vals []string) *strings.Replacer {
	if len(vals) == 0 {
		return nil
	}
	s := append([]string(nil), vals...)
	sort.Slice(s, func(i, j int) bool { return len(s[i]) > len(s[j]) })
	pairs := make([]string, 0, 2*len(s))
	for _, v := range s {
		pairs = append(pairs, v, mask)
	}
	return strings.NewReplacer(pairs...)
}

// String masks every remembered value in s.
func String(s string) string {
	mu.RLock()
	r := replacer
	mu.RUnlock()
	if r == nil {
		return s
	}
	return r.Replace(s)
}

// Reset forgets every value (tests).
func Reset() {
	mu.Lock()
	order, known, replacer = nil, map[string]bool{}, nil
	mu.Unlock()
}

type writer struct{ w io.Writer }

// Writer wraps w: each Write is masked before it is passed on. log.Logger
// writes a whole line per call, so a value never straddles two writes.
func Writer(w io.Writer) io.Writer { return writer{w} }

func (x writer) Write(p []byte) (int, error) {
	s := String(string(p))
	if _, err := io.WriteString(x.w, s); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Stderr is os.Stderr through Writer: the output of every logger the
// bridge makes.
var Stderr = Writer(os.Stderr)
