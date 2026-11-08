package sonic

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// Fan mode (docs/drivers/sonic.md §11.14): the fancontrol floor of a
// platform whose ten chassis fans are driven by lm-sensors' fancontrol in
// pmon. The Celestica DX010 (x86_64-cel_seastone-r0) is the one platform
// with measured floors:
//
//   - Default is the platform's own table (2017, pwmconfig): the ramp from
//     MINTEMP 26 °C on the intake sensor, never below PWM 89 (35 %).
//   - Quiet is the table image patch 0012 bakes into the kit's images: the
//     ramp from 35 °C, never below PWM 51 (20 %), measured on the bench at
//     an ASIC around 56 °C (high 70, critical 75). Never lower: 15 % ran the
//     ASIC past 62 °C without levelling.
//
// MINSTART, MAXTEMP, MAXPWM and the hysteresis are the same in both, and
// thermalctld's policies (full speed over any high threshold) stay as they
// are. Only the F2B table was measured; a B2F unit is offered nothing.
//
// The boot script (/etc/init.d/fancontrol.sh) copies fancontrol-<airflow>
// over fancontrol and pmon copies that into its /etc/fancontrol at every
// start, so a mode is written to fancontrol-<airflow>, copied over
// fancontrol and into pmon, and only fancontrol is restarted inside pmon.
// The mode reported is read back from all three each collect; any of them
// out of step reads as no mode, and the loop writes the configured one.

// fanPlatforms are the platforms with measured floors, by airflow.
var fanPlatforms = map[string]map[string]map[devicemodel.FanMode]fanFloor{
	"x86_64-cel_seastone-r0": {
		"F2B": {
			devicemodel.FanModeDefault: {MinTemp: "26", MinStop: "89", MinPWM: "89"},
			devicemodel.FanModeQuiet:   {MinTemp: "35", MinStop: "51", MinPWM: "51"},
		},
	},
}

// fanFloor is what a mode sets on every PWM.
type fanFloor struct{ MinTemp, MinStop, MinPWM string }

// fanState is the collector's fancontrol section.
type fanState struct {
	Platform string
	Airflow  string
	Files    map[string]map[string]string // "fancontrol", "fancontrol-F2B", "pmon" -> key -> distinct values
}

// parseFan reads the fancontrol section; nil when the platform has no
// fancontrol (the VS, a capture from before the section).
func parseFan(body string) *fanState {
	var raw map[string]json.RawMessage
	if strings.TrimSpace(body) == "" || json.Unmarshal([]byte(body), &raw) != nil || len(raw) == 0 {
		return nil
	}
	fs := &fanState{Files: map[string]map[string]string{}}
	for k, v := range raw {
		switch k {
		case "platform":
			_ = json.Unmarshal(v, &fs.Platform)
		case "airflow":
			_ = json.Unmarshal(v, &fs.Airflow)
		default:
			var m map[string]string
			if json.Unmarshal(v, &m) == nil {
				fs.Files[k] = m
			}
		}
	}
	if fs.Platform == "" {
		return nil
	}
	return fs
}

// floors are the modes this switch can be set to, nil when none: an
// unmeasured platform or airflow, or the direction file missing.
func (fs *fanState) floors() map[devicemodel.FanMode]fanFloor {
	if fs == nil || fs.Files["fancontrol-"+fs.Airflow] == nil {
		return nil
	}
	return fanPlatforms[fs.Platform][fs.Airflow]
}

// why says what floors() found missing, for the refusal.
func (fs *fanState) why() string {
	switch {
	case fs == nil:
		return "the platform has no fancontrol table"
	case fanPlatforms[fs.Platform] == nil:
		return fmt.Sprintf("no measured fan floors for platform %s", fs.Platform)
	case fs.Airflow == "":
		return "the boot script recorded no fan airflow (fan_airflow)"
	case fanPlatforms[fs.Platform][fs.Airflow] == nil:
		return fmt.Sprintf("no measured fan floors for %s airflow on %s", fs.Airflow, fs.Platform)
	}
	return "fancontrol-" + fs.Airflow + " is missing"
}

// modeOf is the mode a file's floor matches, "" for none.
func modeOf(floors map[devicemodel.FanMode]fanFloor, f map[string]string) devicemodel.FanMode {
	for m, fl := range floors {
		if f["MINTEMP"] == fl.MinTemp && f["MINSTOP"] == fl.MinStop && f["MINPWM"] == fl.MinPWM {
			return m
		}
	}
	return ""
}

// mode is the mode the switch runs: the one fancontrol, its direction file
// and pmon's running copy all match. pmon's copy is skipped only when it
// could not be read (docker not reachable), which is logged.
func (fs *fanState) mode() (devicemodel.FanMode, bool) {
	floors := fs.floors()
	names := []string{"fancontrol", "fancontrol-" + fs.Airflow}
	pmon := fs.Files["pmon"] != nil
	if pmon {
		names = append(names, "pmon")
	}
	var cur devicemodel.FanMode
	for i, n := range names {
		m := modeOf(floors, fs.Files[n])
		if m == "" || (i > 0 && m != cur) {
			return "", pmon
		}
		cur = m
	}
	return cur, pmon
}

// fanReport fills the snapshot's fan-mode fields from the section.
func (c *Collector) fanReport(fs *fanState, sys *devicemodel.System) {
	floors := fs.floors()
	if floors == nil {
		if fs != nil {
			c.warnOnce("fan-mode", "fan mode not offered: %s", fs.why())
		}
		return
	}
	for m := range floors {
		sys.FanModes = append(sys.FanModes, m)
	}
	sort.Slice(sys.FanModes, func(i, j int) bool { return sys.FanModes[i] < sys.FanModes[j] })
	m, pmon := fs.mode()
	if !pmon {
		c.warnOnce("fan-pmon", "pmon's /etc/fancontrol could not be read (docker exec pmon); the fan mode is read from the platform files only")
	}
	sys.FanMode = m
}

// fanCommand runs fanScript (stdin).
const fanCommand = "bash -s -- disunified-fancontrol"

// fanScript sets fl on every PWM of the platform's fancontrol-<airflow>
// (written only if it changes, through a temporary file moved into place),
// copies it over fancontrol and into pmon, and restarts fancontrol there.
func fanScript(fs *fanState, fl fanFloor) string {
	dir := "/usr/share/sonic/device/" + fs.Platform
	vals, _ := json.Marshal(map[string]string{"MINTEMP": fl.MinTemp, "MINSTOP": fl.MinStop, "MINPWM": fl.MinPWM})
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("d=" + dir + "\n")
	b.WriteString("src=\"$d/fancontrol-" + fs.Airflow + "\"\n")
	b.WriteString(`sudo python3 - "$src" <<'DISUNIFIED_FANCONTROL'` + "\n")
	b.WriteString(`import os, sys, tempfile
path = sys.argv[1]
vals = ` + string(vals) + `
with open(path) as f:
    old = f.read()
out = []
for line in old.split("\n"):
    k, eq, rest = line.partition("=")
    if eq and k in vals:
        line = k + "=" + " ".join(t.rpartition("=")[0] + "=" + vals[k] for t in rest.split())
    out.append(line)
new = "\n".join(out)
if new != old:
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".fancontrol.")
    with os.fdopen(fd, "w") as f:
        f.write(new)
    os.chmod(tmp, 0o644)
    os.replace(tmp, path)
`)
	b.WriteString("DISUNIFIED_FANCONTROL\n")
	b.WriteString("sudo cp \"$src\" \"$d/fancontrol\"\n")
	b.WriteString("docker cp \"$d/fancontrol\" pmon:/etc/fancontrol\n")
	b.WriteString("docker exec pmon supervisorctl restart fancontrol\n")
	return b.String()
}

// ApplyFanMode implements devicemodel.FanModeController.
func (c *Collector) ApplyFanMode(ctx context.Context, mode devicemodel.FanMode) error {
	c.mu.Lock()
	fs := c.fan
	c.mu.Unlock()
	fl, ok := fs.floors()[mode]
	if !ok {
		return fmt.Errorf("sonic: fan mode %s not offered: %s", mode, fs.why())
	}
	c.Log.Printf("sonic fan mode %s: fancontrol-%s MINTEMP %s MINSTOP %s MINPWM %s on every PWM, copied over fancontrol and into pmon, fancontrol restarted", mode, fs.Airflow, fl.MinTemp, fl.MinStop, fl.MinPWM)
	if _, err := c.run(ctx, fanCommand, fanScript(fs, fl)); err != nil {
		return fmt.Errorf("sonic: fan mode %s: %w", mode, err)
	}
	return nil
}
