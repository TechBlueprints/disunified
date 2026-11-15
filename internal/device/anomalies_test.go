package device

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	aristaeos "github.com/TechBlueprints/disunified/internal/drivers/arista-eos"
)

// portAnomalies: `anomalies` carries only what the switch itself detected
// (today: a loop-protection hold, 512); satisfaction is unchanged.
func TestPortAnomalies(t *testing.T) {
	up := devicemodel.Port{Up: true, SpeedMbps: 10000, SpeedCaps: []int{1000, 10000}}
	if bits, sat, reason := portAnomalies(up, false, nil); bits != 0 || sat != 100 || reason != 0 {
		t.Errorf("healthy port = %d/%d/%d, want 0/100/0", bits, sat, reason)
	}

	// A loop-protection hold, in both spellings drivers report it.
	for _, f := range []string{"errdisabled: loop-protect (10 MAC moves in 5 s)", "errdisabled: loopprotect"} {
		held := devicemodel.Port{Up: false, Fault: f}
		if bits, sat, reason := portAnomalies(held, false, nil); bits != portAnomalyLoopProtection || sat != 100 || reason != 0 {
			t.Errorf("%q = %d/%d/%d, want 512/100/0", f, bits, sat, reason)
		}
	}

	// Every other fault a driver reports is not a condition in the
	// anomalies table: nothing is set for it.
	for _, f := range []string{"errdisabled: bpduguard", "errdisabled: link-flap", "errdisabled: portsec", "errdisabled: xcvr-unsupported", "errdisabled: speed-misconfigured"} {
		p := devicemodel.Port{Up: false, Fault: f}
		if bits, sat, _ := portAnomalies(p, false, nil); bits != 0 || sat != 100 {
			t.Errorf("%q = %d/%d, want 0/100", f, bits, sat)
		}
	}

	// Satisfaction as before: an uplink below its top speed costs 10 (and
	// sets no bit); lifetime drops cost 10 (reason 1), lifetime errors or
	// uncorrected FEC codewords 15 (reason 2).
	slow := devicemodel.Port{Up: true, SpeedMbps: 1000, SpeedCaps: []int{1000, 10000}}
	if bits, sat, reason := portAnomalies(slow, true, nil); bits != 0 || sat != 90 || reason != 0 {
		t.Errorf("slow uplink = %d/%d/%d, want 0/90/0", bits, sat, reason)
	}
	if _, sat, _ := portAnomalies(slow, false, nil); sat != 100 {
		t.Errorf("slow non-uplink satisfaction = %d", sat)
	}
	dropped := up
	dropped.Counters = devicemodel.Counters{TxDropped: 14}
	if bits, sat, reason := portAnomalies(dropped, false, nil); bits != 0 || sat != 90 || reason != 1 {
		t.Errorf("drops = %d/%d/%d, want 0/90/1", bits, sat, reason)
	}
	errs := up
	errs.Counters = devicemodel.Counters{RxErrors: 10}
	if bits, sat, reason := portAnomalies(errs, false, nil); bits != 0 || sat != 85 || reason != 2 {
		t.Errorf("errors = %d/%d/%d, want 0/85/2", bits, sat, reason)
	}
	both := up
	both.Counters = devicemodel.Counters{RxErrors: 1, RxDropped: 1}
	if _, sat, reason := portAnomalies(both, false, nil); sat != 75 || reason != 3 {
		t.Errorf("errors+drops = %d/%d", sat, reason)
	}
	fec := up
	fec.Health = devicemodel.PortHealth{HasFECCounters: true, FECUncorrected: 6}
	if bits, sat, reason := portAnomalies(fec, false, nil); bits != 0 || sat != 85 || reason != 2 {
		t.Errorf("fec uncorrected = %d/%d/%d, want 0/85/2", bits, sat, reason)
	}
}

// Regression (2026-11-13): the controller reads port_table[].anomalies in
// its own table, where the bridge's old link-flap bit (64) is "rogue DHCP
// server" and its old topology-flap bit (32) "DHCP packet rate exceeded";
// each raised a false alert. The Arista's real capture, then a second
// collect in which port 49 flapped, saw STP changes, raised both optic
// alarms and grew errors, drops and uncorrected FEC codewords, and other
// ports were errdisabled for reasons that are not loop protection: every
// port still reports anomalies 0, and no port carries custom_anomalies.
func TestPortAnomaliesNoFalseAlerts(t *testing.T) {
	ft := aristaeos.NewFixtureTransport(filepath.Join("..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
	c := aristaeos.NewCollector(ft)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ip := "192.0.2.9"
	if len(snap.System.Addresses) > 0 {
		ip = snap.System.Addresses[0].IP
	}
	desc, err := DescriptorFor("UDC48X6", snap, Identity{MAC: snap.System.MAC, IP: ip, Hostname: "arista", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	st := State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	sess.SetCapabilities(c.Capabilities())
	sess.SetSnapshot(snap)
	sess.SetGatewayIP("192.0.2.1")
	sess.BuildPayload(time.Now()) // the previous inform the session remembers

	next := *snap
	next.TakenAt = snap.TakenAt.Add(70 * time.Second)
	next.Ports = append([]devicemodel.Port(nil), snap.Ports...)
	var flapped, faulted int
	for i := range next.Ports {
		p := &next.Ports[i]
		switch {
		case p.Index == 49:
			if !p.Up || !p.Health.HasFECCounters {
				t.Fatalf("port 49 in the capture: up %v, FEC counters %v; the test needs an up optical port with FEC", p.Up, p.Health.HasFECCounters)
			}
			p.Health.LinkChanges += 4 // two drops and two recoveries
			p.Health.STPChanges += 5
			p.Health.OpticRxAlarm, p.Health.OpticTxAlarm = true, true
			p.Health.FECUncorrected += 3
			p.Counters.RxErrors += 10
			p.Counters.TxDropped += 10
			flapped++
		case !p.Up && p.Fault == "" && faulted < 4:
			p.Fault = []string{"errdisabled: bpduguard", "errdisabled: link-flap", "errdisabled: portsec", "errdisabled: xcvr-unsupported"}[faulted]
			faulted++
		}
	}
	if flapped != 1 || faulted != 4 {
		t.Fatalf("capture changed: flapped %d, faulted %d", flapped, faulted)
	}
	sess.SetSnapshot(&next)
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	for _, e := range m["port_table"].([]any) {
		p := e.(map[string]any)
		if a, ok := p["anomalies"].(float64); ok && a != 0 {
			t.Errorf("port %v anomalies = %v, want 0", p["port_idx"], a)
		}
		if v, ok := p["custom_anomalies"]; ok {
			t.Errorf("port %v sends custom_anomalies = %v; the controller computes that field", p["port_idx"], v)
		}
	}
	p49 := portEntry(t, m, 49)
	if _, ok := p49["anomalies"]; !ok {
		t.Error("port 49 has no anomalies key; real switches send 0 when healthy")
	}
	// The raw fields the controller derives its own alerts from are still sent.
	if p49["link_down_count"] == nil || p49["sfp_rxfault"] != true || p49["sfp_txfault"] != true || p49["sfp_found"] != true {
		t.Errorf("port 49 raw fields: link_down_count %v sfp_found %v sfp_rxfault %v sfp_txfault %v",
			p49["link_down_count"], p49["sfp_found"], p49["sfp_rxfault"], p49["sfp_txfault"])
	}
}

// The port's own protection drop counters set their anomalies bit only on
// growth since the previous inform, up or down: no history, no growth or a
// reset sets nothing.
func TestPortAnomaliesProtectionDrops(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *devicemodel.PortHealth, n uint64)
		bit  int
	}{
		{"broadcast storm", func(h *devicemodel.PortHealth, n uint64) { h.StormDropsBroadcast = n }, 1},
		{"multicast storm", func(h *devicemodel.PortHealth, n uint64) { h.StormDropsMulticast = n }, 2},
		{"unicast storm", func(h *devicemodel.PortHealth, n uint64) { h.StormDropsUnicast = n }, 4},
		{"DHCP rate", func(h *devicemodel.PortHealth, n uint64) { h.DHCPRateDrops = n }, 32},
		{"rogue DHCP", func(h *devicemodel.PortHealth, n uint64) { h.RogueDHCPDrops = n }, 64},
		{"MAC locking", func(h *devicemodel.PortHealth, n uint64) { h.MACLockingDrops = n }, 256},
		{"802.1X failures", func(h *devicemodel.PortHealth, n uint64) { h.Dot1XFailures = n }, 128},
	} {
		for _, up := range []bool{true, false} {
			port := func(n uint64) devicemodel.Port {
				p := devicemodel.Port{Up: up, SpeedMbps: 10000, SpeedCaps: []int{1000, 10000}}
				tc.set(&p.Health, n)
				return p
			}
			hist := func(n uint64) *portHistory {
				return &portHistory{Health: port(n).Health}
			}
			if bits, _, _ := portAnomalies(port(17), false, hist(12)); bits != tc.bit {
				t.Errorf("%s (up %v) grew 12 -> 17: bits %d, want %d", tc.name, up, bits, tc.bit)
			}
			if bits, _, _ := portAnomalies(port(17), false, hist(17)); bits != 0 {
				t.Errorf("%s (up %v) flat at 17: bits %d, want 0", tc.name, up, bits)
			}
			if bits, _, _ := portAnomalies(port(17), false, nil); bits != 0 {
				t.Errorf("%s (up %v) with no history: bits %d, want 0", tc.name, up, bits)
			}
			if bits, _, _ := portAnomalies(port(3), false, hist(17)); bits != 0 {
				t.Errorf("%s (up %v) reset 17 -> 3: bits %d, want 0", tc.name, up, bits)
			}
		}
	}

	// Through the session: the first inform after a start has no history,
	// the next one sees the growth, and one with no further growth clears it.
	snap := &devicemodel.Snapshot{TakenAt: time.Now(), Ports: []devicemodel.Port{{Index: 1, Up: true, SpeedMbps: 10000, SpeedCaps: []int{1000, 10000}}}}
	snap.Ports[0].Health.RogueDHCPDrops = 5
	snap.Ports[0].Health.StormDropsBroadcast = 9
	desc, err := DescriptorFor("USWF07D", snap, Identity{MAC: "02:00:00:00:00:01", IP: "192.0.2.9", Hostname: "sw", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}, nil, time.Now())
	anomaliesOf := func(s *devicemodel.Snapshot) any {
		sess.SetSnapshot(s)
		var m map[string]any
		if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
			t.Fatal(err)
		}
		return portEntry(t, m, 1)["anomalies"]
	}
	if a := anomaliesOf(snap); a != float64(0) {
		t.Errorf("first inform: anomalies %v, want 0 (no history)", a)
	}
	next := *snap
	next.TakenAt = snap.TakenAt.Add(70 * time.Second)
	next.Ports = append([]devicemodel.Port(nil), snap.Ports...)
	next.Ports[0].Health.RogueDHCPDrops = 8
	if a := anomaliesOf(&next); a != float64(portAnomalyRogueDHCP) {
		t.Errorf("rogue DHCP drops grew: anomalies %v, want 64", a)
	}
	again := next
	again.TakenAt = next.TakenAt.Add(70 * time.Second)
	if a := anomaliesOf(&again); a != float64(0) {
		t.Errorf("no further growth: anomalies %v, want 0", a)
	}
}

// A port its own protection shut down reports the cause's bit, and a fault
// with no such cause reports none.
func TestPortAnomaliesProtectionHold(t *testing.T) {
	for fault, want := range map[string]int{
		"errdisabled: storm-broadcast":                 portAnomalyBroadcastStorm,
		"errdisabled: storm-multicast":                 portAnomalyMulticastStorm,
		"errdisabled: storm-unicast":                   portAnomalyUnicastStorm,
		"errdisabled: dhcp-rate-limit (412 frames/s)":  portAnomalyDHCPRate,
		"errdisabled: dot1x":                           portAnomalyDot1X,
		"errdisabled: mac-locking (02:00:00:00:00:01)": portAnomalyMACLocking,
		"errdisabled: loop-protect":                    portAnomalyLoopProtection,
		"errdisabled: denial-of-service":               portAnomalyDoS,
		"errdisabled: bpduguard":                       0,
		"errdisabled: speed-misconfigured":             0,
		"":                                             0,
	} {
		p := devicemodel.Port{Index: 1, Fault: fault}
		if bits, _, _ := portAnomalies(p, false, nil); bits != want {
			t.Errorf("fault %q: anomalies = %d, want %d", fault, bits, want)
		}
	}
}

// error_disabled and its reason are set for the two protections UniFi's
// model shuts a port for (docs/drivers/sonic.md §11.20) and for nothing else:
// the controller raises its BPDU-guard and loop-protection alerts from them
// (§11.23).
func TestErrorDisabled(t *testing.T) {
	for fault, want := range map[string]int{
		"errdisabled: bpduguard":                       errDisabledBPDUGuard,
		"errdisabled: loop-protect (10 MAC moves)":     errDisabledLoopProtection,
		"errdisabled: loopprotect":                     errDisabledLoopProtection,
		"errdisabled: storm-broadcast":                 0,
		"errdisabled: mac-locking (02:00:00:00:00:01)": 0,
		"errdisabled: speed-misconfigured":             0,
		"":                                             0,
	} {
		if got := errorDisabled(fault); got != want {
			t.Errorf("fault %q: error_disabled_reason %d, want %d", fault, got, want)
		}
	}
}

// LastPayload is the inform as encoded: a growth bit the inform carried is in
// it, though a second build of the same state no longer sees the growth
// (2026-11-15: payload-last.json read anomalies 0 for the very inform that
// raised the DHCP-rate alert).
func TestLastPayloadKeepsTheSentAnomalies(t *testing.T) {
	snap := &devicemodel.Snapshot{TakenAt: time.Now(), Ports: []devicemodel.Port{{Index: 1, Up: true, SpeedMbps: 10000, SpeedCaps: []int{1000, 10000}}}}
	desc, err := DescriptorFor("USWF07D", snap, Identity{MAC: "02:00:00:00:00:01", IP: "192.0.2.9", Hostname: "sw", UDAPIVersion: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	sess := NewSession(desc, "http://192.0.2.1:8080/inform", State{Key: "0123456789abcdef0123456789abcdef", Adopted: true, UseAESGCM: true, CfgVersion: "abc"}, nil, time.Now())
	if sess.LastPayload() != nil {
		t.Fatal("LastPayload before any inform: want nil")
	}
	sess.SetSnapshot(snap)
	if _, err := sess.EncodeInform(time.Now()); err != nil {
		t.Fatal(err)
	}
	next := *snap
	next.TakenAt = snap.TakenAt.Add(70 * time.Second)
	next.Ports = append([]devicemodel.Port(nil), snap.Ports...)
	next.Ports[0].Health.DHCPRateDrops = 397
	sess.SetSnapshot(&next)
	if _, err := sess.EncodeInform(time.Now()); err != nil {
		t.Fatal(err)
	}
	anomalies := func(b []byte) any {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return portEntry(t, m, 1)["anomalies"]
	}
	if a := anomalies(sess.LastPayload()); a != float64(portAnomalyDHCPRate) {
		t.Errorf("LastPayload: anomalies %v, want %d (the growth the inform carried)", a, portAnomalyDHCPRate)
	}
	if a := anomalies(sess.BuildPayload(time.Now())); a != float64(0) {
		t.Errorf("a second build: anomalies %v, want 0 (the history has moved on)", a)
	}
}
