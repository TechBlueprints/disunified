package informloop

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
)

// The DX010 on Network 11, replayed (docs/wire-shapes.md §9.1): its own
// on-switch reply log of 2026-11-15 on 11.0.99, against a capture of the
// switch taken the same morning
// (sonic-igmp-202511.20261115-6c8eb1cf7/collect-net11.txt, as it was
// before and after the runs: every override back on its pre-test copy).
// controller-11.0.99/replies-dx010.ndjson is scrubbed with
// scripts/sanitize-controller.py, its addresses seeded with the capture's
// scrub so both are one network (192.0.2.1 the switch on Vlan4094,
// 192.0.2.5 the gateway and DNS server, 192.0.2.2/.3 the NTP servers,
// 192.0.2.4 the syslog host; the routed test subnet 198.18.4.0/30, the
// Inter-VLAN routing transit 198.18.253.0/24), and the switch's name
// aligned with the capture's (host-1). Records 0 and 1 are the push that
// was current that morning and the noop after it; then every record of
// the live runs of docs/drivers/sonic.md §11.24.5 and §11.16:
//
//   - 3: an unknown-unicast storm level of 1 % on port 16 (Ethernet60) from
//     the port editor; 5: the level removed;
//   - 9: Routing + Aggregating on ports 15+16 (198.18.4.5/30): the
//     push names only the first member (port 15 `opmode=aggregate lag=1`,
//     routing interface `type=lag lag=15`) and has no key for port 16 at
//     all, as on 10.6.109; the controller's first routed network brings the
//     Inter-VLAN routing transit (VLAN 4040);
//   - 12: back to Switching (the UI then edits 15 and 16 together); 14: the
//     REST restore of both overrides, the same push as record 0.
//
// Every other record is a reconcile that must write nothing, as live; the
// one live write the fixture runner cannot show is record 10's kernel MTU
// of the new Vlan4040's netdev (`ip link set dev Vlan4040 mtu 1500`: the
// capture has no such netdev to read).
func TestReplaySonicDX010Network11(t *testing.T) {
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-igmp-202511.20261115-6c8eb1cf7", "collect-net11.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// The image's orchagent carries the kit's sonic-swss 0003 and 0004, read
	// on the switch the same day (the bridge logged the L2 ACL table type;
	// orchagent holds 0004's notice).
	fr.OrchL2, fr.OrchL2MCFix = "1", "1"
	// The capture's l2d.py is the one deployed that day; this replay is about
	// the controller's pushes, not an l2d.py update.
	fr.CurrentL2d()
	var drv strings.Builder
	coll := sonic.NewCollector(fr)
	coll.Log = log.New(&drv, "", 0)
	coll.L2 = true
	coll.IGMP = true
	coll.Dial = "192.0.2.1"
	coll.Redial = fr.Dial
	coll.StateDir = t.TempDir()
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen, last := 0, ""
	written := func() string {
		c := fr.Recorded()
		last = strings.Join(c[seen:], "\n")
		seen = len(c)
		return last
	}
	got := map[int]string{}
	runReplay(t, replayDriver{
		Name: "sonic-dx010-11.0.99", Replies: "controller-11.0.99/replies-dx010.ndjson", Model: "USWF07D",
		Collector: coll, Control: coll, Snapshot: snap,
		Start:     &device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", UseAESGCM: true},
		GatewayIP: "192.0.2.5",
		Written:   written,
		Loop: func(c *Config) {
			c.AddressController = coll
			c.ControlIGMP, c.ControlNTP, c.ControlSyslog, c.ControlHostname = true, true, true, true
			// SSH keys stay out: both scrubs replace the keys, so the push's
			// no longer parse as key lines and the capture's block differs.
			c.ControlReboot, c.ControlSNMP = true, true
			c.AllowInitialChanges = true // long adopted: the bridge had applied pushes for two weeks
			// The names the deployed bridge treats as the controller's
			// defaults (cmd/disunified): "Port N", the profile's "QSFP28 N",
			// and the driver's own (the alias, EthN).
			c.DefaultPortNames = map[int][]string{}
			for _, p := range snap.Ports {
				c.DefaultPortNames[p.Index] = append([]string{fmt.Sprintf("Port %d", p.Index), fmt.Sprintf("QSFP28 %d", p.Index)}, coll.DefaultPortNames(p)...)
			}
		},
		Check: func(t *testing.T, i int, rec replayRecord, sess *device.Session, l *Loop, logText string) {
			got[i] = last
			if l.state != StateConnected {
				t.Errorf("reply %d: state %v, want CONNECTED throughout", i, l.state)
			}
		},
	})
	save := "sudo config save -y"
	l2mcDel := func(port string) []string {
		var out []string
		for _, g := range []string{"224.0.1.129", "224.0.1.1", "224.0.1.22", "224.0.1.35", "224.0.1.41", "239.255.255.250", "239.255.255.253"} {
			out = append(out, "sudo sonic-db-cli CONFIG_DB del 'L2MC_STATIC_MEMBER|Vlan4094|"+g+"|"+port+"'")
		}
		return out
	}
	want := map[int][]string{
		// The ten site VLANs the 2026-11-02 Allow All run left (no port
		// carries them, so no push names them): deleted on the first push
		// (docs/drivers/sonic.md §11.29).
		0: pruneTen,
		3: {"sudo config interface storm-control add Ethernet60 unknown-unicast 1000000", save},
		5: {"sudo config interface storm-control del Ethernet60 unknown-unicast", save},
		// The transit VLAN and its address first (the device-wide apply),
		// then the one-member aggregate (the port apply; port 16 is in no
		// key, so Ethernet60 stays in Vlan4094), l2d's and l2mcd's config,
		// and the aggregate's address once the PortChannel exists.
		9: append(append(append([]string{
			"sudo config vlan add 4040", "sudo sonic-db-cli CONFIG_DB hset 'VLAN|Vlan4040' mtu 1500", save,
			"sudo config interface ip add Vlan4040 198.18.253.2/24", save,
			"sudo config portchannel add PortChannel1 --fast-rate true --min-links 1",
			"sudo sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel1' mtu 1500"},
			l2mcDel("Ethernet56")...),
			"sudo config vlan member del 4094 Ethernet56", "sudo config portchannel member add PortChannel1 Ethernet56", save,
			"bash -s -- disunified-l2", "sudo python3 - disunified-l2mc", save),
			"sudo config interface ip add PortChannel1 198.18.4.5/30", save),
		// The addresses out first, then the member, the port back in its
		// VLAN and the PortChannel gone, and last Vlan4040, which the push
		// no longer names (live, before §11.29, it was deleted by hand).
		// Port 16's interim name "QSFP28 16" is a default: not written.
		12: {
			`sh -c 'for i in $(seq 1 60); do vtysh -c "show ip route vrf all static" </dev/null | grep -qw PortChannel1 || exit 0; sleep 0.5; done; exit 0' disunified-rif-wait`,
			`sh -c 'for i in $(seq 1 60); do vtysh -c "show ip route vrf all static" </dev/null | grep -qw Vlan4040 || exit 0; sleep 0.5; done; exit 0' disunified-rif-wait`,
			"sudo config interface ip remove PortChannel1 198.18.4.5/30 </dev/null",
			"sudo config interface ip remove Vlan4040 198.18.253.2/24 </dev/null", save,
			"sudo config portchannel member del PortChannel1 Ethernet56", "sudo config vlan member add -u 4094 Ethernet56",
			"sudo config portchannel del PortChannel1", save,
			"bash -s -- disunified-l2", "sudo python3 - disunified-l2mc", save,
			"sudo config vlan del 4040", save},
	}
	recs := loadReplies(t, "controller-11.0.99/replies-dx010.ndjson")
	if len(recs) != 16 {
		t.Fatalf("%d records in replies-dx010.ndjson, want 16", len(recs))
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
	if strings.Contains(drv.String(), "port 16") || strings.Contains(strings.Join(fr.Recorded(), "\n"), "PortChannel1 Ethernet60") {
		t.Error("port 16 reached the aggregate: the push names no other member")
	}
}
