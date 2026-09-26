package apcups

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// serve answers one Modbus TCP request with body (after the MBAP header) and
// returns the request it saw.
func serve(t *testing.T, body []byte) (addr string, got chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got = make(chan []byte, 1)
	go func() {
		defer ln.Close()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		hdr := make([]byte, 7)
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		req := make([]byte, binary.BigEndian.Uint16(hdr[4:])-1)
		io.ReadFull(c, req)
		got <- req
		resp := make([]byte, 7+len(body))
		copy(resp, hdr[:4]) // echo the transaction and protocol ids
		binary.BigEndian.PutUint16(resp[4:], uint16(1+len(body)))
		resp[6] = hdr[6]
		copy(resp[7:], body)
		c.Write(resp)
	}()
	return ln.Addr().String(), got
}

func TestModbusReadsHoldingRegisters(t *testing.T) {
	addr, got := serve(t, []byte{3, 4, 0x20, 0x02, 0x00, 0x00})
	m := NewModbus(addr)
	m.Timeout = 2 * time.Second
	regs, err := m.ReadRegisters(context.Background(), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 2 || regs[0] != 0x2002 || regs[1] != 0 {
		t.Errorf("regs = %#v", regs)
	}
	req := <-got
	if req[0] != 3 || binary.BigEndian.Uint16(req[1:]) != 0 || binary.BigEndian.Uint16(req[3:]) != 2 {
		t.Errorf("request = %#v, want function 3, start 0, count 2", req)
	}
}

// An exception is the device answering; it is surfaced as such and not
// retried on a new connection.
func TestModbusSurfacesAnException(t *testing.T) {
	addr, _ := serve(t, []byte{0x83, 0x02})
	m := NewModbus(addr)
	m.Timeout = 2 * time.Second
	_, err := m.ReadRegisters(context.Background(), 0, 4)
	var ex *Exception
	if !errors.As(err, &ex) || ex.Code != 0x02 {
		t.Fatalf("err = %v, want a modbus exception 0x02", err)
	}
}

func TestModbusDefaultsThePort(t *testing.T) {
	if NewModbus("192.0.2.30").Addr != "192.0.2.30:502" {
		t.Error("port 502 not defaulted")
	}
	if NewModbus("192.0.2.30:1502").Addr != "192.0.2.30:1502" {
		t.Error("explicit port not kept")
	}
}

// There is no write function in this package, by design of this version.
func TestModbusHasNoWritePath(t *testing.T) {
	// Compile-time: nothing to call. This test documents the intent so a
	// future write path is added deliberately, with its own tests.
}
