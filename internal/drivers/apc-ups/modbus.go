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
	// WriteRegisters writes values at start (function 16). The only write
	// this package makes is the outlet command word at register 1538.
	WriteRegisters(ctx context.Context, start int, values []uint16) error
}

// Modbus is one persistent Modbus TCP connection to the UPS's SmartConnect
// port. It implements Read Holding Registers (3) and Write Multiple
// Registers (16); the only thing ever written is the outlet command word
// (apply.go), and the frame is held byte-exact to what NUT's apc_modbus
// sends, since no write has been exercised against the unit.
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

// transact sends one request body (function code onward) inside an MBAP
// header and returns the response body, or an *Exception.
func (m *Modbus) transact(body []byte) ([]byte, error) {
	m.tid++
	req := make([]byte, 7+len(body))
	binary.BigEndian.PutUint16(req[0:], m.tid)
	binary.BigEndian.PutUint16(req[2:], 0) // protocol id
	binary.BigEndian.PutUint16(req[4:], uint16(1+len(body)))
	req[6] = m.UnitID
	copy(req[7:], body)

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
	resp := make([]byte, n)
	if _, err := io.ReadFull(m.conn, resp); err != nil {
		return nil, fmt.Errorf("apc-ups: read body: %w", err)
	}
	if resp[0]&0x80 != 0 {
		if len(resp) < 2 {
			return nil, errors.New("apc-ups: truncated exception")
		}
		return nil, &Exception{Function: resp[0] &^ 0x80, Code: resp[1]}
	}
	return resp, nil
}

func (m *Modbus) read(start, count int) ([]uint16, error) {
	body := make([]byte, 5)
	body[0] = 3 // read holding registers
	binary.BigEndian.PutUint16(body[1:], uint16(start))
	binary.BigEndian.PutUint16(body[3:], uint16(count))
	resp, err := m.transact(body)
	if err != nil {
		return nil, err
	}
	if resp[0] != 3 || len(resp) < 2 || int(resp[1]) != 2*count || len(resp) < 2+2*count {
		return nil, fmt.Errorf("apc-ups: malformed response (%d bytes for %d registers)", len(resp), count)
	}
	regs := make([]uint16, count)
	for i := range regs {
		regs[i] = binary.BigEndian.Uint16(resp[2+2*i:])
	}
	return regs, nil
}

// WriteRegisters writes values at start with Write Multiple Registers (16),
// the function NUT's apc_modbus uses for the command word. A failed request
// is retried once on a fresh connection, like a read; an exception is not.
func (m *Modbus) WriteRegisters(ctx context.Context, start int, values []uint16) error {
	if len(values) < 1 || len(values) > 123 {
		return fmt.Errorf("apc-ups: write of %d registers is outside 1..123", len(values))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := m.dial(ctx); err != nil {
			return err
		}
		err := m.write(start, values)
		if err == nil {
			return nil
		}
		lastErr = err
		m.drop()
		var ex *Exception
		if errors.As(err, &ex) {
			break
		}
	}
	return lastErr
}

func (m *Modbus) write(start int, values []uint16) error {
	body := make([]byte, 6+2*len(values))
	body[0] = 16 // write multiple registers
	binary.BigEndian.PutUint16(body[1:], uint16(start))
	binary.BigEndian.PutUint16(body[3:], uint16(len(values)))
	body[5] = byte(2 * len(values))
	for i, v := range values {
		binary.BigEndian.PutUint16(body[6+2*i:], v)
	}
	resp, err := m.transact(body)
	if err != nil {
		return err
	}
	// The reply echoes the function, start and count.
	if len(resp) < 5 || resp[0] != 16 || int(binary.BigEndian.Uint16(resp[1:])) != start || int(binary.BigEndian.Uint16(resp[3:])) != len(values) {
		return fmt.Errorf("apc-ups: malformed write response (%d bytes)", len(resp))
	}
	return nil
}
