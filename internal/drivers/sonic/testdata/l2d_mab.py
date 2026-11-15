# Runs disunified-l2d's MAC authentication (Dot1xGuard, mac_based) against
# fakes: Redis tables held in dicts, ebtables as a list of chains, the port's
# packet socket and the RADIUS client's UDP socket as queues. The RADIUS
# server's answers are the ones the DX010's hand run received
# (mab-live.txt), and the guard's request authenticators are that run's, so
# the captured answers verify against the requests this l2d builds. Prints
# one line per effect, in order:
#   RUN <argv>                      a subprocess (ebtables)
#   PST <table> <key> f=v ...       an APPL_DB ProducerStateTable write
#   PSTDEL <table> <key>
#   SET <table>|<key> f=v ... / DEL <table>|<key>
#   SEND <server> <attribute>=<value> ...   a RADIUS request, decoded
#   LOG <message>
#   STEP <name> / STATE <what> <value>
#
# usage: python3 l2d_mab.py <l2d.py> <mab-live.txt>
import binascii
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


sw = types.ModuleType("swsscommon.swsscommon")
sw.DBConnector, sw.ProducerStateTable, sw.Table = Conn, PST, Table
sw.SubscriberStateTable = sw.Select = _Stub


class Flusher(object):
    """fdborch's flush request channel: prints each request."""

    def __init__(self, conn, channel):
        self.channel = channel

    def send(self, op, data, values):
        print("FLUSH %s %s %s" % (self.channel, op, data))


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

REQ = re.compile(r"^\s*[\d.]+ REQ (\S+) code=1 id=(\d+) auth=([0-9a-f-]+)$")
ANS = re.compile(r"^\s*[\d.]+ ANS (\w+) ([0-9a-f]+)$")
ATTR = re.compile(r"^\s+attr (\S+) (.*)$")
section, runs, cur = None, [], None
for line in open(sys.argv[2]):
    line = line.rstrip("\n")
    if line.startswith("@@@ "):
        section = line[4:].strip()
        continue
    if section != "run":
        continue
    m = REQ.match(line)
    if m:
        cur = {"mac": m.group(1), "id": int(m.group(2)), "auth": binascii.unhexlify(m.group(3).replace("-", "")), "attrs": []}
        runs.append(cur)
        continue
    m = ATTR.match(line)
    if m and cur is not None:
        cur["attrs"].append((m.group(1), m.group(2)))
        continue
    m = ANS.match(line)
    if m and cur is not None:
        cur["result"], cur["answer"] = m.group(1), binascii.unhexlify(m.group(2))
SECRET = "dui-test-secret"  # the hand run's test secret (mab-live.txt)
SWITCH = "02:00:00:00:00:02"  # the switch's MAC, as the capture's Called-Station-Id has it
A, B = runs[0]["mac"], runs[1]["mac"]

# --- fakes -------------------------------------------------------------------

TMP = tempfile.mkdtemp(prefix="duimab", dir="/tmp")
CHAINS = {"INPUT": [], "FORWARD": [], "OUTPUT": []}


class Done(object):
    def __init__(self, out="", rc=0):
        self.stdout, self.stderr, self.returncode = out, "", rc


EB = ebmod.Filter(CHAINS)


def ebtables(args):
    out, rc = EB.ebtables(args)
    return Done(out, rc)


def ebtables_restore(text):
    lines, rc = EB.restore(text)
    for l in lines:
        print("EB " + l)
    return Done("", rc)


def run(args, check=False, stdin=None):
    print("RUN " + " ".join(args))
    if args[0] == l2d.EBTABLES_RESTORE:
        return ebtables_restore(stdin)
    if args[0] == l2d.EBTABLES:
        return ebtables(args[1:])
    return Done()


class Queue(object):
    """A non-blocking socket fed by the test."""
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
    s = Queue()
    SOCKS.setdefault(family, []).append(s)
    return s


fake = types.SimpleNamespace(AF_PACKET=17, AF_INET=realsocket.AF_INET, AF_INET6=realsocket.AF_INET6,
                             AF_UNIX=realsocket.AF_UNIX, SOCK_RAW=realsocket.SOCK_RAW,
                             SOCK_DGRAM=realsocket.SOCK_DGRAM, htons=realsocket.htons,
                             inet_aton=realsocket.inet_aton, socket=fake_socket)


class Clock(object):
    now = 1000.0

    def time(self):
        return Clock.now

    def __getattr__(self, name):
        import time
        return getattr(time, name)


AUTHS = [r["auth"] for r in runs]
real_urandom = os.urandom
EB.watch(l2d.Dot1xGuard, "dot1x")
EB.watch(l2d.MacLockGuard, "maclock")
l2d.run = run
l2d.log = lambda msg, prio=None: print("LOG " + msg)
l2d.socket = fake
l2d.time = Clock()
l2d.os.urandom = lambda n: AUTHS.pop(0) if n == 16 and AUTHS else real_urandom(n)
l2d.SYS = TMP + "/sys"
l2d.PROC = TMP + "/proc"
l2d.DOT1X_RUN = TMP + "/run"
l2d.DOT1X_SECRET = TMP + "/secret"
l2d.HOSTAPD = TMP + "/no-hostapd"  # MAC authentication needs no hostapd
for d in ("sys/Ethernet120", "sys/Ethernet112", "proc"):
    os.makedirs(os.path.join(TMP, d), exist_ok=True)
for p in ("Ethernet120", "Ethernet112"):
    with open("%s/sys/%s/carrier" % (TMP, p), "w") as f:
        f.write("1\n")
with open(l2d.DOT1X_SECRET, "w") as f:
    f.write(SECRET + "\n")

STORE["CONFIG_DB"] = {
    "DEVICE_METADATA|localhost": {"mac": SWITCH},
    "PORT|Ethernet112": {"admin_status": "up", "learn_mode": "drop"},
    "PORT|Ethernet120": {"admin_status": "up", "learn_mode": "drop"},
    "VLAN|Vlan4094": {"vlanid": "4094"},
    "VLAN|Vlan3997": {"vlanid": "3997"},
    "VLAN_MEMBER|Vlan4094|Ethernet120": {"tagging_mode": "untagged"},
    "VLAN_MEMBER|Vlan3997|Ethernet120": {"tagging_mode": "tagged"},
    "VLAN_MEMBER|Vlan4094|Ethernet112": {"tagging_mode": "untagged"},
}
STORE["APPL_DB"] = {"PORT_TABLE:Ethernet120": {"admin_status": "up", "learn_mode": "drop"},
                    "PORT_TABLE:Ethernet112": {"admin_status": "up", "learn_mode": "drop"}}
STORE["STATE_DB"] = {"PORT_TABLE|Ethernet120": {"state": "ok"}, "PORT_TABLE|Ethernet112": {"state": "ok"}}
STORE["ASIC_DB"] = {}

CONF = {"dot1x": {"servers": [{"ip": "127.0.0.1", "port": 18120}], "nas_ip": "127.0.0.1",
                  "ports": {"Ethernet120": "mac_based", "Ethernet112": "force_unauthorized"},
                  "idle_timeout": {"Ethernet120": 20}}}


class L2(object):
    def __init__(self):
        self.db = l2d.DB()


def step(n):
    print("STEP " + n)


def state(what, value):
    print("STATE %s %s" % (what, json.dumps(value, sort_keys=True)))


NAMES = {1: "User-Name", 2: "User-Password", 4: "NAS-IP-Address", 6: "Service-Type", 30: "Called-Station-Id",
         31: "Calling-Station-Id", 32: "NAS-Identifier", 61: "NAS-Port-Type", 80: "Message-Authenticator",
         87: "NAS-Port-Id"}


def decode(pkt, secret):
    """A request as the capture writes it, its password and Message-
    Authenticator checked with the secret: [(name, value)]."""
    auth, out, pos = pkt[4:20], [], 20
    while pos < len(pkt):
        typ, alen = pkt[pos], pkt[pos + 1]
        v = pkt[pos + 2:pos + alen]
        if typ in (1, 30, 31, 32, 87):
            val = v.decode()
        elif typ == 4:
            val = ".".join(str(x) for x in v)
        elif typ in (6, 61):
            val = str(struct.unpack("!I", v)[0])
        else:
            val = "<%d bytes>" % len(v)
        if typ == 2:
            key = hashlib.md5(secret + auth).digest()
            print("STATE password %s" % json.dumps(bytes(a ^ b for a, b in zip(v, key)).rstrip(b"\0").decode()))
        if typ == 80:
            zeroed = pkt[:pos + 2] + b"\0" * 16 + pkt[pos + 18:]
            import hmac
            ok = hmac.new(secret, zeroed, hashlib.md5).digest() == v
            print("STATE message-authenticator %s" % json.dumps(ok))
        out.append((NAMES.get(typ, str(typ)), val))
        pos += alen
    return out


def radius():
    return SOCKS[realsocket.AF_INET][0]


def sent():
    """Each request sent since the last call: printed, and checked against
    the capture's request with the same identifier."""
    out = []
    while radius().sent:
        pkt, addr = radius().sent.pop(0)
        attrs = decode(pkt, SECRET.encode())
        print("SEND %s:%d id=%d %s" % (addr[0], addr[1], pkt[1], " ".join("%s=%s" % a for a in attrs)))
        want = [r for r in runs if r["id"] == pkt[1] and r["mac"] == dict(attrs)["Calling-Station-Id"].replace("-", ":").lower()]
        if want:
            state("as captured", attrs == want[0]["attrs"] and pkt[4:20] == want[0]["auth"])
        out.append(pkt)
    return out


def frame(src, vlan=3997, etype=b"\x08\x06"):
    s = bytes(int(x, 16) for x in src.split(":"))
    return b"\xff" * 6 + s + struct.pack("!HH", 0x8100, vlan) + etype + b"\0" * 46


def arrive(g, data):
    pkt = SOCKS[fake.AF_PACKET][0]
    pkt.q.append((data, ("Ethernet120", 0x0003, 0, 1, b"")))
    g.readable(pkt.fileno())


def answer(g, data):
    radius().q.append((data, ("127.0.0.1", 18120)))
    g.readable(radius().fileno())


def row():
    state("row", STORE["STATE_DB"].get(l2d.DOT1X_TABLE + "|Ethernet120"))


l2 = L2()
g = l2d.Dot1xGuard(l2)

step("start")
g.sync(CONF)
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))
state("learn", STORE["APPL_DB"]["PORT_TABLE:Ethernet120"].get("learn_mode"))
state("learn 112", STORE["APPL_DB"]["PORT_TABLE:Ethernet112"].get("learn_mode"))

step("accept")
arrive(g, frame(A))
arrive(g, frame(A))  # a second frame while the first is asked about: no second request
sent()
answer(g, runs[0]["answer"])
row()
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))

step("reject")
Clock.now += 6
arrive(g, frame(B))
sent()
answer(g, runs[1]["answer"])
row()

step("quiet")
Clock.now += 3
arrive(g, frame(B))
arrive(g, frame("02:00:00:00:0b:01", etype=b"\x88\x8e"))  # an 802.1X host: hostapd's
arrive(g, frame(SWITCH))  # the switch's own frames, looped
state("requests", len(sent()))

step("idle")
Clock.now += 25
g.sync(CONF)
row()
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))

step("return")
arrive(g, frame(A))
sent()
answer(g, runs[2]["answer"])
row()

step("forged")
Clock.now += 1
arrive(g, frame("02:00:00:00:0a:03"))
forged = bytearray(runs[2]["answer"])
forged[1] = sent()[0][1]  # the new request's identifier, the rest an old accept's
answer(g, bytes(forged))
row()

step("unwatched")
g.sync({"dot1x": {}})
state("learn", STORE["APPL_DB"]["PORT_TABLE:Ethernet120"].get("learn_mode"))
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))
row()
state("row 112", STORE["STATE_DB"].get(l2d.DOT1X_TABLE + "|Ethernet112"))
state("fdb", sorted(k for k in STORE["APPL_DB"] if k.startswith("FDB_TABLE:")))
shutil.rmtree(TMP, ignore_errors=True)
