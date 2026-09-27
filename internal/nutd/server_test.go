package nutd

import (
	"bufio"
	"context"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

func snapshot() *devicemodel.Snapshot {
	return &devicemodel.Snapshot{
		System: devicemodel.System{
			Vendor: "APC", Model: "Smart-UPS 1500", Serial: "SSJ00000000", Version: "15.5",
			Battery: &devicemodel.Battery{
				Charging: true, ChargePct: 98, Runtime: 366 * time.Second, VoltageV: 54.2,
				LoadPct: 80.3, RealPowerW: 1084, ApparentPowerVA: 1120, RealPowerRatingW: 1350, ApparentRatingVA: 1440,
				OutputVoltageV: 113.4, OutputCurrentA: 9.9, OutputFrequencyHz: 60, HasOutput: true,
				TransferHighV: 127, TransferLowV: 106,
			},
		},
		Outlets: []devicemodel.Outlet{{Index: 1, On: true}, {Index: 2, On: true, Switchable: true}},
	}
}

// start runs a server on a free port with spec and returns a connected
// client's reader/writer.
func start(t *testing.T, src Source, spec unificfg.NUTServer) (*Server, *bufio.Reader, func(string)) {
	t.Helper()
	s := New(src, log.New(testWriter{t}, "", 0))
	s.Host = "127.0.0.1"
	spec.Port = freePort(t)
	if err := s.ApplyNUT(&spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	r := bufio.NewReader(c)
	send := func(line string) {
		if _, err := c.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	return s, r, send
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func line(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	s, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(s, "\n")
}

// readList reads a BEGIN LIST ... END LIST block and returns its lines.
func readList(t *testing.T, r *bufio.Reader) []string {
	t.Helper()
	first := line(t, r)
	if !strings.HasPrefix(first, "BEGIN LIST ") {
		t.Fatalf("expected BEGIN LIST, got %q", first)
	}
	var out []string
	for {
		l := line(t, r)
		if strings.HasPrefix(l, "END LIST ") {
			return out
		}
		out = append(out, l)
	}
}

// What upsc does: LIST UPS, then LIST VAR; what upsmon does: USERNAME,
// PASSWORD, LOGIN, then GET VAR ups.status on every poll.
func TestServesTheUPSVariablesTheWayUpscAndUpsmonAskForThem(t *testing.T) {
	s, r, send := start(t, snapshot, unificfg.NUTServer{Enabled: true, ID: "ups"})
	send("LIST UPS")
	if got := readList(t, r); len(got) != 1 || got[0] != `UPS ups "APC Smart-UPS 1500"` {
		t.Errorf("LIST UPS = %v", got)
	}
	send("LIST VAR ups")
	vars := map[string]string{}
	for _, l := range readList(t, r) {
		f := strings.SplitN(l, " ", 4)
		if len(f) != 4 || f[0] != "VAR" || f[1] != "ups" {
			t.Fatalf("bad VAR line %q", l)
		}
		vars[f[2]] = strings.Trim(f[3], `"`)
	}
	for k, want := range map[string]string{
		"ups.status": "OL CHRG", "battery.charge": "98", "battery.runtime": "366", "battery.voltage": "54.2",
		"ups.load": "80.3", "ups.realpower": "1084", "ups.realpower.nominal": "1350", "ups.power.nominal": "1440",
		"output.voltage": "113.4", "output.current": "9.90", "output.frequency": "60.0",
		"input.transfer.high": "127", "input.transfer.low": "106",
		"ups.model": "Smart-UPS 1500", "ups.serial": "SSJ00000000", "ups.firmware": "15.5", "device.type": "ups",
		"outlet.group.1.status": "on", "outlet.group.1.switchable": "no", "outlet.group.2.switchable": "yes",
	} {
		if vars[k] != want {
			t.Errorf("%s = %q, want %q", k, vars[k], want)
		}
	}
	for _, absent := range []string{"input.voltage", "battery.temperature", "ups.efficiency"} {
		if _, has := vars[absent]; has {
			t.Errorf("%s published although the device does not measure it", absent)
		}
	}
	send("USERNAME monuser")
	if got := line(t, r); got != "OK" {
		t.Errorf("USERNAME -> %q", got)
	}
	send("PASSWORD anything")
	if got := line(t, r); got != "OK" {
		t.Errorf("PASSWORD -> %q", got)
	}
	send("LOGIN ups")
	if got := line(t, r); got != "OK" {
		t.Errorf("LOGIN without a required credential -> %q, want OK", got)
	}
	send("GET VAR ups ups.status")
	if got := line(t, r); got != `VAR ups ups.status "OL CHRG"` {
		t.Errorf("GET VAR -> %q", got)
	}
	send("GET NUMLOGINS ups")
	if got := line(t, r); got != "NUMLOGINS ups 1" {
		t.Errorf("NUMLOGINS -> %q", got)
	}
	if c := s.Clients(); len(c) != 1 || c[0] != "127.0.0.1" {
		t.Errorf("Clients = %v, want the logged-in peer", c)
	}
	send("PRIMARY ups")
	if got := line(t, r); got != "OK PRIMARY-GRANTED" {
		t.Errorf("PRIMARY -> %q", got)
	}
	send("GET VAR ups no.such")
	if got := line(t, r); got != "ERR VAR-NOT-SUPPORTED" {
		t.Errorf("unknown var -> %q", got)
	}
	send("GET VAR other ups.status")
	if got := line(t, r); got != "ERR UNKNOWN-UPS" {
		t.Errorf("unknown ups -> %q", got)
	}
	send("FSD ups")
	if got := line(t, r); got != "ERR ACCESS-DENIED" {
		t.Errorf("FSD must be refused, got %q", got)
	}
	send("STARTTLS")
	if got := line(t, r); got != "ERR FEATURE-NOT-SUPPORTED" {
		t.Errorf("STARTTLS -> %q", got)
	}
	send("LOGOUT")
	if got := line(t, r); got != "OK Goodbye" {
		t.Errorf("LOGOUT -> %q", got)
	}
	time.Sleep(50 * time.Millisecond)
	if c := s.Clients(); len(c) != 0 {
		t.Errorf("Clients after LOGOUT = %v, want none", c)
	}
}

// With the controller's credential switch on, LOGIN needs the pushed
// username and password; a password with a space arrives quoted.
func TestLoginCredentialIsEnforcedWhenRequired(t *testing.T) {
	_, r, send := start(t, snapshot, unificfg.NUTServer{Enabled: true, ID: "ups", CredentialRequired: true, Username: "monuser", Password: "pass word"})
	send("LOGIN ups")
	if got := line(t, r); got != "ERR ACCESS-DENIED" {
		t.Errorf("LOGIN without credentials -> %q", got)
	}
	send("USERNAME monuser")
	line(t, r)
	send("PASSWORD wrong")
	line(t, r)
	send("LOGIN ups")
	if got := line(t, r); got != "ERR ACCESS-DENIED" {
		t.Errorf("LOGIN with the wrong password -> %q", got)
	}
	send(`PASSWORD "pass word"`)
	line(t, r)
	send("LOGIN ups")
	if got := line(t, r); got != "OK" {
		t.Errorf("LOGIN with the right credentials -> %q", got)
	}
}

// ups.status spells what upsmon reacts to.
func TestStatusFlags(t *testing.T) {
	b := &devicemodel.Battery{OnBattery: true, LowBattery: true, Overload: true}
	if got := Status(b); got != "OB LB OVER" {
		t.Errorf("Status = %q", got)
	}
	if got := Status(&devicemodel.Battery{}); got != "OL" {
		t.Errorf("Status(online) = %q", got)
	}
}

// The controller's push drives the listener: the same spec is a no-op, a
// changed one restarts on the new port, an absent block stops it.
func TestApplyStartsRestartsAndStops(t *testing.T) {
	s := New(snapshot, log.New(testWriter{t}, "", 0))
	s.Host = "127.0.0.1"
	spec := unificfg.NUTServer{Enabled: true, ID: "ups", Port: freePort(t)}
	if err := s.ApplyNUT(&spec); err != nil {
		t.Fatal(err)
	}
	first := s.Addr()
	if err := s.ApplyNUT(&spec); err != nil || s.Addr() != first {
		t.Errorf("same spec must be a no-op: %v, addr %s -> %s", err, first, s.Addr())
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("ping: %v", err)
	}
	spec.Port = freePort(t)
	if err := s.ApplyNUT(&spec); err != nil {
		t.Fatal(err)
	}
	if s.Addr() == first {
		t.Error("a changed port must restart the listener")
	}
	if err := s.ApplyNUT(nil); err != nil {
		t.Fatal(err)
	}
	if s.Addr() != "" {
		t.Error("an absent block must stop the listener")
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Error("ping must fail once stopped")
	}
	off := unificfg.NUTServer{Enabled: false}
	if err := s.ApplyNUT(&off); err != nil || s.Addr() != "" {
		t.Errorf("disabled block: %v, addr %q", err, s.Addr())
	}
}

// Before the first snapshot the server answers rather than crashes.
func TestNoSnapshotYet(t *testing.T) {
	_, r, send := start(t, func() *devicemodel.Snapshot { return nil }, unificfg.NUTServer{Enabled: true, ID: "ups"})
	send("GET VAR ups ups.status")
	if got := line(t, r); got != `VAR ups ups.status "WAIT"` {
		t.Errorf("GET VAR before a snapshot -> %q", got)
	}
}

func TestFieldsHonoursQuotes(t *testing.T) {
	got := fields(`PASSWORD "pass word" x`)
	if len(got) != 3 || got[1] != "pass word" {
		t.Errorf("fields = %q", got)
	}
}
