package sonic

import (
	"strconv"
	"strings"
)

// The optic's own fault flags (docs/drivers/sonic.md §10.10): receive loss
// of signal and transmit fault, as the module reports them. A UniFi switch
// sends them as sfp_rxfault / sfp_txfault, and the controller raises its
// SFP alert ("Loss of receive signal detected on SFP inserted into <device>
// Port N") from the first.
//
// xcvrd (sonic-platform-daemons 202505 and 202511, dom/dom_mgr.py) writes
// them every 60 s to STATE_DB TRANSCEIVER_STATUS_FLAG|<first interface of
// the cage>, one field per optical channel: rx<k>los and tx<k>fault, each
// "True", "False" or "N/A" (sonic-platform-common sff8636.py / sff8472.py
// get_transceiver_status_flags: "N/A" where the module does not implement
// the flag). On an SFF-8636 module (QSFP28) they are the latched indicators
// of page 0 bytes 3 and 4, read and so cleared on every poll: a fault that
// lasts shows on every poll, one that cleared within a minute shows once.
//
// Only the module's flags are read. The link being down says nothing (a
// down port with light has no fault on the ECS), and a reading against the
// module's alarm thresholds is not what the ECS reports either.

func init() { laterSections = append(laterSections, "xcvr_flag") }

// channelFlags is one module's per-channel flags, channel k at index k-1.
type channelFlags struct{ rxLOS, txFault []bool }

// moduleFlags parses one TRANSCEIVER_STATUS_FLAG entry. Only "True" is a
// fault; "False", "N/A" and a missing channel are none.
func moduleFlags(f map[string]string) channelFlags {
	var m channelFlags
	set := func(list *[]bool, k int, v string) {
		if k < 1 || k > 16 {
			return
		}
		for len(*list) < k {
			*list = append(*list, false)
		}
		(*list)[k-1] = v == "True"
	}
	for field, v := range f {
		if n, ok := channelOf(field, "rx", "los"); ok {
			set(&m.rxLOS, n, v)
		} else if n, ok := channelOf(field, "tx", "fault"); ok {
			set(&m.txFault, n, v)
		}
	}
	return m
}

// channelOf reads the channel number of a field "<pre><k><post>" (rx3los).
func channelOf(field, pre, post string) (int, bool) {
	s, ok := strings.CutPrefix(field, pre)
	if !ok {
		return 0, false
	}
	s, ok = strings.CutSuffix(s, post)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0
}

// anySet reports whether a channel in [from, to) (0-based) is set.
func anySet(l []bool, from, to int) bool {
	for i := from; i < to && i < len(l); i++ {
		if l[i] {
			return true
		}
	}
	return false
}

// opticFaults fills each cage's OpticRxAlarm / OpticTxAlarm from its
// module's flags: any channel's. A split cage's lanes get the channels they
// run on, in order -- of a four-channel module, a 4x25G lane one each, a
// 2x50G lane two -- by the host-lane order of a breakout (SFF-8636 channel k
// on the module's electrical lane k; not seen on a split module yet). Only
// a cage with a module seated (TRANSCEIVER_INFO) reads them: xcvrd removes
// the entry with the module, but an empty cage must never carry a fault.
func opticFaults(out []cagePort, db map[string]table) {
	flags := db["xcvr_flag"].byName("|")
	for i := range out {
		c := &out[i]
		if c.Optic == nil || len(c.Interfaces) == 0 {
			continue
		}
		f, ok := flags[c.Interfaces[0]]
		if !ok {
			continue
		}
		m := moduleFlags(f)
		c.Health.OpticRxAlarm = anySet(m.rxLOS, 0, len(m.rxLOS))
		c.Health.OpticTxAlarm = anySet(m.txFault, 0, len(m.txFault))
		n := len(c.lanes)
		if n < 2 {
			continue
		}
		for j := range c.lanes {
			l := &c.lanes[j].Health
			l.OpticRxAlarm = laneSet(m.rxLOS, j, n)
			l.OpticTxAlarm = laneSet(m.txFault, j, n)
		}
	}
}

// laneSet reports whether a channel lane j of n runs on is set: the
// channels split evenly in order, or all of them when the module has fewer
// channels than the cage has lanes.
func laneSet(l []bool, j, n int) bool {
	if len(l) < n {
		return anySet(l, 0, len(l))
	}
	return anySet(l, j*len(l)/n, (j+1)*len(l)/n)
}
