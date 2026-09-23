package apcpdu

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// FixtureRunner replays recorded snmpwalk output instead of talking to a PDU,
// and records what would have been written. Every fixture under
// docs/fixtures/apc-aos-3.9.2 is real output captured from Clint's AP7931 and
// scrubbed by scripts/sanitize-apc.py; nothing here is hand-written.
type FixtureRunner struct {
	// Values is the whole recorded MIB, numeric OID (no leading dot) -> value.
	Values map[string]string
	// Sets and Configs record writes in order, so a test can assert exactly
	// what the driver would have sent.
	Sets    []string
	Configs []string
	// FailSet makes the next SetInt fail, for the refusal paths.
	FailSet error
	// Config is the recorded config.ini (docs/fixtures/apc-aos-3.9.2/config.ini).
	Config []byte
}

// walkLine matches one line of `snmpwalk -On` output:
//
//	.1.3.6.1.4.1.318.1.1.12.1.5.0 = STRING: "AP7931"
var walkLine = regexp.MustCompile(`^\.?([0-9.]+) = ([A-Za-z0-9-]+): ?(.*)$`)

// NewFixtureRunner loads every walk-*.txt in dir into one MIB.
func NewFixtureRunner(dir string) (*FixtureRunner, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	f := &FixtureRunner{Values: map[string]string{}}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "walk-") || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		b, err := os.ReadFile(dir + "/" + e.Name())
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			m := walkLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
			if m == nil {
				continue
			}
			f.Values[m[1]] = normaliseFixtureValue(m[2], m[3])
		}
	}
	if len(f.Values) == 0 {
		return nil, fmt.Errorf("apc-pdu: no walk-*.txt fixture lines in %s", dir)
	}
	if b, err := os.ReadFile(dir + "/config.ini"); err == nil {
		f.Config = b
	}
	return f, nil
}

func (f *FixtureRunner) GetConfig(_ context.Context) ([]byte, error) {
	if f.Config == nil {
		return nil, fmt.Errorf("apc-pdu: fixture has no config.ini")
	}
	return f.Config, nil
}

// normaliseFixtureValue turns snmpwalk's rendered value into what the live
// runner's pduValue would have produced for the same object.
func normaliseFixtureValue(typ, raw string) string {
	v := strings.TrimSpace(raw)
	switch typ {
	case "STRING":
		// APC's DisplayStrings are printed quoted; the wire value is not.
		if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
			v = v[1 : len(v)-1]
		}
	case "Timeticks":
		// "(470770) 1:18:27.70" -> the raw tick count.
		if i := strings.Index(v, ")"); strings.HasPrefix(v, "(") && i > 1 {
			v = v[1:i]
		}
	case "OID":
		v = strings.TrimPrefix(v, ".")
	}
	return v
}

func (f *FixtureRunner) Walk(_ context.Context, root string) (map[string]string, error) {
	root = strings.TrimPrefix(root, ".")
	out := map[string]string{}
	for oid, v := range f.Values {
		if oid == root || strings.HasPrefix(oid, root+".") {
			out[oid] = v
		}
	}
	return out, nil
}

func (f *FixtureRunner) SetInt(_ context.Context, oid string, v int) error {
	if f.FailSet != nil {
		return f.FailSet
	}
	f.Sets = append(f.Sets, fmt.Sprintf("%s=%d", oid, v))
	// A write the device accepted is visible to the next read, the way the
	// real agent behaves -- with one exception the card itself makes. An
	// immediate-reboot command is not a state: the card opens the relay,
	// waits its configured reboot duration and closes it again, and by the
	// next poll the outlet reads back as on. A fixture that stored the 3
	// would make the following reconcile see an outlet that is off and
	// switch it on, which the real card never asks for.
	if v == outletCmdReboot && strings.HasPrefix(strings.TrimPrefix(oid, "."), oidOutletCtlCmd+".") {
		v = outletCmdOn
	}
	f.Values[strings.TrimPrefix(oid, ".")] = fmt.Sprint(v)
	return nil
}

func (f *FixtureRunner) PutConfig(_ context.Context, body []byte) error {
	f.Configs = append(f.Configs, string(body))
	return nil
}
