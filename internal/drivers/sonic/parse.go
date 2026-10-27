package sonic

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// sections splits the collector's output on its "@@@ <name>" markers.
func sections(out string) map[string]string {
	sec := map[string]string{}
	name := ""
	var body strings.Builder
	flush := func() {
		if name != "" {
			sec[name] = body.String()
		}
		body.Reset()
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "@@@ ") {
			flush()
			name = strings.TrimSpace(strings.TrimPrefix(line, "@@@ "))
			continue
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	flush()
	return sec
}

// decodeJSON decodes a section, tolerating an empty one (a missing tool).
func decodeJSON(name, body string, v any) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		return fmt.Errorf("sonic: section %s: %w", name, err)
	}
	return nil
}

// table is one Redis section: key -> field -> value, as collect.py dumps it.
type table map[string]map[string]string

func parseTable(name, body string) (table, error) {
	t := table{}
	if err := decodeJSON(name, body, &t); err != nil {
		return nil, err
	}
	return t, nil
}

// byName re-keys a table by the part after the table name and its
// separator ("PORT|Ethernet0" -> "Ethernet0", "PORT_TABLE:Ethernet0" ->
// "Ethernet0"). CONFIG_DB and STATE_DB separate with '|', APPL_DB and
// COUNTERS_DB with ':'.
func (t table) byName(sep string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(t))
	for k, v := range t {
		if _, rest, ok := strings.Cut(k, sep); ok {
			out[rest] = v
		}
	}
	return out
}

// first returns the only (or the first, sorted) entry's fields; nil if empty.
func (t table) first() map[string]string {
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil
	}
	return t[keys[0]]
}

// known: SONiC writes "N/A" (and sometimes "") for a value the platform
// does not provide; the VS writes N/A for every pmon field.
func known(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && !strings.EqualFold(s, "N/A") && !strings.EqualFold(s, "None")
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atou(s string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return n
}

func atof(s string) (float64, bool) {
	if !known(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}

func isTrue(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "up", "on", "enabled", "yes":
		return true
	}
	return false
}

// laneCount counts a PORT entry's serdes lanes ("65,66,67,68" -> 4).
func laneCount(s string) int {
	n := 0
	for _, f := range strings.Split(s, ",") {
		if strings.TrimSpace(f) != "" {
			n++
		}
	}
	return n
}

// speedList parses STATE_DB supported_speeds ("10000,25000,40000,100000"),
// ascending and de-duplicated; empty on the VS.
func speedList(s string) []int {
	seen := map[int]bool{}
	var out []int
	for _, f := range strings.Split(s, ",") {
		if n := atoi(f); n > 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// fecOf maps SONiC's FEC vocabulary ("rs", "fc", "none"; "N/A" on the VS)
// onto the model's.
func fecOf(s string) devicemodel.FEC {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "rs":
		return devicemodel.FECRS
	case "fc":
		return devicemodel.FECFC
	case "none":
		return devicemodel.FECDisabled
	}
	return devicemodel.FECUnknown
}

// lldpCaps reads an LLDP system-capabilities value as lldp_syncd stores
// it: the LLDP-MIB BITS string in hex bytes ("28 00"), bit 0 the most
// significant bit of the first byte. Bit 2 is bridge (0x20), bit 4 router
// (0x08); a SONiC switch announces "28 00", both.
func lldpCaps(s string) (bridge, router bool) {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return false, false
	}
	b, err := strconv.ParseUint(f[0], 16, 8)
	if err != nil {
		return false, false
	}
	return b&0x20 != 0, b&0x08 != 0
}

// yamlValue reads one top-level "key: value" from sonic_version.yml.
func yamlValue(body, key string) string {
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `'"`)
		}
	}
	return ""
}

// vendorOf names the hardware vendor from the SKU and the platform string
// (x86_64-<vendor>_<board>-r0).
func vendorOf(hwsku, platform string) string {
	l := strings.ToLower(hwsku)
	if strings.Contains(l, "seastone") || strings.Contains(l, "dx010") || strings.HasPrefix(l, "celestica") {
		return "Celestica"
	}
	tok := platform
	if _, rest, ok := strings.Cut(tok, "-"); ok {
		tok = rest
	}
	tok, _, _ = strings.Cut(tok, "_")
	switch strings.ToLower(tok) {
	case "cel":
		return "Celestica"
	case "arista":
		return "Arista"
	case "dell", "dellemc":
		return "Dell"
	case "mlnx", "nvidia":
		return "NVIDIA"
	case "accton":
		return "Accton"
	case "quanta":
		return "Quanta"
	case "ingrasys":
		return "Ingrasys"
	case "", "kvm":
		return "SONiC"
	}
	return tok
}

// --- ip -j addr / route / neigh ---

type ipAddr struct {
	Ifname   string   `json:"ifname"`
	Flags    []string `json:"flags"`
	Address  string   `json:"address"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
		Scope     string `json:"scope"`
		Dynamic   bool   `json:"dynamic"`
	} `json:"addr_info"`
}

func (a ipAddr) hasFlag(f string) bool {
	for _, x := range a.Flags {
		if x == f {
			return true
		}
	}
	return false
}

type ipRoute struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
	Dev     string `json:"dev"`
}

type ipNeigh struct {
	Dst    string `json:"dst"`
	Dev    string `json:"dev"`
	LLAddr string `json:"lladdr"`
}

// --- /proc ---

type cpuTimes struct{ total, idle uint64 }

func parseCPUTimes(body string) cpuTimes {
	f := strings.Fields(body)
	if len(f) < 5 || f[0] != "cpu" {
		return cpuTimes{}
	}
	var t cpuTimes
	for i, x := range f[1:] {
		n, _ := strconv.ParseUint(x, 10, 64)
		t.total += n
		if i == 3 {
			t.idle = n
		}
	}
	return t
}

func parseLoadAvg(body string) []float64 {
	f := strings.Fields(body)
	if len(f) < 3 {
		return nil
	}
	out := make([]float64, 0, 3)
	for _, x := range f[:3] {
		v, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return nil
		}
		out = append(out, v)
	}
	return out
}

func secondsDuration(secs float64) time.Duration {
	return time.Duration(secs * float64(time.Second))
}

// meminfo parses /proc/meminfo into kB values.
func meminfo(body string) map[string]uint64 {
	m := map[string]uint64{}
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = atou(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
	}
	return m
}
