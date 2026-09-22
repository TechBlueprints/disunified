#!/usr/bin/env python3
"""Scrub a text fixture (snmpwalk output from an APC rack PDU).

Rewrites MAC addresses (consistent mapping into 02:00:00:xx:xx:xx, the
locally administered range), IPv4 addresses (into 192.0.2.0/24), the card's
serial number, and the device/host names that identify a site, keeping every
other byte so parsers see real output.

Usage: scripts/sanitize-apc.py <in> <out>
"""
import re, sys, ipaddress

# Rows of ipAddrTable (4.20), ipRouteTable (4.21) and ipNetToMediaTable (4.22)
# end in a dotted-quad address (4.22 rows carry an ifIndex before it).
IPMIB_ROW_RE = re.compile(r'^(\.?1\.3\.6\.1\.2\.1\.4\.2[0-2]\.1\.\d+\.(?:\d+\.)?)((?:\d{1,3}\.){3}\d{1,3})$')
MAC_RE = re.compile(r'\b(?:[0-9A-Fa-f]{1,2}:){5}[0-9A-Fa-f]{1,2}\b')
IP_RE = re.compile(r'\b(?:\d{1,3}\.){3}\d{1,3}\b')
# A value that is a hostname or FQDN: the PDU's own name shows up in sysName.0,
# rPDUIdentName (318.1.1.12.1.1.0) and inside trap/event strings.
HOSTNAME_RE = re.compile(r'\b[A-Za-z0-9][A-Za-z0-9-]*(?:\.[A-Za-z0-9-]+)+\b')
# APC serials are printed bare in the serial OID and as "SN: <serial>" in sysDescr.
SERIAL_RE = re.compile(r'\b[A-Z]{2}\d{10}\b')

macs, ips, hosts = {}, {}, {}

def map_mac(m):
    # snmpwalk prints ifPhysAddress with unpadded octets (0:c0:b7:...); normalise
    # to compare, but keep the caller's formatting decision simple and stable.
    key = ':'.join(f'{int(p, 16):02x}' for p in m.group(0).split(':'))
    if key not in macs:
        n = len(macs) + 1
        macs[key] = f'02:00:00:{(n >> 16) & 255:02x}:{(n >> 8) & 255:02x}:{n & 255:02x}'
    return macs[key]

def map_ip(m):
    s = m.group(0)
    try:
        ip = ipaddress.ip_address(s)
    except ValueError:
        return s
    if ip.is_loopback or ip.is_multicast or ip.is_unspecified or s.startswith('255.'):
        return s
    # Netmasks and OID-looking dotted quads inside an OID path are left alone by
    # the caller (we only run this on value text, not on the OID column).
    if s not in ips:
        ips[s] = str(ipaddress.ip_address('192.0.2.0') + len(ips) + 1)
    return ips[s]

def map_host(m):
    s = m.group(0)
    # Leave MIB module names, OID paths and firmware file names that contain dots.
    if s.startswith(('SNMPv2', 'IF-MIB', 'DISMAN', 'SNMP-', 'apc_hw', 'v3.', 'v4.')):
        return s
    if re.fullmatch(r'[\d.]+', s):      # a bare OID / version number
        return s
    if not re.search(r'[A-Za-z]{2}', s.split('.')[-1]):   # last label must look like a TLD
        return s
    if s not in hosts:
        hosts[s] = f'pdu-{len(hosts) + 1}.example.net'
    return hosts[s]

out = []
for line in open(sys.argv[1], encoding='utf-8', errors='replace'):
    line = line.rstrip('\n')
    # Split the OID column from the value so dotted OIDs are never rewritten --
    # with one exception. The IP-MIB address, route and ARP tables index their
    # rows BY ADDRESS, so a real address sits in the OID suffix itself
    # (.1.3.6.1.2.1.4.22.1.2.<ifIndex>.10.0.0.1). Those rows get the same
    # mapping as the value column, so a row's key and any value naming the
    # same address still agree after scrubbing.
    if ' = ' in line:
        oid, _, value = line.partition(' = ')
        m = IPMIB_ROW_RE.match(oid)
        if m:
            oid = m.group(1) + map_ip(re.match(r'(?:\d{1,3}\.){3}\d{1,3}', m.group(2)))
        value = SERIAL_RE.sub('SSJ00000000', value)
        value = MAC_RE.sub(map_mac, value)
        # An OID-valued line carries a MIB path, never a site identifier, and a
        # numeric OID is indistinguishable from a dotted quad (.1.3.6.1... reads
        # as an address). Rewriting it would destroy the value the parser reads
        # -- sysObjectID is how the device names its own model.
        if not value.startswith('OID:'):
            value = IP_RE.sub(map_ip, value)
            value = HOSTNAME_RE.sub(map_host, value)
        line = f'{oid} = {value}'
    out.append(line)

with open(sys.argv[2], 'w', encoding='utf-8') as f:
    f.write('\n'.join(out) + '\n')
