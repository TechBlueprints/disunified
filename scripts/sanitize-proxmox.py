#!/usr/bin/env python3
"""Scrub a text fixture (the Proxmox collector's tagged-section output).

Rewrites MAC addresses (consistent mapping into 02:00:00:xx:xx:xx, the
locally administered range), IPv4 addresses (into 192.0.2.0/24), DMI and
optic serial numbers, and VM/container names (name: <n> -> name: vm-<n>),
keeping every other byte so parsers see real output.

Usage: scripts/sanitize-proxmox.py <in> <out>
"""
import re, sys, ipaddress

MAC_RE = re.compile(r'\b([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}\b')
BRIDGE_ID_RE = re.compile(r'\b([0-9a-f]{4})\.([0-9a-f]{12})\b')
IP_RE = re.compile(r'\b(?:\d{1,3}\.){3}\d{1,3}\b')

macs, ips, names = {}, {}, {}

def map_mac(m):
    key = m.lower()
    if key not in macs:
        n = len(macs) + 1
        macs[key] = f'02:00:00:{(n>>16)&255:02x}:{(n>>8)&255:02x}:{n&255:02x}'
    out = macs[key]
    return out.upper() if m.isupper() else out

def map_ip(s):
    try:
        ip = ipaddress.ip_address(s)
    except ValueError:
        return s
    if ip.is_loopback or ip.is_multicast or ip.is_unspecified or s.startswith('255.'):
        return s
    if s not in ips:
        ips[s] = str(ipaddress.ip_address('192.0.2.0') + len(ips) + 1)
    return ips[s]

def map_name(s):
    if s not in names:
        names[s] = f'vm-{len(names)+1}'
    return names[s]

out = []
for line in open(sys.argv[1], encoding='utf-8', errors='replace'):
    line = line.rstrip('\n')
    line = MAC_RE.sub(lambda m: map_mac(m.group(0)), line)
    line = BRIDGE_ID_RE.sub(lambda m: m.group(1) + '.' + map_mac(':'.join(m.group(2)[i:i+2] for i in range(0, 12, 2))).replace(':', ''), line)
    line = IP_RE.sub(lambda m: map_ip(m.group(0)), line)
    line = re.sub(r'^(product_serial|board_serial)=.*', r'\1=SCRUBBED', line)
    line = re.sub(r'^(\s*Vendor SN\s*:\s*).*', r'\1SCRUBBED', line)
    line = re.sub(r'^(name|hostname): (.*)$', lambda m: f'{m.group(1)}: {map_name(m.group(2))}', line)
    out.append(line)
open(sys.argv[2], 'w').write('\n'.join(out) + '\n')
