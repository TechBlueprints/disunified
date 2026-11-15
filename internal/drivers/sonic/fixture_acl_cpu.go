package sonic

import (
	"fmt"
	"regexp"
	"strings"
)

// The FixtureRunner's kernel mirror (acl_cpu.go): the script's
// iptables-restore input is kept as CPURules and its marker becomes the
// acl_cpu section, so the next Collect reads the hash back. A write must
// restore, hook PREROUTING and leave a marker; a removal must unhook, drop
// the chains and the marker.

var cpuMarkRe = regexp.MustCompile(`(?m)^echo ([0-9a-f]{16}) > ` + regexp.QuoteMeta(cpuMark) + `$`)

func (f *FixtureRunner) applyCPUScript(script string) error {
	if !strings.HasPrefix(script, "set -e\n") {
		return fmt.Errorf("acl cpu script without set -e")
	}
	var restore string
	for _, l := range strings.Split(script, "\n") {
		if strings.HasPrefix(l, "printf '%s\\n' ") && strings.HasSuffix(l, " | iptables-restore -n") {
			restore = strings.TrimSuffix(l, " | iptables-restore -n")
		}
	}
	if restore == "" {
		for _, want := range []string{"iptables -t raw -D PREROUTING", "iptables -t raw -X", "rm -f " + cpuMark} {
			if !strings.Contains(script, want) {
				return fmt.Errorf("acl cpu removal without %q", want)
			}
		}
		f.CPURules = nil
		f.UseSection("acl_cpu", "")
		return nil
	}
	w := shellWords(restore)
	if len(w) < 3 || w[0] != "printf" {
		return fmt.Errorf("acl cpu: unreadable restore line")
	}
	lines := w[2:]
	if lines[0] != "*raw" || lines[len(lines)-1] != "COMMIT" {
		return fmt.Errorf("acl cpu: restore input is not one raw-table commit")
	}
	if !strings.Contains(script, "iptables -t raw -A PREROUTING -i \"$i\" -j "+cpuChain) {
		return fmt.Errorf("acl cpu write without its PREROUTING hooks")
	}
	m := cpuMarkRe.FindStringSubmatch(script)
	if m == nil {
		return fmt.Errorf("acl cpu write without its marker")
	}
	if m[1] != cpuHash(lines) {
		return fmt.Errorf("acl cpu marker %s is not the restore input's hash %s", m[1], cpuHash(lines))
	}
	f.CPURules = lines
	f.UseSection("acl_cpu", m[1])
	return nil
}
