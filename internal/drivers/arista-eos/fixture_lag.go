package aristaeos

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// Aggregates in FixtureTransport: `channel-group N mode active`, `no
// channel-group`, `interface Port-Channel<n>` and `no interface
// Port-Channel<n>`, in the forms the switch printed after each
// (docs/fixtures/arista-eos-4.26.14M/channel-group-po9/README.md):
//
//   - channel-group-po9/: one copper member in and out, then a Port-Channel
//     removed with its member still in it;
//   - empty-po-none/ -> empty-po-lag5/ -> empty-po-lag5-removed-fixed/:
//     the driver's build and removal batches on Ethernet2 and the empty
//     cage's Ethernet54/1, every read the collector makes;
//   - split-cage54/ -> split-cage54-flexxg/: three lanes of a split cage
//     into Port-Channel1541;
//   - empty-po-leftovers/: a Port-Channel no member ever joined (Po4: what
//     `interface Port-Channel<n>` re-creates, docs/drivers/arista-eos.md §4).
//
// Every member in those captures has no link, so only a port without one
// is modelled: with a partner LACP would bundle it (`lagMember`), which no
// capture shows. Joining a Port-Channel whose VLANs differ from the
// member's, a new Port-Channel from a member that is not a plain trunk, a
// member moved from one group straight to another and any mode but
// `active` are recorded in Unmodelled and change nothing.

var poNameRe = regexp.MustCompile(`^Port-Channel(\d+)$`)

// The Port-Channel's own entries, as captured. physicalAddress is set from
// the first member (empty-po-lag5, split-cage54-flexxg, channel-group-po9:
// the member's) or left all zeros for one created without a member (a
// Port-Channel with none, channel-group-po9/2-no-channel-group).
const (
	poInterfaceJSON = `{"bandwidth": 0, "description": "", "fallbackEnabled": false, "fallbackEnabledType": "fallbackNone",
 "forwardingModel": "bridged", "hardware": "portChannel", "interfaceAddress": [],
 "interfaceCounters": {"counterRefreshTime": 1794765267.053552, "inBroadcastPkts": 0, "inDiscards": 0, "inMulticastPkts": 0,
  "inOctets": 0, "inTotalPkts": 0, "inUcastPkts": 0, "linkStatusChanges": 1, "outBroadcastPkts": 0, "outDiscards": 0,
  "outMulticastPkts": 0, "outOctets": 0, "outUcastPkts": 0, "totalInErrors": 0, "totalOutErrors": 0},
 "interfaceStatistics": {"inBitsRate": 0.0, "inPktsRate": 0.0, "outBitsRate": 0.0, "outPktsRate": 0.0, "updateInterval": 300.0},
 "interfaceStatus": "notconnect", "l2Mru": 0, "l3MtuConfigured": false, "lastStatusChangeTimestamp": 1794765266.3038557,
 "lineProtocolStatus": "lowerLayerDown", "memberInterfaces": {}, "mtu": 9214, "name": "", "physicalAddress": "00:00:00:00:00:00"}`
	poStatusJSON = `{"autoNegotiateActive": false, "autoNegotigateActive": false, "bandwidth": 0, "description": "", "duplex": "duplexFull",
 "interfaceType": "N/A", "lineProtocolStatus": "lowerLayerDown", "linkStatus": "notconnect",
 "vlanInformation": {"interfaceForwardingModel": "bridged", "interfaceMode": "trunk"}}`
	// Po4 of empty-po-leftovers: a Port-Channel with nothing configured.
	poBareStatusVLAN  = `{"interfaceForwardingModel": "bridged", "interfaceMode": "bridged", "vlanId": 1}`
	poBareSwitchports = `{"enabled": true, "switchportInfo": {"accessVlanId": 1, "accessVlanName": "default", "dot1qVlanTagDisallowed": false,
 "dot1qVlanTagRequired": false, "dot1qVlanTagRequiredStatus": false, "dynamicAllowedVlans": {}, "dynamicBlockedVlans": {},
 "dynamicTrunkGroups": [], "macLearning": true, "mode": "access", "phoneTrunk": false, "phoneTrunkUntagged": false, "phoneVlan": 0,
 "sourceportFilterMode": "enabled", "staticTrunkGroups": [], "tpid": "0x8100", "tpidStatus": true, "trunkAllowedVlans": "ALL",
 "trunkingNativeVlanId": 1, "trunkingNativeVlanName": "default", "vlanForwardingMode": "allowedVlansOnly"}}`
	poSummaryJSON = `{"inactive": false, "lacpMode": "active", "linkState": "down", "ports": {}, "protocol": "lacp"}`
	// A member without a link: a copper port's entry (Ethernet2 in
	// empty-po-lag5) and a cage lane's, which carries lacpState
	// (Ethernet54/1 there, Ethernet54/2-4 in split-cage54-flexxg).
	poMemberJSON   = `{"exceedsMaxWeight": false, "intf": "", "lacpMisconfig": {"status": "noAgg"}, "lagMember": false, "linkDown": true, "staticLag": false, "suspended": false}`
	poLaneLACPJSON = `{"activity": true, "aggregation": true, "collecting": false, "defaulted": true, "distributing": false, "expired": false, "synchronization": false, "timeout": false}`
)

func decodeTemplate(s string) map[string]any {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		panic("fixture template: " + err.Error())
	}
	return v
}

// ifEntry is an interface's entry in a reply's top-level map.
func (f *FixtureTransport) ifEntry(cmd, top, name string) map[string]any {
	return child(child(f.doc(cmd).vOrNil(), top, false), name, false)
}

// memberOf is the group an interface's running config puts it in, 0 for none.
func (f *FixtureTransport) memberOf(iface string) int {
	for l := range f.rcSection("interface "+iface, false) {
		if rest, ok := strings.CutPrefix(l, "channel-group "); ok {
			n, _ := strconv.Atoi(strings.Fields(rest)[0])
			return n
		}
	}
	return 0
}

// portChannelExists: the running config has the interface.
func (f *FixtureTransport) portChannelExists(name string) bool {
	top := f.rcTop()
	_, ok := top["interface "+name]
	return ok
}

// createPortChannel adds a Port-Channel to every reply that lists one:
// from a member (its switchport entry and mode, its address) or bare.
func (f *FixtureTransport) createPortChannel(name string, from string) {
	top := f.rcTop()
	if top == nil {
		return
	}
	section := map[string]any{"cmds": map[string]any{}, "comments": []any{}}
	ifc := decodeTemplate(poInterfaceJSON)
	ifc["name"] = name
	st := decodeTemplate(poStatusJSON)
	sp := decodeTemplate(poBareSwitchports)
	if from != "" {
		child(section, "cmds", false)["switchport mode trunk"] = nil
		if m := f.ifEntry("show interfaces", "interfaces", from); m != nil && m["physicalAddress"] != nil {
			ifc["physicalAddress"] = m["physicalAddress"]
		}
		sp = cloneValue(f.ifEntry("show interfaces switchport", "switchports", from))
	} else {
		st["vlanInformation"] = decodeTemplate(poBareStatusVLAN)
	}
	top["interface "+name] = section
	if m := child(f.doc("show interfaces").vOrNil(), "interfaces", false); m != nil {
		m[name] = ifc
	}
	if m := child(f.doc("show interfaces status").vOrNil(), "interfaceStatuses", false); m != nil {
		m[name] = st
	}
	if m := child(f.doc("show interfaces switchport").vOrNil(), "switchports", false); m != nil && sp != nil {
		m[name] = sp
	}
}

func cloneValue(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return cloneDoc(&fixtureDoc{v: m}).v
}

// plainTrunk: the member's switchport is the default trunk, the only form a
// capture shows a new Port-Channel taking from its member.
func plainTrunk(info map[string]any) bool {
	return info["mode"] == "trunk" && info["trunkAllowedVlans"] == "ALL" && jsonInt(info["trunkingNativeVlanId"]) == 1
}

func jsonInt(v any) int {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case float64:
		return int(n)
	case int:
		return n
	}
	return -1
}

// linkless: the port has no link (notconnect, or shut).
func (f *FixtureTransport) linkless(iface string) bool {
	e := f.ifEntry("show interfaces", "interfaces", iface)
	return e != nil && (e["interfaceStatus"] == "notconnect" || e["interfaceStatus"] == "disabled")
}

// joinChannel is `channel-group N mode active` on iface; false when not
// modelled.
func (f *FixtureTransport) joinChannel(iface, c string) bool {
	f2 := strings.Fields(c)
	if len(f2) != 4 || f2[2] != "mode" || f2[3] != "active" {
		return false
	}
	n, err := strconv.Atoi(f2[1])
	if err != nil {
		return false
	}
	switch m := f.memberOf(iface); {
	case m == n:
		return true
	case m != 0:
		return false // straight from one group to another: not captured
	}
	if !f.linkless(iface) {
		return false
	}
	info := child(f.ifEntry("show interfaces switchport", "switchports", iface), "switchportInfo", false)
	if info == nil {
		return false
	}
	name := "Port-Channel" + strconv.Itoa(n)
	if f.portChannelExists(name) {
		po := child(f.ifEntry("show interfaces switchport", "switchports", name), "switchportInfo", false)
		if !reflect.DeepEqual(po, info) {
			return false // what a member takes from a Port-Channel unlike it is not captured
		}
	} else {
		if !plainTrunk(info) {
			return false
		}
		f.createPortChannel(name, iface)
	}
	sum := child(f.doc("show port-channel summary").vOrNil(), "portChannels", true)
	if sum == nil {
		return false
	}
	pe := child(sum, name, false)
	if pe == nil {
		pe = decodeTemplate(poSummaryJSON)
		sum[name] = pe
	}
	me := decodeTemplate(poMemberJSON)
	me["intf"] = iface
	if strings.Contains(iface, "/") {
		me["lacpState"] = decodeTemplate(poLaneLACPJSON)
	}
	child(pe, "ports", true)[iface] = me
	f.rcSection("interface "+iface, true)[c] = nil
	if e := f.ifEntry("show interfaces", "interfaces", iface); e != nil {
		e["forwardingModel"], e["interfaceMembership"] = "dataLink", "Member of "+name
	}
	if e := f.ifEntry("show interfaces status", "interfaceStatuses", iface); e != nil {
		e["vlanInformation"] = map[string]any{"interfaceForwardingModel": "dataLink", "vlanExplanation": "in Po" + strconv.Itoa(n)}
	}
	return true
}

// leaveChannel is `no channel-group` on iface (also what `no interface
// Port-Channel<n>` does to each member); false when not modelled. The
// member reads as before it joined: bridged, and in the status table its
// own mode (trunk, or an access port's `bridged` with its VLAN, as
// Ethernet50/3 of the poll capture).
func (f *FixtureTransport) leaveChannel(iface string) bool {
	n := f.memberOf(iface)
	if n == 0 {
		return true
	}
	if !f.canLeave(iface) {
		return false
	}
	info := child(f.ifEntry("show interfaces switchport", "switchports", iface), "switchportInfo", false)
	vi := map[string]any{"interfaceForwardingModel": "bridged", "interfaceMode": "trunk"}
	if info["mode"] == "access" {
		vi = map[string]any{"interfaceForwardingModel": "bridged", "interfaceMode": "bridged", "vlanId": info["accessVlanId"]}
	}
	s := f.rcSection("interface "+iface, false)
	for l := range s {
		if strings.HasPrefix(l, "channel-group ") {
			delete(s, l)
		}
	}
	name := "Port-Channel" + strconv.Itoa(n)
	if sum := child(f.doc("show port-channel summary").vOrNil(), "portChannels", false); sum != nil {
		if pe := child(sum, name, false); pe != nil {
			ports := child(pe, "ports", false)
			delete(ports, iface)
			if len(ports) == 0 {
				delete(sum, name) // channel-group-po9/2-no-channel-group
			}
		}
	}
	if e := f.ifEntry("show interfaces", "interfaces", iface); e != nil {
		e["forwardingModel"] = "bridged"
		delete(e, "interfaceMembership")
	}
	if e := f.ifEntry("show interfaces status", "interfaceStatuses", iface); e != nil {
		e["vlanInformation"] = vi
	}
	return true
}

// members is every interface the running config puts in group n.
func (f *FixtureTransport) members(n int) []string {
	var out []string
	for section := range f.rcTop() {
		if iface, ok := strings.CutPrefix(section, "interface "); ok && f.memberOf(iface) == n {
			out = append(out, iface)
		}
	}
	return out
}

// canLeave: leaveChannel would model iface's leaving.
func (f *FixtureTransport) canLeave(iface string) bool {
	info := child(f.ifEntry("show interfaces switchport", "switchports", iface), "switchportInfo", false)
	return info != nil && (info["mode"] == "trunk" || info["mode"] == "access")
}
