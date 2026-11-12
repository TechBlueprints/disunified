#!/usr/bin/env python3
"""Scrub captures from an APC Back-UPS Pro network model's NMC (AOS 6):
the web pages the apc-backups driver reads and the card's config.ini.

Pages: the session token in every /NMC/<token>/ path becomes TOKEN; the
UPS, card and battery serial numbers become placeholders of the same length
(SSJ000000000, serial-00001); the
card's MAC (both "00 C0 B7 .." and colon forms, and the apcXXXXXX host name
derived from it) becomes 02:00:00:00:00:01; IPv4 addresses map into
192.0.2.0/24; the host name and its domain become ups.example.net; outlet
names that name real equipment become "outlet-N" (the card's own defaults
and "unplugged" are kept). Everything else stays byte for byte, so the
parser sees real pages.

config.ini: only the sections the driver reads are kept -- [NetworkTCP/IP],
[NetworkDNS], [SystemID] and the UPS section ([Back-UPS/...]) -- because
the rest (SNMP communities, e-mail, RADIUS, users) is configuration that
never belongs in a public repo; the kept sections are scrubbed as above.

Usage: scripts/sanitize-apc-backups.py <in-dir> <out-dir>
"""
import ipaddress
import os
import re
import sys

TOKEN_RE = re.compile(r'/NMC/[A-Za-z0-9_-]+/')
SERIAL_RE = re.compile(r'\b[0-9][A-Z][0-9]{4}[A-Z][0-9]{5}\b')
MAC_SP_RE = re.compile(r'\b([0-9A-F]{2} ){5}[0-9A-F]{2}\b')
MAC_RE = re.compile(r'\b([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}\b')
IP_RE = re.compile(r'\b(?:\d{1,3}\.){3}\d{1,3}\b')
KEEP_SECTIONS = ('NetworkTCP/IP', 'NetworkDNS', 'SystemID')
KEEP_NAMES = {'unplugged', ''}

serials, ips = {}, {}


def placeholder_serial(s):
    """A serial's documentation placeholder, the original's length (SSJ and
    zeros; an 11-character serial is SSJ00000000); an empty one stays empty.
    A fixture that changed a value's length could not carry the one that
    failed (docs/wire-shapes.md §1)."""
    return ('SSJ' + '0' * len(s))[:len(s)]

def map_serial(s):
    # The first serial met (the UPS's, on the About page) becomes the
    # documented SSJ0... of its length; any further one (the battery pack) a
    # placeholder of its length that is not serial-shaped (serial-00001), so
    # two serials stay apart and the checker's allowed form stays SSJ0+.
    if s not in serials:
        n = len(serials)
        serials[s] = placeholder_serial(s) if not n else 'serial-%0*d' % (max(len(s) - 7, 1), n)
    return serials[s]


def map_ip(s):
    try:
        ip = ipaddress.ip_address(s)
    except ValueError:
        return s
    if ip.is_loopback or ip.is_unspecified or s.startswith('255.'):
        return s
    if s not in ips:
        ips[s] = '192.0.2.%d' % (len(ips) + 1)
    return ips[s]


def scrub(text, hostname, domain, mac_hex, names):
    text = TOKEN_RE.sub('/NMC/TOKEN/', text)
    text = SERIAL_RE.sub(lambda m: map_serial(m.group(0)), text)
    text = MAC_SP_RE.sub('02 00 00 00 00 01', text)
    text = MAC_RE.sub('02:00:00:00:00:01', text)
    if mac_hex:
        text = re.sub(r'\bapc' + mac_hex + r'\b', 'apc000001', text, flags=re.I)
    if hostname:
        text = text.replace(hostname, 'ups.example.net')
    if domain:
        text = text.replace(domain, 'example.net')
    for i, n in enumerate(names, 1):
        if n and n not in KEEP_NAMES:
            text = re.sub(r'(?<![\w-])' + re.escape(n) + r'(?![\w-])', 'outlet-%d' % i, text)
    text = IP_RE.sub(lambda m: map_ip(m.group(0)), text)
    return text


def reduce_ini(text):
    out, keep = [], False
    for line in text.split('\n'):
        t = line.strip()
        if t.startswith('[') and t.endswith(']'):
            sec = t[1:-1]
            keep = sec in KEEP_SECTIONS or sec.startswith('Back-UPS/')
        if keep or t.startswith(';') and not out:
            out.append(line)
    return '\n'.join(out)


def main(src, dst):
    os.makedirs(dst, exist_ok=True)
    files = sorted(os.listdir(src))
    ini = open(os.path.join(src, 'config.ini'), encoding='utf-8', errors='replace').read() if 'config.ini' in files else ''
    hostname = domain = mac_hex = ''
    names = []
    m = re.search(r'^Name=(\S+)', ini, re.M)
    if m:
        hostname = m.group(1)
        domain = hostname.split('.', 1)[1] if '.' in hostname else ''
    m = re.search(r'^Override=((?:[0-9A-F]{2} ){5}[0-9A-F]{2})', ini, re.M)
    if m:
        mac_hex = m.group(1).replace(' ', '')[-6:]
    cfg = os.path.join(src, 'uloutcfg2.htm')
    if os.path.exists(cfg):
        page = open(cfg, encoding='utf-8', errors='replace').read()
        for m in re.finditer(r'name="(?:MOG|SOG)\dName"[^>]*value="([^"]*)"', page):
            names.append(m.group(1))
    for f in files:
        if not (f.endswith('.htm') or f == 'config.ini'):
            continue
        text = open(os.path.join(src, f), encoding='utf-8', errors='replace').read()
        if f == 'config.ini':
            text = reduce_ini(text)
        open(os.path.join(dst, f), 'w', encoding='utf-8').write(scrub(text, hostname, domain, mac_hex, names))
    print('scrubbed %d files: %d serials, %d IPv4, host %r, %d outlet names' % (len(files), len(serials), len(ips), hostname, len([n for n in names if n not in KEEP_NAMES])))


if __name__ == '__main__':
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
