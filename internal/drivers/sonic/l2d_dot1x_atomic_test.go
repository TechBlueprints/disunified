package sonic

import (
	"os"
	"strings"
	"testing"
)

// dot1xAtomicFixture is the DX010's run of l2d's DUI_DOT1X writes through
// admissions, revocations, restarts and a link loss (docs/drivers/sonic.md
// §11.26.11): xtables-monitor's events for the deployed l2d (old) and this
// one (new).
const dot1xAtomicFixture = "../../../docs/fixtures/sonic-igmp-202511.20261115-6c8eb1cf7/dot1x-atomic-live.txt"

// chainTxn is DUI_DOT1X after one nf_tables transaction that touched it.
type chainTxn struct {
	prog  string
	rules []string
}

// replayChain applies a run's xtables-monitor events to the chain as
// ebtables -L printed it at the start, and returns it after each
// transaction (NEWGEN) that touched it.
func replayChain(t *testing.T, sections map[string][]string, run string) []chainTxn {
	t.Helper()
	var chain []string
	cur := ""
	for _, l := range sections[run+"_start"] {
		if s, ok := strings.CutPrefix(l, "Bridge chain: "); ok {
			cur = strings.TrimSuffix(strings.Fields(s)[0], ",")
		} else if cur == "DUI_DOT1X" && strings.HasPrefix(l, "-") {
			chain = append(chain, strings.TrimSpace(l))
		}
	}
	var out []chainTxn
	touched := false
	for _, l := range sections[run+"_events"] {
		l = strings.TrimSpace(l)
		if e, ok := strings.CutPrefix(l, "EVENT: ebtables -t filter "); ok {
			op, rest, _ := strings.Cut(e, " ")
			c, rule, _ := strings.Cut(rest, " ")
			if c != "DUI_DOT1X" {
				continue
			}
			touched = true
			switch op {
			case "-A":
				chain = append(chain, rule)
			case "-D":
				i := -1
				for j, r := range chain {
					if r == rule {
						i = j
						break
					}
				}
				if i < 0 {
					t.Fatalf("%s: a delete of a rule the chain does not hold: %s", run, l)
				}
				chain = append(chain[:i], chain[i+1:]...)
			default:
				t.Fatalf("%s: an event the replay does not model: %s", run, l)
			}
		} else if strings.HasPrefix(l, "NEWGEN:") {
			if touched {
				prog := ""
				for _, f := range strings.Fields(l) {
					if v, ok := strings.CutPrefix(f, "NAME="); ok {
						prog = v
					}
				}
				out = append(out, chainTxn{prog, append([]string(nil), chain...)})
			}
			touched = false
		}
	}
	return out
}

// On the switch, every DUI_DOT1X write of this l2d is one transaction by
// ebtables-restore after which the closed port still drops (14 writes:
// admissions, idle revocations, the link loss, admissions again; the
// restarts wrote nothing), where the deployed l2d's 88 one-rule
// transactions left the port without its DROP 71 times.
func TestDot1xChainAtomicLive(t *testing.T) {
	data, err := os.ReadFile(dot1xAtomicFixture)
	if err != nil {
		t.Fatal(err)
	}
	sections := map[string][]string{}
	cur := ""
	for _, l := range strings.Split(string(data), "\n") {
		if s, ok := strings.CutPrefix(l, "@@@ "); ok {
			cur = s
			continue
		}
		if cur != "" {
			sections[cur] = append(sections[cur], l)
		}
	}
	const drop = "-i Ethernet116 -j DROP"
	open := func(txns []chainTxn) int {
		n := 0
		for _, x := range txns {
			has := false
			for _, r := range x.rules {
				has = has || r == drop
			}
			if !has {
				n++
			}
		}
		return n
	}
	nw, old := replayChain(t, sections, "new"), replayChain(t, sections, "old")
	if len(nw) != 14 || open(nw) != 0 {
		t.Errorf("new: %d transactions, %d without the port's DROP; want 14 and 0", len(nw), open(nw))
	}
	for _, x := range nw {
		if x.prog != "ebtables-restor" { // the kernel keeps 15 characters of the name
			t.Errorf("new: a transaction by %q, not ebtables-restore", x.prog)
		}
	}
	if len(old) != 88 || open(old) != 71 {
		t.Errorf("old: %d transactions, %d without the port's DROP; want 88 and 71", len(old), open(old))
	}
	if last := nw[len(nw)-1].rules; len(last) == 0 || last[len(last)-1] != drop {
		t.Errorf("new: the chain does not end in the port's DROP: %v", last)
	}
}
