package apcups

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// FixtureRunner replays a register dump instead of talking to a UPS. The
// dump (docs/fixtures/apc-smtl-15.5/registers.txt) is real output captured
// from Clint's SMTL1500RM3UC over its SmartConnect port and scrubbed by
// scripts/sanitize-apc-ups.py; nothing here is hand-written. One line per
// register: "<address> <value>".
type FixtureRunner struct {
	Regs map[int]uint16
	// Reads records every block read in order, so a test can assert the
	// collector asks for exactly what the reference driver asks for.
	Reads []string
	// Writes records every register write as "<start>=<hex words>", so a
	// test can assert exactly what the driver would have sent.
	Writes []string
	// FailWrite makes the next WriteRegisters fail, for the refusal paths.
	FailWrite error
}

// NewFixtureRunner loads registers.txt from dir.
func NewFixtureRunner(dir string) (*FixtureRunner, error) {
	f, err := os.Open(dir + "/registers.txt")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fr := &FixtureRunner{Regs: map[int]uint16{}}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		a, v, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("apc-ups: bad fixture line %q", line)
		}
		addr, err1 := strconv.Atoi(a)
		val, err2 := strconv.ParseUint(strings.TrimSpace(v), 10, 16)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("apc-ups: bad fixture line %q", line)
		}
		fr.Regs[addr] = uint16(val)
	}
	if len(fr.Regs) == 0 {
		return nil, fmt.Errorf("apc-ups: no registers in %s/registers.txt", dir)
	}
	return fr, sc.Err()
}

// ReadRegisters serves a block; a register the dump lacks is answered the
// way the device answers an unmapped address, with an exception.
func (f *FixtureRunner) ReadRegisters(_ context.Context, start, count int) ([]uint16, error) {
	f.Reads = append(f.Reads, fmt.Sprintf("%d,%d", start, count))
	out := make([]uint16, count)
	for i := range out {
		v, ok := f.Regs[start+i]
		if !ok {
			return nil, &Exception{Function: 3, Code: 0x02}
		}
		out[i] = v
	}
	return out, nil
}

// WriteRegisters records the write and, for the outlet command word, moves
// the group's status bit the way the unit would: on sets it, off clears it,
// a reboot is not a state -- the unit opens and closes the relay and by the
// next poll the group reads back as on (the PDU fixture makes the same
// distinction).
func (f *FixtureRunner) WriteRegisters(_ context.Context, start int, values []uint16) error {
	if f.FailWrite != nil {
		err := f.FailWrite
		f.FailWrite = nil
		return err
	}
	var hex string
	for _, v := range values {
		hex += fmt.Sprintf("%04x", v)
	}
	f.Writes = append(f.Writes, fmt.Sprintf("%d=%s", start, hex))
	if start != regOutletCommand || len(values) != 2 {
		return nil
	}
	word := uint32(values[0])<<16 | uint32(values[1])
	for g := 0; g < 4; g++ {
		if word&groupTarget(g) == 0 {
			continue
		}
		reg := regOutletGroup0 + 3*g + 1 // low word of the group's status field
		switch {
		case word&cmdOutputOn != 0:
			f.Regs[reg] |= outletGroupOn
		case word&cmdOutputOff != 0:
			f.Regs[reg] &^= outletGroupOn
		case word&cmdOutputReboot != 0:
			f.Regs[reg] |= outletGroupOn
		}
	}
	return nil
}
