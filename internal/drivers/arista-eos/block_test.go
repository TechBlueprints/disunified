package aristaeos

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// UniFi's client Block on the 7160 (block.go, docs/drivers/arista-eos.md
// §9.9): the blocked clients the Arista bridge's own setparam carried while
// a test client was blocked beside the site's one
// (controller-11.0.99/replies-arista-block.ndjson), written as static MAC
// drop entries in the form the switch showed one
// (arista-eos-4.26.14M/mac-drop/).

func blockedFromArista(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(aclFixtureDir, "..", "controller-11.0.99", "replies-arista-block.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var rec struct {
			Payload struct {
				BlockedSTA *string `json:"blocked_sta"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Payload.BlockedSTA != nil {
			if macs := device.ParseBlockedSTA(*rec.Payload.BlockedSTA); len(macs) == 2 {
				return macs
			}
		}
	}
	t.Fatal("capture changed: no setparam with two blocked clients")
	return nil
}

func TestMACDropReadBack(t *testing.T) {
	var rc showRunningConfig
	b, err := os.ReadFile(filepath.Join(aclFixtureDir, "mac-drop", "show-running-config-mac-drop.json"))
	if err != nil || json.Unmarshal(b, &rc) != nil {
		t.Fatal(err)
	}
	hw, _ := net.ParseMAC("02:00:00:00:00:01")
	if got := macDropsFrom(rc); !reflect.DeepEqual(got, map[string]bool{macDropLine(hw, 1): true}) {
		t.Errorf("drop entries %v, want the line the driver writes", got)
	}
	// The entry in the MAC table is on no port: not a client.
	var mt showMACAddressTable
	b, err = os.ReadFile(filepath.Join(aclFixtureDir, "mac-drop", "show-mac-address-table-address-mac-drop.json"))
	if err != nil || json.Unmarshal(b, &mt) != nil || len(mt.UnicastTable.Entries) != 1 {
		t.Fatal(err)
	}
	if got := macTable(mt, parseEthName); len(got) != 0 {
		t.Errorf("a drop entry reported as a client: %+v", got)
	}
}

func TestBlockedClientsAsDropEntries(t *testing.T) {
	c, ft := aclCollector(t, nil)
	c.mu.Lock()
	vlans := c.last.VLANs
	c.mu.Unlock()
	if len(vlans) == 0 {
		t.Fatal("capture changed: no VLANs")
	}
	macs := blockedFromArista(t)
	d := devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: macs}
	before := len(ft.Configured)
	n, err := c.ApplyDevice(context.Background(), d)
	if err != nil || n == 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := ft.Configured[before:]
	if len(got) != 1 {
		t.Fatalf("batches %q, want one", got)
	}
	var lines []string
	for _, m := range macs {
		hw, _ := net.ParseMAC(m)
		for _, v := range vlans {
			lines = append(lines, macDropLine(hw, v))
		}
	}
	want := append(append([]string{"enable", "configure"}, sortedCopy(lines)...), "end", "write memory")
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("batch\n  got  %q\n  want %q", got[0], want)
	}
	if again, err := c.ApplyDevice(context.Background(), d); again != 0 || err != nil {
		t.Errorf("second apply: n=%d err=%v", again, err)
	}
	// The next collect reads the entries back from the running config.
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again, err := c.ApplyDevice(context.Background(), d); again != 0 || err != nil {
		t.Errorf("after a collect: n=%d err=%v", again, err)
	}
	if len(ft.Unmodelled) != 0 {
		t.Errorf("unmodelled writes: %q", ft.Unmodelled)
	}

	// The test client unblocked: its entries go, the other client's stay.
	before = len(ft.Configured)
	if _, err := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: macs[1:]}); err != nil {
		t.Fatal(err)
	}
	got = ft.Configured[before:]
	if len(got) != 1 {
		t.Fatalf("unblock batches %q", got)
	}
	hw, _ := net.ParseMAC(macs[0])
	for _, l := range got[0][2 : len(got[0])-2] {
		if !strings.HasPrefix(l, "no mac address-table static "+eosMAC(hw)+" vlan ") {
			t.Errorf("unblock wrote %q", l)
		}
	}
	if len(got[0]) != 4+len(vlans) {
		t.Errorf("unblock batch %q, want one removal per VLAN", got[0])
	}
	// Not yet said (a restart before the first setparam): left alone.
	before = len(ft.Configured)
	if n, err := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{}); n != 0 || err != nil || len(ft.Configured) != before {
		t.Errorf("an unknown list wrote: n=%d err=%v", n, err)
	}
}

// The switch's own MAC and its gateway's are held.
func TestBlockedGatewayHeldOnTheArista(t *testing.T) {
	c, ft := aclCollector(t, nil)
	c.mu.Lock()
	self := c.last.System.MAC
	c.mu.Unlock()
	before := len(ft.Configured)
	if n, err := c.ApplyDevice(context.Background(), devicemodel.DeviceDesired{BlockedSet: true, BlockedClients: []string{self}}); n != 0 || err != nil || len(ft.Configured) != before {
		t.Errorf("blocking the switch itself wrote: n=%d err=%v", n, err)
	}
}

func sortedCopy(xs []string) []string {
	out := append([]string(nil), xs...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
