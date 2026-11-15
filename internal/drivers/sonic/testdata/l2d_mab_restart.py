# Replays the DX010's run of MAC authentication (MAB) and a Credentials
# port over restarts of disunified-l2d (mab-restart-live.txt, the run named
# on the command line) against l2d.py with fakes: Redis tables held in dicts,
# ebtables as a list of chains, the port's hostapd as a pid file and a
# control socket answering the captured station list, the packet and RADIUS
# sockets as queues, the clock the capture's. Every input is the capture's:
# when each test host sent a frame (its "HOST tx" lines), each RADIUS answer
# with its request's authenticator and identifier, hostapd's station replies
# and events, when l2d was restarted, stopped and started and when the link
# went. Prints one line per effect, in order (as l2d_dot1x.py and
# l2d_mab.py):
#   RUN <argv> / PST <table> <key> f=v ... / PSTDEL <table> <key>
#   SET <table>|<key> f=v ... / HDEL <table>|<key> <field> / DEL <table>|<key>
#   FLUSH <channel> <op> <data> / CTRL <request> / LOG <message>
#   GAP <rules>   the port's chain dropped a host that was admitted (or every
#                 host of an open Credentials port) after an ebtables write
#   AT <t> ...    the capture's time of an event
#   STEP <name> / STATE <what> <value>
#
# usage: python3 l2d_mab_restart.py <l2d.py> <mab-restart-live.txt> <run>
import fnmatch
import hashlib
import importlib.util
import json
import os
import re
import shutil
import socket as realsocket
import struct
import sys
import tempfile
import threading
import time
import types

STORE = {}


class Conn(object):
    def __init__(self, name, *args):
        self.name = name
        STORE.setdefault(name, {})

    def keys(self, pattern):
        return sorted(k for k in STORE[self.name] if fnmatch.fnmatchcase(k, pattern))

    def hgetall(self, key):
        return dict(STORE[self.name].get(key, {}))


def sep(conn):
    return ":" if conn.name in ("APPL_DB", "ASIC_DB", "COUNTERS_DB") else "|"


class PST(object):
    def __init__(self, conn, table):
        self.conn, self.table = conn, table

    def set(self, key, pairs):
        print("PST %s %s %s" % (self.table, key, " ".join("%s=%s" % p for p in pairs)))
        STORE[self.conn.name].setdefault(self.table + ":" + key, {}).update(dict(pairs))

    def _del(self, key):
        print("PSTDEL %s %s" % (self.table, key))
        STORE[self.conn.name].pop(self.table + ":" + key, None)


class Table(object):
    def __init__(self, conn, table):
        self.conn, self.table = conn, table

    def full(self, key):
        return self.table + sep(self.conn) + key

    def set(self, key, pairs):
        print("SET %s %s" % (self.full(key), " ".join("%s=%s" % p for p in pairs)))
        STORE[self.conn.name].setdefault(self.full(key), {}).update(dict(pairs))

    def hdel(self, key, field):
        print("HDEL %s %s" % (self.full(key), field))
        STORE[self.conn.name].get(self.full(key), {}).pop(field, None)

    def _del(self, key):
        if self.full(key) in STORE[self.conn.name]:
            print("DEL %s" % self.full(key))
        STORE[self.conn.name].pop(self.full(key), None)


class _Stub(object):
    def __init__(self, *args, **kwargs):
        pass

    def __getattr__(self, name):
        return lambda *args, **kwargs: None


class Flusher(object):
    def __init__(self, conn, channel):
        self.channel = channel

    def send(self, op, data, values):
        print("FLUSH %s %s %s" % (self.channel, op, data))


sw = types.ModuleType("swsscommon.swsscommon")
sw.DBConnector, sw.ProducerStateTable, sw.Table = Conn, PST, Table
sw.SubscriberStateTable = sw.Select = _Stub
sw.NotificationProducer = Flusher
sw.FieldValuePairs = lambda pairs: pairs
pkg = types.ModuleType("swsscommon")
pkg.swsscommon = sw
sys.modules["swsscommon"], sys.modules["swsscommon.swsscommon"] = pkg, sw

spec = importlib.util.spec_from_file_location("l2d", sys.argv[1])
l2d = importlib.util.module_from_spec(spec)
spec.loader.exec_module(l2d)
ebspec = importlib.util.spec_from_file_location("l2d_ebtables", os.path.join(os.path.dirname(os.path.abspath(__file__)), "l2d_ebtables.py"))
ebmod = importlib.util.module_from_spec(ebspec)
ebspec.loader.exec_module(ebmod)

# --- the capture ---------------------------------------------------------------

RUN = sys.argv[3]
sections, name = {}, None
for line in open(sys.argv[2]):
    line = line.rstrip("\n")
    if line.startswith("@@@ "):
        name = line[4:].strip()
        sections[name] = []
    elif name:
        sections[name].append(line)
LINE = re.compile(r"^\s*(-?[\d.]+) (.*)$")
log_ = [(float(m.group(1)), m.group(2)) for m in map(LINE.match, sections[RUN]) if m]

SWITCH = "02:00:00:00:00:02"  # the switch's MAC after the scrub
PORT = "Ethernet116"
SECRET = "dui-test-secret"  # the run's test secret
A, B, C, H = "02:00:00:00:0a:01", "02:00:00:00:0a:02", "02:00:00:00:0a:03", "02:00:00:00:0b:01"

# Each test host's frames: (time, mac)
FRAMES = []
for t, msg in log_:
    m = re.match(r"HOST tx (\S+)$", msg) or re.match(r"HOST (\S+) one ARP$", msg)
    if m:
        FRAMES.append((t, m.group(1)))


def when(pattern, after=-1.0):
    for t, msg in log_:
        if t > after and re.search(pattern, msg):
            return t
    raise SystemExit("not in the capture: %r after %s" % (pattern, after))


# RADIUS: each test host's request (its authenticator, identifier, the
# attributes) and the answer as received
REQ = re.compile(r"^\s*([\d.]+) REQ code=1 id=(\d+) auth=([0-9a-f-]+)$")
ATTR = re.compile(r"^\s+attr (\S+) (.*)$")
ANS = re.compile(r"^\s*([\d.]+) ANS id=(\d+) (\w+) ([0-9a-f]+)$")
radius, cur = [], None
for l in sections[RUN + "_radius"]:
    m, a, n = REQ.match(l), ATTR.match(l), ANS.match(l)
    if m:
        cur = {"t": float(m.group(1)), "id": int(m.group(2)), "auth": bytes.fromhex(m.group(3).replace("-", "")), "attrs": []}
        radius.append(cur)
    elif a and cur is not None:
        cur["attrs"].append((a.group(1), a.group(2)))
    elif n and cur is not None and int(n.group(2)) == cur["id"]:
        cur["answer"], cur["kind"] = bytes.fromhex(n.group(4)), n.group(3)
        cur = None
# l2d's own requests (hostapd's carry its EAP, and its own authenticator)
radius = [r for r in radius if dict(r["attrs"]).get("Service-Type") == "10"]
for r in radius:
    r["mac"] = dict(r["attrs"])["Calling-Station-Id"].replace("-", ":").lower()

# hostapd: the station replies at a snapshot, the events
STA_LINE = re.compile(r"^\s+STA (\d+) (\".*\")$")


def stations(snap):
    out = []
    i = [k for k, l in enumerate(sections[RUN]) if l.endswith("SNAP " + snap)][0]
    for l in sections[RUN][i + 1:]:
        m = STA_LINE.match(LINE.match(l).group(2)) if LINE.match(l) else None
        if not m and "SNAP " in l:
            break
        if m:
            out.append(json.loads(m.group(2)))
    return out


EVENTS = [(t, msg[len("EV <3>"):]) for t, msg in log_ if msg.startswith("EV <3>AP-STA-")]

# --- fakes -------------------------------------------------------------------

TMP = tempfile.mkdtemp(prefix="duimr", dir="/tmp")  # AF_UNIX paths are short on macOS
CHAINS = {"INPUT": [], "FORWARD": [], "OUTPUT": []}
HELD = set()  # over a restart: the hosts the port must keep letting through
OPEN = [False]  # over a restart: an open Credentials port, every host passes
WATCH = [False]  # a restart is under way (start() sets it)


class Done(object):
    def __init__(self, out="", rc=0):
        self.stdout, self.stderr, self.returncode = out, "", rc


def gap():
    if not WATCH[0]:
        return
    rules = CHAINS.get(l2d.DOT1X_CHAIN) or []
    port = [r for r in rules if r[:2] == ["-i", PORT]]
    if ["-i", PORT, "-j", "DROP"] not in port:
        return
    before = port[:port.index(["-i", PORT, "-j", "DROP"])]
    if OPEN[0] or any(["-i", PORT, "-s", m, "-j", "RETURN"] not in before for m in HELD):
        print("GAP %s" % json.dumps(port))


EB = ebmod.Filter(CHAINS)
EB.after.append(lambda: gap())


def ebtables(args):
    out, rc = EB.ebtables(args)
    return Done(out, rc)


def ebtables_restore(text):
    lines, rc = EB.restore(text)
    for l in lines:
        print("EB " + l)
    return Done("", rc)


class Hostapd(object):
    """The port's hostapd: its control socket answers ATTACH, the captured
    STA-FIRST / STA-NEXT replies (STA), DEAUTHENTICATE; send() hands an
    event to the attached listener."""

    def __init__(self, path):
        self.path, self.listener, self.sta = path, None, [""]
        self.s = realsocket.socket(realsocket.AF_UNIX, realsocket.SOCK_DGRAM)
        self.s.bind(path)
        threading.Thread(target=self.serve, daemon=True).start()

    def serve(self):
        while True:
            try:
                data, addr = self.s.recvfrom(4096)
            except OSError:
                return
            req = data.decode()
            if req not in ("ATTACH",):
                print("CTRL " + req)
            out = "OK\n"
            if req == "ATTACH":
                self.listener = addr
            elif req == "STA-FIRST":
                out = self.sta[0]
            elif req.startswith("STA-NEXT "):
                macs = [r.split("\n", 1)[0] for r in self.sta]
                i = macs.index(req.split()[1]) if req.split()[1] in macs else len(self.sta) - 1
                out = self.sta[i + 1] if i + 1 < len(self.sta) else ""
            elif req.startswith("DEAUTHENTICATE "):
                mac = req.split()[1]
                self.sta = [r for r in self.sta if r.split("\n", 1)[0] != mac] or [""]
            self.s.sendto(out.encode(), addr)

    def send(self, msg):
        self.s.sendto(msg.encode(), self.listener)


HOSTAPD = []


def run(args, check=False, stdin=None):
    print("RUN " + " ".join(args))
    if args[0] == l2d.EBTABLES_RESTORE:
        return ebtables_restore(stdin)
    if args[0] == l2d.EBTABLES:
        return ebtables(args[1:])
    if args[0] == l2d.HOSTAPD:
        conf = args[-1]
        with open(args[args.index("-P") + 1], "w") as f:
            f.write("4000\n")
        os.makedirs("%s/4000" % l2d.PROC, exist_ok=True)
        with open("%s/4000/cmdline" % l2d.PROC, "w") as f:
            f.write("%s\0-B\0-P\0x\0%s\0" % (l2d.HOSTAPD, conf))
        HOSTAPD.append(Hostapd("%s/ctrl/%s" % (l2d.DOT1X_RUN, PORT)))
        return Done()
    return Done()


class Queue(object):
    """A non-blocking socket fed by the test (the packet and RADIUS sockets)."""
    fdno = 1000

    def __init__(self):
        Queue.fdno += 1
        self.fd, self.q, self.sent = Queue.fdno, [], []

    def fileno(self):
        return self.fd

    def setblocking(self, flag):
        pass

    def bind(self, addr):
        pass

    def close(self):
        pass

    def recvfrom(self, n):
        if not self.q:
            raise BlockingIOError()
        return self.q.pop(0)

    def sendto(self, data, addr):
        self.sent.append((data, addr))


SOCKS = {}


def fake_socket(family, kind, proto=0):
    if family == realsocket.AF_UNIX:
        return realsocket.socket(family, kind, proto)  # hostapd's control socket
    s = Queue()
    SOCKS.setdefault(family, []).append(s)
    return s


fake = types.SimpleNamespace(AF_PACKET=17, AF_INET=realsocket.AF_INET, AF_INET6=realsocket.AF_INET6,
                             AF_UNIX=realsocket.AF_UNIX, SOCK_RAW=realsocket.SOCK_RAW,
                             SOCK_DGRAM=realsocket.SOCK_DGRAM, htons=realsocket.htons,
                             inet_aton=realsocket.inet_aton, socket=fake_socket)


class Clock(object):
    now = 0.0

    def time(self):
        return Clock.now

    def __getattr__(self, name):
        return getattr(time, name)


AUTHS = []
real_urandom = os.urandom
EB.watch(l2d.Dot1xGuard, "dot1x")
EB.watch(l2d.MacLockGuard, "maclock")
l2d.run = run


def fake_log(msg, prio=None):
    print("LOG " + msg)
    if "no longer authorised" in msg:
        print("TIME %.2f %s" % (Clock.now, msg.split()[1]))


l2d.log = fake_log
l2d.socket = fake
l2d.time = Clock()
l2d.os.urandom = lambda n: AUTHS.pop(0) if n == 16 and AUTHS else real_urandom(n)
l2d.SYS = TMP + "/sys"
l2d.PROC = TMP + "/proc"
l2d.DOT1X_RUN = TMP + "/run"
l2d.DOT1X_SECRET = TMP + "/secret"
l2d.HOSTAPD = TMP + "/hostapd"
os.makedirs(TMP + "/sys/" + PORT, exist_ok=True)
os.makedirs(TMP + "/proc", exist_ok=True)
with open(l2d.HOSTAPD, "w") as f:
    f.write("#!/bin/sh\n")
os.chmod(l2d.HOSTAPD, 0o755)
with open(l2d.DOT1X_SECRET, "w") as f:
    f.write(SECRET + "\n")


def carrier(up):
    with open("%s/sys/%s/carrier" % (TMP, PORT), "w") as f:
        f.write("1\n" if up else "0\n")


carrier(True)
STORE["CONFIG_DB"] = {
    "DEVICE_METADATA|localhost": {"mac": SWITCH},
    "PORT|" + PORT: {"admin_status": "up", "learn_mode": "drop"},
    "VLAN|Vlan4094": {"vlanid": "4094"},
    "VLAN_MEMBER|Vlan4094|" + PORT: {"tagging_mode": "untagged"},
}
STORE["APPL_DB"] = {"PORT_TABLE:" + PORT: {"admin_status": "up", "learn_mode": "drop"}}
STORE["STATE_DB"] = {"PORT_TABLE|" + PORT: {"state": "ok"}}
STORE["ASIC_DB"] = {}


def conf(mode):
    return {"dot1x": {"servers": [{"ip": "127.0.0.1", "port": 18120}], "nas_ip": "127.0.0.1",
                      "ports": {PORT: mode}, "idle_timeout": {PORT: 40}}}


class L2(object):
    def __init__(self):
        self.db = l2d.DB()


def step(n):
    print("STEP " + n)


def state(what, value):
    print("STATE %s %s" % (what, json.dumps(value, sort_keys=True)))


def row():
    state("row", STORE["STATE_DB"].get(l2d.DOT1X_TABLE + "|" + PORT))


def arp(src):
    s = bytes(int(x, 16) for x in src.split(":"))
    body = struct.pack("!HHBBH", 1, 0x0800, 6, 4, 1) + s + b"\0" * 10 + bytes([192, 0, 2, 1])
    f = b"\xff" * 6 + s + b"\x08\x06" + body
    return f + b"\0" * (60 - len(f))


l2 = L2()
G = [None]
UP = [True]
MODE = ["mac_based"]
DONE = set()  # the frames already fed


def radius_sock():
    return SOCKS[realsocket.AF_INET][-1]


def answer_requests():
    """Each request l2d sent: checked against the capture's (same host, the
    next one not yet answered), then answered with the captured answer."""
    for s in SOCKS.get(realsocket.AF_INET, []):
        while s.sent:
            pkt, addr = s.sent.pop(0)
            want = [r for r in radius if r["id"] == pkt[1] and not r.get("used")]
            if not want:
                print("SEND unexpected id=%d" % pkt[1])
                continue
            r = want[0]
            r["used"] = True
            print("SEND %s id=%d as captured %s" % (r["mac"], pkt[1], json.dumps(pkt[4:20] == r["auth"])))
            s.q.append((r["answer"], ("127.0.0.1", 18120)))
            G[0].readable(s.fileno())


def feed_until(t):
    """Runs the daemon's loop up to the capture's time t: a sync every 2 s,
    each captured frame at its time (when l2d runs and the link is up), each
    request answered as the capture's was."""
    while True:
        nxt = min([ft for ft, m in FRAMES if (ft, m) not in DONE and ft <= t] + [t])
        g = G[0]
        while g is not None and g.next_sync <= nxt:
            Clock.now = g.next_sync
            g.sync(conf(MODE[0]))
            g.next_sync += 2.0
        Clock.now = nxt
        due = [(ft, m) for ft, m in FRAMES if (ft, m) not in DONE and ft <= nxt]
        for f in due:
            DONE.add(f)
            if g is not None and UP[0] and SOCKS.get(fake.AF_PACKET):
                # the next captured request for this host gets its identifier
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


def l2d_admitted():
    return G[0].admitted.get(PORT, set()) if G[0] else set()


def start(t):
    """l2d starts (a restart's second half): a new Dot1xGuard and its first sync."""
    Clock.now = t
    print("AT %.2f l2d starts" % t)
    g = l2d.Dot1xGuard(l2)
    G[0] = g
    WATCH[0] = True
    g.sync(conf(MODE[0]))
    WATCH[0] = False
    g.next_sync = t + 2.0
    return g


def stop(t):
    feed_until(t)
    print("AT %.2f l2d stops" % t)
    G[0].stopping()
    G[0] = None


def event(t, msg):
    feed_until(t)
    print("AT %.2f %s" % (t, msg))
    HOSTAPD[-1].send("<3>" + msg)
    time.sleep(0.05)
    G[0].readable(G[0].socks[PORT].fileno())


def wait_ctrl():
    time.sleep(0.05)


# --- the run -------------------------------------------------------------------

step("mab start")
g = start(when(r"^L2D reload$"))
row()
state("learn", STORE["APPL_DB"]["PORT_TABLE:" + PORT].get("learn_mode"))

step("admit")
t_restart = when(r"^L2D restart$")
feed_until(t_restart)
row()
state("fdb", sorted(k for k in STORE["APPL_DB"] if k.startswith("FDB_TABLE:")))
state("seen", {m: round(G[0].seen[(PORT, m)], 2) for m in (A, C)})
print("TIME %.2f restart" % t_restart)

step("restart")
HELD |= {A, C}
stop(t_restart)
row()
g = start(when(r"^L2D restart done"))
row()
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))

step("idle")
t_idle = when(r"^WAIT C idle: yes")
feed_until(t_idle)
print("AT %.2f the capture saw C go" % t_idle)
row()

step("refused after restart")
feed_until(when(r"^SNAP refused$"))
row()

step("again")
feed_until(when(r"^SNAP before stop$"))
row()

step("stop")
t_stop = when(r"^L2D stop$")
stop(t_stop)
row()
# what the switch's l2d wrote as it stopped, in the run's own seconds
T0 = float(re.search(r" epoch ([\d.]+) ", [m for _, m in log_ if m.startswith("START ")][0]).group(1))
for t, msg in log_:
    if msg.startswith("  ROW") and t > t_stop:
        cap = json.loads(msg[len("  ROW "):]).get("mab_seen", "")
        state("captured seen", {k: round(float(v) - T0, 2) for k, v in (x.split("=") for x in cap.split(",") if x)})
        break

step("start, C idle over the stop")
HELD.clear()
HELD.add(A)
t_start = when(r"^L2D start done", t_stop)
g = start(t_start)
row()
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))

step("link lost during a stop")
t_stop2 = when(r"^L2D stop$", t_start)
stop(t_stop2)
HELD.clear()
t_off = when(r"^LOOP off$", t_stop2)
carrier(False)
UP[0] = False
g = start(when(r"^L2D start done", t_off))
row()
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))
feed_until(when(r"^LOOP on$", t_off))
carrier(True)
UP[0] = True

step("link back")
feed_until(when(r"^SNAP link back$"))
row()

step("credentials")
MODE[0] = "auto"
HOSTAPD[-1].sta = stations("credentials start")
feed_until(when(r"^L2D reload$", t_off) + 0.1)
for t, ev in EVENTS:
    if "CONNECTED" in ev and not "DIS" in ev:
        event(t, ev)
        break
HOSTAPD[-1].sta = stations("opened")
row()
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))
state("learn", STORE["APPL_DB"]["PORT_TABLE:" + PORT].get("learn_mode"))

step("credentials restart")
OPEN[0] = True
t_r2 = when(r"^L2D restart$", t_off)
stop(t_r2)
g = start(when(r"^L2D restart done", t_r2))
wait_ctrl()
row()
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))
state("learn", STORE["APPL_DB"]["PORT_TABLE:" + PORT].get("learn_mode"))

step("logoff")
OPEN[0] = False
for t, ev in EVENTS:
    if "DISCONNECTED" in ev:
        HOSTAPD[-1].sta = stations("logoff")
        event(t, ev)
row()
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))
state("learn", STORE["APPL_DB"]["PORT_TABLE:" + PORT].get("learn_mode"))

step("credentials link lost during a stop")
# The host authenticates again (its captured event), then the link goes
# while l2d is stopped: the next l2d closes the port at once.
for t, ev in EVENTS:
    if "CONNECTED" in ev and not "DIS" in ev:
        HOSTAPD[-1].sta = stations("opened")
        G[0].event(PORT, ev)
        G[0].publish()
t = Clock.now
stop(t)
carrier(False)
g = start(t + 5)
wait_ctrl()
row()
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))
state("learn", STORE["APPL_DB"]["PORT_TABLE:" + PORT].get("learn_mode"))
state("sta", [r.split("\n", 1)[0] for r in HOSTAPD[-1].sta])
shutil.rmtree(TMP, ignore_errors=True)
