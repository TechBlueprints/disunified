package aristaeos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
)

// The bridge on the switch itself (options.transport: local,
// docs/drivers/arista-on-switch.md): eAPI over the unix socket, and a probe
// in place of a fresh dial.

// socketServer answers eAPI on a unix socket from the captured replies of
// the fixture transport, in the envelope the switch's socket returned live
// (2026-11-18: the HTTPS server's, `{"jsonrpc","id","result"}`).
func socketServer(t *testing.T) (socket string, authSeen *[]string) {
	t.Helper()
	// A short path: a unix socket's is limited to about 100 bytes.
	dir, err := os.MkdirTemp("", "dui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket = filepath.Join(dir, "command-api.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ft := NewFixtureTransport(filepath.Join("..", "..", "..", "docs", "fixtures", "arista-eos-4.26.14M"))
	var seen []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		var req rpcRequest
		if err := json.Unmarshal(body, &req); err != nil || r.URL.Path != "/command-api" || req.Method != "runCmds" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		res, err := ft.Run(r.Context(), req.Params.Cmds)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return socket, &seen
}

// The whole start-up read set runs over the socket, with no credentials
// sent.
func TestLocalTransportStartsOverTheUnixSocket(t *testing.T) {
	socket, auth := socketServer(t)
	old := sourceAddr
	sourceAddr = func(addr string) (string, error) {
		if addr != "192.0.2.2:8080" {
			return "", fmt.Errorf("unexpected probe %q", addr)
		}
		return fixtureHost, nil
	}
	defer func() { sourceAddr = old }()
	dev, err := Driver{}.Open(context.Background(), devicemodel.DriverConfig{Options: map[string]string{
		"transport": "local", "socket": socket, "probe": "192.0.2.2:8080",
	}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dev.Close()
	snap, err := dev.Start(context.Background())
	if err != nil {
		t.Fatalf("start over the socket: %v", err)
	}
	if len(snap.Ports) == 0 {
		t.Error("no ports read over the socket")
	}
	c := dev.(*Collector)
	if !c.local || c.CurrentAddress() != fixtureHost {
		t.Errorf("local=%v address=%q, want the address the switch reaches the probe from (%s)", c.local, c.CurrentAddress(), fixtureHost)
	}
	if iface, _ := managementInterface(snap, c.host); iface == "" {
		t.Error("no management interface found for the switch's own address")
	}
	if len(*auth) == 0 {
		t.Fatal("the socket saw no request")
	}
	for _, a := range *auth {
		if a != "" {
			t.Fatalf("credentials sent over the socket: %q", a)
		}
	}
	if c.dialAddrs() != nil {
		t.Error("an on-switch bridge dials no address an IP rule could cut")
	}
}

func TestLocalTransportOptions(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  devicemodel.DriverConfig
		want string
	}{
		"url with local":    {devicemodel.DriverConfig{URL: "https://192.0.2.1/command-api", Options: map[string]string{"transport": "local"}}, "remove url"},
		"no address":        {devicemodel.DriverConfig{Options: map[string]string{"transport": "local"}}, "own address is unknown"},
		"bad address":       {devicemodel.DriverConfig{Options: map[string]string{"transport": "local", "address": "switch"}}, "not an IP address"},
		"unknown transport": {devicemodel.DriverConfig{Options: map[string]string{"transport": "telnet"}}, "want local"},
	} {
		if _, err := (Driver{}).Open(context.Background(), tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	// A missing socket says what to set on the switch.
	dev, err := Driver{}.Open(context.Background(), devicemodel.DriverConfig{Options: map[string]string{
		"transport": "local", "socket": filepath.Join(t.TempDir(), "absent.sock"), "address": fixtureHost,
	}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := dev.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "protocol unix-socket") {
		t.Errorf("start without a socket: %v", err)
	}
}

func localCollector(t *testing.T, probe func(ctx context.Context, addr, from string) error) (*Collector, *FixtureTransport) {
	t.Helper()
	c, ft := addressCollector(t, nil)
	c.local, c.probeAddr, c.probe = true, "192.0.2.2:8080", probe
	return c, ft
}

// On the switch an address move is proven by reaching the controller from
// the new address; the confirm goes over the same socket.
func TestLocalAddressMoveIsConfirmedByTheProbe(t *testing.T) {
	var probed []string
	c, ft := localCollector(t, func(_ context.Context, addr, from string) error {
		probed = append(probed, addr+" from "+from)
		return nil
	})
	changed, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.40", PrefixLen: 16, Gateway: "192.0.2.2"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(probed) != 1 || probed[0] != "192.0.2.2:8080 from 192.0.2.40" {
		t.Errorf("probes = %v, want one from the new address", probed)
	}
	n := len(ft.Configured)
	if n < 2 || !strings.Contains(strings.Join(ft.Configured[n-2], " / "), "commit timer 00:02:00") ||
		strings.Join(ft.Configured[n-1], " / ") != "enable / configure session dui-address-t commit / write memory" {
		t.Errorf("batches = %v, want the timed session then its confirm", ft.Configured)
	}
	if c.CurrentAddress() != "192.0.2.40" {
		t.Errorf("address = %q after the move", c.CurrentAddress())
	}
}

// A probe that fails confirms nothing: the switch's timer reverts.
func TestLocalAddressMoveIsLeftToRevertWhenTheProbeFails(t *testing.T) {
	c, ft := localCollector(t, func(context.Context, string, string) error { return errors.New("no route to host") })
	changed, err := c.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.40", PrefixLen: 16})
	if err == nil || changed || !strings.Contains(err.Error(), "revert") {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if last := strings.Join(ft.Configured[len(ft.Configured)-1], " / "); !strings.Contains(last, "commit timer") {
		t.Errorf("last batch = %q, want the unconfirmed session", last)
	}
	if c.CurrentAddress() != fixtureHost {
		t.Errorf("address = %q after a failed move", c.CurrentAddress())
	}
}

// The probe is a real TCP connect, bound to the given source.
func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tcpProbe(ctx, ln.Addr().String(), "127.0.0.1"); err != nil {
		t.Errorf("probe from loopback: %v", err)
	}
	if err := tcpProbe(ctx, ln.Addr().String(), "192.0.2.77"); err == nil {
		t.Error("a probe bound to an address this host does not have succeeded")
	}
}
