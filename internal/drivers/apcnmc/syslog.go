// Package apcnmc is what the APC drivers share about the management card
// itself rather than the device behind it: the card's config.ini, which every
// NMC on site (AOS 3.9.2 on the rack PDU, AOS 6.0.1 on the Back-UPS) takes
// over FTP as a partial file -- a bare section and the keys to change --
// and applies live, a little after the transfer. It is not a driver and
// registers nothing; apc-pdu and apc-backups use it.
package apcnmc

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// syslogSection is the section that holds the card's syslog servers. AOS
// 3.9.2 spells it [NetworkSYSLOG]; it is matched without regard to case and
// written back the way the card spelled it.
const syslogSection = "NetworkSYSLOG"

// Syslog is a card's syslog section as read from its config.ini: whether it
// sends (Syslog=enabled), and the numbered server slots (Server1..4 with
// Port1..4 on AOS 3.9.2). The facility and the severity mapping are kept
// only to be logged; they are never written.
type Syslog struct {
	Section string      // the name as the card spells it
	Keys    [][2]string // every key and value, in file order
}

// ParseSyslog finds the syslog section in a whole config.ini.
func ParseSyslog(cfg []byte) (Syslog, bool) {
	name, keys, ok := section(cfg, syslogSection)
	return Syslog{Section: name, Keys: keys}, ok
}

func (s Syslog) get(k string) (string, bool) {
	for _, kv := range s.Keys {
		if kv[0] == k {
			return kv[1], true
		}
	}
	return "", false
}

// slots is the number of numbered server slots the card has.
func (s Syslog) slots() int {
	n := 0
	for {
		if _, ok := s.get(fmt.Sprintf("Server%d", n+1)); !ok {
			return n
		}
		n++
	}
}

// Hosts is where the card sends, in slot order and in the controller's form
// ("ip", or "ip:port" when the port is not 514); nil when syslog is off or
// every slot is empty (0.0.0.0).
func (s Syslog) Hosts() []string {
	if v, _ := s.get("Syslog"); !strings.EqualFold(v, "enabled") {
		return nil
	}
	var out []string
	for n := 1; n <= s.slots(); n++ {
		ip, _ := s.get(fmt.Sprintf("Server%d", n))
		if ip == "" || ip == "0.0.0.0" || ip == "::" {
			continue
		}
		port, ok := s.get(fmt.Sprintf("Port%d", n))
		if !ok || port == "" || port == "514" {
			out = append(out, ip)
			continue
		}
		out = append(out, ip+":"+port)
	}
	return out
}

// String renders the section for a log line.
func (s Syslog) String() string {
	parts := make([]string, 0, len(s.Keys))
	for _, kv := range s.Keys {
		parts = append(parts, kv[0]+"="+kv[1])
	}
	return "[" + s.Section + "] " + strings.Join(parts, ", ")
}

// Render is the partial config.ini that makes the card send to exactly
// hosts: syslog on, the hosts in the first slots, and any other slot that
// holds a server emptied. No hosts switches syslog off and leaves the slots
// as they are. Only keys the card's own section has are written, so a card
// that spells its section differently is refused, not guessed at; the
// facility and the severity mapping are left alone.
func (s Syslog) Render(hosts []string) ([]byte, error) {
	if _, ok := s.get("Syslog"); !ok {
		return nil, fmt.Errorf("the card's %s has no Syslog key", s)
	}
	var b strings.Builder
	b.WriteString("[" + s.Section + "]\r\n")
	if len(hosts) == 0 {
		b.WriteString("Syslog=disabled\r\n")
		return []byte(b.String()), nil
	}
	slots := s.slots()
	if len(hosts) > slots {
		return nil, fmt.Errorf("the card has %d syslog server slots, the controller pushed %d hosts", slots, len(hosts))
	}
	b.WriteString("Syslog=enabled\r\n")
	for n := 1; n <= slots; n++ {
		if n > len(hosts) {
			if v, _ := s.get(fmt.Sprintf("Server%d", n)); v != "" && v != "0.0.0.0" {
				fmt.Fprintf(&b, "Server%d=0.0.0.0\r\n", n)
			}
			continue
		}
		ip, port, err := splitHost(hosts[n-1])
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "Server%d=%s\r\n", n, ip)
		if _, ok := s.get(fmt.Sprintf("Port%d", n)); ok {
			fmt.Fprintf(&b, "Port%d=%s\r\n", n, port)
		} else if port != "514" {
			return nil, fmt.Errorf("the card's %s has no Port%d key for port %s", s, n, port)
		}
	}
	return []byte(b.String()), nil
}

// splitHost takes the controller's "ip" or "ip:port". Anything but an IP
// literal is refused: the value goes into a config file line.
func splitHost(h string) (ip, port string, err error) {
	if net.ParseIP(h) != nil {
		return h, "514", nil
	}
	ip, port, err = net.SplitHostPort(h)
	if err != nil || net.ParseIP(ip) == nil {
		return "", "", fmt.Errorf("syslog host %q is not an IP address", h)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("syslog host %q has no valid port", h)
	}
	return ip, port, nil
}

// DefaultSettle is how long after an upload the card's file is read back.
// The card applies a config.ini a little after the transfer -- seconds for
// outlet names, about a minute for its TCP/IP section -- and a second upload
// arriving while the first is being applied is dropped without a word.
const DefaultSettle = 60 * time.Second

// maxMisses is how many uploads the card may fail to take before the
// applier stops writing the same hosts again.
const maxMisses = 3

// SyslogApplier keeps one card's syslog servers on the controller's remote
// syslog host. It reads the section once from the config.ini the driver
// downloads at Start (Load), writes a partial when the controller's hosts
// differ, and confirms the write by reading the whole file back once the
// card has had time to apply it -- the next Apply at least Settle later --
// so what it logs is what the card holds.
type SyslogApplier struct {
	Get    func(context.Context) ([]byte, error) // the card's whole config.ini
	Put    func(context.Context, []byte) error   // a partial config.ini
	Logf   func(format string, args ...any)      // nil = silent
	Settle time.Duration                         // 0 = DefaultSettle
	Now    func() time.Time                      // nil = time.Now

	mu       sync.Mutex
	cur      Syslog
	loaded   bool
	awaiting bool      // an upload has not been read back yet
	want     []string  // the hosts last uploaded
	at       time.Time // when
	misses   int       // uploads of want the card did not take
	refused  string    // the last refusal logged, so it is logged once
}

// Load takes the card's syslog section from its whole config.ini and logs
// it. It reports whether the card has one.
func (a *SyslogApplier) Load(cfg []byte) bool {
	s, ok := ParseSyslog(cfg)
	a.mu.Lock()
	a.cur, a.loaded = s, ok
	a.mu.Unlock()
	if ok {
		a.logf("syslog: the card's config.ini reads %s", s)
	} else {
		a.logf("syslog: the card's config.ini has no [%s] section", syslogSection)
	}
	return ok
}

// Apply makes the card send to hosts (nil: stop sending). It is a diff
// against what the card was last read as; changed reports an upload.
func (a *SyslogApplier) Apply(ctx context.Context, hosts []string) (changed bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.loaded {
		a.refuse(fmt.Sprintf("the card's config.ini has no [%s] section; its syslog host is left alone", syslogSection))
		return false, nil
	}
	if a.awaiting {
		if a.now().Sub(a.at) < a.settle() {
			return false, nil
		}
		b, err := a.Get(ctx)
		if err != nil {
			return false, fmt.Errorf("read config.ini back: %w", err)
		}
		if s, ok := ParseSyslog(b); ok {
			a.cur = s
		}
		a.awaiting = false
		if equal(a.cur.Hosts(), a.want) {
			a.misses = 0
			a.logf("syslog: the card's config.ini now reads %s", a.cur)
		} else {
			a.misses++
			a.logf("syslog: the card's config.ini still reads %s after the upload (%d of %d)", a.cur, a.misses, maxMisses)
		}
	}
	if equal(a.cur.Hosts(), hosts) {
		return false, nil
	}
	if !equal(hosts, a.want) {
		a.misses = 0
	}
	if a.misses >= maxMisses {
		a.refuse(fmt.Sprintf("the card did not take syslog host %v in %d uploads; not writing it again until the controller's host changes or the bridge restarts", hosts, maxMisses))
		return false, nil
	}
	body, err := a.cur.Render(hosts)
	if err != nil {
		a.refuse(err.Error() + "; its syslog host is left alone")
		return false, nil
	}
	a.logf("syslog: the card sends to %v, the controller's host is %v; uploading %s", a.cur.Hosts(), hosts, strings.ReplaceAll(strings.TrimSpace(string(body)), "\r\n", " "))
	if err := a.Put(ctx, body); err != nil {
		return false, fmt.Errorf("upload config.ini: %w", err)
	}
	a.awaiting, a.want, a.at = true, append([]string(nil), hosts...), a.now()
	return true, nil
}

func (a *SyslogApplier) refuse(msg string) {
	if a.refused == msg {
		return
	}
	a.refused = msg
	a.logf("syslog: %s", msg)
}

func (a *SyslogApplier) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

func (a *SyslogApplier) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *SyslogApplier) settle() time.Duration {
	if a.Settle > 0 {
		return a.Settle
	}
	return DefaultSettle
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
