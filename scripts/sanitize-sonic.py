#!/usr/bin/env python3
"""Scrub a SONiC collector capture (internal/drivers/sonic/collect.py's
tagged-section output).

Rewrites MAC addresses (a consistent mapping into 02:00:00:xx:xx:xx, upper
case kept where the original was upper case), IPv4 addresses (into
192.0.2.0/24; prefix lengths are separate fields and stay), IPv6 addresses
other than loopback (global ones into 2001:db8::/32, link-local ones into
fe80::/64 -- both carry the real MACs in their EUI-64 half), the switch's
hostname (host-1), SNMP community names (community-N), port descriptions
that carry a mapped value, and the chassis serial (SSJ00000000) when the
hardware fills it. Everything else -- build info, the hardware SKU, the
SAI counters, port OIDs -- stays byte for byte so the parser sees real
output. Lists of LLDP management addresses are rewritten element-wise by
the same patterns.

Usage: scripts/sanitize-sonic.py <in> <out>
"""
import ipaddress
import json
import re
import sys

MAC_RE = re.compile(r'(?<![0-9A-Fa-f])(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}(?![0-9A-Fa-f:])')
IP_RE = re.compile(r'(?<![0-9.])(?:\d{1,3}\.){3}\d{1,3}(?![0-9.])')
# Any run of hex digits and colons with at least two colons; ipaddress
# decides whether it is really an IPv6 address (a MAC is not).
IP6_RE = re.compile(r'(?<![0-9A-Za-z:.])[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}(?![0-9A-Za-z:.])')

macs, ips, ip6s, lls = {}, {}, {}, {}
KEEP_MACS = ('00:00:00:00:00:00', 'ff:ff:ff:ff:ff:ff')


def map_mac(m):
    key = m.lower()
    if key in KEEP_MACS:
        return m
    if key not in macs:
        n = len(macs) + 1
        macs[key] = f'02:00:00:{(n >> 16) & 255:02x}:{(n >> 8) & 255:02x}:{n & 255:02x}'
    out = macs[key]
    return out.upper() if m.upper() == m and m.lower() != m else out


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
        ip = ipaddress.IPv6Address(s)
    except ValueError:
        return s
    if ip.is_loopback or ip.is_unspecified or ip.is_multicast:
        return s
    if ip.is_link_local:
        if s not in lls:
            lls[s] = f'fe80::{len(lls) + 1:x}'
        return lls[s]
    if s not in ip6s:
        ip6s[s] = f'2001:db8::{len(ip6s) + 1:x}'
    return ip6s[s]


def section(text, name):
    m = re.search(r'^@@@ ' + re.escape(name) + r'\n(.*?)(?=^@@@ )', text, re.S | re.M)
    return m.group(1) if m else ''


def set_section(text, name, body):
    return re.sub(r'(^@@@ ' + re.escape(name) + r'\n)(.*?)(?=^@@@ )', lambda m: m.group(1) + body, text, count=1, flags=re.S | re.M)


def dump(entries):
    """The collector's own layout: one key per line, sorted."""
    keys = sorted(entries)
    if not keys:
        return '{}\n'
    return '{\n' + ',\n'.join(json.dumps(k) + ': ' + json.dumps(entries[k], sort_keys=True) for k in keys) + '\n}\n'


def main(src, dst):
    text = open(src).read()
    if '@@@ end' not in text:
        sys.exit(f'{src}: not a complete collector run (no "@@@ end")')

    # Names: the hostname (also LLDP's own system name), every SNMP
    # community (key names only in the capture, but a community is a
    # password), and the serial when the hardware reports one.
    names = {}
    hostname = section(text, 'hostname').strip()
    if hostname:
        names[hostname] = 'host-1'
    try:
        meta = json.loads(section(text, 'metadata') or '{}')
        h = (meta.get('DEVICE_METADATA|localhost') or {}).get('hostname')
        if h and h not in names:
            names[h] = 'host-1'
        snmp = json.loads(section(text, 'cfg_snmp') or '{}')
        renamed = {}
        for i, k in enumerate(sorted(snmp), 1):
            renamed['SNMP_COMMUNITY|community-%d' % i] = snmp[k]
        text = set_section(text, 'cfg_snmp', dump(renamed))
        smeta = json.loads(section(text, 'state_metadata') or '{}')
        serial = (smeta.get('DEVICE_METADATA|localhost') or {}).get('chassis_serial_number')
        if serial and serial not in ('N/A', ''):
            names[serial] = 'SSJ00000000'
    except json.JSONDecodeError as e:
        sys.exit(f'{src}: Redis section not understood: {e}')

    out = text
    for old in sorted(names, key=len, reverse=True):
        out = re.sub(r'(?<![A-Za-z0-9_.-])' + re.escape(old) + r'(?![A-Za-z0-9_-])', names[old], out)
    out = MAC_RE.sub(lambda m: map_mac(m.group(0)), out)
    out = IP_RE.sub(lambda m: map_ip(m.group(0)), out)
    out = IP6_RE.sub(lambda m: map_ip6(m.group(0)), out)

    # Port descriptions are kept unless they still carry something mapped
    # above (an address or name an operator typed into one).
    for sec, sep in (('cfg_port', '|'), ('appl_port', ':')):
        try:
            ports = json.loads(section(out, sec) or '{}')
        except json.JSONDecodeError as e:
            sys.exit(f'{src}: {sec} not understood: {e}')
        touched = False
        for k, fields in ports.items():
            d = fields.get('description', '')
            if d and (re.search(r'02:00:00:|192\.0\.2\.|2001:db8:|host-1', d)):
                fields['description'] = 'port-' + k.split(sep, 1)[-1]
                touched = True
        if touched:
            out = set_section(out, sec, dump(ports))

    open(dst, 'w').write(out)
    print(f'scrubbed {src}: {len(macs)} MACs, {len(ips)} IPv4, {len(ip6s)} global IPv6, {len(lls)} link-local IPv6, {len(names)} names')


if __name__ == '__main__':
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
