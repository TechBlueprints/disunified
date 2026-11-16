#!/usr/bin/env python3
"""Scrub controller-side captures for use as fixtures, in place.

Handles three formats:
  * a device inform (JSON object) — e.g. a real switch's `last.inform` from a
    UniFi OS support bundle (unifi/devices/<type>/<mac>/last.inform);
  * the bridge's reply log (NDJSON, one record per inform: status, payload,
    effects) — inform-log/<mac>.ndjson;
  * the controller's alert log (JSON, the v2 system-log/device-alert answer):
    an alert's message is rebuilt from its message_raw and its scrubbed
    parameters, so no device, person or WLAN name stays in the text (a
    reduced capture without the parameter a message names has the names
    mapped so far replaced in it); a parameter id that is a name is scrubbed
    as that name, one that is a MAC as that MAC. Tested by
    scripts/sanitize-controller_test.py.

Keeps structure and types intact; rewrites everything that identifies a site,
a device or a person: MAC addresses (consistent mapping into 02:00:00:xx:xx:xx;
a MAC-shaped mask -- ones then zeros, as in an ACL rule -- is kept),
IPv4 (192.0.2.0/24, 198.51.100.0/24) and IPv6 (2001:db8::/32; a link-local
address, whose EUI-64 half is a real MAC, into fe80::N) addresses,
serials (a device's as its scrubbed MAC's digits, any other as SSJ0...
of the same length), hostnames/device names, and every secret or id: auth keys, the site
key, password hashes, SSH public keys, SNMP communities, TURN credentials,
UUIDs and object ids. system_cfg / mgmt_cfg text inside payloads is scrubbed
line by line the same way. cfgversion values are kept (not secret; tests
assert on them).

SSH key comments (sshd.auth.key.N.comment: an owner's e-mail address or
machine name) become key-N, numbered by first appearance within a file.

Usage: scripts/sanitize-controller.py <file>...   (writes the file back;
           .ndjson = one reply per line, .txt = one system_cfg push, else JSON)
       scripts/sanitize-controller.py [--subnet <real>/<n>=<test>/<n>]... <file>...
           keeps a routing capture's subnets: every address in <real> becomes
           the same host of <test> (a reserved test range: 198.18.0.0/15),
           and addresses already in a test range stay
       scripts/sanitize-controller.py [--ip <real>=<test>]... <file>...
           maps one address as another capture's scrub did (a reply log
           replayed against a driver capture: the switch's own address and
           its gateway must be the capture's); addresses already in a test
           range stay, and every other one is numbered past the seeded ones
       scripts/sanitize-controller.py [--mac <real>=<test>]... <file>...
           maps one MAC as another capture's scrub did, or to a placeholder
           of its own (a blocked client that must not collide with a driver
           capture's switch MAC); every other one is numbered past the seeded
       scripts/sanitize-controller.py [--subnet ...]... --text <file>...
           a host's plain-text capture (a neighbour table, `ip addr`), every
           line scrubbed; give it the --subnet mappings of the controller
           capture it is read against, so the two still agree
       scripts/sanitize-controller.py --comments-only <file>...
           rewrites only the SSH key comments, any format (.ndjson, .json,
           system_cfg .txt), every other byte untouched -- for fixtures
           scrubbed before comments were
"""
import base64, json, re, sys, ipaddress

MAC_RE = re.compile(r'\b([0-9a-f]{2}[:.-]){5}[0-9a-f]{2}\b|\b[0-9a-f]{4}\.[0-9a-f]{4}\.[0-9a-f]{4}\b', re.I)
IP_RE = re.compile(r'\b(?:\d{1,3}\.){3}\d{1,3}\b')
# Any run of hex digits and colons with at least two colons (so "fe80::1"
# with its "::" matches too); ipaddress decides whether it is an address.
IP6_RE = re.compile(r'(?<![0-9A-Za-z:.])[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}(?:/\d{1,3})?(?![0-9A-Za-z:.])')
SERIAL_RE = re.compile(r'\b[A-Z]{3}\d{8}\b')
UUID_RE = re.compile(r'\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b', re.I)
HEX24_RE = re.compile(r'\b[0-9a-f]{24}\b')   # Mongo object ids (device_id, _id, site id)
HEX32_RE = re.compile(r'\b[0-9a-f]{32}\b')   # auth keys, site key, guest tokens

SECRET_KEYS = {'_authkey', 'x_authkey', 'authkey', 'guest_token', 'password', 'x_password', 'x_secret', 'secret',
               'username', 'credential', 'community', 'psk', 'x_ssh_hostkey_fingerprint'}
ID_KEYS = {'anon_id', 'guid', 'hash_id', '_id', '_devsiteid', 'device_id', 'uuid', 'id', 'site_id',
           'unifi.anonymous_controller_id', 'unifi.anonymous_site_id', 'unifi.siteid', 'unifi.reporterid'}
HOST_KEYS = {'hostname', 'fqdn', 'name', 'uplink_device_name', 'neighborDevice', 'systemName'}
SERIAL_KEYS = {'serial', 'sfp_serial', 'serial_no', 'vendorSn'}
VERSION_KEYS = {'version', 'required_version', 'kernel_version'}
VERSION_RE = re.compile(r'\d+(\.\d+)+')
IFNAME_RE = re.compile(r'(eth|br|bond|lo|switch)\d*(\.\d+)?')

macs, ips, ip6s, hosts, ids = {}, {}, {}, {}, {}

def placeholder_serial(s):
    """A serial's documentation placeholder, the original's length (SSJ and
    zeros; an 11-character serial is SSJ00000000); an empty one stays empty.
    A fixture that changed a value's length could not carry the one that
    failed (docs/wire-shapes.md §1)."""
    return ('SSJ' + '0' * len(s))[:len(s)]

def map_mac(m):
    key = re.sub(r'[^0-9a-f]', '', m.lower())
    # A mask (contiguous ones then zeros: an ACL rule's src.mask, the
    # all-zero mask of "any") is not an address; mapping it would corrupt
    # the rule.
    if '01' not in format(int(key, 16), '048b'):
        return m
    if key not in macs:
        used, n = set(macs.values()), len(macs)
        while True:   # past any MAC --mac seeded
            n += 1
            cand = f'02:00:00:{(n>>16)&255:02x}:{(n>>8)&255:02x}:{n&255:02x}'
            if cand not in used:
                break
        macs[key] = cand
    return macs[key]

# --subnet A=B (repeatable): an address inside the real subnet A becomes the
# same host of the test subnet B, so a routing capture keeps which addresses
# share a subnet (a routed interface, its DHCP range, its next hops). With
# any --subnet given, addresses already in a reserved test range are kept as
# they are (a route to 198.51.100.0/24 stays one). Every other address is
# numbered by first appearance, as without the option.
subnets = []
seeded = False   # --ip given: test ranges are kept, as with --subnet
KEEP_NETS = [ipaddress.ip_network(n) for n in ('198.51.100.0/24', '203.0.113.0/24', '198.18.0.0/15')]

def map_ip(s):
    try:
        ip = ipaddress.ip_address(s)
    except ValueError:
        return s
    if ip.is_loopback or ip.is_multicast or ip.is_unspecified or s.startswith('255.'):
        return s
    # 0.0.0.0/8 is never a host's address; a dotted id there (an OSPF area,
    # switch.ospf.area.N.number=0.0.0.1) is kept.
    if ip.version == 4 and ip in ipaddress.ip_network('0.0.0.0/8'):
        return s
    for real, test in subnets:
        if ip.version == 4 and ip in real:
            return str(test.network_address + (int(ip) - int(real.network_address)))
    if (subnets or seeded) and any(ip in n for n in KEEP_NETS):
        return s
    if s not in ips:
        used, n = set(ips.values()), len(ips)
        while True:   # past any address --ip seeded
            n += 1
            cand = str(ipaddress.ip_address('192.0.2.0') + n) if n < 250 else str(ipaddress.ip_address('198.51.100.0') + n - 249)
            if cand not in used:
                break
        ips[s] = cand
    return ips[s]

def map_ip6(s):
    addr, _, plen = s.partition('/')
    try:
        ip = ipaddress.ip_address(addr)
    except ValueError:
        return s
    if ip.version != 6 or ip.is_loopback or ip.is_unspecified or ip.is_multicast:
        return s
    if addr not in ip6s:
        # A link-local address carries the interface's real MAC in its
        # EUI-64 half: it gets a fe80:: number of its own.
        base, n = ('fe80::', len([a for a in ip6s if a.lower().startswith('fe8')]) + 1) if ip.is_link_local else ('2001:db8::', len([a for a in ip6s if not a.lower().startswith('fe8')]) + 1)
        ip6s[addr] = str(ipaddress.ip_address(base) + n)
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

# SSH key comments (sshd.auth.key.N.comment) carry the key owner's e-mail
# address or machine name: key-N, numbered by first appearance in a file.
comments = {}

def map_comment(s):
    if not s:
        return s
    if s not in comments:
        comments[s] = f'key-{len(comments)+1}'
    return comments[s]

COMMENT_RE = re.compile(r'(sshd\.auth\.key\.\d+\.comment=)([^\\\n"]*)')

def scrub_comments_only(raw):
    """Rewrite only the SSH key comments, in any file format (ndjson, json,
    plain system_cfg text), leaving every other byte as it is -- for
    re-scrubbing a fixture that was scrubbed before comments were."""
    return COMMENT_RE.sub(lambda m: m.group(1) + map_comment(m.group(2)), raw)

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
        elif k.endswith('.secret') or k.endswith('.x_secret'):
            v = '<secret>'   # a RADIUS server's shared secret (switch.radius.auth.N.secret, 2026-11-15)
        elif re.search(r'\.(?:x_)?(?:passphrase|psk|key)$|_(?:password|passphrase|secret|key)$', k) or ('.community.' in k and k.endswith('.name')):
            v = '<secret>' if v else v   # by name, as internal/unificfg/secrets.go (SNMPv3 and the rest: not captured)
        elif re.match(r'switch\.bgp\.\d+\.raw_config_base64$', k):
            v = scrub_frr_b64(v)   # an uploaded FRR file: neighbour passwords, then the addresses in it
        elif re.match(r'sshd\.auth\.key\.\d+\.value$', k):
            v = '<pubkey>'
        elif re.match(r'sshd\.auth\.key\.\d+\.comment$', k):
            v = map_comment(v)   # e-mail addresses and machine names
        elif k in ID_KEYS or k.startswith('unifi.anonymous') or k.endswith('id'):
            v = map_id(v) if (UUID_RE.fullmatch(v) or HEX24_RE.fullmatch(v)) else v
        elif '.community.' in k and k.endswith('.name'):
            v = '<community>'
        elif re.match(r'users\.\d+\.name$', k) and v not in ('admin', 'nobody', 'root', 'ubnt'):
            v = map_host(v)
        elif re.match(r'resolv\.host\.\d+\.name$', k) and v not in ('localhost',):
            v = map_host(v)   # the device's UniFi name, spaces removed (CLAUDE.md §7)
        elif re.match(r'sshd\.auth\.key\.\d+\.name$', k):
            v = map_host(v)   # key comments carry the owner's machine name
        elif k in ('system.timezone', 'locale.timezone', 'unifi.version'):
            pass   # a POSIX TZ rule (M3.2.0) and a version look like FQDNs; neither identifies anything
        else:
            v = scrub_plain(v)
        out.append(f'{k}={v}')
    return '\n'.join(out) + ('\n' if text.endswith('\n') else '')

# The end of an FRR password line is its secret (`neighbor X password LINE`,
# `ip ospf authentication-key KEY`, `... md5 KEY`, `key-string LINE`): the
# same rule as unificfg.RedactFRR. A file the bridge already redacted
# (<redacted>) is left so.
FRR_SECRET_RE = re.compile(r'^(.*?\b(?:password|authentication-key|key-string|md5))[ \t]+(\S.*?)[ \t]*$')

def scrub_frr_b64(v):
    if not v:
        return v
    try:
        text = base64.b64decode(v, validate=True).decode()
    except Exception:
        return '<secret>'
    out = []
    for line in text.split('\n'):
        m = FRR_SECRET_RE.match(line)
        if m and not line.lstrip().startswith('!') and m.group(2) != '<redacted>':
            line = m.group(1) + ' <secret>'
        out.append(scrub_plain(line))
    return base64.b64encode('\n'.join(out).encode()).decode()

FQDN_RE = re.compile(r'\b[A-Za-z0-9][A-Za-z0-9-]*(?:\.[A-Za-z0-9][A-Za-z0-9-]*){2,}\b')

def scrub_plain(s):
    s = MAC_RE.sub(lambda m: map_mac(m.group(0)), s)
    s = FQDN_RE.sub(lambda m: m.group(0) if IP_RE.fullmatch(m.group(0)) or m.group(0).endswith(('.ubnt.com', '.ntp.org', '.ui.com')) else map_host(m.group(0)), s)
    s = IP_RE.sub(lambda m: map_ip(m.group(0)), s)
    s = IP6_RE.sub(lambda m: map_ip6(m.group(0)) if ':' in m.group(0) and re.search(r'[0-9a-f]{1,4}:[0-9a-f:]*:', m.group(0), re.I) and not MAC_RE.fullmatch(m.group(0)) else m.group(0), s)
    s = SERIAL_RE.sub(lambda m: placeholder_serial(m.group(0)), s)
    s = UUID_RE.sub(lambda m: map_id(m.group(0)), s)
    s = HEX32_RE.sub(lambda m: map_id(m.group(0)), s)
    s = HEX24_RE.sub(lambda m: map_id(m.group(0)), s)
    return s

# An id that is a MAC (an alert's DEVICE.id, UPLINK.id) is mapped as a MAC,
# so it stays one and matches the device's scrubbed mac elsewhere; a count or
# an index (PORT.id "17", CHANNEL.id "6") and an enum (ACTION.id
# VERIFY_LINK_SPEED_AND_CHECK_CABLE_DAMAGE) identify nothing and are kept.
ID_KEEP_RE = re.compile(r'[0-9]+|[A-Z][A-Z0-9_]*')

def scrub_id(o):
    if not o:
        return o
    if MAC_RE.fullmatch(o):
        return map_mac(o)
    if ID_KEEP_RE.fullmatch(o):
        return o
    return map_id(o)

# The controller's alert log (v2 system-log/device-alert): an alert's
# `message` is its `message_raw` with each {PARAM} replaced by
# parameters.PARAM.name, so the names a message carries are the parameters'.
# A parameter's `id` is sometimes its name too (CONSOLE_NAME, USER), and
# composite names repeat a device's: DEVICE_WITH_PORT is "<device> Port N",
# CONSOLE_WITH_DEVICE_NAME "<console>'s <device>". Parameters whose names
# identify nothing are listed here; every other one is a name and is mapped.
ALERT_KEEP_PARAMS = {'ACTION', 'COUNT', 'DURATION', 'PORT', 'CHANNEL', 'PREVIOUS_CHANNEL', 'RADIO_BAND',
                     'CHANNEL_WIDTH', 'AVG_INTERFERENCE', 'AVG_UTILIZATION', 'LEARN_MORE'}
PLACEHOLDER_RE = re.compile(r'\{([A-Z0-9_]+)\}')
persons, wlans = {}, {}

def map_person(s):
    if s and s not in persons:
        persons[s] = f'person-{len(persons)+1}'
    return persons.get(s, s)

def map_wlan(s):
    if s and s not in wlans:
        wlans[s] = f'wlan-{len(wlans)+1}'
    return wlans.get(s, s)

def map_alert_name(param, s):
    """A parameter's name, scrubbed as what it names."""
    if not isinstance(s, str) or not s:
        return s
    if param == 'USER':
        return map_person(s)
    if param == 'WLAN':
        return map_wlan(s)
    m = re.fullmatch(r'(.+?)( Port \d+)', s)
    if param == 'DEVICE_WITH_PORT' and m:
        return map_host(m.group(1)) + m.group(2)
    if param == 'CONSOLE_WITH_DEVICE_NAME' and "'s " in s:
        console, _, dev = s.partition("'s ")
        return map_host(console) + "'s " + map_host(dev)
    return map_host(s)

def render(raw, names):
    return PLACEHOLDER_RE.sub(lambda m: names.get(m.group(1), m.group(0)), raw)

def scrub_alert(o):
    params = o['parameters']
    real = {p: v.get('name') for p, v in params.items() if isinstance(v, dict) and isinstance(v.get('name'), str)}
    out_params = {}
    for p, v in params.items():
        if not isinstance(v, dict):
            out_params[p] = scrub(v, p)
            continue
        q = {}
        for k, x in v.items():
            if p in ALERT_KEEP_PARAMS and k in ('id', 'name'):
                q[k] = scrub_plain(x) if isinstance(x, str) and not ID_KEEP_RE.fullmatch(x) else x
            elif k == 'name':
                q[k] = map_alert_name(p, x)
            elif k == 'id' and isinstance(x, str) and x and x == v.get('name'):
                q[k] = map_alert_name(p, x)   # the id is the name (CONSOLE_NAME, USER)
            elif k == 'id' and isinstance(x, str):
                q[k] = scrub_id(x) if (MAC_RE.fullmatch(x) or ID_KEEP_RE.fullmatch(x) or UUID_RE.fullmatch(x)
                                       or HEX24_RE.fullmatch(x) or HEX32_RE.fullmatch(x)) else map_alert_name(p, x)
            else:
                q[k] = scrub(x, k)
        out_params[p] = q
    r = {}
    for k, v in o.items():
        if k == 'parameters':
            r[k] = out_params
        elif k == 'message' and isinstance(v, str):
            raw = o.get('message_raw')
            scrubbed = {p: q.get('name') for p, q in out_params.items() if isinstance(q, dict) and isinstance(q.get('name'), str)}
            if isinstance(raw, str) and render(raw, real) == v and not PLACEHOLDER_RE.search(render(raw, scrubbed)):
                r[k] = render(raw, scrubbed)
            else:
                # Not built from its parameters as expected: every name this
                # file has mapped so far is replaced, longest first.
                for name, ph in sorted(list(hosts.items()) + list(persons.items()) + list(wlans.items()), key=lambda kv: -len(kv[0])):
                    v = v.replace(name, ph)
                r[k] = scrub_plain(v)
        elif k == 'id' and isinstance(v, str):
            r[k] = scrub_id(v)
        else:
            r[k] = scrub(v, k)
    return r

def scrub(o, key=None):
    if isinstance(o, dict):
        if isinstance(o.get('parameters'), dict) and 'message_raw' in o:
            return scrub_alert(o)
        r = {k: scrub(v, k) for k, v in o.items()}
        # A device's serial is its MAC's 12 hex digits, upper case, on every
        # real UniFi device (and every bridged one: device.SerialFor), so a
        # scrubbed device keeps that shape: its scrubbed MAC's digits.
        mac = r.get('mac')
        if r.get('serial') and isinstance(mac, str) and re.fullmatch(r'([0-9a-f]{2}:){5}[0-9a-f]{2}', mac, re.I):
            r['serial'] = mac.replace(':', '').upper()
        return r
    if isinstance(o, list):
        return [scrub(v, key) for v in o]
    if isinstance(o, str):
        if key in ('system_cfg', 'mgmt_cfg'):
            return scrub_cfg_text(o)
        if key in SECRET_KEYS:
            return '<secret>' if o else o
        if key == 'hash_id' and isinstance(o, str) and o:
            # 16 hex digits on a real device: the placeholder keeps the
            # length, as a serial's does (docs/wire-shapes.md §1)
            return map_id(o)[-len(o):]
        if key in ID_KEYS:
            return scrub_id(o)
        if key in SERIAL_KEYS:
            return placeholder_serial(o)
        if key == 'chassis_id' and not MAC_RE.fullmatch(o):
            # an LLDP neighbour's chassis id of the local subtype is its
            # serial (seen on a laptop dock, 2026-11-14)
            return placeholder_serial(o)
        if key in VERSION_KEYS and VERSION_RE.fullmatch(o):
            return o   # a firmware or kernel version looks like an FQDN; it identifies nothing
        if key == 'name' and IFNAME_RE.fullmatch(o):
            return o   # an interface name (if_table's eth0), not a host
        if key in HOST_KEYS:
            return map_host(o)
        return scrub_plain(o)
    return o

args = sys.argv[1:]
while args and args[0] in ('--subnet', '--ip', '--mac'):
    real, _, test = args[1].partition('=')
    if args[0] == '--mac':
        if not MAC_RE.fullmatch(real) or not MAC_RE.fullmatch(test):
            sys.exit(f'--mac {args[1]}: not two MAC addresses')
        macs[re.sub(r'[^0-9a-f]', '', real.lower())] = test.lower()
        args = args[2:]
        continue
    if args[0] == '--ip':
        ipaddress.ip_address(real), ipaddress.ip_address(test)
        ips[real] = test
        seeded = True
        args = args[2:]
        continue
    real, test = ipaddress.ip_network(real), ipaddress.ip_network(test)
    if real.prefixlen != test.prefixlen:
        sys.exit(f'--subnet {args[1]}: the two prefix lengths differ')
    subnets.append((real, test))
    args = args[2:]
comments_only = bool(args) and args[0] == '--comments-only'
if comments_only:
    args = args[1:]
# --text: a host's text capture beside a controller one (the bridge's own
# neighbour table and addresses, read against rest/networkconf): every line
# scrubbed as plain text, with the same --subnet/--ip/--mac mappings.
text_mode = bool(args) and args[0] == '--text'
if text_mode:
    args = args[1:]
for path in args:
    comments.clear()   # key-N numbering is per file
    raw = open(path).read()
    if comments_only:
        open(path, 'w').write(scrub_comments_only(raw))
        continue
    if text_mode:
        open(path, 'w').write('\n'.join(scrub_plain(l) for l in raw.splitlines()) + ('\n' if raw.endswith('\n') else ''))
        continue
    if path.endswith('.ndjson'):
        lines = []
        for line in raw.splitlines():
            if not line.strip():
                continue
            lines.append(json.dumps(scrub(json.loads(line)), sort_keys=True))
        open(path, 'w').write('\n'.join(lines) + '\n')
    elif path.endswith('.txt'):
        # a system_cfg push saved as text (the bridge's reply log holds it as
        # the payload's system_cfg string)
        open(path, 'w').write(scrub_cfg_text(raw))
    else:
        data = json.loads(raw)
        with open(path, 'w') as f:
            json.dump(scrub(data), f, indent=2, sort_keys=True)
            f.write('\n')
if comments_only:
    print(f'rewrote SSH key comments in {len(args)} files')
else:
    print(f'scrubbed {len(args)} files: {len(macs)} MACs, {len(ips)} IPv4, {len(ip6s)} IPv6, {len(hosts)} names, {len(ids)} ids/secrets')
