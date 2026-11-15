package sonic

import (
	"fmt"
	"regexp"
	"strings"
)

// The FixtureRunner's kernel chain of blocked clients (block.go): the
// script's `ebtables -A` sources are kept as BlockMACs and its marker
// becomes the mac_block section, so the next Collect reads the hash back.
// A write must create or empty the chain, hook INPUT and leave a marker
// that is the sources' hash; a removal must unhook, drop the chain and the
// marker.

var (
	blockMarkRe = regexp.MustCompile(`(?m)^echo ([0-9a-f]{16}) > ` + regexp.QuoteMeta(blockMark) + `$`)
	blockAddRe  = regexp.MustCompile(`(?m)^ebtables -A ` + blockChain + ` -s '?([0-9a-f:]{17})'? -j DROP$`)
)

func (f *FixtureRunner) applyBlockScript(script string) error {
	if !strings.HasPrefix(script, "set -e\n") {
		return fmt.Errorf("mac block script without set -e")
	}
	adds := blockAddRe.FindAllStringSubmatch(script, -1)
	if len(adds) == 0 {
		for _, want := range []string{"ebtables -D INPUT -j " + blockChain, "ebtables -X " + blockChain, "rm -f " + blockMark} {
			if !strings.Contains(script, want) {
				return fmt.Errorf("mac block removal without %q", want)
			}
		}
		f.BlockMACs = nil
		f.UseSection("mac_block", "")
		return nil
	}
	for _, want := range []string{"ebtables -N " + blockChain + " -P RETURN", "ebtables -F " + blockChain, "ebtables -I INPUT 1 -j " + blockChain} {
		if !strings.Contains(script, want) {
			return fmt.Errorf("mac block write without %q", want)
		}
	}
	var macs []string
	for _, a := range adds {
		macs = append(macs, a[1])
	}
	m := blockMarkRe.FindStringSubmatch(script)
	if m == nil {
		return fmt.Errorf("mac block write without its marker")
	}
	if m[1] != blockHash(macs) {
		return fmt.Errorf("mac block marker %s is not the chain's hash %s", m[1], blockHash(macs))
	}
	f.BlockMACs = macs
	f.UseSection("mac_block", m[1])
	return nil
}
