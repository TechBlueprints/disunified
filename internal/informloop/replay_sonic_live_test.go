package informloop

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// The SONiC lab session, replayed: replies-sonic.ndjson is the bridge's own
// reply log from the VS guest on 2026-10-27 (scrubbed with
// scripts/sanitize-controller.py; the one bridge-side POST error record
// dropped). Its addresses are aligned with the VS capture's scrub, so the
// replay is one network: 192.0.2.1 is the switch's in-band address
// (Vlan4094), 192.0.2.2 the gateway and controller, 192.0.2.10 the site's
// NTP/DNS server, 192.0.2.201 the address the controller moved the switch
// to and back from.
//
// The bridge wrote no ports for the adoption and naming pushes (records
// 0..94), as it did live -- the device-wide settings UniFi owns follow it
// from the first push, as the bridge runs them now -- and with port control
// on from record 95: the seeded
// push, port 2 disabled and renamed, Allow All, Block All, a custom tagged
// set, an aggregate of ports 5+6, a mirror 8 <- 7, port 9 forced 100G with
// RS-FEC and storm control, the mirror and the aggregate removed, jumbo
// on, IP Settings static to .201 and back to .200, and Manage -> Restart
// (twice). Each push must write exactly what the driver wrote live (or
// what it writes now, where a live run found a fix), and every noop reply
// in between is a reconcile that must write nothing.
func TestReplaySonicLabSession(t *testing.T) {
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238524-ff3c0c9f6", "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var drv strings.Builder
	coll := sonic.NewCollector(fr)
	coll.Log = log.New(&drv, "", 0)
	coll.Dial = "192.0.2.1" // in-band, Vlan4094
	coll.Redial = fr.Dial
	coll.StateDir = t.TempDir()
	coll.Resolve = func(host string) (string, error) { return "192.0.2.123", nil } // 0.ubnt.pool.ntp.org
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen, last := 0, ""
	written := func() string { // runReplay calls it after each reply, before Check
		c := fr.Recorded()
		last = strings.Join(c[seen:], "\n")
		seen = len(c)
		return last
	}
	ports := map[int]bool{}
	for i := 2; i <= 32; i++ {
		ports[i] = true // port 1 is the uplink: never written, as live
	}
	jumbo := func(mtu string) []string {
		var out []string
		for i := 1; i < 32; i++ {
			out = append(out, fmt.Sprintf("sudo config interface mtu Ethernet%d %s", i*4, mtu))
		}
		return out
	}
	save := "sudo config save -y"
	want := map[int][]string{
		// Device-wide settings follow UniFi without port control: the
		// first push after adoption brings its NTP servers and time zone,
		// the push after naming its hostname. (Live, before 2026-10-31,
		// they waited for port control at record 95.)
		2:  {"sudo config ntp add 192.0.2.10", "sudo config ntp add 192.0.2.123", "sudo config clock timezone MST7MDT", save},
		3:  {"sudo config hostname SONiCSeastone-DX010", save},
		95: append(jumbo("1500"), save),
		99: {"sudo config interface shutdown Ethernet4", "sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet4' description 'lab-2'", save},
		101: {"sudo config vlan add -m 2,8,9,10,69,1000,2000,3000,4000,4010", save,
			"sudo config interface startup Ethernet4", "sudo config vlan member add -e -m 4094 Ethernet4", "sudo config vlan member add -e -m 4094 Ethernet12", save},
		102: {"sudo config vlan member del -e -m 4094 Ethernet4", save},
		103: {"sudo config vlan member add -m 2,69 Ethernet8", save},
		104: {"sudo config portchannel add PortChannel1 --fast-rate true --min-links 1", "sudo sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel1' mtu 1500",
			"sudo config vlan member del 4094 Ethernet16", "sudo config portchannel member add PortChannel1 Ethernet16",
			"sudo config vlan member del 4094 Ethernet20", "sudo config portchannel member add PortChannel1 Ethernet20",
			"sudo config vlan member add -u 4094 PortChannel1", save},
		108: {"sudo config vlan member del 4094 Ethernet28", "sudo config mirror_session span add dui8 Ethernet28 Ethernet24 both", save},
		112: {"sudo config interface fec Ethernet32 rs", "sudo config interface storm-control add Ethernet32 broadcast 5000000",
			"sudo config interface storm-control add Ethernet32 unknown-multicast 1000000", save},
		114: {"sudo config mirror_session remove dui8", "sudo config vlan member add -u 4094 Ethernet28", save},
		121: {"sudo config portchannel member del PortChannel1 Ethernet16", "sudo config portchannel member del PortChannel1 Ethernet20",
			"sudo config vlan member add -u 4094 Ethernet16", "sudo config vlan member add -u 4094 Ethernet20",
			"sudo config vlan member del 4094 PortChannel1", "sudo config portchannel del PortChannel1", save},
		126: append(jumbo("9216"), save),
		145: {"sudo config interface ip add Vlan4094 192.0.2.201/16", "sudo config dns nameserver add 192.0.2.10",
			"echo 1 | sudo tee /proc/sys/net/ipv4/conf/Vlan4094/promote_secondaries >/dev/null",
			"sudo config interface ip remove Vlan4094 192.0.2.1/16", "ip -4 -o addr show dev Vlan4094 | grep -q ' 192.0.2.201/16 '", save},
		155: {"sudo reboot"}, // Manage -> Restart: {"_type":"reboot"} (logged UNHANDLED live before the fix)
		177: {"sudo reboot"},
		181: {"sudo config interface ip add Vlan4094 192.0.2.1/16",
			"echo 1 | sudo tee /proc/sys/net/ipv4/conf/Vlan4094/promote_secondaries >/dev/null",
			"sudo config interface ip remove Vlan4094 192.0.2.201/16", "ip -4 -o addr show dev Vlan4094 | grep -q ' 192.0.2.1/16 '", save},
		// Settings -> System -> remote logging switched on (the site's syslog
		// host, 2026-10-27, appended after the session): the host arrives as
		// syslog.remote.ip/port/status; the parser knew only syslog.ip until
		// this capture, so no driver had ever received a host.
		273: {"sudo config syslog add 192.0.2.40 --port 514", save},
	}
	got := map[int]string{}
	runReplay(t, replayDriver{
		Name: "sonic-lab", Replies: "replies-sonic.ndjson", Model: "USWF07D",
		Collector: coll, Snapshot: snap,
		Written: written,
		// No port control until record 95, as live; the device-wide settings
		// UniFi owns and the address follow the controller from the start,
		// as the bridge runs them now.
		Loop: func(c *Config) {
			c.Controller, c.AddressController = nil, coll
			c.ControlIGMP, c.ControlNTP, c.ControlSyslog, c.ControlHostname = true, true, true, true
			c.ControlReboot, c.ControlSSHKeys, c.ControlSNMP = true, true, true
		},
		Check: func(t *testing.T, i int, rec replayRecord, sess *device.Session, l *Loop, logText string) {
			got[i] = last
			if i == 94 {
				// Control switched on (live: a bridge restart with the control
				// block; the read-only pushes had been applied, so the first
				// push with control is not held).
				l.cfg.Controller, l.cfg.ControlPorts = coll, ports
				l.everApplied = true
				l.cfg.DefaultPortNames = map[int][]string{}
				for _, p := range snap.Ports {
					l.cfg.DefaultPortNames[p.Index] = append([]string{fmt.Sprintf("Port %d", p.Index), fmt.Sprintf("QSFP28 %d", p.Index)}, coll.DefaultPortNames(p)...)
				}
			}
		},
	})
	recs := loadReplies(t, "replies-sonic.ndjson")
	if len(recs) != 274 {
		t.Errorf("%d records in replies-sonic.ndjson, want 274", len(recs))
	}
	for i := range recs {
		w, ok := want[i]
		switch {
		case ok && got[i] != strings.Join(w, "\n"):
			t.Errorf("record %d: commands\n  got  %q\n  want %q", i, strings.Split(got[i], "\n"), w)
		case !ok && got[i] != "":
			t.Errorf("record %d (%s) wrote %q; a reconcile or a push that changes nothing must write nothing", i, replyKind(recs[i]), got[i])
		}
	}
	if coll.CurrentAddress() != "192.0.2.1" {
		t.Errorf("the bridge ends on %q, want back on 192.0.2.1", coll.CurrentAddress())
	}
}

func replyKind(r replayRecord) string {
	var p map[string]any
	_ = json.Unmarshal(r.Payload, &p)
	t, _ := p["_type"].(string)
	return t
}

// The jumbo flip with an aggregate present, through the loop, on the lab
// session's own aggregate push (record 104, ports 5+6 -> PortChannel1) with
// switch.jumboframes set each way: SONiC refuses `config interface mtu` on
// a member ("'interface_name' is in portchannel!", probed), so the
// PortChannel and each member's PORT mtu move by hset; every other port by
// `config interface mtu`; the reconcile after each writes nothing, and the
// PortChannel's LACP setting (fast rate, min-links 1, set by the create) is
// not written again.
func TestReplaySonicJumboFlipWithLAG(t *testing.T) {
	recs := loadReplies(t, "replies-sonic.ndjson")
	var agg map[string]any
	if err := json.Unmarshal(recs[104].Payload, &agg); err != nil {
		t.Fatal(err)
	}
	text, _ := agg["system_cfg"].(string)
	if !strings.Contains(text, "switch.port.5.opmode=aggregate") || !strings.Contains(text, "switch.jumboframes=disabled") {
		t.Fatal("record 104 is not the aggregate push with jumbo off")
	}
	r := newSonicRig(t, text, func(c *Config, _ *sonic.Collector) {
		c.ControlPorts = map[int]bool{}
		for i := 2; i <= 32; i++ {
			c.ControlPorts[i] = true
		}
	})
	if cmds := r.applyAndReconcile(t); !contains(cmds, "sudo config portchannel member add PortChannel1 Ethernet20") {
		t.Fatalf("the aggregate was not built: %q", cmds)
	}
	ver := 1
	flip := func(on bool, mtu string) []string {
		t.Helper()
		ver++
		v := fmt.Sprintf("v%d", ver)
		body := strings.Replace(text, "switch.jumboframes=disabled", map[bool]string{true: "switch.jumboframes=enabled", false: "switch.jumboframes=disabled"}[on], 1)
		payload, _ := json.Marshal(map[string]any{"_type": "setparam", "cfgversion": v, "system_cfg": body})
		r.sess.Apply(time.Now(), payload)
		n := len(r.fr.Commands)
		if !r.loop.applyPending(context.Background()) {
			t.Fatal("the push was not consumed")
		}
		if got, _, _ := r.sess.Applied(); got != v {
			t.Fatalf("jumbo %v not applied (cfgversion %q); log:\n%s", on, got, r.log.String())
		}
		cmds := append([]string(nil), r.fr.Commands[n:]...)
		if _, err := r.coll.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
		n = len(r.fr.Commands)
		r.loop.reconcile(context.Background())
		if again := r.fr.Commands[n:]; len(again) != 0 {
			t.Errorf("jumbo %v: the reconcile wrote %q", on, again)
		}
		member := regexp.MustCompile(`^sudo config interface mtu Ethernet(16|20) `)
		for _, c := range cmds {
			if member.MatchString(c) {
				t.Errorf("jumbo %v: `config interface mtu` on a LAG member: %q", on, c)
			}
			if strings.Contains(c, "fast_rate") {
				t.Errorf("jumbo %v: the LACP setting the create wrote was written again: %q", on, c)
			}
		}
		for _, w := range []string{
			"sudo sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel1' mtu " + mtu,
			"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet16' mtu " + mtu,
			"sudo sonic-db-cli CONFIG_DB hset 'PORT|Ethernet20' mtu " + mtu,
			"sudo config interface mtu Ethernet4 " + mtu,
		} {
			if !contains(cmds, w) {
				t.Errorf("jumbo %v: missing %q in %q", on, w, cmds)
			}
		}
		if n := count(cmds, regexp.MustCompile(`^sudo config interface mtu Ethernet\d+ `+mtu+`$`)); n != 29 {
			t.Errorf("jumbo %v: %d ports by config interface mtu, want 29 (31 controlled, 2 in the LAG)", on, n)
		}
		return cmds
	}
	on := flip(true, "9216")
	flip(false, "1500")
	t.Logf("jumbo on with the LAG: %q", on)
}
