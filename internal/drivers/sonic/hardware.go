package sonic

import (
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The hardware and aggregate tables a UniFi switch reports and the DX010
// can back (docs/drivers/sonic.md §11.4): LACP state per aggregate member,
// fan modules, power supplies with their own chip's sensors, the FDB's
// capacity from CRM, and the management interface's IPv6 addresses. The
// reference for every shape is the inform of the ECS Aggregation, UniFi's
// own SONiC switch (docs/fixtures/controller-10.6.109/).

// lacpSync is the LACP state bit SYNCHRONIZATION (IEEE 802.1AX): the
// partner has the link in the right aggregate.
const lacpSync = 0x08

// lacpOf reads one STATE_DB LAG_MEMBER_TABLE entry, tlm_teamd's copy of
// the member in `teamdctl <PortChannel> state dump` (sonic-swss tlm_teamd,
// values_store.h). A member is bundled when teamd enables it: selected, its
// aggregator the selected one, and the partner in sync (libteam's
// lacp_port_should_be_enabled); a member without link is never selected.
// nil for an empty entry (no teamd state yet).
func lacpOf(f map[string]string) *devicemodel.LACP {
	if len(f) == 0 {
		return nil
	}
	l := &devicemodel.LACP{
		Active: isTrue(f["link.up"]) && isTrue(f["runner.selected"]) && isTrue(f["runner.aggregator.selected"]) &&
			atoi(f["runner.partner_lacpdu_info.state"])&lacpSync != 0,
	}
	// teamd reports 00:00:00:00:00:00 until a partner's LACPDU arrives.
	if hw, err := net.ParseMAC(f["runner.partner_lacpdu_info.system"]); err == nil && hw.String() != "00:00:00:00:00:00" {
		l.PartnerSystem = hw.String()
	}
	return l
}

// setLACP gives each aggregate member its LACP state. A cage's aggregate
// is its first interface's (the one buildPorts takes the LAG from).
func setLACP(ports []cagePort, members table) {
	byMember := map[string]map[string]string{}
	for k, f := range members {
		parts := strings.Split(k, "|") // LAG_MEMBER_TABLE|PortChannel1|Ethernet0
		if len(parts) == 3 {
			byMember[parts[1]+"|"+parts[2]] = f
		}
	}
	for i := range ports {
		p := &ports[i].Port
		if p.LAG == "" || len(p.Interfaces) == 0 {
			continue
		}
		p.LACP = lacpOf(byMember[p.LAG+"|"+p.Interfaces[0]])
	}
}

// fanModules reads STATE_DB FAN_INFO as the fan modules a UniFi switch
// lists: one row per drawer ("Fan<N>" for the platform's Drawer<N>, as the
// ECS Aggregation names its Drawer N), with the mean of its rotors' speeds,
// the highest target, OK only while every rotor is present and healthy, and
// the airflow direction thermalctld reports. A fan in no drawer is a row of
// its own -- unless it is a supply's (named after a PSU_INFO entry), which
// belongs to the supply as on a UniFi switch.
func fanModules(fans map[string]map[string]string, psus map[string]map[string]string) []devicemodel.Fan {
	type drawer struct {
		name             string
		speed, n, target int
		ok, present      bool
		dir              string
	}
	byDrawer := map[string]*drawer{}
	var order []string
	for _, name := range sortedKeys(fans) {
		f := fans[name]
		d := f["drawer_name"]
		if !known(d) {
			if psuFan(name, psus) {
				continue
			}
			d = name
		}
		dr := byDrawer[d]
		if dr == nil {
			dr = &drawer{name: d, ok: true}
			byDrawer[d] = dr
			order = append(order, d)
		}
		present := isTrue(f["presence"])
		dr.present = dr.present || present
		dr.ok = dr.ok && present && isTrue(f["status"])
		if present {
			dr.speed += atoi(f["speed"])
			dr.n++
		}
		if t := atoi(f["speed_target"]); t > dr.target {
			dr.target = t
		}
		if dir := strings.ToLower(f["direction"]); dr.dir == "" && (dir == "intake" || dir == "exhaust") {
			dr.dir = dir
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return drawerNum(order[i]) < drawerNum(order[j]) })
	out := make([]devicemodel.Fan, 0, len(order))
	for _, d := range order {
		dr := byDrawer[d]
		f := devicemodel.Fan{Label: dr.name, TargetPct: dr.target, OK: dr.ok, Missing: !dr.present, Direction: dr.dir}
		if n := drawerNum(d); n < 1<<30 && strings.HasPrefix(strings.ToLower(d), "drawer") {
			f.Label = "Fan" + strconv.Itoa(n)
		}
		if dr.n > 0 {
			f.SpeedPct = (dr.speed + dr.n/2) / dr.n
		}
		out = append(out, f)
	}
	return out
}

// psuFan: thermalctld names a supply's fan after the supply ("PSU-1 FAN-1").
func psuFan(name string, psus map[string]map[string]string) bool {
	for p := range psus {
		if strings.HasPrefix(name, p+" ") {
			return true
		}
	}
	return false
}

// drawerNum is the number in "Drawer3" / "Drawer 3"; large for none.
func drawerNum(s string) int {
	s = strings.TrimSpace(s)
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if n, err := strconv.Atoi(s[i:]); err == nil {
		return n
	}
	return 1 << 30
}

// psuChips names each supply's PMBus chip by its hwmon device (i2c
// "<bus>-<addr>"), per platform: the platform's sonic_platform/psu.py
// (PSU_I2C_MAPPING) reads the same chip for PSU_INFO, but psud keeps only
// one temperature and the output power of it. The chip also has the
// supply's other sensors, its fan's RPM and the power drawn from the feed.
var psuChips = map[string]map[string]string{
	// sonic-buildimage device/celestica/x86_64-cel_seastone-r0/sonic_platform/psu.py
	"x86_64-cel_seastone-r0": {"PSU-1": "10-005a", "PSU-2": "11-005b"},
}

// powerSupplies reads STATE_DB PSU_INFO, then the supply's own chip where
// the platform names it.
func powerSupplies(psus map[string]map[string]string, platform string, hwmon table) []devicemodel.PSU {
	chips := map[string]map[string]string{}
	for _, f := range hwmon {
		if d := f["device"]; d != "" {
			chips[d] = f
		}
	}
	var out []devicemodel.PSU
	for _, name := range sortedKeys(psus) {
		f := psus[name]
		p := devicemodel.PSU{Slot: name, Model: f["model"], Present: isTrue(f["presence"]), OK: isTrue(f["status"]), HotSwap: isTrue(f["is_replaceable"])}
		if !known(p.Model) {
			p.Model = ""
		}
		if known(f["serial"]) {
			p.Serial = strings.TrimSpace(f["serial"])
		}
		p.OutputW, _ = atof(f["power"])
		p.CapacityW, _ = atof(f["max_power"])
		if t, ok := atof(f["temp"]); ok {
			p.TempsC = []float64{t}
		}
		if c := chips[psuChips[platform][name]]; c != nil && p.Present {
			chipReadings(&p, c)
		}
		out = append(out, p)
	}
	return out
}

// chipReadings takes a PMBus chip's hwmon readings (milli-units, power in
// micro-watts): every temperature in sensor order, every fan's RPM, and
// the input power (the "pin" label).
func chipReadings(p *devicemodel.PSU, c map[string]string) {
	nums := func(prefix string) []int {
		var n []int
		for k := range c {
			if rest, ok := strings.CutPrefix(k, prefix); ok {
				if i, err := strconv.Atoi(strings.TrimSuffix(rest, "_input")); err == nil && strings.HasSuffix(rest, "_input") {
					n = append(n, i)
				}
			}
		}
		sort.Ints(n)
		return n
	}
	var temps []float64
	for _, i := range nums("temp") {
		if v, ok := atof(c["temp"+strconv.Itoa(i)+"_input"]); ok {
			temps = append(temps, v/1000)
		}
	}
	if len(temps) > 0 {
		p.TempsC = temps
	}
	for _, i := range nums("fan") {
		if v, ok := atof(c["fan"+strconv.Itoa(i)+"_input"]); ok {
			p.FanRPM = append(p.FanRPM, int(v))
		}
	}
	for _, i := range nums("power") {
		k := "power" + strconv.Itoa(i)
		if c[k+"_label"] == "pin" {
			if v, ok := atof(c[k+"_input"]); ok {
				p.InputW = v / 1e6
			}
		}
	}
}

// fdbCapacity reads orchagent's resource counters (COUNTERS_DB CRM:STATS):
// the FDB's capacity is what is used plus what the ASIC still has free.
// 0, 0 when CRM has not polled (or the table is absent).
func fdbCapacity(crm table) (capacity, used int) {
	f := crm["CRM:STATS"]
	u, uok := f["crm_stats_fdb_entry_used"]
	a, aok := f["crm_stats_fdb_entry_available"]
	if !uok || !aok {
		return 0, 0
	}
	return atoi(u) + atoi(a), atoi(u)
}

// ipv6Of is the interface's IPv6 addresses as UniFi switches report them,
// "<address>/<prefix>", global ones first, then the link-local.
func ipv6Of(addrs []ipAddr, iface string) []string {
	var global, link []string
	for _, a := range addrs {
		if a.Ifname != iface {
			continue
		}
		for _, ai := range a.AddrInfo {
			if ai.Family != "inet6" {
				continue
			}
			s := ai.Local + "/" + strconv.Itoa(ai.PrefixLen)
			switch ai.Scope {
			case "global":
				global = append(global, s)
			case "link":
				link = append(link, s)
			}
		}
	}
	return append(global, link...)
}
