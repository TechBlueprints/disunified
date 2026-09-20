#!/usr/bin/env python3
"""Scrub controller-side captures for use as fixtures, in place.

Handles two formats:
  * a device inform (JSON object) — e.g. a real switch's `last.inform` from a
    UniFi OS support bundle (unifi/devices/<type>/<mac>/last.inform);
  * the bridge's reply log (NDJSON, one record per inform: status, payload,
    effects) — inform-log/<mac>.ndjson.

Keeps structure and types intact; rewrites everything that identifies a site,
a device or a person: MAC addresses (consistent mapping into 02:00:00:xx:xx:xx),
IPv4 (192.0.2.0/24, 198.51.100.0/24) and IPv6 (2001:db8::/32) addresses,
serials, hostnames/device names, and every secret or id: auth keys, the site
key, password hashes, SSH public keys, SNMP communities, TURN credentials,
UUIDs and object ids. system_cfg / mgmt_cfg text inside payloads is scrubbed
line by line the same way. cfgversion values are kept (not secret; tests
assert on them).

Usage: scripts/sanitize-captures.py <file>...   (writes the file back)
"""
import json, re, sys, ipaddress

MAC_RE = re.compile(r'\b([0-9a-f]{2}[:.-]){5}[0-9a-f]{2}\b|\b[0-9a-f]{4}\.[0-9a-f]{4}\.[0-9a-f]{4}\b', re.I)
IP_RE = re.compile(r'\b(?:\d{1,3}\.){3}\d{1,3}\b')
IP6_RE = re.compile(r'\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{0,4}(?:/\d{1,3})?\b', re.I)
SERIAL_RE = re.compile(r'\b[A-Z]{3}\d{8}\b')
UUID_RE = re.compile(r'\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b', re.I)
HEX24_RE = re.compile(r'\b[0-9a-f]{24}\b')   # Mongo object ids (device_id, _id, site id)
HEX32_RE = re.compile(r'\b[0-9a-f]{32}\b')   # auth keys, site key, guest tokens

SECRET_KEYS = {'_authkey', 'x_authkey', 'authkey', 'guest_token', 'password', 'x_password',
               'username', 'credential', 'community', 'psk', 'x_ssh_hostkey_fingerprint'}
ID_KEYS = {'anon_id', 'guid', 'hash_id', '_id', '_devsiteid', 'device_id', 'uuid', 'id', 'site_id',
           'unifi.anonymous_controller_id', 'unifi.anonymous_site_id', 'unifi.siteid', 'unifi.reporterid'}
HOST_KEYS = {'hostname', 'fqdn', 'name', 'uplink_device_name', 'neighborDevice', 'systemName'}
SERIAL_KEYS = {'serial', 'sfp_serial', 'serial_no', 'vendorSn'}

macs, ips, ip6s, hosts, ids = {}, {}, {}, {}, {}

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
    if ip.is_loopback or ip.is_multicast or ip.is_unspecified or s.startswith('255.'):
        return s
    if s not in ips:
        n = len(ips) + 1
        ips[s] = str(ipaddress.ip_address('192.0.2.0') + n) if n < 250 else str(ipaddress.ip_address('198.51.100.0') + n - 249)
    return ips[s]

def map_ip6(s):
    addr, _, plen = s.partition('/')
    try:
        ip = ipaddress.ip_address(addr)
    except ValueError:
        return s
    if ip.version != 6 or ip.is_link_local and False:
        return s
    if addr not in ip6s:
        ip6s[addr] = str(ipaddress.ip_address('2001:db8::') + len(ip6s) + 1)
    return ip6s[addr] + ('/' + plen if plen else '')

def map_host(s):
    if not s or s in ('localhost', 'UBNT', 'unifi'):
        return s
    if s not in hosts:
        hosts[s] = f'device-{len(hosts)+1}'
    return hosts[s]

def map_id(s):
    if s not in ids:
        ids[s] = f'{len(ids)+1:032x}'
    return ids[s]

def scrub_cfg_text(text):
    out = []
    for line in text.splitlines():
        k, sep, v = line.partition('=')
        if not sep:
            out.append(line); continue
        if k == 'unifi.key' or k.endswith('.authkey'):
            v = '<key>'
        elif k.endswith('.password'):
            v = '<hash>'
        elif re.match(r'sshd\.auth\.key\.\d+\.value$', k):
            v = '<pubkey>'
        elif k in ID_KEYS or k.startswith('unifi.anonymous') or k.endswith('id'):
            v = map_id(v) if (UUID_RE.fullmatch(v) or HEX24_RE.fullmatch(v)) else v
        elif '.community.' in k and k.endswith('.name'):
            v = '<community>'
        elif re.match(r'users\.\d+\.name$', k) and v not in ('admin', 'nobody', 'root', 'ubnt'):
            v = map_host(v)
        elif re.match(r'sshd\.auth\.key\.\d+\.name$', k):
            v = map_host(v)   # key comments carry the owner's machine name
        else:
            v = scrub_plain(v)
        out.append(f'{k}={v}')
    return '\n'.join(out) + ('\n' if text.endswith('\n') else '')

FQDN_RE = re.compile(r'\b[A-Za-z0-9][A-Za-z0-9-]*(?:\.[A-Za-z0-9][A-Za-z0-9-]*){2,}\b')

def scrub_plain(s):
    s = MAC_RE.sub(lambda m: map_mac(m.group(0)), s)
    s = FQDN_RE.sub(lambda m: m.group(0) if IP_RE.fullmatch(m.group(0)) or m.group(0).endswith(('.ubnt.com', '.ntp.org', '.ui.com')) else map_host(m.group(0)), s)
    s = IP_RE.sub(lambda m: map_ip(m.group(0)), s)
    s = IP6_RE.sub(lambda m: map_ip6(m.group(0)) if ':' in m.group(0) and re.search(r'[0-9a-f]{1,4}:[0-9a-f:]*:', m.group(0), re.I) and not MAC_RE.fullmatch(m.group(0)) else m.group(0), s)
    s = SERIAL_RE.sub('SSJ00000000', s)
    s = UUID_RE.sub(lambda m: map_id(m.group(0)), s)
    s = HEX32_RE.sub(lambda m: map_id(m.group(0)), s)
    s = HEX24_RE.sub(lambda m: map_id(m.group(0)), s)
    return s

def scrub(o, key=None):
    if isinstance(o, dict):
        return {k: scrub(v, k) for k, v in o.items()}
    if isinstance(o, list):
        return [scrub(v, key) for v in o]
    if isinstance(o, str):
        if key in ('system_cfg', 'mgmt_cfg'):
            return scrub_cfg_text(o)
        if key in SECRET_KEYS:
            return '<secret>' if o else o
        if key in ID_KEYS:
            return map_id(o) if o else o
        if key in SERIAL_KEYS:
            return 'SSJ00000000' if o else o
        if key in HOST_KEYS:
            return map_host(o)
        return scrub_plain(o)
    return o

for path in sys.argv[1:]:
    raw = open(path).read()
    if path.endswith('.ndjson'):
        lines = []
        for line in raw.splitlines():
            if not line.strip():
                continue
            lines.append(json.dumps(scrub(json.loads(line)), sort_keys=True))
        open(path, 'w').write('\n'.join(lines) + '\n')
    else:
        data = json.loads(raw)
        with open(path, 'w') as f:
            json.dump(scrub(data), f, indent=2, sort_keys=True)
            f.write('\n')
print(f'scrubbed {len(sys.argv)-1} files: {len(macs)} MACs, {len(ips)} IPv4, {len(ip6s)} IPv6, {len(hosts)} names, {len(ids)} ids/secrets')
