package informloop

import (
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// DHCP guarding and the switch ACL rules (docs/drivers/sonic.md §11.7):
// the controller's real pushes to the ECS Aggregation in the neutral model.

func readECSPush(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "controller-10.6.109", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDesiredDeviceGuardsAndACLs(t *testing.T) {
	var buf strings.Builder
	l := &Loop{cfg: Config{Logger: log.New(&buf, "", 0)}}

	snoop := l.desiredDevice(readECSPush(t, "system_cfg-ecs-dhcp-snoop.txt"))
	wantGuards := []devicemodel.DHCPGuard{
		{VLAN: 1, Server: "192.0.2.1"}, {VLAN: 69, Server: "192.0.2.5"}, {VLAN: 69, Server: "192.0.2.1"},
		{VLAN: 2, Server: "192.0.2.6"}, {VLAN: 2, Server: "192.0.2.1"}, {VLAN: 2, Server: "192.0.2.7"},
	}
	if snoop.DHCPSnooping == nil || !*snoop.DHCPSnooping || !reflect.DeepEqual(snoop.DHCPGuards, wantGuards) {
		t.Errorf("snooping %v, guards %+v", snoop.DHCPSnooping, snoop.DHCPGuards)
	}
	// No rule in this push: the device's rules of both kinds are none.
	if !snoop.IPACLSet || len(snoop.IPACLs) != 0 || !snoop.MACACLSet || len(snoop.MACACLs) != 0 {
		t.Errorf("rules: ip %v %v, mac %v %v", snoop.IPACLSet, snoop.IPACLs, snoop.MACACLSet, snoop.MACACLs)
	}
	off := l.desiredDevice(readECSPush(t, "system_cfg-ecs-aggregation.txt"))
	if off.DHCPSnooping == nil || *off.DHCPSnooping || off.DHCPGuards != nil {
		t.Errorf("setting off: %v %v", off.DHCPSnooping, off.DHCPGuards)
	}

	text := readECSPush(t, "system_cfg-ecs-acl.txt")
	d := l.desiredDevice(text)
	wantIP := []devicemodel.IPACL{
		{Name: "dui-test-allow-ip-1", Permit: true, Src: devicemodel.IPACLEnd{CIDR: "192.0.2.0/24"}, Dst: devicemodel.IPACLEnd{CIDR: "198.51.100.0/24"}},
		{Name: "dui-test-block-ip-1", Protocol: "tcp", Src: devicemodel.IPACLEnd{CIDR: "203.0.113.0/24"}, Dst: devicemodel.IPACLEnd{CIDR: "192.0.2.0/24", Port: 9}},
		{Name: "dui-test-block-ip-2", Protocol: "tcp", Src: devicemodel.IPACLEnd{CIDR: "203.0.113.0/24"}, Dst: devicemodel.IPACLEnd{CIDR: "192.0.2.0/24", Port: 1001}},
	}
	wantMAC := []devicemodel.MACACL{
		{Name: "dui-test-allow-mac-1", Permit: true, VLAN: 4010,
			Src: devicemodel.MACACLEnd{MAC: "02:00:00:00:00:01", Mask: "ff:ff:ff:ff:ff:ff"}, Dst: devicemodel.MACACLEnd{MAC: "02:00:00:00:00:02", Mask: "ff:ff:ff:ff:ff:ff"}},
		{Name: "dui-test-block-mac-1", VLAN: 4010, Src: devicemodel.MACACLEnd{MAC: "02:00:00:00:00:00", Mask: "ff:ff:ff:ff:ff:00"}},
	}
	if !d.IPACLSet || !reflect.DeepEqual(d.IPACLs, wantIP) {
		t.Errorf("ip rules %v:\n got  %+v\n want %+v", d.IPACLSet, d.IPACLs, wantIP)
	}
	if !d.MACACLSet || !reflect.DeepEqual(d.MACACLs, wantMAC) {
		t.Errorf("mac rules %v:\n got  %+v\n want %+v", d.MACACLSet, d.MACACLs, wantMAC)
	}
	if buf.Len() != 0 {
		t.Errorf("captured shapes logged: %s", buf.String())
	}

	// A shape no capture shows (UDP) holds the IP rules, logged once until
	// it changes; the MAC rules are unaffected.
	udp := strings.Replace(text, "switch.acl.ip.2.type=tcp", "switch.acl.ip.2.type=udp", 1)
	for i := 0; i < 2; i++ {
		h := l.desiredDevice(udp)
		if h.IPACLSet || h.IPACLs != nil || !h.MACACLSet || len(h.MACACLs) != 2 {
			t.Errorf("held: ip %v %v, mac %v %d", h.IPACLSet, h.IPACLs, h.MACACLSet, len(h.MACACLs))
		}
	}
	if n := strings.Count(buf.String(), "switch ACL rules HELD"); n != 1 {
		t.Errorf("held rules logged %d times, want once:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), `IP rule "dui-test-block-ip-1": protocol "udp"`) {
		t.Errorf("the log names the rule and the shape:\n%s", buf.String())
	}
	l.desiredDevice(text)
	if !strings.Contains(buf.String(), "every rule in the push is translated again") {
		t.Errorf("the hold's end is logged:\n%s", buf.String())
	}
}
