package proxmox

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/TechBlueprints/switch-to-unifi/internal/switchmodel"
)

// sections splits the collector script's output ("@@@ name" lines) into
// name -> body. A section that is missing (the tool is not installed) is
// simply absent; a missing mandatory section is the caller's error.
func sections(out string) map[string]string {
	m := map[string]string{}
	var name string
	var body strings.Builder
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	flush := func() {
		if name != "" {
			m[name] = strings.TrimRight(body.String(), "\n")
		}
		body.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "@@@ ") {
			flush()
			name = strings.TrimSpace(line[4:])
			continue
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	flush()
	return m
}

// keyValues parses "k=v" lines.
func keyValues(body string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// subsections splits "## name" delimited text into name -> body, in order.
func subsections(body string) (names []string, bodies map[string]string) {
	bodies = map[string]string{}
	var cur string
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
			cur = strings.TrimSpace(line[3:])
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	flush()
	return names, bodies
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
	Link      string   `json:"link"` // a VLAN/macvlan device's parent
	Linkinfo  struct {
		InfoKind      string `json:"info_kind"`
		InfoSlaveKind string `json:"info_slave_kind"`
		InfoSlaveData struct {
			State    string `json:"state"`
			Isolated bool   `json:"isolated"`
			Guard    bool   `json:"guard"`
			Cost     int    `json:"cost"`
		} `json:"info_slave_data"`
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

// --- bridge -j -compressvlans vlan show ---

type brVLANs struct {
	Ifname string `json:"ifname"`
	VLANs  []struct {
		VLAN    int      `json:"vlan"`
		VLANEnd int      `json:"vlanEnd"`
		Flags   []string `json:"flags"`
	} `json:"vlans"`
}

// --- bridge -j -s fdb show br X dynamic ---

type fdbEntry struct {
	MAC     string   `json:"mac"`
	Ifname  string   `json:"ifname"`
	VLAN    int      `json:"vlan"`
	Used    int      `json:"used"`
	Updated int      `json:"updated"`
	Flags   []string `json:"flags"`
	Master  string   `json:"master"`
	State   string   `json:"state"`
}

// --- /etc/pve/.vmlist ---

type vmList struct {
	IDs map[string]struct {
		Node string `json:"node"`
		Type string `json:"type"`
	} `json:"ids"`
}

// --- lldpcli -f json0 show neighbors details ---

type lldpJSON0 struct {
	LLDP []struct {
		Interface []struct {
			Name    string `json:"name"`
			Chassis []struct {
				ID []struct {
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"id"`
				Name       []struct{ Value string } `json:"name"`
				Descr      []struct{ Value string } `json:"descr"`
				MgmtIP     []struct{ Value string } `json:"mgmt-ip"`
				Capability []struct {
					Type    string `json:"type"`
					Enabled bool   `json:"enabled"`
				} `json:"capability"`
			} `json:"chassis"`
			Port []struct {
				ID []struct {
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"id"`
				Descr []struct{ Value string } `json:"descr"`
			} `json:"port"`
		} `json:"interface"`
	} `json:"lldp"`
}

func decodeJSON(name, body string, v any) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil // optional/empty section
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		return fmt.Errorf("proxmox: parsing %s: %w", name, err)
	}
	return nil
}

// --- guest NIC config lines ---

// guestNIC is one netN line of a QEMU VM or LXC container config, cluster-wide.
type guestNIC struct {
	Kind   string // "qemu" | "lxc"
	VMID   int
	Node   string // node the guest lives on (from the config path)
	Name   string // VM name / CT hostname
	Index  int    // netN
	Model  string // virtio, e1000, rtl8139, vmxnet3, e1000e; "veth" for lxc
	MAC    string // lower-case
	Bridge string
	Tag    int   // 0 = none
	Trunks []int // sorted; nil = none
	// LinkDown, Firewall, and the rest are kept as the raw option list so a
	// rewrite preserves everything the bridge does not manage.
	LinkDown bool
	Firewall bool
	MTU      int
	Template bool
	Raw      string   // the option string as found in the config
	Tags     []string // the guest's tags, as listed in its config (tags.go)
}

// Key is the stable identity of a guest NIC across the cluster.
func (n guestNIC) Key() string { return fmt.Sprintf("%s/%d/net%d", n.Kind, n.VMID, n.Index) }

// HostIface is the kernel interface QEMU/LXC creates for this NIC on its node.
func (n guestNIC) HostIface() string {
	if n.Kind == "lxc" {
		return fmt.Sprintf("veth%di%d", n.VMID, n.Index)
	}
	return fmt.Sprintf("tap%di%d", n.VMID, n.Index)
}

// BridgeMember is the interface that sits in the bridge for this NIC: the
// tap/veth itself, or the fwpr side of the firewall bridge when firewall=1.
func (n guestNIC) BridgeMember() string {
	if n.Firewall {
		return fmt.Sprintf("fwpr%dp%d", n.VMID, n.Index)
	}
	return n.HostIface()
}

// parseGuestConfigs parses the qemu/lxc sections: "## /etc/pve/nodes/<node>/<kind>/<id>.conf"
// followed by the filtered top-level lines.
func parseGuestConfigs(body, kind string) ([]guestNIC, error) {
	names, bodies := subsections(body)
	var out []guestNIC
	for _, path := range names {
		parts := strings.Split(path, "/")
		if len(parts) < 3 {
			continue
		}
		node := parts[len(parts)-3]
		idStr := strings.TrimSuffix(parts[len(parts)-1], ".conf")
		vmid, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		name, template := "", false
		var tags []string
		var nics []guestNIC
		for _, line := range strings.Split(bodies[path], "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			switch {
			case k == "name" || k == "hostname":
				name = v
			case k == "template":
				template = v == "1"
			case k == "tags":
				tags = strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
			case strings.HasPrefix(k, "net"):
				idx, err := strconv.Atoi(strings.TrimPrefix(k, "net"))
				if err != nil {
					continue
				}
				n := parseNICOptions(v, kind)
				n.Kind, n.VMID, n.Node, n.Index = kind, vmid, node, idx
				nics = append(nics, n)
			}
		}
		for i := range nics {
			nics[i].Name, nics[i].Template, nics[i].Tags = name, template, tags
			out = append(out, nics[i])
		}
	}
	return out, nil
}

// parseNICOptions parses "virtio=AA:BB:..,bridge=vmbr0,tag=8,trunks=2;10,firewall=1,link_down=1".
func parseNICOptions(raw, kind string) guestNIC {
	n := guestNIC{Raw: raw}
	for _, opt := range strings.Split(raw, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(opt), "=")
		switch k {
		case "virtio", "e1000", "e1000e", "rtl8139", "vmxnet3", "ne2k_pci", "pcnet", "e1000-82540em", "e1000-82544gc", "e1000-82545em", "i82551", "i82557b", "i82559er":
			n.Model, n.MAC = k, strings.ToLower(v)
		case "hwaddr":
			n.MAC = strings.ToLower(v)
		case "bridge":
			n.Bridge = v
		case "tag":
			n.Tag, _ = strconv.Atoi(v)
		case "trunks":
			n.Trunks = parseVLANRanges(v)
		case "link_down":
			n.LinkDown = v == "1"
		case "firewall":
			n.Firewall = v == "1"
		case "mtu":
			n.MTU, _ = strconv.Atoi(v)
		}
	}
	if kind == "lxc" && n.Model == "" {
		n.Model = "veth"
	}
	return n
}

// parseVLANRanges parses Proxmox's "10;20-30" trunk syntax.
func parseVLANRanges(s string) []int {
	var out []int
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' }) {
		a, b, isRange := strings.Cut(part, "-")
		lo, err := strconv.Atoi(strings.TrimSpace(a))
		if err != nil {
			continue
		}
		hi := lo
		if isRange {
			if hi, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
				continue
			}
		}
		for v := lo; v <= hi && v-lo < 4096; v++ {
			out = append(out, v)
		}
	}
	return uniqueSorted(out)
}

// formatVLANRanges renders sorted VLAN IDs as Proxmox trunks ("10;20-30").
func formatVLANRanges(ids []int) string {
	ids = uniqueSorted(ids)
	var parts []string
	for i := 0; i < len(ids); {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", ids[i], ids[j]))
		} else {
			parts = append(parts, strconv.Itoa(ids[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ";")
}

// --- ethtool ---

type ethtoolInfo struct {
	Speeds  []int // supported link speeds, Mbps, ascending
	FEC     bool  // supports RS or BASER FEC
	Autoneg bool
	Speed   int // current, Mbps (0 = unknown)
	Duplex  string
	Port    string // "Twisted Pair", "Direct Attach Copper", "FIBRE", ...
	Link    bool
}

func parseEthtool(body string) ethtoolInfo {
	var e ethtoolInfo
	inModes := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Supported link modes:"):
			inModes = true
			t = strings.TrimSpace(strings.TrimPrefix(t, "Supported link modes:"))
		case strings.HasPrefix(t, "Supported pause"), strings.HasPrefix(t, "Supports auto"), strings.HasPrefix(t, "Advertised"):
			inModes = false
		}
		if inModes {
			for _, mode := range strings.Fields(t) {
				if mbps := linkModeSpeed(mode); mbps > 0 {
					e.Speeds = append(e.Speeds, mbps)
				}
			}
			continue
		}
		switch {
		case strings.HasPrefix(t, "Supported FEC modes:"):
			v := strings.TrimPrefix(t, "Supported FEC modes:")
			e.FEC = strings.Contains(v, "RS") || strings.Contains(v, "BASER")
		case strings.HasPrefix(t, "Auto-negotiation:"):
			e.Autoneg = strings.Contains(t, "on")
		case strings.HasPrefix(t, "Speed:"):
			v := strings.TrimSpace(strings.TrimPrefix(t, "Speed:"))
			if n, err := strconv.Atoi(strings.TrimSuffix(v, "Mb/s")); err == nil {
				e.Speed = n
			}
		case strings.HasPrefix(t, "Duplex:"):
			e.Duplex = strings.TrimSpace(strings.TrimPrefix(t, "Duplex:"))
		case strings.HasPrefix(t, "Port:"):
			e.Port = strings.TrimSpace(strings.TrimPrefix(t, "Port:"))
		case strings.HasPrefix(t, "Link detected:"):
			e.Link = strings.Contains(t, "yes")
		}
	}
	e.Speeds = uniqueSorted(e.Speeds)
	return e
}

// linkModeSpeed turns "100000baseCR4/Full" into 100000.
func linkModeSpeed(mode string) int {
	i := strings.Index(mode, "base")
	if i <= 0 {
		return 0
	}
	n, err := strconv.Atoi(mode[:i])
	if err != nil {
		return 0
	}
	return n
}

// ethtoolModule is `ethtool -m`: the transceiver EEPROM and DOM, if any.
type ethtoolModule struct {
	Present    bool
	Identifier string // "QSFP28", "SFP", ...
	Vendor     string
	Part       string
	Serial     string
	Type       string // "Transceiver type" line
	TempC      float64
	VoltageV   float64
	HasDOM     bool
}

func parseEthtoolModule(body string) ethtoolModule {
	var m ethtoolModule
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "Identifier":
			m.Present = true
			if i := strings.Index(v, "("); i >= 0 {
				m.Identifier = strings.Trim(v[i:], "()")
			}
		case "Vendor name":
			m.Vendor = v
		case "Vendor PN":
			m.Part = v
		case "Vendor SN":
			m.Serial = v
		case "Transceiver type":
			m.Type = v
		case "Module temperature":
			if f, err := strconv.ParseFloat(strings.Fields(v)[0], 64); err == nil && f != 0 {
				m.TempC, m.HasDOM = f, true
			}
		case "Module voltage":
			if f, err := strconv.ParseFloat(strings.Fields(v)[0], 64); err == nil && f != 0 {
				m.VoltageV, m.HasDOM = f, true
			}
		}
	}
	return m
}

// --- /proc/net/bonding/X ---

type bond struct {
	Name        string
	Mode        string
	ActiveSlave string
	Primary     string
	Slaves      []bondSlave
}

type bondSlave struct {
	Name     string
	Up       bool
	SpeedMb  int
	PermAddr string
	Failures uint64
}

func parseBonding(body string) []bond {
	names, bodies := subsections(body)
	var out []bond
	for _, n := range names {
		b := bond{Name: n}
		var cur *bondSlave
		for _, line := range strings.Split(bodies[n], "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			switch k {
			case "Bonding Mode":
				b.Mode = v
			case "Currently Active Slave":
				b.ActiveSlave = v
			case "Primary Slave":
				b.Primary = strings.Fields(v + " ")[0]
			case "Slave Interface":
				b.Slaves = append(b.Slaves, bondSlave{Name: v})
				cur = &b.Slaves[len(b.Slaves)-1]
			case "MII Status":
				if cur != nil {
					cur.Up = v == "up"
				}
			case "Speed":
				if cur != nil {
					cur.SpeedMb, _ = strconv.Atoi(strings.Fields(v + " ")[0])
				}
			case "Permanent HW addr":
				if cur != nil {
					cur.PermAddr = strings.ToLower(v)
				}
			case "Link Failure Count":
				if cur != nil {
					cur.Failures, _ = strconv.ParseUint(v, 10, 64)
				}
			}
		}
		out = append(out, b)
	}
	return out
}

// --- hwmon ---

type hwmonChip struct {
	Name   string
	Values map[string]string // temp1_input=54000 ...
}

func parseHwmon(body string) []hwmonChip {
	names, bodies := subsections(body)
	var out []hwmonChip
	for _, n := range names {
		out = append(out, hwmonChip{Name: n, Values: keyValues(bodies[n])})
	}
	return out
}

func (h hwmonChip) float(key string, div float64) (float64, bool) {
	v, ok := h.Values[key]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	return f / div, true
}

// --- /proc/stat ---

type cpuTimes struct{ total, idle uint64 }

func parseCPUTimes(body string) cpuTimes {
	f := strings.Fields(body)
	if len(f) < 5 || f[0] != "cpu" {
		return cpuTimes{}
	}
	var t cpuTimes
	for i, s := range f[1:] {
		n, _ := strconv.ParseUint(s, 10, 64)
		t.total += n
		if i == 3 || i == 4 { // idle, iowait
			t.idle += n
		}
	}
	return t
}

func uniqueSorted(ids []int) []int {
	seen := map[int]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// --- ip -j addr show dev X ---

type ipAddr struct {
	Ifname   string `json:"ifname"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
		Scope     string `json:"scope"`
	} `json:"addr_info"`
}

// --- ip -j neigh show dev X ---

type ipNeigh struct {
	Dst    string   `json:"dst"`
	LLAddr string   `json:"lladdr"`
	State  []string `json:"state"`
}

func parseLoadAvg(body string) []float64 {
	f := strings.Fields(body)
	if len(f) < 3 {
		return nil
	}
	out := make([]float64, 0, 3)
	for _, v := range f[:3] {
		x, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil
		}
		out = append(out, x)
	}
	return out
}

// parseEthtoolFEC reads `ethtool --show-fec`: the active encoding.
func parseEthtoolFEC(body string) switchmodel.FEC {
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "Active FEC encoding" {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(v)) {
		case "RS":
			return switchmodel.FECRS
		case "BASER":
			return switchmodel.FECFC
		case "OFF", "NONE":
			return switchmodel.FECDisabled
		}
	}
	return switchmodel.FECUnknown
}

// --- mstpctl -f json showbridge / showportdetail (mstpd 0.2.0) ---
//
// Captured from mstpd 0.2.0-2 on Proxmox VE 9.1 (docs/fixtures/proxmox-9.1.6/
// mstpctl-*.json). Every value is a string; bridge and port ids are
// "<prio nibble>.<sys-id ext>.<MAC>" ("F.000.02:00:00:00:00:01": priority
// 15 x 4096 = 61440) and "<prio>.<port number>".

type mstpBridge struct {
	Bridge              string `json:"bridge"`
	Enabled             string `json:"enabled"`
	BridgeID            string `json:"bridge-id"`
	DesignatedRoot      string `json:"designated-root"`
	RootPort            string `json:"root-port"`
	PathCost            string `json:"path-cost"`
	MaxAge              string `json:"max-age"`
	ForwardDelay        string `json:"forward-delay"`
	HelloTime           string `json:"hello-time"`
	ForceProtocol       string `json:"force-protocol-version"`
	TopologyChangeCount string `json:"topology-change-count"`
	TopologyChange      string `json:"topology-change"`
}

type mstpPort struct {
	Port             string `json:"port"`
	Enabled          string `json:"enabled"`
	Role             string `json:"role"`
	State            string `json:"state"`
	ExternalPortCost string `json:"external-port-cost"`
	AdminEdgePort    string `json:"admin-edge-port"`
	OperEdgePort     string `json:"oper-edge-port"`
	BPDUGuardPort    string `json:"bpdu-guard-port"`
	BPDUGuardError   string `json:"bpdu-guard-error"`
	NumTransitionFwd string `json:"num-transition-fwd"`
	NumTransitionBlk string `json:"num-transition-blk"`
	NumRxTCN         string `json:"num-rx-tcn"`
	Disputed         string `json:"disputed"`
	BAInconsistent   string `json:"ba-inconsistent"`
}

// mstpID splits "F.000.02:00:00:00:00:01" into the priority (61440) and the
// MAC (lower-case).
func mstpID(id string) (priority int, mac string) {
	parts := strings.Split(id, ".")
	if len(parts) != 3 {
		return 0, ""
	}
	n, err := strconv.ParseInt(parts[0], 16, 32)
	if err != nil {
		return 0, ""
	}
	return int(n) * 4096, strings.ToLower(parts[2])
}

// mstpPriority is the priority nibble mstpctl settreeprio takes (0-15).
func mstpPriority(priority int) int { return priority / 4096 }

func atoiDefault(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
