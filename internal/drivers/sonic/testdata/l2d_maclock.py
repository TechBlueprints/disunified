# Runs disunified-l2d's MAC locking (MacLockGuard: a secured port's count of
# frames from sources not on its list) against fakes: Redis tables held in dicts, ebtables as a list of chains,
# and packet sockets that hand over the frames captured on the DX010
# (maclock-live.txt, frames_ethernet36: the switch's own BPDU, then a test
# source's frames, as the port's netdev delivered them under cpu_trap).
# Prints one line per effect, in order:
#   RUN <argv>                      a subprocess (ebtables, ip)
#   PST <table> <key> f=v ...       an APPL_DB ProducerStateTable write
#   SET <table>|<key> f=v ... / DEL <table>|<key>   a plain Table write
#   LOG <message>
#   STEP <name> / STATE <what> <value>
#
# usage: python3 l2d_maclock.py <l2d.py> <maclock-live.txt> <tmpdir>
import fnmatch
import importlib.util
import json
import os
import sys
import types

STORE = {}  # db name -> {full key: {field: value}}


class Conn(object):
    def __init__(self, name, *args):
        self.name = name
        STORE.setdefault(name, {})

    def keys(self, pattern):
        return sorted(k for k in STORE[self.name] if fnmatch.fnmatchcase(k, pattern))

    def hgetall(self, key):
        return dict(STORE[self.name].get(key, {}))

    def hdel(self, key, field):
        STORE[self.name].get(key, {}).pop(field, None)


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

sections, name = {}, None
for line in open(sys.argv[2]):
    if line.startswith("@@@ "):
        name = line[4:].strip()
        sections[name] = []
    elif name and line.strip() and not line.startswith("#"):
        sections[name].append(line.strip())
frames = [bytes.fromhex(h) for h in sections["frames_ethernet36"]]

# --- fakes -------------------------------------------------------------------

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
    if args[:4] == ["ip", "-4", "route", "get"]:
        return Done("%s dev Vlan4094 src 192.0.2.200 uid 0\n    cache\n" % args[4])
    if args[:4] == ["ip", "-4", "neigh", "show"]:
        return Done("%s dev Vlan4094 lladdr 02:00:00:00:00:02 REACHABLE\n" % args[4])
    return Done()


class Sock(object):
    """A packet socket on one netdev; frames are queued per netdev."""
    queues = {}
    count = 0

    def __init__(self, *args):
        Sock.count += 1
        self.fd = 100 + Sock.count
        self.dev = None

    def bind(self, addr):
        self.dev = addr[0]
        Sock.queues.setdefault(self.dev, [])

    def setblocking(self, flag):
        pass

    def fileno(self):
        return self.fd

    def recvfrom(self, n):
        q = Sock.queues[self.dev]
        if not q:
            raise BlockingIOError()
        data, pkttype = q.pop(0)
        return data[:n], (self.dev, 0x88b5, pkttype, 1, b"")

    def close(self):
        print("CLOSE socket %s" % self.dev)


def deliver(dev, data, pkttype=0):
    Sock.queues.setdefault(dev, []).append((data, pkttype))


EB.watch(l2d.Dot1xGuard, "dot1x")
EB.watch(l2d.MacLockGuard, "maclock")
l2d.run = run
l2d.log = lambda msg, prio=None: print("LOG " + msg)
l2d.socket.socket = Sock
if not hasattr(l2d.socket, "AF_PACKET"):  # macOS has no packet sockets
    l2d.socket.AF_PACKET = 17
l2d.SYS = sys.argv[3]
for n in ("Ethernet0", "Ethernet36"):
    os.makedirs(os.path.join(l2d.SYS, n), exist_ok=True)
CONF = os.path.join(sys.argv[3], "l2.json")
l2d.CONF = CONF


def conf(ports):
    with open(CONF, "w") as f:
        json.dump({"mac_lock": {"ports": ports}} if ports else {}, f)


# --- the switch: a secured empty cage (Ethernet36) and the uplink ------------

STORE["CONFIG_DB"] = {
    "PORT|Ethernet0": {"admin_status": "up"},
    "PORT|Ethernet36": {"admin_status": "up", "learn_mode": "drop"},  # what the driver writes
    "DEVICE_METADATA|localhost": {"mac": "02:00:00:00:00:01"},
}
STORE["APPL_DB"] = {
    "PORT_TABLE:Ethernet0": {"admin_status": "up", "oper_status": "up"},
    "PORT_TABLE:Ethernet36": {"admin_status": "up", "learn_mode": "drop"},
}
STORE["STATE_DB"] = {"PORT_TABLE|Ethernet0": {"state": "ok"}, "PORT_TABLE|Ethernet36": {"state": "ok"}}

d = l2d.L2d()
m = d.maclock


def sync():
    d.conf = l2d.load_conf()
    m.sync(d.conf)


def feed(dev, data, pkttype=0):
    deliver(dev, data, pkttype)
    for fd in m.fds():
        m.readable(fd)


def row():
    return json.dumps(STORE["STATE_DB"].get("DUI_MACLOCK_TABLE|Ethernet36"), sort_keys=True)


bpdu, test = frames[0], frames[1]

print("STEP watch")
conf({"Ethernet36": ["02:00:00:00:00:91"]})
sync()
print("STATE learn_mode %s" % STORE["APPL_DB"]["PORT_TABLE:Ethernet36"].get("learn_mode"))
print("STEP second sync")
sync()

print("STEP own BPDU and listed source")
feed("Ethernet36", bpdu)
for f in frames[1:]:
    feed("Ethernet36", f)
feed("Ethernet36", test, pkttype=4)  # the switch's own transmission of it
sync()
print("STATE row %s" % row())

print("STEP source not on the list")
conf({"Ethernet36": ["02:00:00:00:00:99"]})
sync()
for f in frames[1:]:
    feed("Ethernet36", f)
print("STATE before publish %s" % row())
sync()
print("STATE row %s" % row())
print("STATE admin %s" % STORE["APPL_DB"]["PORT_TABLE:Ethernet36"].get("admin_status"))

print("STEP restart")
d = l2d.L2d()
m = d.maclock
sync()
feed("Ethernet36", test)
sync()
print("STATE row %s" % row())

print("STEP unwatch")
conf({})
sync()
print("STATE learn_mode %s chains %s row %s" % (STORE["APPL_DB"]["PORT_TABLE:Ethernet36"].get("learn_mode"), sorted(CHAINS), row()))

print("STEP teardown")
conf({"Ethernet36": ["02:00:00:00:00:91"]})
sync()
m.teardown()
print("STATE learn_mode %s chains %s" % (STORE["APPL_DB"]["PORT_TABLE:Ethernet36"].get("learn_mode"), sorted(CHAINS)))
