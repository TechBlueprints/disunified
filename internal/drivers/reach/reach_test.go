package reach

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

func TestPrefixLen(t *testing.T) {
	for mask, want := range map[string]int{"255.255.255.0": 24, "255.255.0.0": 16, "": 0, "not-a-mask": 0, "2001:db8::": 0} {
		if got := PrefixLen(mask); got != want {
			t.Errorf("PrefixLen(%q) = %d, want %d", mask, got, want)
		}
	}
}

// The options are taken as written, MACs lower-cased; an operator MAC wins
// over the driver's, none leaves the driver's own.
func TestOptionsFillTheSystem(t *testing.T) {
	var id Identity
	id.SetOptions("192.0.2.4", map[string]string{"mac": " 02:00:00:00:00:AB ", "netmask": "255.255.0.0", "gateway_mac": "02:00:00:00:00:FE", "gateway": "192.0.2.1"})
	sys := devicemodel.System{MAC: "02:00:00:00:00:01"}
	id.Fill(context.Background(), &sys)
	if sys.MAC != "02:00:00:00:00:ab" || sys.GatewayMAC != "02:00:00:00:00:fe" || sys.Gateway != "192.0.2.1" {
		t.Errorf("mac %q gateway_mac %q gateway %q", sys.MAC, sys.GatewayMAC, sys.Gateway)
	}
	if len(sys.Addresses) != 1 || sys.Addresses[0].IP != "192.0.2.4" || sys.Addresses[0].PrefixLen != 16 {
		t.Errorf("addresses %+v, want 192.0.2.4/16", sys.Addresses)
	}
	var none Identity
	sys = devicemodel.System{MAC: "02:00:00:00:00:01"}
	none.Fill(context.Background(), &sys)
	if sys.MAC != "02:00:00:00:00:01" || len(sys.Addresses) != 0 {
		t.Errorf("without options: mac %q addresses %+v", sys.MAC, sys.Addresses)
	}
}

// The bridge container's own network namespace on the site's Podman host
// (captured 2026-11-15 with nsenter, scrubbed by sanitize-controller.py
// --text with the subnet mappings of rest-networkconf-subnets.json, so the
// two agree): its LAN macvlan holds 198.18.106.85/16 on the Default network
// the UPSes are on, the podman bridge 198.19.6.2/24, and the neighbour
// table answers for the gateway 198.18.0.1 with 02:00:00:00:00:02.
func bridgeNetns(t *testing.T) (neighbours func() (map[string]string, error), local func() ([]net.Addr, error)) {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "docs", "fixtures", "bridge-linux-6.12")
	arp, err := os.ReadFile(filepath.Join(dir, "proc-net-arp.txt"))
	if err != nil {
		t.Fatal(err)
	}
	addr, err := os.ReadFile(filepath.Join(dir, "ip-4-addr-route.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var addrs []net.Addr
	for _, m := range regexp.MustCompile(`inet (\S+)`).FindAllStringSubmatch(string(addr), -1) {
		ip, n, err := net.ParseCIDR(m[1])
		if err != nil {
			t.Fatal(err)
		}
		addrs = append(addrs, &net.IPNet{IP: ip, Mask: n.Mask})
	}
	return func() (map[string]string, error) { return ParseProcNetARP(arp), nil },
		func() ([]net.Addr, error) { return addrs, nil }
}

func TestParseProcNetARP(t *testing.T) {
	neighbours, _ := bridgeNetns(t)
	table, _ := neighbours()
	if len(table) != 10 || table["198.18.0.1"] != "02:00:00:00:00:02" || table["198.19.6.1"] != "02:00:00:00:00:08" {
		t.Errorf("table %v: want the capture's 10 complete entries", table)
	}
	if got := ParseProcNetARP([]byte("IP address       HW type     Flags       HW address            Mask     Device\n" +
		"198.18.0.9       0x1         0x0         00:00:00:00:00:00     *        eth0\n")); len(got) != 0 {
		t.Errorf("an incomplete entry (flags 0x0) was taken: %v", got)
	}
}

// The site's networks as unifiapi.NetworkFor reads them from the capture
// (internal/unifiapi/subnet_test.go): Default and corporate-1.
func siteNetworks(ctx context.Context, ip string) (devicemodel.SiteNetwork, error) {
	switch a := net.ParseIP(ip); {
	case (&net.IPNet{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(16, 32)}).Contains(a):
		return devicemodel.SiteNetwork{PrefixLen: 16, Gateway: "198.18.0.1"}, nil
	case (&net.IPNet{IP: net.IPv4(198, 19, 1, 0), Mask: net.CIDRMask(24, 32)}).Contains(a):
		return devicemodel.SiteNetwork{PrefixLen: 24, Gateway: "198.19.1.1"}, nil
	}
	return devicemodel.SiteNetwork{}, errors.New("no network holds " + ip)
}

func identityAt(t *testing.T, addr string, opts map[string]string) *Identity {
	neighbours, local := bridgeNetns(t)
	id := &Identity{SiteNetwork: siteNetworks, Neighbours: neighbours, LocalAddrs: local, Driver: "test",
		Nudge: func(string) { t.Error("nudged with the gateway's entry present") }}
	id.SetOptions(addr, opts)
	return id
}

// With no options a device on the bridge's own segment is reported with the
// site network's prefix and gateway and the gateway's L2 address from the
// bridge's neighbour table: what the hand-typed options said.
func TestDerivedOnTheBridgesSegment(t *testing.T) {
	id := identityAt(t, "198.18.110.238", nil)
	var sys devicemodel.System
	id.Fill(context.Background(), &sys)
	if sys.GatewayMAC != "02:00:00:00:00:02" || sys.Gateway != "198.18.0.1" ||
		len(sys.Addresses) != 1 || sys.Addresses[0].PrefixLen != 16 {
		t.Errorf("gateway_mac %q gateway %q addresses %+v; want 02:00:00:00:00:02 via 198.18.0.1, /16", sys.GatewayMAC, sys.Gateway, sys.Addresses)
	}
	if gw := id.GatewayFor(context.Background(), "198.19.1.40"); gw != "198.19.1.1" {
		t.Errorf("GatewayFor an address on corporate-1 = %q, want its gateway 198.19.1.1", gw)
	}
}

// The options still win, each on its own.
func TestOptionsOverrideTheDerived(t *testing.T) {
	id := identityAt(t, "198.18.110.238", map[string]string{"netmask": "255.255.255.0", "gateway": "198.18.0.254", "gateway_mac": "02:00:00:00:00:FE"})
	var sys devicemodel.System
	id.Fill(context.Background(), &sys)
	if sys.GatewayMAC != "02:00:00:00:00:fe" || sys.Gateway != "198.18.0.254" || sys.Addresses[0].PrefixLen != 24 {
		t.Errorf("gateway_mac %q gateway %q addresses %+v; want the options'", sys.GatewayMAC, sys.Gateway, sys.Addresses)
	}
	if gw := id.GatewayFor(context.Background(), "198.19.1.40"); gw != "198.18.0.254" {
		t.Errorf("GatewayFor = %q, want options.gateway", gw)
	}
	id = identityAt(t, "198.18.110.238", map[string]string{"gateway_mac": "02:00:00:00:00:fe"})
	sys = devicemodel.System{}
	id.Fill(context.Background(), &sys)
	if sys.GatewayMAC != "02:00:00:00:00:fe" || sys.Gateway != "198.18.0.1" || sys.Addresses[0].PrefixLen != 16 {
		t.Errorf("gateway_mac alone: %q via %q /%d; want the option's MAC, the site's gateway and prefix", sys.GatewayMAC, sys.Gateway, sys.Addresses[0].PrefixLen)
	}
}

// A device on a segment the bridge has no address on: its own neighbour
// table knows its own gateway, not the device's, so the gateway MAC is the
// option or nothing; the prefix and gateway still come from the site.
func TestOffTheBridgesSegment(t *testing.T) {
	id := identityAt(t, "198.19.1.40", nil)
	id.Nudge = func(string) { t.Error("nudged a gateway on another segment") }
	var sys devicemodel.System
	id.Fill(context.Background(), &sys)
	if sys.GatewayMAC != "" || sys.Gateway != "198.19.1.1" || sys.Addresses[0].PrefixLen != 24 {
		t.Errorf("gateway_mac %q gateway %q addresses %+v; want no MAC, 198.19.1.1, /24", sys.GatewayMAC, sys.Gateway, sys.Addresses)
	}
	id = identityAt(t, "198.19.1.40", map[string]string{"gateway_mac": "02:00:00:00:00:fe"})
	sys = devicemodel.System{}
	id.Fill(context.Background(), &sys)
	if sys.GatewayMAC != "02:00:00:00:00:fe" {
		t.Errorf("gateway_mac %q, want the option", sys.GatewayMAC)
	}
	// No site network and no options: an address, nothing else.
	id = identityAt(t, "203.0.113.5", nil)
	sys = devicemodel.System{}
	id.Fill(context.Background(), &sys)
	if sys.GatewayMAC != "" || sys.Gateway != "" || len(sys.Addresses) != 1 || sys.Addresses[0].PrefixLen != 0 {
		t.Errorf("no network: gateway_mac %q gateway %q addresses %+v", sys.GatewayMAC, sys.Gateway, sys.Addresses)
	}
}

// A gateway the bridge has not resolved is nudged once a minute at most and
// read on a later collect; an entry that goes missing again, or a site
// read that fails, keeps the last good value.
func TestMissingEntryAndFailedReadsKeepTheLast(t *testing.T) {
	full, local := bridgeNetns(t)
	table, _ := full()
	gwMAC := table["198.18.0.1"]
	delete(table, "198.18.0.1")
	nudges := make(chan string, 4)
	siteErr := error(nil)
	id := &Identity{
		SiteNetwork: func(ctx context.Context, ip string) (devicemodel.SiteNetwork, error) {
			if siteErr != nil {
				return devicemodel.SiteNetwork{}, siteErr
			}
			return siteNetworks(ctx, ip)
		},
		Neighbours: func() (map[string]string, error) { return table, nil }, LocalAddrs: local,
		Nudge: func(ip string) { nudges <- ip }, Driver: "test",
	}
	id.SetOptions("198.18.110.238", nil)
	fill := func() devicemodel.System {
		var sys devicemodel.System
		id.Fill(context.Background(), &sys)
		return sys
	}
	if sys := fill(); sys.GatewayMAC != "" {
		t.Errorf("no entry yet: gateway_mac %q", sys.GatewayMAC)
	}
	select {
	case ip := <-nudges:
		if ip != "198.18.0.1" {
			t.Errorf("nudged %s, want the gateway", ip)
		}
	case <-time.After(time.Second):
		t.Fatal("the gateway was not nudged")
	}
	fill()
	select {
	case ip := <-nudges:
		t.Errorf("nudged %s twice within a minute", ip)
	case <-time.After(50 * time.Millisecond):
	}
	table["198.18.0.1"] = gwMAC
	if sys := fill(); sys.GatewayMAC != gwMAC {
		t.Errorf("entry present: gateway_mac %q, want %q", sys.GatewayMAC, gwMAC)
	}
	delete(table, "198.18.0.1")
	siteErr = errors.New("controller unreachable")
	if sys := fill(); sys.GatewayMAC != gwMAC || sys.Gateway != "198.18.0.1" || sys.Addresses[0].PrefixLen != 16 {
		t.Errorf("entry gone, site unreadable: %q via %q /%d; want the last good values", sys.GatewayMAC, sys.Gateway, sys.Addresses[0].PrefixLen)
	}
}
