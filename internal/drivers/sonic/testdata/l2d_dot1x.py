# Runs disunified-l2d's 802.1X (Dot1xGuard) against fakes: Redis tables
# held in dicts, ebtables as a list of chains, hostapd as a pid file and a
# control socket (a real AF_UNIX datagram socket answered by a thread) that
# replays what the DX010's hostapd said in dot1x-live.txt: its events
# (run_hostapd, run_logoff) and its raw STA-FIRST / STA-NEXT replies
# (run_logoff). Prints one line per effect, in order:
#   RUN <argv>                      a subprocess (ebtables, hostapd)
#   PST <table> <key> f=v ...       an APPL_DB ProducerStateTable write
#   PSTDEL <table> <key>
#   SET <table>|<key> f=v ... / DEL <table>|<key>   a plain Table write
#   CTRL <request>                  a request on hostapd's control socket
#   LOG <message>
#   STEP <name> / STATE <what> <value>
#
# usage: python3 l2d_dot1x.py <l2d.py> <dot1x-live.txt>
import fnmatch
import importlib.util
import json
import os
import re
import shutil
import socket
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

sections, name = {}, None
for line in open(sys.argv[2]):
    line = line.rstrip("\n")
    if line.startswith("@@@ "):
        name = line[4:].strip()
        sections[name] = []
    elif name:
        sections[name].append(line)
EVENT = re.compile(r"^\d\d:\d\d:\d\d (<\d>.*)$")
events = {n: [EVENT.match(l).group(1) for l in sections[n] if EVENT.match(l)] for n in ("run_hostapd", "run_logoff")}
# The raw replies: STA-FIRST's, then each STA-NEXT's, as hostapd sent them.
replies, cur = [], None
for l in sections["run_logoff"]:
    if l == "== STA-FIRST (raw reply)" or l.startswith("-- STA-NEXT "):
        cur = []
        replies.append(cur)
    elif l.startswith("== "):
        cur = None
    elif cur is not None:
        cur.append(l)
STA = ["\n".join(r) + "\n" if r else "" for r in replies]  # the last STA-NEXT answered nothing
SWITCH = STA[0].split("\n", 1)[0]  # hostapd's station for the switch's own looped frames
HOST = STA[1].split("\n", 1)[0]

# --- fakes -------------------------------------------------------------------

TMP = tempfile.mkdtemp(prefix="dui1x", dir="/tmp")  # AF_UNIX paths are short on macOS
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


class Hostapd(object):
    """hostapd's control socket: ATTACH, STA-FIRST, STA-NEXT, DEAUTHENTICATE;
    send() hands an event to the attached listener."""

    def __init__(self, path):
        self.path, self.listener, self.authorized = path, None, False
        self.s = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
        self.s.bind(path)
        threading.Thread(target=self.serve, daemon=True).start()

    def serve(self):
        while True:
            try:
                data, addr = self.s.recvfrom(4096)
            except OSError:
                return
            req = data.decode()
            if req != "ATTACH":
                print("CTRL " + req)
            if req == "ATTACH":
                self.listener = addr
                out = "OK\n"
            elif req == "STA-FIRST":
                out = STA[0]
            elif req.startswith("STA-NEXT "):
                # the switch's station, then the host's while it is authorised
                out = STA[1] if req.split()[1] == SWITCH and self.authorized else ""
            else:
                out = "OK\n"
            self.s.sendto(out.encode(), addr)

    def send(self, msg):
        self.s.sendto(msg.encode(), self.listener)

    def close(self):
        self.s.close()


HOSTAPDS = {}


def run(args, check=False, stdin=None):
    print("RUN " + " ".join(args))
    if args[0] == l2d.EBTABLES_RESTORE:
        return ebtables_restore(stdin)
    if args[0] == l2d.EBTABLES:
        return ebtables(args[1:])
    if args[0] == l2d.HOSTAPD:
        conf = args[-1]
        port = os.path.basename(conf)[:-len(".conf")]
        pid = 4000 + len(HOSTAPDS)
        with open(args[args.index("-P") + 1], "w") as f:
            f.write("%d\n" % pid)
        os.makedirs("%s/%d" % (l2d.PROC, pid), exist_ok=True)
        with open("%s/%d/cmdline" % (l2d.PROC, pid), "w") as f:
            f.write("%s\0-B\0-P\0x\0%s\0" % (l2d.HOSTAPD, conf))
        HOSTAPDS[port] = (pid, Hostapd("%s/ctrl/%s" % (l2d.DOT1X_RUN, port)))
        return Done()
    return Done()


real_kill = os.kill


def kill(pid, sig):
    for port, (p, h) in list(HOSTAPDS.items()):
        if p == pid:
            print("KILL hostapd %s" % port)
            h.close()
            shutil.rmtree("%s/%d" % (l2d.PROC, pid), ignore_errors=True)
            del HOSTAPDS[port]
            return
    real_kill(pid, sig)


EB.watch(l2d.Dot1xGuard, "dot1x")
EB.watch(l2d.MacLockGuard, "maclock")
l2d.run = run
l2d.log = lambda msg, prio=None: print("LOG " + msg)
l2d.os.kill = kill
l2d.SYS = TMP + "/sys"
l2d.PROC = TMP + "/proc"
l2d.DOT1X_RUN = TMP + "/run"
l2d.DOT1X_SECRET = TMP + "/secret"
l2d.HOSTAPD = TMP + "/hostapd"
for d in ("sys/Ethernet120", "sys/Ethernet0", "proc"):
    os.makedirs(os.path.join(TMP, d), exist_ok=True)
with open(TMP + "/sys/Ethernet120/carrier", "w") as f:
    f.write("1\n")
with open(l2d.HOSTAPD, "w") as f:
    f.write("#!/bin/sh\n")
os.chmod(l2d.HOSTAPD, 0o755)
with open(l2d.DOT1X_SECRET, "w") as f:
    f.write("test-secret\n")

STORE["CONFIG_DB"] = {
    "DEVICE_METADATA|localhost": {"mac": SWITCH},
    "PORT|Ethernet0": {"admin_status": "up"},
    "PORT|Ethernet120": {"admin_status": "up", "learn_mode": "drop"},
    "VLAN|Vlan4094": {"vlanid": "4094"},
    "VLAN_MEMBER|Vlan4094|Ethernet0": {"tagging_mode": "untagged"},
    "VLAN_MEMBER|Vlan4094|Ethernet120": {"tagging_mode": "untagged"},
}
STORE["APPL_DB"] = {"PORT_TABLE:Ethernet0": {"admin_status": "up"}, "PORT_TABLE:Ethernet120": {"admin_status": "up"}}
STORE["STATE_DB"] = {"PORT_TABLE|Ethernet0": {"state": "ok"}, "PORT_TABLE|Ethernet120": {"state": "ok"}}
STORE["ASIC_DB"] = {}

CONF = {"dot1x": {"servers": [{"ip": "192.0.2.1", "port": 1812}], "nas_ip": "192.0.2.200",
                  "ports": {"Ethernet120": "auto", "Ethernet0": "force_authorized"}}}


class L2(object):
    def __init__(self):
        self.db = l2d.DB()


def step(n):
    print("STEP " + n)


def pump(g, secs=0.3):
    end = time.time() + secs
    while time.time() < end:
        for fd in g.fds():
            g.readable(fd)
        time.sleep(0.02)


def state(what, value):
    print("STATE %s %s" % (what, json.dumps(value, sort_keys=True)))


l2 = L2()
g = l2d.Dot1xGuard(l2)

step("start")
g.sync(CONF)
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))
state("input", CHAINS["INPUT"])
state("conf", open(l2d.DOT1X_RUN + "/Ethernet120.conf").read().split("\n"))
state("conf mode", oct(os.stat(l2d.DOT1X_RUN + "/Ethernet120.conf").st_mode & 0o777))

step("second sync")
g.sync(CONF)

step("captured events")
h = HOSTAPDS["Ethernet120"][1]
for ev in events["run_hostapd"]:
    h.send(ev)
    if ev.startswith("<3>AP-STA-CONNECTED"):
        h.authorized = True
    pump(g, 0.1)
pump(g)
state("row", STORE["STATE_DB"].get(l2d.DOT1X_TABLE + "|Ethernet120"))
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))

step("second host, Credentials")
connected = [e for e in events["run_hostapd"] if "AP-STA-CONNECTED" in e][0]
OTHER = [e for e in events["run_hostapd"] if "EAP-FAILURE2" in e][0].split()[-1]
h.send(connected.replace(HOST, OTHER))
pump(g)
state("row", STORE["STATE_DB"].get(l2d.DOT1X_TABLE + "|Ethernet120"))
h.send("<3>AP-STA-DISCONNECTED " + OTHER)  # one host still holds the port open
pump(g)
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))

step("second host, multiple devices")
CONF["dot1x"]["ports"]["Ethernet120"] = "multi_host"
g.want["Ethernet120"] = "multi_host"
h.send(connected.replace(HOST, OTHER))
pump(g)
state("row", STORE["STATE_DB"].get(l2d.DOT1X_TABLE + "|Ethernet120"))
h.send("<3>AP-STA-DISCONNECTED " + OTHER)
pump(g)
CONF["dot1x"]["ports"]["Ethernet120"] = "auto"

step("restart")
g = l2d.Dot1xGuard(l2)
g.sync(CONF)
pump(g)
state("row", STORE["STATE_DB"].get(l2d.DOT1X_TABLE + "|Ethernet120"))

step("logoff")
for ev in events["run_logoff"]:
    if "DISCONNECTED" in ev:
        h.authorized = False
        h.send(ev)
pump(g)
state("row", STORE["STATE_DB"].get(l2d.DOT1X_TABLE + "|Ethernet120"))
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))

step("unwatched")
g.sync({"dot1x": {}})
state("chain", CHAINS.get(l2d.DOT1X_CHAIN))
state("input", CHAINS["INPUT"])
state("copp", STORE["CONFIG_DB"].get("COPP_TRAP|" + l2d.COPP_EAPOL_KEY))
state("row", STORE["STATE_DB"].get(l2d.DOT1X_TABLE + "|Ethernet120"))
state("hostapd", sorted(HOSTAPDS))
shutil.rmtree(TMP, ignore_errors=True)
