#!/usr/bin/env python3
"""Tests for scripts/sanitize-controller.py on the controller's alert log
(v2 system-log/device-alert), over the committed alert fixtures.

Each fixture is a real capture, already scrubbed. The test gives it its
identifiers back -- every placeholder name, MAC, address and id replaced by
a site-looking value assembled at run time (so this file passes
check-site-info.sh) -- runs the scrub script on that, and requires:

  * none of the site-looking values is left anywhere in the output;
  * the output is the committed fixture again, up to the numbering of the
    placeholders (a fixture scrubbed in one run with another file starts at
    device-2): every name inside a message, a composite name
    ("<device> Port N", "<console>'s <device>") and an id that is a name, a
    MAC under `id` as the device's scrubbed MAC, a port's and a count's id
    kept;
  * every message is its message_raw with the parameters' names, where the
    fixture kept every parameter the message names.

  scripts/sanitize-controller_test.py
"""
import glob
import json
import os
import re
import subprocess
import sys
import tempfile

here = os.path.dirname(os.path.abspath(__file__))
root = os.path.dirname(here)
fixtures = sorted(glob.glob(os.path.join(root, 'docs', 'fixtures', 'controller-*', 'system-log-device-alert-*.json')))

# Placeholders the scrub script writes, and how each is given a site-looking
# value back. The values are built here so no literal one is in the file.
PLACEHOLDERS = [
    (re.compile(r'\bdevice-(\d+)\b'), lambda n: 'rack' + 'sw' + chr(0x61 + int(n) % 26) + n),
    (re.compile(r'\bperson-(\d+)\b'), lambda n: 'Pat Q' + 'uinn' + n),
    (re.compile(r'\bwlan-(\d+)\b'), lambda n: 'Home' + 'Net' + n),
    (re.compile(r'\b02:00:00:00:00:([0-9a-f]{2})\b'), lambda n: '5' + 'a:5a:5a:5a:5a:' + n),
    (re.compile(r'\b192\.0\.2\.(\d+)\b'), lambda n: '1' + '0.9.8.' + n),
    (re.compile(r'\b0{24}([0-9a-f]{8})\b'), lambda n: n + '-1111-4222-8333-' + '444455556666'),
]


def reidentify(text):
    planted = set()
    for rx, fn in PLACEHOLDERS:
        def sub(m, fn=fn):
            v = fn(m.group(1))
            planted.add(v)
            return v
        text = rx.sub(sub, text)
    return text, planted


def canonical(text):
    """Renumber each placeholder kind by first appearance."""
    for rx, _ in PLACEHOLDERS:
        seen = {}
        def sub(m):
            seen.setdefault(m.group(0), len(seen) + 1)
            return '<%s:%d>' % (rx.pattern[:12], seen[m.group(0)])
        text = rx.sub(sub, text)
    return text


def render(raw, params):
    return re.sub(r'\{([A-Z0-9_]+)\}', lambda m: str((params.get(m.group(1)) or {}).get('name')), raw)


def main():
    if not fixtures:
        print('FAIL: no alert fixtures found')
        return 1
    fails = 0
    with tempfile.TemporaryDirectory() as work:
        for path in fixtures:
            name = os.path.relpath(path, root)
            committed = json.load(open(path))
            text, planted = reidentify(json.dumps(committed))
            if not planted:
                print('FAIL: %s: nothing to re-identify' % name)
                fails += 1
                continue
            tmp = os.path.join(work, os.path.basename(path))
            open(tmp, 'w').write(text)
            subprocess.run([sys.executable, '-I', os.path.join(here, 'sanitize-controller.py'), tmp],
                           check=True, capture_output=True)
            out = open(tmp).read()
            problems = []
            left = sorted(v for v in planted if v in out)
            if left:
                problems.append('site values left in the output: %s' % ', '.join(left[:5]))
            got = json.loads(out)
            if canonical(json.dumps(got, sort_keys=True)) != canonical(json.dumps(committed, sort_keys=True)):
                problems.append('the output is not the committed fixture again (up to placeholder numbering)')
            for i, a in enumerate(got['data']):
                # A reduced fixture can lack a placeholder's parameter
                # (DEVICE_WITH_PORT); its message is scrubbed by the names
                # mapped so far, which the two checks above cover.
                if any(p not in a['parameters'] for p in re.findall(r'\{([A-Z0-9_]+)\}', a['message_raw'])):
                    continue
                if render(a['message_raw'], a['parameters']) != a['message']:
                    problems.append('alert %d: message %r is not message_raw with the parameters\' names' % (i, a['message']))
            if problems:
                fails += 1
                print('FAIL: %s\n    %s' % (name, '\n    '.join(problems)))
            else:
                print('ok: %s (%d values planted, none left)' % (name, len(planted)))
    return 1 if fails else 0


if __name__ == '__main__':
    sys.exit(main())
