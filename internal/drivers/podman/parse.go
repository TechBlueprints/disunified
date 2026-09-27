package podman

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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

// keyValues parses "k=v" lines.
func keyValues(body string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return kv
}

// subsections splits a body on "## <name>" headers, in order.
func subsections(body string) (names []string, bodies map[string]string) {
	bodies = map[string]string{}
	cur := ""
	var b strings.Builder
	flush := func() {
		if cur != "" {
			bodies[cur] = b.String()
			names = append(names, cur)
		}
		b.Reset()
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			flush()
			cur = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	flush()
	return names, bodies
}

// decodeJSON decodes a section, tolerating an empty one (a missing tool).
func decodeJSON(name, body string, v any) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		return fmt.Errorf("podman: section %s: %w", name, err)
	}
	return nil
}

// --- ip -j -s -d link show ---

type ipLink struct {
	Ifindex   int      `json:"ifindex"`
	Ifname    string   `json:"ifname"`
	Flags     []string `json:"flags"`
	MTU       int      `json:"mtu"`
	Master    string   `json:"master"`
	Operstate string   `json:"operstate"`
	Address   string   `json:"address"`
	Link      string   `json:"link"`
	Linkinfo  struct {
		InfoKind string `json:"info_kind"`
	} `json:"linkinfo"`
	Stats64 struct {
		Rx struct {
			Bytes     uint64 `json:"bytes"`
			Packets   uint64 `json:"packets"`
			Errors    uint64 `json:"errors"`
			Dropped   uint64 `json:"dropped"`
			Multicast uint64 `json:"multicast"`
		} `json:"rx"`
		Tx struct {
			Bytes   uint64 `json:"bytes"`
			Packets uint64 `json:"packets"`
			Errors  uint64 `json:"errors"`
			Dropped uint64 `json:"dropped"`
		} `json:"tx"`
	} `json:"stats64"`
}

func (l ipLink) hasFlag(f string) bool {
	for _, x := range l.Flags {
		if x == f {
			return true
		}
	}
	return false
}

// --- ip -j addr / route / neigh ---

type ipAddr struct {
	Ifname   string `json:"ifname"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
		Scope     string `json:"scope"`
		Dynamic   bool   `json:"dynamic"`
	} `json:"addr_info"`
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

// --- bridge -j -s fdb show dynamic ---

type fdbEntry struct {
	MAC     string `json:"mac"`
	Ifname  string `json:"ifname"`
	Master  string `json:"master"`
	Updated int    `json:"updated"`
}

// --- podman (reduced by collect.sh) ---

type network struct {
	Name      string   `json:"name"`
	ID        string   `json:"id"`
	Driver    string   `json:"driver"` // bridge, macvlan, ipvlan
	Interface string   `json:"interface"`
	Subnets   []string `json:"subnets"`
	Internal  bool     `json:"internal"`
}

type container struct {
	ID       string   `json:"id"`
	Names    []string `json:"names"`
	State    string   `json:"state"` // running, exited, created, ...
	Status   string   `json:"status"`
	Created  int64    `json:"created"` // epoch seconds
	Image    string   `json:"image"`
	Networks []string `json:"networks"`
	Exited   bool     `json:"exited"`
	Pod      string   `json:"pod"`
}

func (c container) Name() string {
	if len(c.Names) > 0 {
		return c.Names[0]
	}
	return c.ID
}

type endpoint struct {
	MAC       string   `json:"mac"`
	IP        string   `json:"ip"`
	Prefix    int      `json:"prefix"`
	Gateway   string   `json:"gateway"`
	NetworkID string   `json:"network_id"`
	Aliases   []string `json:"aliases"`
	Interface string   `json:"interface"`
}

type inspected struct {
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Sandbox  string              `json:"sandbox"`
	Networks map[string]endpoint `json:"networks"`
	Started  string              `json:"started"`
	Running  bool                `json:"running"`
	Hostname string              `json:"hostname"`
	Image    string              `json:"image"`
}

// --- phys / ethtool ---

type ethtoolInfo struct {
	Speed   int // Mb/s, 0 unknown
	Duplex  string
	Autoneg bool
	Speeds  []int
}

// parseEthtool reads the few lines used: Speed, Duplex, Auto-negotiation
// and the supported link modes (for speed capabilities).
func parseEthtool(body string) ethtoolInfo {
	var et ethtoolInfo
	seen := map[int]bool{}
	inModes := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Supported link modes:"):
			inModes = true
			t = strings.TrimSpace(strings.TrimPrefix(t, "Supported link modes:"))
		case strings.HasPrefix(t, "Supported pause"), strings.HasPrefix(t, "Supports auto"), strings.HasPrefix(t, "Advertised"):
			inModes = false
		case strings.HasPrefix(t, "Speed:"):
			n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(t, "Speed:")), "Mb/s"))
			if n > 0 {
				et.Speed = n
			}
		case strings.HasPrefix(t, "Duplex:"):
			et.Duplex = strings.TrimSpace(strings.TrimPrefix(t, "Duplex:"))
		case strings.HasPrefix(t, "Auto-negotiation:"):
			et.Autoneg = strings.Contains(t, "on")
		}
		if inModes {
			for _, m := range strings.Fields(t) {
				if s := linkModeSpeed(m); s > 0 && !seen[s] {
					seen[s] = true
					et.Speeds = append(et.Speeds, s)
				}
			}
		}
	}
	return et
}

// linkModeSpeed turns "10000baseT/Full" into 10000.
func linkModeSpeed(mode string) int {
	i := strings.Index(mode, "base")
	if i <= 0 {
		return 0
	}
	n, _ := strconv.Atoi(mode[:i])
	return n
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

// kb parses a /proc/meminfo value ("16305200 kB").
func kb(s string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "kB")), 10, 64)
	return n
}

func atoiDefault(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
