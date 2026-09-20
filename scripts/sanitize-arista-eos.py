#!/usr/bin/env python3
"""Scrub identifying values from captured EOS JSON fixtures, in place.

Keeps structure and types intact so parsers can be tested against real output,
but rewrites: MAC addresses (consistent per-MAC mapping into 02:00:00:xx:xx:xx),
the chassis serial, IPv4 addresses (into 192.0.2.0/24 and 198.51.100.0/24 test
nets, consistent per-address), and LLDP neighbor hostnames.

Usage: scripts/sanitize-arista-eos.py docs/fixtures/<eos-version>/*.json
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

def map_host(s):
    if s not in hosts:
        hosts[s] = f'neighbor-{len(hosts)+1}'
    return hosts[s]

def scrub_str(s, key=None):
    if key in ('neighborDevice', 'systemName', 'hostname', 'fqdn') and s and s != 'localhost':
        return map_host(s)
    if key in ('systemDescription', 'portDescription', 'neighborPortDescription', 'description', 'vendorSn') and s:
        return 'scrubbed'
    s = MAC_RE.sub(lambda m: map_mac(m.group(0)), s)
    s = IP_RE.sub(lambda m: map_ip(m.group(0)), s)
    s = SERIAL_RE.sub('SSJ00000000', s)
    return s

def scrub(o, key=None):
    if isinstance(o, dict):
        return {scrub_str(k): scrub(v, k) for k, v in o.items()}
    if isinstance(o, list):
        return [scrub(v, key) for v in o]
    if isinstance(o, str):
        return scrub_str(o, key)
    return o

for path in sys.argv[1:]:
    with open(path) as f:
        data = json.load(f)
    with open(path, 'w') as f:
        json.dump(scrub(data), f, indent=2, sort_keys=True)
        f.write('\n')
print(f'scrubbed {len(sys.argv)-1} files: {len(macs)} MACs, {len(ips)} IPs, {len(hosts)} hostnames')
