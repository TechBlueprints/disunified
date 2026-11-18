#!/usr/bin/env python3
"""Scrub a text fixture (the Podman collector's tagged-section output).

Rewrites MAC addresses (consistent mapping into 02:00:00:xx:xx:xx), IPv4
addresses (into 192.0.2.0/24, keeping any /prefix), global IPv6 addresses
(into 2001:db8::/32), the host name and its domain (host-1.example.net),
DMI serials/UUIDs and the machine id, and container/network names other
than the bridge's own (app-N / net-N, aliases included), keeping every
other byte so the parser sees real output.

Usage: scripts/sanitize-podman.py <in> <out>
"""
import hashlib
import ipaddress
import json
import re
import sys

MAC_RE = re.compile(r'\b([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}\b')
IP_RE = re.compile(r'\b(?:\d{1,3}\.){3}\d{1,3}\b')
# Any run of hex digits and colons with at least two colons (so "::1" forms
# match too); ipaddress decides whether it is an address.
IP6_RE = re.compile(r'(?<![0-9A-Za-z:.])[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}(?![0-9A-Za-z:.])')
HEX32_RE = re.compile(r'\b[0-9a-f]{32}\b')
UUID_RE = re.compile(r'\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b')

macs, ips, ip6s, lls = {}, {}, {}, {}
KEEP_NAMES = ('disunified',)


def placeholder_serial(s):
    """A serial's documentation placeholder, the original's length (SSJ and
    zeros; an 11-character serial is SSJ00000000); an empty one stays empty.
    A fixture that changed a value's length could not carry the one that
    failed (docs/wire-shapes.md §1)."""
    return ('SSJ' + '0' * len(s))[:len(s)]

def map_mac(m):
    key = m.lower()
    if key not in macs:
        n = len(macs) + 1
        macs[key] = f'02:00:00:{(n >> 16) & 255:02x}:{(n >> 8) & 255:02x}:{n & 255:02x}'
    return macs[key]


def map_ip(s):
    try:
        ip = ipaddress.ip_address(s)
    except ValueError:
        return s
    if ip.is_loopback or ip.is_unspecified or ip.is_multicast or s.startswith('255.'):
        return s
    if s not in ips:
        ips[s] = f'192.0.2.{len(ips) + 1}'
    return ips[s]


def map_ip6(s):
    try:
        ip = ipaddress.ip_address(s)
    except ValueError:
        return s
    if ip.is_loopback or ip.is_unspecified or ip.is_multicast:
        return s
    if ip.is_link_local:
        # The EUI-64 half of a link-local address is the interface's real MAC.
        if s not in lls:
            lls[s] = f'fe80::{len(lls) + 1:x}'
        return lls[s]
    if s not in ip6s:
        ip6s[s] = f'2001:db8::{len(ip6s) + 1}'
    return ip6s[s]


def section(text, name):
    m = re.search(r'^@@@ ' + re.escape(name) + r'\n(.*?)(?=^@@@ )', text, re.S | re.M)
    return m.group(1) if m else ''


def block_mark(listing):
    """The client-Block table's fingerprint (internal/drivers/podman/block.go
    blockRuleset) recomputed from a listing: the sha256 of the chains' text,
    first 6 bytes. The real one is a hash over the real MACs, short enough to
    search, so a scrubbed listing carries the mark of its scrubbed MACs (what
    the driver would write for them). None when the listing has no table."""
    try:
        objs = json.loads(listing).get('nftables') or []
    except json.JSONDecodeError:
        return None
    dev, macs = None, None
    for o in objs:
        if 'chain' in o:
            dev = dev or o['chain'].get('dev')
        if 'rule' in o and macs is None:
            for e in o['rule'].get('expr') or []:
                r = e.get('match', {}).get('right')
                if isinstance(r, str):
                    macs = [r]
                elif isinstance(r, dict) and 'set' in r:
                    macs = [m for m in r['set'] if isinstance(m, str)]
    if not dev or not macs:
        return None
    body = ''
    for hook in ('ingress', 'egress'):
        body += f'\tchain {hook} {{\n\t\ttype filter hook {hook} device {json.dumps(dev)} priority filter; policy accept;\n'
        body += f'\t\tether saddr {{ {", ".join(sorted(macs))} }} counter drop\n\t}}\n'
    return hashlib.sha256(body.encode()).hexdigest()[:12]


def main(src, dst):
    text = open(src).read()

    # Names: the host and its domain, then every container and network
    # that is not the bridge's own, plus their aliases.
    hostname = section(text, 'hostname').strip()
    names = {}
    if hostname:
        names[hostname] = 'host-1'
    ps = section(text, 'ps').strip()
    inspect = section(text, 'inspect').strip()
    nets = section(text, 'networks').strip()
    app, net = 0, 0
    try:
        for c in json.loads(ps) if ps else []:
            for n in c.get('names') or []:
                if n not in names and not any(k in n for k in KEEP_NAMES):
                    app += 1
                    names[n] = f'app-{app}'
        for c in json.loads(inspect) if inspect else []:
            for e in (c.get('networks') or {}).values():
                for a in e.get('aliases') or []:
                    if a not in names and not any(k in a for k in KEEP_NAMES) and not re.fullmatch(r'[0-9a-f]{12}', a):
                        app += 1
                        names[a] = f'app-{app}'
            h = c.get('hostname')
            if h and h not in names and not re.fullmatch(r'[0-9a-f]{12}', h) and not any(k in h for k in KEEP_NAMES):
                app += 1
                names[h] = f'app-{app}'
        for n in json.loads(nets) if nets else []:
            nm = n.get('name')
            if nm and nm not in names and not any(k in nm for k in KEEP_NAMES) and nm != 'podman':
                net += 1
                names[nm] = f'net-{net}'
    except json.JSONDecodeError as e:
        sys.exit(f'{src}: podman JSON not understood: {e}')

    # Domain: whatever follows the host name in an FQDN.
    fqdn = re.search(re.escape(hostname) + r'\.([a-z0-9.-]+)', text) if hostname else None
    domain = fqdn.group(1) if fqdn else None

    out = text
    for old in sorted(names, key=len, reverse=True):
        out = re.sub(r'(?<![A-Za-z0-9_.-])' + re.escape(old) + r'(?![A-Za-z0-9_-])', names[old], out)
    if domain:
        out = out.replace(domain, 'example.net')
    out = MAC_RE.sub(lambda m: map_mac(m.group(0)), out)
    out = IP_RE.sub(lambda m: map_ip(m.group(0)), out)
    out = IP6_RE.sub(lambda m: map_ip6(m.group(0)), out)
    out = UUID_RE.sub('00000000-0000-4000-8000-000000000000', out)
    # Identifiers: DMI serials and the machine id; container/network ids are
    # random and stay (the parser keys on them within one capture).
    # An empty one stays empty: a VM has none, and the collector then falls
    # back to the DMI UUID (36 characters, more than the controller keeps).
    out = re.sub(r'^(product_serial|board_serial)=(.+)$', lambda m: m.group(1) + '=' + placeholder_serial(m.group(2)), out, flags=re.M)
    block = section(out, 'macblock').strip()
    if block:
        mark = block_mark(block)
        if mark is None:
            sys.exit(f'{src}: macblock listing not understood; its dui: mark would keep the real MACs\' hash')
        out = re.sub(r'"comment": "dui:[0-9a-f]+"', f'"comment": "dui:{mark}"', out)
    mid = section(out, 'machineid').strip()
    if mid:
        out = out.replace(mid, '0' * 32)
    open(dst, 'w').write(out)
    print(f'scrubbed {src}: {len(macs)} MACs, {len(ips)} IPv4, {len(ip6s)} global IPv6, {len(lls)} link-local IPv6, {len(names)} names')


if __name__ == '__main__':
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
