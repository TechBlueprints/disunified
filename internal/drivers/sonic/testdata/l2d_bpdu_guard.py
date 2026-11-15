# Runs disunified-l2d's BPDU-guard and loop-protection holds (§11.28)
# against a collector capture of a tripped BPDU guard: mstpd's report of
# every port is the capture's DUI_STP_PORT_TABLE (Ethernet64:
# bpdu-guard-port yes, bpdu-guard-error yes), CONFIG_DB PORT and APPL_DB
# PORT_TABLE are the capture's, and STATE_DB starts empty. Each step is
# printed as "STEP <name>", then every APPL_DB/STATE_DB write as
# "PUT <db> <table> <key> field=value ...", every delete as
# "DEL <db> <table> <key>", every command as "RUN <argv>" and every log line
# as "LOG <msg>"; "HELD <port> <yes|no>" is what the port's
# DUI_STP_PORT_TABLE row says to the driver.
#
# usage: python3 -I l2d_bpdu_guard.py <l2d.py> <capture>
import fnmatch
import importlib.util
import json
import os
import sys
import tempfile
import types


class _Stub(object):
    def __init__(self, *args, **kwargs):
        self.args = args

    def __getattr__(self, name):
        return lambda *args, **kwargs: None


sw = types.ModuleType("swsscommon.swsscommon")
for name in ("DBConnector", "ProducerStateTable", "Table", "SubscriberStateTable", "Select"):
    setattr(sw, name, _Stub)
sw.FieldValuePairs = lambda pairs: pairs
pkg = types.ModuleType("swsscommon")
pkg.swsscommon = sw
sys.modules["swsscommon"], sys.modules["swsscommon.swsscommon"] = pkg, sw

spec = importlib.util.spec_from_file_location("l2d", sys.argv[1])
l2d = importlib.util.module_from_spec(spec)
spec.loader.exec_module(l2d)

sections, name = {}, None
for line in open(sys.argv[2]):
    if line.startswith("@@@ "):
        name = line[4:].strip()
        sections[name] = ""
    elif name:
        sections[name] += line

dbs = {"APPL_DB": {}, "CONFIG_DB": {}, "STATE_DB": {}}
for k, v in json.loads(sections["cfg_port"]).items():
    dbs["CONFIG_DB"][k] = dict(v)
for k, v in json.loads(sections["appl_port"]).items():
    dbs["APPL_DB"][k] = dict(v)
SEP = {"APPL_DB": ":", "CONFIG_DB": "|", "STATE_DB": "|"}


class T(object):
    """A table: APPL_DB rows merge (a producer's set), the others too."""

    def __init__(self, db, table):
        self.db, self.table = db, table

    def key(self, k):
        return self.table + SEP[self.db] + k

    def set(self, k, pairs):
        print("PUT %s %s %s %s" % (self.db, self.table, k, " ".join("%s=%s" % kv for kv in pairs)))
        dbs[self.db].setdefault(self.key(k), {}).update(dict(pairs))

    def _del(self, k):
        if self.key(k) in dbs[self.db]:
            print("DEL %s %s %s" % (self.db, self.table, k))
            del dbs[self.db][self.key(k)]


class FakeDB(object):
    appl, cfg, state, asic = "APPL_DB", "CONFIG_DB", "STATE_DB", "ASIC_DB"

    def __init__(self):
        self.ifport_pst = T("APPL_DB", "PORT_TABLE")
        self.st_guard = T("STATE_DB", "DUI_LOOP_GUARD_TABLE")
        self.st_bpdu = T("STATE_DB", "DUI_BPDU_GUARD_TABLE")
        self.st_recover = T("STATE_DB", "DUI_PORT_RECOVERY")
        self.st_port = T("STATE_DB", "DUI_STP_PORT_TABLE")
        self.st_bridge = T("STATE_DB", "DUI_STP_TABLE")

    @staticmethod
    def keys(conn, pattern):
        return sorted(k for k in dbs.get(conn, {}) if fnmatch.fnmatchcase(k, pattern))

    @staticmethod
    def hgetall(conn, key):
        return dict(dbs.get(conn, {}).get(key, {}))

    def lag_members(self, lag):
        return []


def mstpctl(fields):
    return {k.replace("_", "-"): v for k, v in fields.items()}


bridge = mstpctl(json.loads(sections["l2_bridge"])["DUI_STP_TABLE|Bridge"])
bridge["force-protocol-version"] = bridge.pop("version")
report = {}
for key, fields in sorted(json.loads(sections["l2_port"]).items()):
    d = mstpctl(fields)
    d["port"] = key.split("|", 1)[1]
    report[d["port"]] = d
conf = json.loads(sections["l2d"])["conf"]
P = "Ethernet64"


class Mstp(object):
    def bridge(self):
        return dict(bridge)

    def ports(self):
        return [dict(d) for d in report.values()]

    def ctl(self, *args):
        print("CALL " + " ".join(args))
        return True


class R(object):
    returncode, stdout, stderr = 0, "", ""


# /sys/class/net: each netdev's flags, IFF_UP set or not by `ip link set`.
l2d.SYS = tempfile.mkdtemp()


def set_flags(dev, up):
    os.makedirs(os.path.join(l2d.SYS, dev), exist_ok=True)
    with open(os.path.join(l2d.SYS, dev, "flags"), "w") as f:
        f.write("0x1003\n" if up else "0x1002\n")


def run(args, check=False, stdin=None):
    print("RUN " + " ".join(args))
    if args[:4] == ["ip", "link", "set", "dev"] and args[5] in ("up", "down"):
        set_flags(args[4], args[5] == "up")
    return R()


l2d.run = run
l2d.put = lambda table, key, pairs: table.set(key, pairs)
l2d.log = lambda msg, prio=None: print("LOG " + msg)


def load(c):
    def f():
        x = json.loads(json.dumps(c))
        return {
            "stp": {"enabled": x["stp"]["enabled"], "version": "rstp", "priority": 32768,
                    "ports": x["stp"]["ports"]},
            "loop_guard": {"enabled": x["loop_guard"]["enabled"], "threshold": 10, "interval": 5.0,
                           "hold": 150.0, "ports": set(x["loop_guard"].get("ports") or [])},
        }
    return f


def fresh():
    d = l2d.L2d.__new__(l2d.L2d)
    d.db = FakeDB()
    d.mstp = Mstp()
    d.conf = load(conf)()
    d.stp_on = True
    d.ports = {p: 0 for p in report}  # every bridge port, disabled
    d.changes, d.blocked, d.bpdu_held, d.bpdu_stale = {}, {}, {}, {}
    return d


def held():
    print("HELD %s %s" % (P, dbs["STATE_DB"].get("DUI_STP_PORT_TABLE|" + P, {}).get("bpdu_guard_held")))


def tick(d):
    """What sync() does for the holds after sync_stp."""
    d.recoveries()
    d.bpdu_tick()
    d.publish()
    d.guard_tick()
    d.reassert()
    held()


def step(name):
    print("STEP " + name)


d = fresh()
step("trip")
tick(d)
step("again")
tick(d)
step("portmgrd")  # a CONFIG_DB write of the port: portmgrd copies admin_status up, netdev up
dbs["APPL_DB"]["PORT_TABLE:" + P]["admin_status"] = "up"
set_flags(P, True)
tick(d)
step("restart")  # a new l2d adopts the hold from STATE_DB
d = fresh()
d.adopt_guard_state()
tick(d)
step("recover")
dbs["STATE_DB"]["DUI_PORT_RECOVERY|" + P] = {"at": "1"}
tick(d)
step("nolink")  # released with no link: mstpd's error is still the old one
tick(d)
step("restart-released")
d = fresh()
d.adopt_guard_state()
tick(d)
step("linkup")  # the link comes up and mstpd's guard fires again
d.count_change(P, 0, 4)
d.ports[P] = 4
tick(d)
step("guardoff")
conf["stp"]["ports"][P]["bpdu_guard"] = False
d.conf = load(conf)()
report[P]["bpdu-guard-port"] = "no"
tick(d)
step("guardon-stale")  # back on over mstpd's old error, no link since: no hold
conf["stp"]["ports"][P]["bpdu_guard"] = True
d.conf = load(conf)()
report[P]["bpdu-guard-port"] = "yes"
tick(d)
step("errorclears")  # the link comes up with no BPDU: mstpd's error clears
report[P]["bpdu-guard-error"] = "no"
tick(d)
conf["stp"]["ports"][P]["bpdu_guard"] = False
d.conf = load(conf)()
report[P]["bpdu-guard-port"] = "no"
report[P]["bpdu-guard-error"] = "yes"
step("nothing")
dbs["STATE_DB"]["DUI_PORT_RECOVERY|" + P] = {"at": "2"}
tick(d)

# Loop protection's hold, on the same port: a CONFIG_DB write under it, a
# Re-enable, then the port's keepalive switched off.
report[P]["bpdu-guard-error"] = "no"
conf["loop_guard"]["enabled"] = True
conf["loop_guard"]["ports"] = [P]
d = fresh()
step("loop-block")
d.block(P, "Vlan4094", "02:00:00:00:00:01", "Ethernet68")
dbs["APPL_DB"]["PORT_TABLE:" + P]["admin_status"] = "up"
tick(d)
step("loop-recover")
dbs["STATE_DB"]["DUI_PORT_RECOVERY|" + P] = {"at": "3"}
tick(d)
step("loop-off")
d.block(P, "Vlan4094", "02:00:00:00:00:01", "Ethernet68")
conf["loop_guard"]["ports"] = []
d.conf = load(conf)()
tick(d)
