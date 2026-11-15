# Replays the DX010's run of MAC authentication (MAB) over SIGKILLs of
# disunified-l2d (mab-kill-live.txt, the run named on the command line)
# against l2d.py, with l2d_mab_restart.py's fakes and its reading of the
# capture (everything in that file above "# --- the run"): Redis tables held
# in dicts, ebtables as a list of chains, hostapd's control socket, the
# packet and RADIUS sockets as queues, the clock the capture's. A kill is
# the daemon gone with nothing written (no stopping()); systemd's restart
# is a new Dot1xGuard on what the Redis fakes hold. Prints what
# l2d_mab_restart.py prints, and
#   KILL <t> / TIME <t> <mac>   when the capture's l2d was killed, when the
#                               replayed one revoked a host
#
# usage: python3 l2d_mab_kill.py <l2d.py> <mab-kill-live.txt> <run>
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
_src = open(os.path.join(HERE, "l2d_mab_restart.py")).read()
exec(compile(_src[:_src.index("# --- the run")], os.path.join(HERE, "l2d_mab_restart.py"), "exec"))


def kill(t):
    """The capture's kill -9: l2d is gone at t, having written nothing more."""
    feed_until(t)
    print("AT %.2f l2d killed" % t)
    print("KILL %.2f" % t)
    G[0] = None


def restarted(after):
    """systemd's restart: the first 802.1X line the new l2d logged (its first
    sync), else the time the rig saw the new pid."""
    t = when(r"^L2D restarted by systemd", after)
    for jt, msg in [(float(m.group(1)), m.group(2)) for m in map(LINE.match, sections[RUN + "_journal"]) if m]:
        if after < jt < t + 10 and msg == "started":
            t = max(t, jt)
            break
    return start(t)


def last_frame(mac, before):
    return max(ft for ft, m in FRAMES if m == mac and ft <= before)


step("mab start")
g = start(when(r"^L2D reload$"))
state("learn", STORE["APPL_DB"]["PORT_TABLE:" + PORT].get("learn_mode"))

step("admit")
feed_until(when(r"^STEP kill, C silent"))
row()
state("fdb", sorted(k for k in STORE["APPL_DB"] if k.startswith("FDB_TABLE:")))

for name, start_pat, kills in (("kill, C silent 8 s", r"^STEP kill, C silent", 1),
                               ("two kills, C silent 12 s", r"^STEP two kills", 2),
                               ("kill just after C's frame", r"^STEP kill just after", 1)):
    t0 = when(start_pat)
    feed_until(t0)
    step(name)
    t = t0
    for _ in range(kills):
        HELD.clear()
        HELD.update(l2d_admitted())
        t = when(r"^L2D kill -9", t)
        print("LASTC %.2f" % last_frame(C, t))
        state("row at kill", STORE["STATE_DB"].get(l2d.DOT1X_TABLE + "|" + PORT))
        kill(t)
        restarted(t)
        row()
    gone = when(r"^GONE " + C + "$", t)
    print("CAPTURED GONE %.2f" % gone)
    feed_until(gone + 3)
    row()

step("end")
feed_until(when(r"^STEP A still admitted"))
row()
state("admitted", sorted(l2d_admitted()))
shutil.rmtree(TMP, ignore_errors=True)
