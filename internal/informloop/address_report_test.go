package informloop

import (
	"context"
	"encoding/json"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/device"
	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/drivers/sonic"
	"github.com/jamesbraid/unifi-emu/inform"
)

// movingAddress applies every static address it is given.
type movingAddress struct{ applied []devicemodel.AddressDesired }

func (m *movingAddress) ApplyAddress(_ context.Context, d devicemodel.AddressDesired) (bool, error) {
	m.applied = append(m.applied, d)
	return true, nil
}

func reported(t *testing.T, sess *device.Session) (ip, connect string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(sess.BuildPayload(time.Now()), &m); err != nil {
		t.Fatal(err)
	}
	ip, _ = m["ip"].(string)
	connect, _ = m["connect_request_ip"].(string)
	return ip, connect
}

// After the controller's IP Settings are applied, the informs report the
// new address (live: they kept the old one, so the controller's device
// list showed a stale address indefinitely).
func TestAppliedAddressIsReported(t *testing.T) {
	static := readFixture(t, "system_cfg-pdu-static-ip.txt") // 192.0.2.1/16 via 192.0.2.2
	desc := inform.Descriptor{MAC: "02:00:00:00:00:01", IP: "192.0.2.50"}
	st := device.State{Adopted: true, Key: "0123456789abcdef0123456789abcdef", PendingCfgVersion: "v1", PendingSystemCfg: static}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", st, nil, time.Now())
	var buf strings.Builder
	ac := &movingAddress{}
	l, err := New(desc, sess, Config{Logger: log.New(&buf, "", 0), AddressController: ac})
	if err != nil {
		t.Fatal(err)
	}
	if ip, _ := reported(t, sess); ip != "192.0.2.50" {
		t.Fatalf("before: ip %q", ip)
	}
	l.applyPending(context.Background())
	if len(ac.applied) != 1 {
		t.Fatalf("ApplyAddress called %d times", len(ac.applied))
	}
	if ip, connect := reported(t, sess); ip != "192.0.2.1" || connect != "192.0.2.1" {
		t.Errorf("after the move the inform reports ip %q connect_request_ip %q, want 192.0.2.1", ip, connect)
	}
	if !strings.Contains(buf.String(), "reporting the device's address as 192.0.2.1 (was 192.0.2.50)") {
		t.Errorf("log = %q", buf.String())
	}
}

// A driver that follows the device's address (sonic, after a move or a
// fallback to another known address) is consulted after every collect.
func TestDriverReportedAddressIsFollowed(t *testing.T) {
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238524-ff3c0c9f6", "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var cur string
	for _, a := range snap.System.Addresses {
		if a.Iface == "Vlan4094" {
			cur = a.IP
		}
	}
	coll.Dial, coll.Redial = cur, fr.Dial
	desc := inform.Descriptor{MAC: snap.System.MAC, IP: cur}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true}, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll})
	if err != nil {
		t.Fatal(err)
	}
	l.collect(context.Background())
	if ip, _ := reported(t, sess); ip != cur {
		t.Fatalf("before the move: ip %q, want %q", ip, cur)
	}
	// The address moves outside the loop's own apply path.
	if changed, err := coll.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.200", PrefixLen: 16}); err != nil || !changed {
		t.Fatalf("ApplyAddress = %v, %v", changed, err)
	}
	l.collect(context.Background())
	if ip, connect := reported(t, sess); ip != "192.0.2.200" || connect != "192.0.2.200" {
		t.Errorf("after the move: ip %q connect_request_ip %q, want 192.0.2.200", ip, connect)
	}
	if n := strings.Count(buf.String(), "reporting the device's address as 192.0.2.200"); n != 1 {
		t.Errorf("logged %d times, want once: %q", n, buf.String())
	}
	l.collect(context.Background())
	if n := strings.Count(buf.String(), "reporting the device's address"); n != 1 {
		t.Errorf("an unchanged address must not be logged again (%d)", n)
	}
}

// An address set explicitly (`ip:`/-ip) is not overridden by the driver's
// unchanged target on every collect; only a move the driver made is
// followed.
func TestExplicitAddressIsKeptUntilTheDriverMoves(t *testing.T) {
	fr, err := sonic.NewFixtureRunner(filepath.Join("..", "..", "docs", "fixtures", "sonic-202505.1238524-ff3c0c9f6", "collect.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	coll := sonic.NewCollector(fr)
	coll.Log = logger
	snap, err := coll.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.System.Addresses {
		if a.Iface == "Vlan4094" {
			coll.Dial = a.IP
		}
	}
	coll.Redial = fr.Dial
	const explicit = "192.0.2.77"
	desc := inform.Descriptor{MAC: snap.System.MAC, IP: explicit}
	sess := device.NewSession(desc, "http://192.0.2.1:8080/inform", device.State{Adopted: true}, nil, time.Now())
	l, err := New(desc, sess, Config{Logger: logger, Collector: coll})
	if err != nil {
		t.Fatal(err)
	}
	l.collect(context.Background())
	l.collect(context.Background())
	if ip, _ := reported(t, sess); ip != explicit {
		t.Errorf("the explicit address was overridden: ip %q", ip)
	}
	if _, err := coll.ApplyAddress(context.Background(), devicemodel.AddressDesired{IP: "192.0.2.200", PrefixLen: 16}); err != nil {
		t.Fatal(err)
	}
	l.collect(context.Background())
	if ip, _ := reported(t, sess); ip != "192.0.2.200" {
		t.Errorf("after the driver moved: ip %q, want 192.0.2.200", ip)
	}
}
