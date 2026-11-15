#!/usr/bin/env python3
"""Scrub identifying values from captured EOS JSON fixtures, in place.

Keeps structure and types intact so parsers can be tested against real output,
but rewrites: MAC addresses (consistent per-MAC mapping into 02:00:00:xx:xx:xx),
the chassis serial, IPv4 addresses (into 192.0.2.0/24 and 198.51.100.0/24 test
nets, consistent per-address), and LLDP neighbor hostnames.

Usage: scripts/sanitize-arista-eos.py docs/fixtures/<eos-version>/*.json [*.txt]

A .txt capture (`phy detail`) gets its transceiver serial, MACs and IPv4
addresses scrubbed with the same mapping.

The mapping lives for one run only: captures a test reads together (two states
of one switch, a scenario's overrides) must be scrubbed in one invocation, or the
same real MAC comes out as two different placeholders.
"""
import json, re, sys, ipaddress

MAC_RE = re.compile(r'\b([0-9a-f]{2}[:.]){5}[0-9a-f]{2}\b|\b[0-9a-f]{4}\.[0-9a-f]{4}\.[0-9a-f]{4}\b', re.I)
IP_RE  = re.compile(r'\b(?:\d{1,3}\.){3}\d{1,3}\b')
SERIAL_RE = re.compile(r'\b[A-Z]{3}\d{8}\b')

macs, ips, hosts = {}, {}, {}

def map_mac(m):
    key = re.sub(r'[^0-9a-f]', '', m.lower())
    if key not in macs:
        n = len(macs) + 1
        macs[key] = f'02:00:00:{(n>>16)&255:02x}:{(n>>8)&255:02x}:{n&255:02x}'
    return macs[key]

def map_ip(s):
    try:
        ip = ipaddress.ip_address(s)
    except ValueError:
        return s
    if ip.is_loopback or ip.is_multicast or ip.is_unspecified or str(ip).startswith('255.'):
        return s
    if s not in ips:
        n = len(ips) + 1
        ips[s] = str(ipaddress.ip_address('192.0.2.0') + n) if n < 250 else str(ipaddress.ip_address('198.51.100.0') + n - 249)
    return ips[s]

ip6s = {}

def map_ip6(s):
    """A whole-string IPv6 address (an LLDP neighbour's management address):
    global ones into 2001:db8::/32, link-local into fe80::N (its EUI-64 half
    is the interface's MAC). Consistent per address within a run."""
    try:
        ip = ipaddress.IPv6Address(s)
    except ValueError:
        return None
    if ip.is_loopback or ip.is_unspecified or ip.is_multicast or ip in ipaddress.IPv6Network('2001:db8::/32'):
        return s
    if s not in ip6s:
        n = len(ip6s) + 1
        ip6s[s] = f'fe80::{n:x}' if ip.is_link_local else f'2001:db8::{n:x}'
    return ip6s[s]

def map_host(s):
    if s not in hosts:
        hosts[s] = f'neighbor-{len(hosts)+1}'
    return hosts[s]

def placeholder_serial(s):
    """A serial's documentation placeholder, the original's length (SSJ and
    zeros; an 11-character serial is SSJ00000000); an empty one stays empty.
    A fixture that changed a value's length could not carry the one that
    failed (docs/wire-shapes.md §1)."""
    return ('SSJ' + '0' * len(s))[:len(s)]

# Running-config lines (`show running-config`'s section keys) that name the
# switch or carry a credential: the earlier captures had these cleaned by
# hand (2026-11-15, the empty-po captures were the first to need the script).
SECRET_RE = re.compile(r'^(username \S+ .*secret \S+ )\S+$')
SSHKEY_RE = re.compile(r'^(username \S+ ssh-key (?:secondary )?\S+ )\S+(?: .*)?$')
HOSTLINE_RE = re.compile(r'^(hostname|dns domain|ip domain-name) \S+$')

def scrub_config_line(s):
    m = SECRET_RE.match(s)
    if m:
        return m.group(1) + '<hash>'
    m = SSHKEY_RE.match(s)
    if m:
        return m.group(1) + '<pubkey>'
    m = HOSTLINE_RE.match(s)
    if m:
        return m.group(1) + (' switch' if m.group(1) == 'hostname' else ' example.net')
    return s

def scrub_str(s, key=None):
    s = scrub_config_line(s)
    if key in ('neighborDevice', 'systemName', 'hostname', 'fqdn') and s and s != 'localhost':
        return map_host(s)
    if key == 'vendorSn' or (key == 'serialNum' and s not in ('N/A', '')):
        # `show inventory`'s serialNum is every module's, PSU's and fan's
        # serial: it was kept as captured until 2026-11-15.
        return placeholder_serial(s)
    if key in ('systemDescription', 'portDescription', 'neighborPortDescription', 'description') and s:
        return 'scrubbed'
    v6 = map_ip6(s)
    if v6 is not None:
        return v6
    s = MAC_RE.sub(lambda m: map_mac(m.group(0)), s)
    s = IP_RE.sub(lambda m: map_ip(m.group(0)), s)
    s = SERIAL_RE.sub(lambda m: placeholder_serial(m.group(0)), s)
    return s

def scrub(o, key=None):
    if isinstance(o, dict):
        return {scrub_str(k): scrub(v, k) for k, v in o.items()}
    if isinstance(o, list):
        return [scrub(v, key) for v in o]
    if isinstance(o, str):
        return scrub_str(o, key)
    return o

# `show interfaces X phy detail` is text: its "Transceiver SN" row is the
# module's serial (kept as captured until 2026-11-15).
TEXT_SN_RE = re.compile(r'^(\s*Transceiver SN\s+)(\S+)', re.M)

for path in sys.argv[1:]:
    if path.endswith('.txt'):
        with open(path) as f:
            text = f.read()
        text = TEXT_SN_RE.sub(lambda m: m.group(1) + placeholder_serial(m.group(2)), text)
        text = MAC_RE.sub(lambda m: map_mac(m.group(0)), text)
        text = IP_RE.sub(lambda m: map_ip(m.group(0)), text)
        with open(path, 'w') as f:
            f.write(text)
        continue
    with open(path) as f:
        data = json.load(f)
    with open(path, 'w') as f:
        json.dump(scrub(data), f, indent=2, sort_keys=True)
        f.write('\n')
print(f'scrubbed {len(sys.argv)-1} files: {len(macs)} MACs, {len(ips)} IPs, {len(hosts)} hostnames')
