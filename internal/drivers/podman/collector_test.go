package podman

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

const fixture = "../../../docs/fixtures/podman-5.8.2/collect.txt"

func startFixture(t *testing.T) (*Collector, *FixtureRunner, *devicemodel.Snapshot) {
	t.Helper()
	fr, err := NewFixtureRunner(fixture)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fr)
	c.Log = log.New(testWriter{t}, "", 0)
	snap, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, fr, snap
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func portByName(snap *devicemodel.Snapshot, name string) (devicemodel.Port, bool) {
	for _, p := range snap.Ports {
		if p.IfName == name {
			return p, true
		}
	}
	return devicemodel.Port{}, false
}

// The host is the switch: its uplink NIC's MAC and address, its hostname,
// podman's version as the firmware; the NIC is the top port and the uplink.
func TestHostIsTheSwitchAndTheNICIsTheUplink(t *testing.T) {
	c, _, snap := startFixture(t)
	sys := snap.System
	if sys.Hostname != "host-1" || sys.Vendor != "Podman" || sys.Version != "5.8.2" {
		t.Errorf("system = %+v", sys)
	}
	if !strings.HasPrefix(sys.Model, "AlmaLinux") {
		t.Errorf("model = %q, want the OS", sys.Model)
	}
	up, ok := portByName(snap, "enp6s18")
	if !ok || up.Index != topSlot || snap.UplinkHint != topSlot {
		t.Fatalf("uplink port = %+v (ok=%v), hint %d; want enp6s18 at %d", up, ok, snap.UplinkHint, topSlot)
	}
	if !up.Up || !up.Enabled || up.SpeedMbps != 100000 || up.Media != devicemodel.MediaQSFP28 {
		t.Errorf("uplink (virtio, speed unknown) = up %v enabled %v speed %d media %s; want up, 100G QSFP28", up.Up, up.Enabled, up.SpeedMbps, up.Media)
	}
	if sys.MAC == "" || sys.MAC != strings.ToLower(sys.MAC) || len(sys.Addresses) == 0 || sys.Gateway == "" {
		t.Errorf("reachability: mac %q addrs %v gateway %q", sys.MAC, sys.Addresses, sys.Gateway)
	}
	if !sys.DHCP {
		t.Errorf("the capture's uplink address is a lease; DHCP must be reported")
	}
	if sys.MemTotalKB == 0 || sys.Uptime == 0 || len(sys.LoadAvg) != 3 {
		t.Errorf("health: mem %d uptime %s load %v", sys.MemTotalKB, sys.Uptime, sys.LoadAvg)
	}
	if got := c.DeviceName(sys); got != "host-1" {
		t.Errorf("DeviceName = %q", got)
	}
	if got := c.DeviceName(devicemodel.System{Hostname: "host-1.example.net"}); got != "host-1" {
		t.Errorf("DeviceName(fqdn) = %q, want the short name", got)
	}
}

// Every container endpoint is a port named after the container; a running
// one is up at 100G with the container's interface counters seen from the
// switch side; a stopped one keeps its slot and shows link down; free slots
// are empty ports; the 54 slots are all accounted for.
func TestContainersArePorts(t *testing.T) {
	c, _, snap := startFixture(t)
	if len(snap.Ports) != topSlot {
		t.Fatalf("%d ports, want %d", len(snap.Ports), topSlot)
	}
	running, ok := portByName(snap, "disunified-ups")
	if !ok {
		t.Fatal("no port for the running container disunified-ups")
	}
	if !running.Present || !running.Up || !running.Enabled || running.SpeedMbps != 100000 {
		t.Errorf("running container port = %+v", running)
	}
	if running.Counters.RxBytes == 0 || running.Counters.TxBytes == 0 {
		t.Errorf("counters not taken from the container's interface: %+v", running.Counters)
	}
	if got := c.PortName(running); got != "disunified-ups" {
		t.Errorf("PortName = %q", got)
	}
	stopped, ok := portByName(snap, "app-1")
	if !ok {
		t.Fatal("no port for the exited container app-1")
	}
	if !stopped.Present || stopped.Up || stopped.SpeedMbps != 0 {
		t.Errorf("stopped container port = present %v up %v speed %d; want present, down", stopped.Present, stopped.Up, stopped.SpeedMbps)
	}
	free := 0
	for _, p := range snap.Ports {
		if len(p.Interfaces) == 0 && !p.Present {
			free++
			if p.Enabled || p.Up {
				t.Errorf("free slot %d must be disabled and down", p.Index)
			}
			if c.PortName(p) != openLabel(p.Index) {
				t.Errorf("free slot %d named %q", p.Index, c.PortName(p))
			}
		}
	}
	if free == 0 {
		t.Error("no free slots left; the capture has far fewer than 48 endpoints")
	}
	// Creation order: the oldest container took slot 1.
	if first, _ := portByName(snap, "app-1"); first.Index != 1 {
		t.Errorf("oldest container at slot %d, want 1", first.Index)
	}
}

// Bridge-network endpoints carry no MAC entries: the LAN never sees those
// MACs, and reporting them would make the controller invent clients. Only
// macvlan/ipvlan endpoints (real LAN addresses) are clients on their port.
func TestOnlyLANEndpointsAreClients(t *testing.T) {
	_, _, snap := startFixture(t)
	if len(snap.MACTable) != 0 {
		t.Errorf("MAC table = %v; the capture has no macvlan container, so no client may be reported", snap.MACTable)
	}
	for _, p := range snap.Ports {
		if len(p.MACs) != 0 {
			t.Errorf("port %d (%s) carries MACs %v", p.Index, p.IfName, p.MACs)
		}
	}
	// A macvlan endpoint, decided by the network's driver.
	if !isLANNetwork(network{Driver: "macvlan"}) || isLANNetwork(network{Driver: "bridge"}) {
		t.Error("isLANNetwork: macvlan yes, bridge no")
	}
}

// Slots are assigned once and written to the host; a second poll writes
// nothing; a removed container frees its slot and a new endpoint takes the
// lowest free one.
func TestSlotsPersistOnTheHost(t *testing.T) {
	c, fr, snap := startFixture(t)
	if len(fr.Commands) != 1 || !strings.Contains(fr.Commands[0], "podman-slots.json") {
		t.Fatalf("first poll must write the slot file once, got %v", fr.Commands)
	}
	var written map[string]int
	if err := json.Unmarshal([]byte(fr.Stdins[0]), &written); err != nil {
		t.Fatal(err)
	}
	want, _ := portByName(snap, "disunified-ups")
	if written[endpointKey("disunified-ups", "disunified-ups_default")] != want.Index {
		t.Errorf("slot file = %v, port %d", written, want.Index)
	}
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fr.Commands) != 1 {
		t.Errorf("a steady poll must not rewrite the slot file: %v", fr.Commands)
	}
	// Remove the oldest container: its slot 1 is freed and everything else
	// keeps its place.
	fr.RemoveContainer("app-1")
	snap2, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, still := portByName(snap2, "app-1"); still {
		t.Error("removed container still has a port")
	}
	if p := snap2.Ports[0]; p.Present || p.Index != 1 {
		t.Errorf("slot 1 must be free after the removal: %+v", p)
	}
	if again, _ := portByName(snap2, "disunified-ups"); again.Index != want.Index {
		t.Errorf("disunified-ups moved from %d to %d", want.Index, again.Index)
	}
	if len(fr.Commands) != 2 {
		t.Errorf("the release must be written back: %v", fr.Commands)
	}
	// A fresh collector on the same host reads the file back.
	c2 := NewCollector(fr)
	c2.Log = c.Log
	snap3, err := c2.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := portByName(snap3, "disunified-ups"); p.Index != want.Index {
		t.Errorf("after a restart disunified-ups is at %d, want %d (from the slot file)", p.Index, want.Index)
	}
}

// Ports seen from the switch: the container's tx is the port's rx.
func TestEndpointCountersAreSwitchSide(t *testing.T) {
	var l ipLink
	l.Stats64.Rx.Bytes, l.Stats64.Tx.Bytes = 10, 20
	p := endpointPort(3, endpointInfo{container: container{Names: []string{"x"}}, link: l, hasLink: true, running: true})
	if p.Counters.RxBytes != 20 || p.Counters.TxBytes != 10 {
		t.Errorf("counters = rx %d tx %d, want rx 20 tx 10", p.Counters.RxBytes, p.Counters.TxBytes)
	}
	multi := endpointPort(4, endpointInfo{container: container{Names: []string{"x"}}, network: network{Name: "lan"}, multi: true})
	if multi.Name != "x lan" {
		t.Errorf("multi-endpoint name = %q", multi.Name)
	}
}

func TestOpenRequiresSSHTarget(t *testing.T) {
	for _, bad := range []string{"", "host", "@host", "root@"} {
		if _, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{SSH: bad}); err == nil {
			t.Errorf("Open(%q) accepted", bad)
		}
	}
	if _, err := (Driver{}).Open(context.Background(), devicemodel.DriverConfig{SSH: "root@192.0.2.9", Options: map[string]string{"slots_file": "relative.json"}}); err == nil {
		t.Error("a relative slots_file must be refused")
	}
}
