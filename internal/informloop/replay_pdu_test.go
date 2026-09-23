package informloop

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	apcpdu "github.com/TechBlueprints/disunified/internal/drivers/apc-pdu"
)

// The PDU replay: the controller's recorded replies to the APC rack PDU
// (replies-pdu.ndjson, cut from the bridge's own reply log on 2026-09-21
// and scrubbed) drive the real driver against the real card capture. Every
// push was made live -- through the controller's API and, for the last
// three, through the UniFi UI's own outlet editor and its Power Cycle
// button -- and the relay on the real AP7931 was seen to follow each one.
// What must come out is the SNMP SET each reply produced, and nothing else.
//
// The controller addresses outlets at the USP-PDU-Pro's AC positions
// (5..20); the card numbers them 1..16. So its outlet 12 is the card's 8,
// its 15 is the card's 11, and its 13 is the card's 9.
func TestReplayControllerRepliesPDU(t *testing.T) {
	fr, err := apcpdu.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "apc-aos-3.9.2"))
	if err != nil {
		t.Fatal(err)
	}
	coll := apcpdu.NewCollector(fr)
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seenSets, seenCfgs := 0, 0
	written := func() string {
		var lines []string
		lines = append(lines, fr.Sets[seenSets:]...)
		for _, c := range fr.Configs[seenCfgs:] {
			lines = append(lines, "config.ini: "+strings.ReplaceAll(c, "\r\n", " "))
		}
		seenSets, seenCfgs = len(fr.Sets), len(fr.Configs)
		return strings.Join(lines, "\n")
	}
	const ctl = "1.3.6.1.4.1.318.1.1.12.3.3.1.1.4." // the outlet CONTROL column
	runReplay(t, replayDriver{
		Name: "apc-pdu", Replies: "replies-pdu.ndjson", Model: "USPPDUP",
		Collector: coll, Control: nil, Snapshot: snap,
		GatewayIP: "192.0.2.2",
		Written:   written,
		Loop: func(c *Config) {
			c.OutletController = coll
			c.ControlOutlets = nil // every outlet
			c.AddressController = coll
		},
		Expect: map[int]replayExpect{
			// 0: 404 pending, 1: adopt via mgmt_cfg, 2: emulated upgrade ->
			// CONNECTED. 3: the first system_cfg, captured before the outlets
			// were realigned to 5..20 so it still says outlet.1..16; every
			// relay enabled and every relay already on, so nothing is written.
			3: {mustNot: []string{ctl}},
			// 4: outlet 12 disabled (an API round trip made with the bridge
			// read-only at the time; here control is on, so it applies).
			4: {must: []string{ctl + "8=2"}, mustNot: []string{"config.ini"}},
			// 5: everything enabled again.
			5: {must: []string{ctl + "8=1"}},
			// 6-7: the API round trip, off then on.
			6: {must: []string{ctl + "8=2"}},
			7: {must: []string{ctl + "8=1"}},
			// 8: the USB outlets (1-4) marked off in the controller. The
			// card has no such outlets; nothing may be written for them.
			8: {mustNot: []string{ctl}},
			// 9-10: the UI's outlet editor, Disabled then Active on outlet 15.
			9:  {must: []string{ctl + "11=2"}},
			10: {must: []string{ctl + "11=1"}},
			// 11: the UI's Power Cycle on outlet 13: relayctl with a
			// selection list -> the card's own immediate-reboot command.
			11: {must: []string{ctl + "9=3"}},
			// 12: the next cycle. The card has finished the reboot and reads
			// the outlet back as on, so the reconcile must not fight it.
			12: {mustNot: []string{ctl}},
			// 13: the PDU's IP Settings set to a static address in the
			// controller. The reply log's scrub gave that address a different
			// documentation value from the card capture's, so this is a real
			// change, and the bridge must write the card's TCP/IP section --
			// with the Override line the card requires, or it is ignored.
			13: {must: []string{"config.ini: [NetworkTCP/IP] Override=02 00 00 00 00 01 BootMode=Manual SystemIP=192.0.2.3 SubnetMask=255.255.0.0 DefaultGateway=192.0.2.4"}, mustNot: []string{ctl}},
		},
		Check: func(t *testing.T, i int, rec replayRecord, sess *device.Session, l *Loop, logText string) {
			switch i {
			case 0:
				if sess.Adopted() || l.state != StatePending {
					t.Errorf("reply 0: the device must still be pending")
				}
			case 1:
				if !sess.Adopted() || l.state != StateAdopting {
					t.Errorf("after the adopt reply: adopted=%v state=%v", sess.Adopted(), l.state)
				}
			case 2:
				if l.state != StateConnected {
					t.Errorf("after the upgrade reply: state=%v", l.state)
				}
			case 11:
				if !strings.Contains(logText, "relayctl outlet 13: power cycle issued (device outlet 9)") {
					t.Errorf("relayctl was not run through the driver; log:\n%s", logText)
				}
				if strings.Contains(logText, `UNHANDLED cmd "relayctl"`) {
					t.Error("relayctl is still reported as unhandled")
				}
			}
		},
	})
	// Exactly one thing went to the card's config file in the whole replay:
	// the static address. The controller sends no outlet names, and its
	// default "Using DHCP" (every push before the last) is never applied --
	// the card started on a manual address and must not have been moved.
	if len(fr.Configs) != 1 {
		t.Errorf("config uploads = %d, want 1 (the static address); got %q", len(fr.Configs), fr.Configs)
	}
	for _, c := range fr.Configs {
		if strings.Contains(c, "DHCP") {
			t.Errorf("the controller's default DHCP setting was applied to the card: %q", c)
		}
	}
}
