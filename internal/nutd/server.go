// Package nutd is a Network UPS Tools server (the upsd side of the NUT
// network protocol, RFC 9271) that serves one UPS from the bridge's own
// snapshot, so hosts that speak NUT -- a NAS, a hypervisor, upsmon -- can
// watch the UPS the bridge already reads and shut themselves down on its
// status. It is what the UniFi controller's "NUT Server" switch means: a
// real UniFi UPS runs upsd on itself; here the bridge runs it, configured
// by the same nutserver block the controller pushes (name, port, optional
// login).
//
// Read-only on purpose: no SET, no INSTCMD, no FSD. Clients that need to
// shut the UPS down do that through the controller (outlet control), not
// through this port.
package nutd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TechBlueprints/disunified/internal/devicemodel"
	"github.com/TechBlueprints/disunified/internal/unificfg"
)

// Source returns the latest snapshot; nil while none has been collected.
type Source func() *devicemodel.Snapshot

// Server is one NUT server. ApplyNUT starts, reconfigures or stops the
// listener to match the controller's push; the loop calls it on every push.
type Server struct {
	src    Source
	logger *log.Logger
	// Host is the address the listener binds; "" = all interfaces. The
	// port comes from the controller.
	Host string

	mu      sync.Mutex
	spec    unificfg.NUTServer // what is running; zero = stopped
	ln      net.Listener
	clients map[string]int // logged-in client address -> sessions
}

// New builds a stopped server over src.
func New(src Source, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{src: src, logger: logger, clients: map[string]int{}}
}

// ApplyNUT makes the running state match spec: nil or disabled stops the
// listener; a changed name, port or login restarts it; the same spec is a
// no-op. Errors come from binding the port.
func (s *Server) ApplyNUT(spec *unificfg.NUTServer) error {
	want := unificfg.NUTServer{}
	if spec != nil && spec.Enabled {
		want = *spec
		if want.Port <= 0 {
			want.Port = 3493
		}
		if want.ID == "" {
			want.ID = "ups"
		}
		if !want.CredentialRequired {
			want.Username, want.Password = "", ""
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if want == s.spec {
		return nil
	}
	if s.ln != nil {
		_ = s.ln.Close()
		s.ln = nil
		s.logger.Printf("NUT server: stopped (%s on port %d)", s.spec.ID, s.spec.Port)
		s.spec = unificfg.NUTServer{}
	}
	if !want.Enabled {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(s.Host, strconv.Itoa(want.Port)))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", want.Port, err)
	}
	s.ln, s.spec = ln, want
	login := "no login"
	if want.CredentialRequired {
		login = "login required"
	}
	s.logger.Printf("NUT server: serving %q on port %d (%s); clients address it as %s@<this host>", want.ID, want.Port, login, want.ID)
	go s.serve(ln, want)
	return nil
}

// Close stops the listener.
func (s *Server) Close() error { return s.ApplyNUT(nil) }

// Addr is the listener's address while running ("" when stopped): tests
// bind port 0 and read it back.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Clients lists the addresses of clients that have logged in (LOGIN),
// sorted; what the inform reports as nut_client_ips.
func (s *Server) Clients() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.clients))
	for ip := range s.clients {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

func (s *Server) serve(ln net.Listener, spec unificfg.NUTServer) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return // closed
		}
		go s.session(c, spec)
	}
}

// session runs one client connection: one command per line, one reply
// per command, until LOGOUT or the peer closes.
func (s *Server) session(c net.Conn, spec unificfg.NUTServer) {
	defer c.Close()
	ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	var user, pass string
	loggedIn := false
	defer func() {
		if loggedIn {
			s.mu.Lock()
			if s.clients[ip]--; s.clients[ip] <= 0 {
				delete(s.clients, ip)
			}
			s.mu.Unlock()
		}
	}()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Minute)) // upsmon polls every few seconds; an idle peer is gone
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		args := fields(strings.TrimRight(line, "\r\n"))
		if len(args) == 0 {
			continue
		}
		cmd := strings.ToUpper(args[0])
		reply := ""
		switch cmd {
		case "LOGOUT":
			_, _ = w.WriteString("OK Goodbye\n")
			_ = w.Flush()
			return
		case "USERNAME":
			if len(args) == 2 {
				user, reply = args[1], "OK\n"
			} else {
				reply = "ERR INVALID-ARGUMENT\n"
			}
		case "PASSWORD":
			if len(args) == 2 {
				pass, reply = args[1], "OK\n"
			} else {
				reply = "ERR INVALID-ARGUMENT\n"
			}
		case "LOGIN":
			switch {
			case len(args) != 2:
				reply = "ERR INVALID-ARGUMENT\n"
			case args[1] != spec.ID:
				reply = "ERR UNKNOWN-UPS\n"
			case spec.CredentialRequired && (user != spec.Username || pass != spec.Password):
				reply = "ERR ACCESS-DENIED\n"
			default:
				if !loggedIn {
					loggedIn = true
					s.mu.Lock()
					s.clients[ip]++
					s.mu.Unlock()
				}
				reply = "OK\n"
			}
		case "PRIMARY", "MASTER":
			// upsmon's primary asks for this before it will run FSD; there
			// is no FSD here, but a primary that is refused logs an error
			// on every poll, so it is granted to a logged-in client.
			switch {
			case len(args) != 2 || args[1] != spec.ID:
				reply = "ERR UNKNOWN-UPS\n"
			case !loggedIn:
				reply = "ERR USERNAME-REQUIRED\n"
			default:
				reply = "OK " + cmd + "-GRANTED\n"
			}
		case "VER":
			reply = "disunified NUT server (upsd protocol 1.3, read-only)\n"
		case "NETVER", "PROTVER":
			reply = "1.3\n"
		case "HELP":
			reply = "Commands: HELP VER NETVER LIST GET USERNAME PASSWORD LOGIN LOGOUT PRIMARY MASTER\n"
		case "STARTTLS":
			reply = "ERR FEATURE-NOT-SUPPORTED\n"
		case "FSD", "SET", "INSTCMD":
			reply = "ERR ACCESS-DENIED\n"
		case "LIST":
			reply = s.list(args[1:], spec)
		case "GET":
			reply = s.get(args[1:], spec)
		default:
			reply = "ERR UNKNOWN-COMMAND\n"
		}
		if _, err := w.WriteString(reply); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

func (s *Server) list(args []string, spec unificfg.NUTServer) string {
	if len(args) == 0 {
		return "ERR INVALID-ARGUMENT\n"
	}
	what := strings.ToUpper(args[0])
	if what == "UPS" {
		return "BEGIN LIST UPS\nUPS " + spec.ID + " " + quote(s.desc()) + "\nEND LIST UPS\n"
	}
	if len(args) < 2 {
		return "ERR INVALID-ARGUMENT\n"
	}
	if args[1] != spec.ID {
		return "ERR UNKNOWN-UPS\n"
	}
	var b strings.Builder
	switch what {
	case "VAR":
		vars := s.vars()
		fmt.Fprintf(&b, "BEGIN LIST VAR %s\n", spec.ID)
		for _, kv := range vars {
			fmt.Fprintf(&b, "VAR %s %s %s\n", spec.ID, kv[0], quote(kv[1]))
		}
		fmt.Fprintf(&b, "END LIST VAR %s\n", spec.ID)
	case "RW", "CMD", "ENUM", "RANGE":
		fmt.Fprintf(&b, "BEGIN LIST %s %s\nEND LIST %s %s\n", what, spec.ID, what, spec.ID)
	case "CLIENT":
		fmt.Fprintf(&b, "BEGIN LIST CLIENT %s\n", spec.ID)
		for _, ip := range s.Clients() {
			fmt.Fprintf(&b, "CLIENT %s %s\n", spec.ID, ip)
		}
		fmt.Fprintf(&b, "END LIST CLIENT %s\n", spec.ID)
	default:
		return "ERR INVALID-ARGUMENT\n"
	}
	return b.String()
}

func (s *Server) get(args []string, spec unificfg.NUTServer) string {
	if len(args) < 2 {
		return "ERR INVALID-ARGUMENT\n"
	}
	what := strings.ToUpper(args[0])
	if args[1] != spec.ID {
		return "ERR UNKNOWN-UPS\n"
	}
	switch what {
	case "NUMLOGINS":
		n := 0
		s.mu.Lock()
		for _, c := range s.clients {
			n += c
		}
		s.mu.Unlock()
		return fmt.Sprintf("NUMLOGINS %s %d\n", spec.ID, n)
	case "UPSDESC":
		return fmt.Sprintf("UPSDESC %s %s\n", spec.ID, quote(s.desc()))
	case "VAR", "TYPE", "DESC":
		if len(args) != 3 {
			return "ERR INVALID-ARGUMENT\n"
		}
		for _, kv := range s.vars() {
			if kv[0] == args[2] {
				switch what {
				case "VAR":
					return fmt.Sprintf("VAR %s %s %s\n", spec.ID, kv[0], quote(kv[1]))
				case "TYPE":
					return fmt.Sprintf("TYPE %s %s STRING:%d\n", spec.ID, kv[0], len(kv[1])+1)
				default:
					return fmt.Sprintf("DESC %s %s %s\n", spec.ID, kv[0], quote(kv[0]))
				}
			}
		}
		return "ERR VAR-NOT-SUPPORTED\n"
	}
	return "ERR INVALID-ARGUMENT\n"
}

// desc is the UPS description LIST UPS carries: vendor and model.
func (s *Server) desc() string {
	snap := s.src()
	if snap == nil {
		return "UPS"
	}
	return strings.TrimSpace(snap.System.Vendor + " " + snap.System.Model)
}

// vars is the variable list in NUT's names, from the snapshot; ordered.
// Only what the device measures is published: a missing input voltage is
// absent, not zero, the same rule the inform follows.
func (s *Server) vars() [][2]string {
	snap := s.src()
	if snap == nil || snap.System.Battery == nil {
		return [][2]string{{"ups.status", "WAIT"}, {"driver.name", "disunified"}}
	}
	b := snap.System.Battery
	var out [][2]string
	add := func(k, v string) { out = append(out, [2]string{k, v}) }
	num := func(f float64, prec int) string { return strconv.FormatFloat(f, 'f', prec, 64) }

	add("device.mfr", snap.System.Vendor)
	add("device.model", snap.System.Model)
	add("device.serial", snap.System.Serial)
	add("device.type", "ups")
	add("driver.name", "disunified")
	add("driver.version", "nutd")
	add("ups.mfr", snap.System.Vendor)
	add("ups.model", snap.System.Model)
	add("ups.serial", snap.System.Serial)
	add("ups.firmware", snap.System.Version)
	add("ups.status", Status(b))
	if b.TransferCause != "" {
		add("ups.status.cause", b.TransferCause) // not a NUT name; kept for humans reading upsc
	}
	add("ups.load", num(b.LoadPct, 1))
	add("ups.realpower", num(b.RealPowerW, 0))
	if b.RealPowerRatingW > 0 {
		add("ups.realpower.nominal", num(b.RealPowerRatingW, 0))
	}
	add("ups.power", num(b.ApparentPowerVA, 0))
	if b.ApparentRatingVA > 0 {
		add("ups.power.nominal", num(b.ApparentRatingVA, 0))
	}
	if b.HasEfficiency {
		add("ups.efficiency", num(b.EfficiencyPct, 1))
	}
	if b.ShutdownDelay > 0 {
		add("ups.delay.shutdown", strconv.Itoa(int(b.ShutdownDelay.Seconds())))
	}
	add("battery.charge", strconv.Itoa(b.ChargePct))
	add("battery.runtime", strconv.Itoa(int(b.Runtime.Seconds())))
	add("battery.voltage", num(b.VoltageV, 1))
	if b.HasTemperature {
		add("battery.temperature", num(b.TemperatureC, 1))
	}
	if b.HasInput {
		add("input.voltage", num(b.InputVoltageV, 1))
	}
	if b.TransferHighV > 0 {
		add("input.transfer.high", strconv.Itoa(b.TransferHighV))
	}
	if b.TransferLowV > 0 {
		add("input.transfer.low", strconv.Itoa(b.TransferLowV))
	}
	if b.HasOutput {
		add("output.voltage", num(b.OutputVoltageV, 1))
		add("output.current", num(b.OutputCurrentA, 2))
		add("output.frequency", num(b.OutputFrequencyHz, 1))
	}
	for i, o := range snap.Outlets {
		state := "off"
		if o.On {
			state = "on"
		}
		prefix := fmt.Sprintf("outlet.group.%d.", i+1)
		add(prefix+"id", strconv.Itoa(o.Index))
		add(prefix+"status", state)
		if o.Name != "" {
			add(prefix+"name", o.Name)
		}
		if o.Switchable {
			add(prefix+"switchable", "yes")
		} else {
			add(prefix+"switchable", "no")
		}
	}
	return out
}

// Status is ups.status the way NUT spells it: OL/OB first, then the
// modifiers a client such as upsmon reacts to (LB, CHRG, BYPASS, OFF,
// OVER, TRIM, BOOST, CAL, TEST), space-separated.
func Status(b *devicemodel.Battery) string {
	var f []string
	if b.OnBattery {
		f = append(f, "OB")
	} else {
		f = append(f, "OL")
	}
	if b.LowBattery {
		f = append(f, "LB")
	}
	if b.Charging {
		f = append(f, "CHRG")
	}
	if b.Bypass {
		f = append(f, "BYPASS")
	}
	if b.OutputOff {
		f = append(f, "OFF")
	}
	if b.Overload {
		f = append(f, "OVER")
	}
	if b.Trim {
		f = append(f, "TRIM")
	}
	if b.Boost {
		f = append(f, "BOOST")
	}
	if b.Calibrating {
		f = append(f, "CAL")
	}
	if b.Testing {
		f = append(f, "TEST")
	}
	if b.Fault {
		f = append(f, "ALARM")
	}
	return strings.Join(f, " ")
}

// quote wraps a value the way upsd does: double quotes, inner quotes and
// backslashes escaped.
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// fields splits a command line on spaces, honouring double quotes (a
// PASSWORD may contain a space).
func fields(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '\\' && inQuote && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			have = true
		case ch == '"':
			inQuote = !inQuote
			have = true
		case ch == ' ' && !inQuote:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(ch)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// errStopped is returned by helpers when the server is not running.
var errStopped = errors.New("nut server is not running")

// Ping dials the running listener and issues VER; a health check for tests
// and for the operator.
func (s *Server) Ping(ctx context.Context) error {
	addr := s.Addr()
	if addr == "" {
		return errStopped
	}
	d := net.Dialer{}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := fmt.Fprint(c, "VER\n"); err != nil {
		return err
	}
	_, err = bufio.NewReader(c).ReadString('\n')
	return err
}
