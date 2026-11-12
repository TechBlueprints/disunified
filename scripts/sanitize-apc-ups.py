#!/usr/bin/env python3
"""Scrub a register dump captured from an APC Smart-UPS over Modbus.

The dump is one line per holding register, "<address> <value>", as
internal/drivers/apc-ups/fixture.go reads it. The only identifying data in
the map is the serial number (registers 564-571, 16 ASCII characters) and the
operator-set UPS name (596-603); both are rewritten to documentation values
(the serial as SSJ and zeros of its own length, its padding kept)
so that every other register is byte-for-byte what the unit sent.

Usage: scripts/sanitize-apc-ups.py <in> <out>
"""
import sys

REWRITE = {596: ("APC UPS", 8)}
SERIAL = (564, 8)  # 16 ASCII characters, space- or NUL-padded

def placeholder_serial(s):
    """A serial's documentation placeholder, the original's length (SSJ and
    zeros; an 11-character serial is SSJ00000000); an empty one stays empty.
    A fixture that changed a value's length could not carry the one that
    failed (docs/wire-shapes.md §1)."""
    return ('SSJ' + '0' * len(s))[:len(s)]

def words(text, n):
    b = text.encode("ascii").ljust(2 * n, b" ")
    return [(b[2 * i] << 8) | b[2 * i + 1] for i in range(n)]

regs = {}
for line in open(sys.argv[1], encoding="utf-8"):
    line = line.strip()
    if not line or line.startswith("#"):
        continue
    a, v = line.split(None, 1)
    regs[int(a)] = int(v)
start, n = SERIAL
if all(start + i in regs for i in range(n)):
    raw = b"".join(bytes([regs[start + i] >> 8, regs[start + i] & 255]) for i in range(n))
    core = raw.rstrip(b" \x00")  # the padding stays as the unit sent it
    new = placeholder_serial(core.decode("ascii", "replace")).encode("ascii") + raw[len(core):]
    for i in range(n):
        regs[start + i] = (new[2 * i] << 8) | new[2 * i + 1]
for start, (text, n) in REWRITE.items():
    if start in regs:
        for i, w in enumerate(words(text, n)):
            regs[start + i] = w
with open(sys.argv[2], "w", encoding="utf-8") as out:
    out.write("# APC Smart-UPS holding registers over Modbus TCP, scrubbed by scripts/sanitize-apc-ups.py\n")
    for a in sorted(regs):
        out.write(f"{a} {regs[a]}\n")
