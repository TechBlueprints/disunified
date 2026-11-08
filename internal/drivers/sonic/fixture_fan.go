package sonic

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var fanValsRe = regexp.MustCompile(`(?m)^vals = (\{.*\})$`)

// applyFan models fanScript on the capture's fancontrol section: the
// direction file takes the script's values, fancontrol and pmon's copy
// become that file, as the script's cp and docker cp make them.
func (f *FixtureRunner) applyFan(script string) error {
	m := fanValsRe.FindStringSubmatch(script)
	if m == nil {
		return fmt.Errorf("fan script without its values")
	}
	var vals map[string]string
	if err := json.Unmarshal([]byte(m[1]), &vals); err != nil {
		return err
	}
	body := sections(f.Fixture)["fancontrol"]
	var sec map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &sec); err != nil || len(sec) == 0 {
		return fmt.Errorf("cp: cannot stat fancontrol: no such file")
	}
	var airflow string
	_ = json.Unmarshal(sec["airflow"], &airflow)
	if !strings.Contains(script, `src="$d/fancontrol-`+airflow+`"`) {
		return fmt.Errorf("fan script writes the wrong direction file (airflow %s)", airflow)
	}
	var src map[string]string
	if err := json.Unmarshal(sec["fancontrol-"+airflow], &src); err != nil {
		return fmt.Errorf("no fancontrol-%s", airflow)
	}
	for k, v := range vals {
		src[k] = v
	}
	b := json.RawMessage(pyDict(src)) // json.dumps(sort_keys=True)'s layout, as collect.py prints it
	for _, n := range []string{"fancontrol-" + airflow, "fancontrol", "pmon"} {
		sec[n] = b
	}
	f.setRaw("fancontrol", sec)
	return nil
}

// setRaw re-renders a section of JSON values in collect.py's dump layout.
func (f *FixtureRunner) setRaw(name string, sec map[string]json.RawMessage) {
	keys := make([]string, 0, len(sec))
	for k := range sec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i == 0 {
			b.WriteString("\n")
		} else {
			b.WriteString(",\n")
		}
		kj, _ := json.Marshal(k)
		b.Write(kj)
		b.WriteString(": ")
		b.Write(sec[k])
	}
	b.WriteString("\n}\n")
	head := "@@@ " + name + "\n"
	i := strings.Index(f.Fixture, head)
	if i < 0 {
		return
	}
	rest := f.Fixture[i+len(head):]
	j := strings.Index(rest, "@@@ ")
	if j < 0 {
		return
	}
	f.Fixture = f.Fixture[:i] + head + b.String() + rest[j:]
}
