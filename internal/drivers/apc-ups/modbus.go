package apcups

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Runner reads holding registers from the UPS. The live runner is a Modbus
// TCP client; tests replay a register dump captured from a real unit.
type Runner interface {
	ReadRegisters(ctx context.Context, start, count int) ([]uint16, error)
}

// Modbus is one persistent Modbus TCP connection to the UPS's SmartConnect
// port. It implements Read Holding Registers (3), the only function this
// read-only first version needs. Nothing in the package writes to the unit
// yet: switching an outlet group (command register 1538) is a follow-on that
// waits on a discussion with the operator and a test outlet known to carry
// no load -- the unit this was written against carries a whole rack.
//
// One connection is kept open and reused. The port's stack is small: a burst
// of short-lived connections made a real unit refuse connections for a while
// (2026-09-26), which is why every read goes through this one session.
type Modbus struct {
	Addr    string        // host:port; the port defaults to 502
	UnitID  byte          // Modbus unit id; APC answers on 1
	Timeout time.Duration // per-request response timeout

	mu   sync.Mutex
	conn net.Conn
	tid  uint16
}

// NewModbus returns a client for addr, adding :502 when no port is given.
func NewModbus(addr string) *Modbus {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "502")
	}
	return &Modbus{Addr: addr, UnitID: 1, Timeout: 3 * time.Second}
}

func (m *Modbus) dial(ctx context.Context) error {
	if m.conn != nil {
		return nil
	}
	d := net.Dialer{Timeout: m.Timeout}
	c, err := d.DialContext(ctx, "tcp", m.Addr)
	if err != nil {
		return fmt.Errorf("apc-ups: connect %s: %w", m.Addr, err)
	}
	m.conn = c
	return nil
}

func (m *Modbus) drop() {
	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
	}
}

// Close ends the session.
func (m *Modbus) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drop()
	return nil
}

// ReadRegisters reads count holding registers from start (function 3). A
// failed request drops the connection and is retried once on a fresh one,
// which covers the port closing an idle session between inform cycles.
func (m *Modbus) ReadRegisters(ctx context.Context, start, count int) ([]uint16, error) {
	if count < 1 || count > 125 {
		return nil, fmt.Errorf("apc-ups: read of %d registers is outside 1..125", count)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := m.dial(ctx); err != nil {
			return nil, err
		}
		regs, err := m.read(start, count)
		if err == nil {
			return regs, nil
		}
		lastErr = err
		m.drop()
		// A Modbus exception is the device answering; a retry would only ask
		// the same question again.
		var ex *Exception
		if errors.As(err, &ex) {
			break
		}
	}
	return nil, lastErr
}

// Exception is a Modbus exception response: the device understood the
// request and refused it (0x02 = illegal data address, the answer to a
// register the map does not define).
type Exception struct {
	Function byte
	Code     byte
}

func (e *Exception) Error() string {
	return fmt.Sprintf("apc-ups: modbus exception 0x%02x for function %d", e.Code, e.Function)
}

func (m *Modbus) read(start, count int) ([]uint16, error) {
	m.tid++
	req := make([]byte, 12)
	binary.BigEndian.PutUint16(req[0:], m.tid)
	binary.BigEndian.PutUint16(req[2:], 0) // protocol id
	binary.BigEndian.PutUint16(req[4:], 6) // bytes to follow
	req[6] = m.UnitID
	req[7] = 3 // read holding registers
	binary.BigEndian.PutUint16(req[8:], uint16(start))
	binary.BigEndian.PutUint16(req[10:], uint16(count))

	if err := m.conn.SetDeadline(time.Now().Add(m.Timeout)); err != nil {
		return nil, err
	}
	if _, err := m.conn.Write(req); err != nil {
		return nil, fmt.Errorf("apc-ups: write: %w", err)
	}
	hdr := make([]byte, 7)
	if _, err := io.ReadFull(m.conn, hdr); err != nil {
		return nil, fmt.Errorf("apc-ups: read header: %w", err)
	}
	if got := binary.BigEndian.Uint16(hdr[0:]); got != m.tid {
		return nil, fmt.Errorf("apc-ups: transaction id %d, want %d (stream out of step)", got, m.tid)
	}
	n := int(binary.BigEndian.Uint16(hdr[4:])) - 1 // minus the unit id already read
	if n < 1 || n > 2+2*125 {
		return nil, fmt.Errorf("apc-ups: implausible frame length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(m.conn, body); err != nil {
		return nil, fmt.Errorf("apc-ups: read body: %w", err)
	}
	if body[0]&0x80 != 0 {
		if len(body) < 2 {
			return nil, errors.New("apc-ups: truncated exception")
		}
		return nil, &Exception{Function: body[0] &^ 0x80, Code: body[1]}
	}
	if body[0] != 3 || len(body) < 2 || int(body[1]) != 2*count || len(body) < 2+2*count {
		return nil, fmt.Errorf("apc-ups: malformed response (%d bytes for %d registers)", len(body), count)
	}
	regs := make([]uint16, count)
	for i := range regs {
		regs[i] = binary.BigEndian.Uint16(body[2+2*i:])
	}
	return regs, nil
}
