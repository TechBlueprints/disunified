#!/usr/bin/env python3
"""Scrub a SONiC collector capture (internal/drivers/sonic/collect.py's
tagged-section output).

Rewrites MAC addresses (a consistent mapping into 02:00:00:xx:xx:xx, upper
case kept where the original was upper case), IPv4 addresses (into
192.0.2.0/24; prefix lengths are separate fields and stay; IPv4
link-local 169.254/16 is kept: it names nothing), IPv6 addresses
other than loopback (global ones into 2001:db8::/32, link-local ones into
fe80::/64 -- both carry the real MACs in their EUI-64 half), the switch's
hostname (host-1), SNMP community names (community-N, the same name in
CONFIG_DB's SNMP_COMMUNITY and in the snmp.yml section), port descriptions
that carry a mapped value, and every serial the hardware fills in -- the
chassis, the PSUs, the fans, the optics (SSJ00000000, an optic's trailing
padding kept). Hardware captures also carry an LLDP neighbour's system
name (a site device name: neighbor-N) and the bridge's managed block of
authorized_keys (the site's public keys: each becomes
"<type> SCRUBBEDKEY<N> key-N"). Everything else -- build info, the
hardware SKU, the SAI counters, port OIDs -- stays byte for byte so the
parser sees real output. Lists of LLDP management addresses are rewritten
element-wise by the same patterns.

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
    if ip.is_loopback or ip.is_unspecified or ip.is_multicast or s.startswith('255.') or ip.is_link_local:
        return s  # IPv4 link-local (169.254/16) names no site and carries no MAC
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
        # One name per community across CONFIG_DB and snmp.yml, so the
        # parser still sees which of the file's communities are imported.
        snmp = json.loads(section(text, 'cfg_snmp') or '{}')
        yml_body = section(text, 'snmp_yml').strip()
        yml = json.loads(yml_body) if yml_body.startswith('{') else None
        communities = sorted({k.split('|', 1)[1] for k in snmp} | set(yml or {}))
        cname = {c: 'community-%d' % i for i, c in enumerate(communities, 1)}
        text = set_section(text, 'cfg_snmp', dump({'SNMP_COMMUNITY|' + cname[k.split('|', 1)[1]]: v for k, v in snmp.items()}))
        if yml is not None:
            text = set_section(text, 'snmp_yml', dump({cname[k]: v for k, v in yml.items()}))
        smeta = json.loads(section(text, 'state_metadata') or '{}')
        serial = (smeta.get('DEVICE_METADATA|localhost') or {}).get('chassis_serial_number')
        if serial and serial not in ('N/A', ''):
            names[serial] = 'SSJ00000000'
        # Hardware serials (the optics pad theirs with spaces; the padding stays).
        for sec in ('xcvr', 'psu', 'fan', 'chassis'):
            t = json.loads(section(text, sec) or '{}')
            touched = False
            for fields in t.values():
                v = fields.get('serial', '')
                if v.strip() not in ('', 'N/A'):
                    fields['serial'] = 'SSJ00000000' + v[len(v.rstrip()):]
                    touched = True
            if touched:
                text = set_section(text, sec, dump(t))
        # An LLDP neighbour's system name is a site device name.
        lldp = json.loads(section(text, 'lldp') or '{}')
        nb = {}
        for fields in lldp.values():
            n = fields.get('lldp_rem_sys_name', '')
            if n:
                fields['lldp_rem_sys_name'] = nb.setdefault(n, 'neighbor-%d' % (len(nb) + 1))
        if nb:
            text = set_section(text, 'lldp', dump(lldp))
        # The switch's own LLDP name is its FQDN once a DNS domain is set
        # ("<hostname>.<site domain>"): the domain goes too.
        loc = json.loads(section(text, 'lldp_loc') or '{}')
        touched = False
        for fields in loc.values():
            n = fields.get('lldp_loc_sys_name', '')
            if '.' in n:
                fields['lldp_loc_sys_name'] = n.split('.', 1)[0] + '.example.net'
                touched = True
        if touched:
            text = set_section(text, 'lldp_loc', dump(loc))
    except json.JSONDecodeError as e:
        sys.exit(f'{src}: Redis section not understood: {e}')

    # The bridge's block of authorized_keys holds the site's public keys.
    keys = []
    for i, line in enumerate(section(text, 'authorized_keys').splitlines(), 1):
        f = line.split()
        if f:
            keys.append(f'{f[0]} SCRUBBEDKEY{i}' + (f' key-{i}' if len(f) > 2 else ''))
    text = set_section(text, 'authorized_keys', ''.join(k + '\n' for k in keys))

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
            if d and (re.search(r'02:00:00:|192\.0\.2\.|2001:db8:|host-1|neighbor-', d)):
                fields['description'] = 'port-' + k.split(sep, 1)[-1]
                touched = True
        if touched:
            out = set_section(out, sec, dump(ports))

    open(dst, 'w').write(out)
    print(f'scrubbed {src}: {len(macs)} MACs, {len(ips)} IPv4, {len(ip6s)} global IPv6, {len(lls)} link-local IPv6, {len(names)} names, {len(keys)} keys')


if __name__ == '__main__':
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
