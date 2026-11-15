#!/usr/bin/env python3
"""The shapes of the values real UniFi devices hold on a controller, and an
audit of bridged devices against them (docs/wire-shapes.md).

A shape is what a scrubbed fixture cannot keep: per JSON path, the value
types, the longest string, the character classes, and whether a number is
ever negative, fractional or past 32 bits. No value is stored, so the
manifest carries nothing of the site it was read from.

  scripts/wire-shapes.py capture --bridged NAME[,NAME...] > shapes.json
      Reads stat/device (read-only) and writes the shapes of every device of
      --type (default usw) that is not one of the bridged ones.

  scripts/wire-shapes.py audit --bridged NAME[,NAME...] [--shapes FILE]
      Compares each bridged device's live object with the shapes (FILE, or
      the controller's other devices when omitted) and prints what differs.

The controller comes from the environment or --env FILE: DUI_UNIFI_URL and
DUI_UNIFI_API_KEY (the older STU_ names are read too), DUI_UNIFI_SITE
(default "default"). Nothing is written to the controller.
"""
import argparse
import json
import os
import re
import ssl
import sys
import urllib.request

# an id is 24 hex digits on Network 10, a UUID on 11 (docs/wire-shapes.md §9)
KEY_ID = re.compile(r'[0-9a-f]{24}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9]+|([0-9a-f]{2}:){5}[0-9a-f]{2}')
MAX_DEPTH = 8


def cls(s):
    """The character class of a string, coarse enough to carry no content."""
    if s == '':
        return 'empty'
    if re.fullmatch(r'[0-9]+', s):
        return 'digits'
    if re.fullmatch(r'-?[0-9.]+', s):
        return 'dotted-num'
    if re.fullmatch(r'([0-9a-f]{2}:){5}[0-9a-f]{2}', s):
        return 'mac'
    if re.fullmatch(r'[0-9a-f]+', s):
        return 'hex'
    if re.fullmatch(r'[0-9A-F]+', s):
        return 'HEX'
    if re.fullmatch(r'[\x21-\x7e]*', s):
        return 'ascii'
    if re.fullmatch(r'[\x20-\x7e]*', s):
        return 'ascii-space'
    return 'non-ascii'


def typ(v):
    if v is None:
        return 'null'
    if isinstance(v, bool):
        return 'bool'
    if isinstance(v, (int, float)):
        return 'num'
    if isinstance(v, str):
        return 'string'
    if isinstance(v, list):
        return 'array'
    return 'object'


def norm_key(k):
    """Keys that are ids, indexes or MACs become <key>; port_16-rx_bytes
    becomes port_<n>-rx_bytes."""
    if KEY_ID.fullmatch(k):
        return '<key>'
    return re.sub(r'(?<=_)[0-9]+(?=-)', '<n>', k)


def walk(v, path, out, depth=0):
    e = out.setdefault(path, {'types': set(), 'strlen': 0, 'charset': set(), 'neg': False, 'frac': False, 'gt_int32': False})
    t = typ(v)
    e['types'].add(t)
    if t == 'string':
        e['strlen'] = max(e['strlen'], len(v))
        e['charset'].add(cls(v))
    elif t == 'num':
        e['neg'] = e['neg'] or v < 0
        e['frac'] = e['frac'] or (isinstance(v, float) and v != int(v))
        e['gt_int32'] = e['gt_int32'] or abs(v) > 2**31 - 1
    if depth >= MAX_DEPTH:
        return
    if t == 'array':
        for x in v:
            walk(x, path + '[]', out, depth + 1)
    elif t == 'object':
        for k, x in v.items():
            walk(x, (path + '.' if path else '') + norm_key(k), out, depth + 1)


# Credentials the controller keeps on a record (Network 11 adds the adoption
# login): even a length says something about a secret, and no bridged
# payload carries them, so their shapes are not kept.
SECRET_PATHS = re.compile(r'x_adopt_password|x_adopt_username|x_authkey|.*password')


def shapes_of(devs):
    out = {}
    for d in devs:
        walk(d, '', out)
    out.pop('', None)
    return {p: e for p, e in out.items() if not SECRET_PATHS.fullmatch(p)}


def to_json(shapes):
    return {p: {'types': sorted(e['types']), 'strlen': e['strlen'], 'charset': sorted(e['charset']),
                'neg': e['neg'], 'frac': e['frac'], 'gt_int32': e['gt_int32']} for p, e in sorted(shapes.items())}


def from_json(paths):
    return {p: {'types': set(e['types']), 'strlen': e['strlen'], 'charset': set(e['charset']),
                'neg': e['neg'], 'frac': e['frac'], 'gt_int32': e['gt_int32']} for p, e in paths.items()}


def differences(mine, ref):
    """(kind, path, note) for every path of mine that real devices hold
    differently. Paths no real device has are the contract tests' business."""
    out = []
    for p, m in sorted(mine.items()):
        r = ref.get(p)
        if r is None:
            continue
        extra = m['types'] - r['types'] - {'null'}
        if extra:
            out.append(('type', p, '%s (real: %s)' % ('/'.join(sorted(extra)), '/'.join(sorted(r['types'])))))
            continue
        if 'string' in m['types'] and 'string' in r['types']:
            if m['strlen'] > r['strlen']:
                out.append(('longer', p, '%d > %d' % (m['strlen'], r['strlen'])))
            new = m['charset'] - r['charset'] - {'empty'}
            # a narrower class inside a wider one real devices use is no difference
            # (internal/device/shapes_test.go mirrors this; keep the two identical)
            if 'ascii-space' in r['charset'] or 'non-ascii' in r['charset']:
                new -= {'ascii', 'digits', 'dotted-num', 'hex', 'HEX', 'mac'}
            if 'non-ascii' in r['charset']:
                new -= {'ascii-space'}
            if 'ascii' in r['charset']:
                new -= {'digits', 'dotted-num', 'hex', 'HEX', 'mac'}
            # a hex string can be all digits (a MAC-derived serial, 0011223344..)
            if 'dotted-num' in r['charset'] or 'hex' in r['charset'] or 'HEX' in r['charset']:
                new -= {'digits'}
            if new:
                out.append(('charset', p, '%s (real: %s)' % ('/'.join(sorted(new)), '/'.join(sorted(r['charset'])))))
        if 'num' in m['types'] and 'num' in r['types']:
            for flag, word in (('neg', 'negative'), ('frac', 'fractional'), ('gt_int32', 'past 32 bits')):
                if m[flag] and not r[flag]:
                    out.append(('number', p, word + ', never on a real device'))
    return out


def fetch(args):
    env = dict(os.environ)
    if args.env:
        for line in open(args.env):
            line = line.strip()
            if '=' in line and not line.startswith('#'):
                k, v = line.split('=', 1)
                env[k.replace('export ', '').strip()] = v.strip().strip('"').strip("'")
    url = env.get('DUI_UNIFI_URL') or env.get('STU_UNIFI_URL')
    key = env.get('DUI_UNIFI_API_KEY') or env.get('STU_UNIFI_API_KEY')
    site = env.get('DUI_UNIFI_SITE') or env.get('STU_UNIFI_SITE') or 'default'
    if not url or not key:
        sys.exit('set DUI_UNIFI_URL and DUI_UNIFI_API_KEY (or pass --env FILE)')
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE  # a console's own certificate
    req = urllib.request.Request(url.rstrip('/') + '/proxy/network/api/s/' + site + '/stat/device',
                                 headers={'X-API-KEY': key, 'Accept': 'application/json'})
    return json.load(urllib.request.urlopen(req, context=ctx, timeout=30))['data']


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument('mode', choices=['capture', 'audit'])
    ap.add_argument('--bridged', required=True, help='names of the bridged devices on the controller, comma-separated')
    ap.add_argument('--env', help='a file of KEY=value lines to read the controller from')
    ap.add_argument('--type', default='usw', help='capture: the device type to take shapes from (default usw)')
    ap.add_argument('--shapes', help='audit: a captured manifest (default: the controller\'s other devices, every type)')
    args = ap.parse_args()
    bridged = {n.strip() for n in args.bridged.split(',') if n.strip()}
    devs = fetch(args)
    real = [d for d in devs if d.get('name') not in bridged]
    if args.mode == 'capture':
        real = [d for d in real if d.get('type') == args.type]
        json.dump({'source': 'stat/device', 'type': args.type, 'devices': len(real), 'paths': to_json(shapes_of(real))},
                  sys.stdout, indent=1, sort_keys=True)
        print()
        return
    ref = from_json(json.load(open(args.shapes))['paths']) if args.shapes else shapes_of(real)
    found = 0
    for d in devs:
        if d.get('name') not in bridged:
            continue
        diffs = differences(shapes_of([d]), ref)
        found += len(diffs)
        print('%s (%s): %d difference(s)' % (d.get('name'), d.get('model'), len(diffs)))
        for kind, p, note in diffs:
            print('  %-8s %-52s %s' % (kind, p, note))
    sys.exit(1 if found else 0)


if __name__ == '__main__':
    main()
