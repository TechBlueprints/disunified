package apcpdu

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
	"github.com/jlaffaye/ftp"
)

// Runner is the driver's single transport seam: everything that touches the
// PDU goes through it, so a test can replace the device with a recorded
// capture (fixture.go). It is deliberately small and protocol-shaped rather
// than feature-shaped.
//
// The split is not arbitrary. The card's SNMP agent serves every read and
// switches outlets, but its outlet-name objects are read-only -- in SNMPv1 a
// read-only object answers a SET with noSuchName, the same error as a missing
// OID -- so a name has to be written through the card's own config file over
// FTP. See docs/drivers/apc-pdu.md.
type Runner interface {
	// Walk returns every OID under root as numeric-OID -> rendered value.
	Walk(ctx context.Context, root string) (map[string]string, error)
	// SetInt writes an INTEGER, which is how an outlet is switched.
	SetInt(ctx context.Context, oid string, v int) error
	// PutConfig uploads a partial config.ini. The card applies it live, with
	// no reboot, a few seconds after the transfer completes.
	PutConfig(ctx context.Context, body []byte) error
}

// SNMP is the live Runner: SNMPv1 for reads and outlet switching, FTP for the
// config file.
type SNMP struct {
	Host           string // address only, no port
	ReadCommunity  string
	WriteCommunity string
	User, Password string // the card's admin/device login, used for FTP

	Timeout time.Duration
	Retries int
}

// NewSNMP builds a live runner with the card's defaults.
func NewSNMP(host string) *SNMP {
	return &SNMP{Host: host, ReadCommunity: "public", WriteCommunity: "private",
		Timeout: 5 * time.Second, Retries: 2}
}

func (s *SNMP) client(community string) *gosnmp.GoSNMP {
	return &gosnmp.GoSNMP{
		Target:    s.Host,
		Port:      161,
		Community: community,
		// SNMPv1 only. This card generation answers v2c too, but v1 is what
		// its access-control table is expressed in and what the outlet OIDs
		// were verified against.
		Version: gosnmp.Version1,
		Timeout: s.Timeout,
		Retries: s.Retries,
	}
}

func (s *SNMP) Walk(ctx context.Context, root string) (map[string]string, error) {
	g := s.client(s.ReadCommunity)
	g.Context = ctx
	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("snmp connect %s: %w", s.Host, err)
	}
	defer g.Conn.Close()
	pdus, err := g.WalkAll(root)
	if err != nil {
		return nil, fmt.Errorf("snmp walk %s: %w", root, err)
	}
	out := make(map[string]string, len(pdus))
	for _, p := range pdus {
		out[strings.TrimPrefix(p.Name, ".")] = pduValue(p)
	}
	return out, nil
}

func (s *SNMP) SetInt(ctx context.Context, oid string, v int) error {
	if s.WriteCommunity == "" {
		return fmt.Errorf("apc-pdu: no write community configured, cannot set %s", oid)
	}
	g := s.client(s.WriteCommunity)
	g.Context = ctx
	if err := g.Connect(); err != nil {
		return fmt.Errorf("snmp connect %s: %w", s.Host, err)
	}
	defer g.Conn.Close()
	res, err := g.Set([]gosnmp.SnmpPDU{{Name: oid, Type: gosnmp.Integer, Value: v}})
	if err != nil {
		// A timeout here is the card's way of refusing a write: with access
		// type "Write" rather than "Write+" it drops SETs silently, with no
		// error and no log entry, which reads exactly like a network fault.
		return fmt.Errorf("snmp set %s=%d: %w (if this times out, the community needs access type Write+, not Write)", oid, v, err)
	}
	if res != nil && res.Error != gosnmp.NoError {
		return fmt.Errorf("snmp set %s=%d: %s", oid, v, res.Error)
	}
	return nil
}

func (s *SNMP) PutConfig(ctx context.Context, body []byte) error {
	c, err := ftp.Dial(s.Host+":21", ftp.DialWithContext(ctx), ftp.DialWithTimeout(s.Timeout))
	if err != nil {
		return fmt.Errorf("ftp dial %s: %w", s.Host, err)
	}
	defer func() { _ = c.Quit() }()
	if err := c.Login(s.User, s.Password); err != nil {
		return fmt.Errorf("ftp login: %w", err)
	}
	if err := c.Stor("config.ini", strings.NewReader(string(body))); err != nil {
		return fmt.Errorf("ftp store config.ini: %w", err)
	}
	return nil
}

// pduValue renders a PDU the way snmpwalk's text output does, so the live
// runner and a recorded fixture hand the parser the same strings.
func pduValue(p gosnmp.SnmpPDU) string {
	switch p.Type {
	case gosnmp.OctetString:
		b, _ := p.Value.([]byte)
		// An OctetString is either text or raw bytes, and the MIB says which.
		// ifPhysAddress is six raw bytes; handing those back as a Go string
		// would put binary into the snapshot. Render anything unprintable as
		// colon-hex, which is what snmpwalk shows for a PhysAddress and what
		// the recorded fixtures therefore contain.
		if isPrintable(b) {
			return string(b)
		}
		parts := make([]string, len(b))
		for i, c := range b {
			parts[i] = fmt.Sprintf("%02x", c)
		}
		return strings.Join(parts, ":")
	case gosnmp.ObjectIdentifier:
		s, _ := p.Value.(string)
		return s
	default:
		switch v := p.Value.(type) {
		case int:
			return strconv.Itoa(v)
		case int64:
			return strconv.FormatInt(v, 10)
		case uint:
			return strconv.FormatUint(uint64(v), 10)
		case uint32:
			return strconv.FormatUint(uint64(v), 10)
		case uint64:
			return strconv.FormatUint(v, 10)
		case string:
			return v
		case []byte:
			return string(v)
		default:
			return fmt.Sprint(p.Value)
		}
	}
}

// isPrintable reports whether b is text rather than a binary value.
func isPrintable(b []byte) bool {
	for _, c := range b {
		if c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}
