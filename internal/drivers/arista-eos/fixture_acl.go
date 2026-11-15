package aristaeos

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// The ACL read-back in FixtureTransport: a list the transport writes is
// served by `show ip access-lists <name>` and listed in `show ip
// access-lists summary` as the switch printed its own lists after the
// driver's session wrote them (session-dui-acl-test-ports.txt ->
// show-ip-access-lists-DUI_IP_ACL_Ethernet2.json, -Ethernet54-1.json and
// show-ip-access-lists-summary-dui-acl.json; mDNS-block in
// show-ip-access-lists.json for a list without `counters per-entry`):
//
//   - an entry is its sequence number and its text as the running config
//     shows it (the two read the same in every capture); a remark carries
//     only those and convertSymbols, a rule its action and an empty
//     counterData, which is what a written entry reads until a packet
//     matches it. Its ruleFilter, EOS's parse of the match, is left out:
//     the driver never reads it, and deriving it from the text is not
//     done. An entry nothing wrote keeps the reply it had (a capture's
//     counts);
//   - countersEnabled follows `counters per-entry`;
//   - the summary lists the list with the interfaces the running config
//     binds it to inbound, configured and active alike (each commit the
//     captures show programmed; one that does not is a capture of its own,
//     acl-commit-warning/), and its entry count.
//
// A list no capture and no write knows has no reply, as before.

const (
	aclListJSON    = `{"chipName": "", "countersEnabled": false, "countersIncomplete": false, "dynamic": false, "implicitRule": [], "name": "", "readonly": false, "sequence": [], "staleCounters": false, "standard": false, "type": "Ip4Acl"}`
	aclRuleJSON    = `{"action": "", "convertSymbols": true, "copyToCpuDst": "none", "counterData": {}, "log": false, "mirrorSession": "", "payload": {"headerStart": false, "payload": []}, "sequenceNumber": 0, "text": ""}`
	aclSummaryJSON = `{"activeEgressIntfs": [], "activeIngressIntfs": [], "configuredEgressIntfs": [], "configuredIngressIntfs": [], "countersEnabled": false, "countersIncomplete": false, "dynamic": false, "name": "", "notFullySupportedByHardware": false, "readonly": false, "sequenceLength": 0, "standard": false, "type": "Ip4Acl"}`
)

const aclShowPrefix = "show ip access-lists "

// aclList is the reply entry of a list for writing: the capture's, or a
// new one. fresh replaces whatever was there (a list removed and written
// again starts empty).
func (f *FixtureTransport) aclList(name string, fresh bool) map[string]any {
	cmd := aclShowPrefix + name
	d := f.docs[cmd]
	if d == nil || d.path != f.path(cmd) {
		d = f.doc(cmd)
	}
	if d == nil || fresh {
		l := decodeTemplate(aclListJSON)
		l["name"] = name
		d = &fixtureDoc{path: f.path(cmd), v: map[string]any{"aclList": []any{l}}}
		if f.docs == nil {
			f.docs = map[string]*fixtureDoc{}
		}
		f.docs[cmd] = d
	}
	for _, e := range sliceOf(d.v["aclList"]) {
		if l := obj(e); l != nil && l["name"] == name {
			return l
		}
	}
	return nil
}

func sliceOf(v any) []any { s, _ := v.([]any); return s }

// aclSetEntry writes entry seq (text as the running config shows it).
func (f *FixtureTransport) aclSetEntry(name string, seq int, text string) {
	l := f.aclList(name, false)
	if l == nil {
		return
	}
	var e map[string]any
	if strings.HasPrefix(text, "remark ") {
		e = map[string]any{"convertSymbols": true}
	} else {
		e = decodeTemplate(aclRuleJSON)
		e["action"] = strings.Fields(text)[0]
	}
	e["sequenceNumber"], e["text"] = jsonNum(seq), text
	f.aclDropEntry(name, seq)
	seqs := append(sliceOf(l["sequence"]), e)
	sort.SliceStable(seqs, func(i, j int) bool {
		return jsonInt(obj(seqs[i])["sequenceNumber"]) < jsonInt(obj(seqs[j])["sequenceNumber"])
	})
	l["sequence"] = seqs
}

func (f *FixtureTransport) aclDropEntry(name string, seq int) {
	l := f.aclList(name, false)
	if l == nil {
		return
	}
	var keep []any
	for _, e := range sliceOf(l["sequence"]) {
		if jsonInt(obj(e)["sequenceNumber"]) != seq {
			keep = append(keep, e)
		}
	}
	if keep == nil {
		keep = []any{}
	}
	l["sequence"] = keep
}

// aclListsBoundIn is the lists a running-config section binds.
func aclListsBoundIn(section map[string]any) []string {
	var out []string
	for l := range section {
		if n, ok := strings.CutPrefix(l, "ip access-group "); ok && strings.HasSuffix(n, " in") {
			out = append(out, strings.TrimSuffix(n, " in"))
		}
	}
	return out
}

// syncACLSummary lists every list the transport has written to in the
// summary as the running config now has it (f.mu held).
func (f *FixtureTransport) syncACLSummary() {
	var touched []string
	for cmd, d := range f.docs {
		if n, ok := strings.CutPrefix(cmd, aclShowPrefix); ok && n != "summary" && d.path == f.path(cmd) {
			touched = append(touched, n)
		}
	}
	if len(touched) == 0 {
		return
	}
	sd := f.doc(aclShowPrefix + "summary")
	top := f.rcTop()
	if sd == nil || top == nil {
		return
	}
	bound := map[string][]string{}
	for section, body := range top {
		if iface, ok := strings.CutPrefix(section, "interface "); ok {
			for _, n := range aclListsBoundIn(child(obj(body), "cmds", false)) {
				bound[n] = append(bound[n], iface)
			}
		}
	}
	entries := sliceOf(sd.v["aclList"])
	index := map[string]int{}
	for i, e := range entries {
		if n, ok := obj(e)["name"].(string); ok {
			index[n] = i
		}
	}
	sort.Strings(touched)
	gone := map[int]bool{}
	for _, n := range touched {
		rc := f.rcSection("ip access-list "+n, false)
		i, listed := index[n]
		if rc == nil {
			if listed {
				gone[i] = true
			}
			continue
		}
		var e map[string]any
		if listed {
			e = obj(entries[i])
		} else {
			e = decodeTemplate(aclSummaryJSON)
			e["name"] = n
			entries = append(entries, e)
			index[n] = len(entries) - 1
		}
		_, counted := rc[aclCounters]
		e["countersEnabled"] = counted
		e["sequenceLength"] = jsonNum(len(sliceOf(f.aclList(n, false)["sequence"])))
		ifs := bound[n]
		sort.Strings(ifs)
		var named []any
		for _, iface := range ifs {
			named = append(named, map[string]any{"name": iface})
		}
		if named == nil {
			named = []any{}
		}
		e["configuredIngressIntfs"], e["activeIngressIntfs"] = named, cloneSlice(named)
	}
	var keep []any
	for i, e := range entries {
		if !gone[i] {
			keep = append(keep, e)
		}
	}
	sort.SliceStable(keep, func(i, j int) bool { return obj(keep[i])["name"].(string) < obj(keep[j])["name"].(string) })
	sd.v["aclList"] = keep
}

func cloneSlice(s []any) []any {
	out := make([]any, len(s))
	for i, v := range s {
		if m := obj(v); m != nil {
			c := map[string]any{}
			for k, x := range m {
				c[k] = x
			}
			out[i] = c
		} else {
			out[i] = v
		}
	}
	return out
}

func jsonNum(n int) any { return json.Number(strconv.Itoa(n)) }
