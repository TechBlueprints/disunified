#!/usr/bin/env python3
"""check-help.py <help.go> <capture-dir> [--stp]

Apply the disunified sonic driver's start-up check (commandChecks in
internal/drivers/sonic/help.go) to `config ... --help` texts captured with
cli-help-capture.sh: every sub-command the driver needs must be listed under
"Commands:" in its group's help. Exit 1 when one is missing.
"""
import os
import re
import sys

helpgo, cap = sys.argv[1], sys.argv[2]
with_stp = "--stp" in sys.argv[3:]
src = open(helpgo).read()
checks = re.findall(r'\{group: "([^"]*)", subs: \[\]string\{([^}]*)\}(, stp: true)?\}', src)
missing = 0
for group, subs, stp in checks:
    if stp and not with_stp:
        continue
    want = re.findall(r'"([^"]+)"', subs)
    f = os.path.join(cap, "config_" + group.replace(" ", "_") + ".txt")
    if not os.path.exists(f):
        print("NO CAPTURE  config %s --help (%s)" % (group, f)); missing += 1; continue
    text = open(f).read()
    cmds = set()
    if "Commands:" in text:
        for line in text.split("Commands:", 1)[1].splitlines():
            m = re.match(r"^  (\S+)", line)
            if m:
                cmds.add(m.group(1))
    gone = [w for w in want if w not in cmds]
    if gone:
        missing += len(gone)
        print("MISSING     config %-25s %s" % (group or "(top)", " ".join(gone)))
    else:
        print("ok          config %-25s %d sub-commands" % (group or "(top)", len(want)))
sys.exit(1 if missing else 0)
