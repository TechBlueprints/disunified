package sonic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The FixtureRunner's model of disunified-l2d's installation (l2.go): the
// capture's l2d section is the switch's state, and each write updates it the
// way the switch would, so the next Collect reads it back.

var installRe = regexp.MustCompile(`^t=\$\(mktemp\) && echo (\S+) \| base64 -d > "\$t" && sudo install -D -m (\d+) "\$t" (\S+) && rm -f "\$t"$`)

func (f *FixtureRunner) l2Inst() l2Inst {
	in := parseL2Inst(f.section("l2d"))
	if in.Files == nil {
		in.Files = map[string]string{}
	}
	return in
}

func (f *FixtureRunner) setL2Inst(in l2Inst) {
	b, _ := json.Marshal(in)
	f.setSection("l2d", string(b)+"\n")
}

// applyL2 runs a `bash -s -- disunified-l2` script.
func (f *FixtureRunner) applyL2(script string) error {
	in := f.l2Inst()
	if !in.present {
		return fmt.Errorf("disunified-l2: the capture has no l2d section")
	}
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || line == "set -e":
		case installRe.MatchString(line):
			m := installRe.FindStringSubmatch(line)
			body, err := base64.StdEncoding.DecodeString(m[1])
			if err != nil {
				return fmt.Errorf("%s: %v", m[3], err)
			}
			f.L2Files[m[3]] = string(body)
			if m[3] == l2ConfPath {
				var c l2Conf
				if err := json.Unmarshal(body, &c); err != nil {
					return fmt.Errorf("%s: %v", l2ConfPath, err)
				}
				in.Conf = &c
				continue
			}
			in.Files[m[3]] = sha(string(body))
		case line == "sudo systemctl daemon-reload", line == "sudo systemctl enable disunified-l2d >/dev/null 2>&1":
		case line == "sudo systemctl stop disunified-l2d-deadman.timer >/dev/null 2>&1 || true":
			in.Deadman = "inactive"
		case strings.HasPrefix(line, "sudo systemd-run --quiet --unit=disunified-l2d-deadman "):
			if in.Deadman == "active" {
				return fmt.Errorf("Unit disunified-l2d-deadman.timer already exists")
			}
			in.Deadman = "active"
		case line == "sudo systemctl restart disunified-l2d":
			if in.Files[l2dPath] == "" || in.Files[l2dUnitPath] == "" {
				return fmt.Errorf("Unit disunified-l2d.service not found")
			}
			in.Service = "active"
		case line == "sudo systemctl reload disunified-l2d":
			if in.Service != "active" {
				return fmt.Errorf("disunified-l2d.service is not active, cannot reload")
			}
		default:
			return fmt.Errorf("disunified-l2: unexpected line %q", line)
		}
	}
	if in.Conf != nil && in.Service == "active" && in.Mstpd != "" && in.Conf.STP.Enabled {
		in.STPState = "2"
	}
	f.setL2Inst(in)
	return nil
}

// buildMstpd models the transient build unit having run to its end.
func (f *FixtureRunner) buildMstpd(script string) error {
	in := f.l2Inst()
	if !in.Docker {
		return fmt.Errorf("docker: command not found")
	}
	if !strings.Contains(script, "sudo systemd-run --quiet --collect --unit=disunified-mstpd-build /bin/sh "+mstpdBuildIn+"/host.sh") {
		return fmt.Errorf("disunified-mstpd-build: the build is not started in its own unit")
	}
	if f.BuildFails {
		in.Build = "failed: wget: bad address 'github.com'"
	} else {
		in.Build, in.Mstpd = "ok", mstpdVersion+"-"
	}
	f.setL2Inst(in)
	return nil
}

// l2Check answers l2CheckCommand: mstpd runs while the daemon is up with
// spanning tree on (and the fresh connection reaches the switch at all).
func (f *FixtureRunner) l2Check() string {
	in := f.l2Inst()
	if in.Service == "active" && in.Conf != nil && in.Conf.STP.Enabled && in.Mstpd != "" && !f.STPCutsUplink {
		return "running\n"
	}
	return "\n"
}
