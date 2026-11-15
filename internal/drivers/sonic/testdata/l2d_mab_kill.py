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

# This run's first two requests went out together (REQ id=0, REQ id=1, then
# both answers), so each answer goes to the open request with its id, not
# only to the last one read.
radius, open_ = [], {}
for l in sections[RUN + "_radius"]:
    m, a, n = REQ.match(l), ATTR.match(l), ANS.match(l)
    if m:
        cur = {"t": float(m.group(1)), "id": int(m.group(2)), "auth": bytes.fromhex(m.group(3).replace("-", "")), "attrs": []}
        radius.append(cur)
        open_[cur["id"]] = cur
    elif a and radius:
        radius[-1]["attrs"].append((a.group(1), a.group(2)))
    elif n and int(n.group(2)) in open_:
        r = open_.pop(int(n.group(2)))
        r["answer"], r["kind"] = bytes.fromhex(n.group(4)), n.group(3)
radius = [r for r in radius if dict(r["attrs"]).get("Service-Type") == "10"]
for r in radius:
    r["mac"] = dict(r["attrs"])["Calling-Station-Id"].replace("-", ":").lower()


def kill(t):
    """The capture's kill -9: l2d is gone at t, having written nothing more."""
    feed_until(t)
    print("AT %.2f l2d killed" % t)
    print("KILL %.2f" % t)
    G[0] = None


def restarted(after):
    """systemd's restart: the new l2d's first sync, which is its first
    802.1X line ("... kept over a restart"), else the time the rig saw the
    new pid."""
    t = when(r"^L2D restarted by systemd", after)
    for jt, msg in [(float(m.group(1)), m.group(2)) for m in map(LINE.match, sections[RUN + "_journal"]) if m]:
        if after < jt < t + 10 and msg.endswith("kept over a restart"):
            t = max(t, jt)
            break
    g = start(t)
    g.next_sync = t + SYNC_STEP
    return g


# The switch's l2d syncs every 2 s and again on its netlink and FDB events,
# so when it ran mab_tick is not in the capture; syncing every 0.5 s here
# puts a revocation within 0.5 s of the first moment it was due.
SYNC_STEP = 0.5


def feed_until(t):
    """l2d_mab_restart.py's feed_until with a sync every SYNC_STEP."""
    while True:
        nxt = min([ft for ft, m in FRAMES if (ft, m) not in DONE and ft <= t] + [t])
        g = G[0]
        while g is not None and g.next_sync <= nxt:
            Clock.now = g.next_sync
            g.sync(conf(MODE[0]))
            g.next_sync += SYNC_STEP
        Clock.now = nxt
        for f in [(ft, m) for ft, m in FRAMES if (ft, m) not in DONE and ft <= nxt]:
            DONE.add(f)
            if g is not None and UP[0] and SOCKS.get(fake.AF_PACKET):
                nr = [r for r in radius if r["mac"] == f[1] and not r.get("used") and r["t"] >= f[0] - 0.05]
                if nr and f[1] not in l2d_admitted():
                    g.next_id = nr[0]["id"]
                    AUTHS[:] = [nr[0]["auth"]]
                pkt = SOCKS[fake.AF_PACKET][-1]
                pkt.q.append((arp(f[1]), (PORT, 0x0003, 0, 1, b"")))
                g.readable(pkt.fileno())
                answer_requests()
        if nxt >= t:
            return


def last_frame(mac, before):
    return max(ft for ft, m in FRAMES if m == mac and ft <= before)


step("mab start")
g = start(when(r"^L2D reload$"))
g.next_sync = Clock.now + SYNC_STEP
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
