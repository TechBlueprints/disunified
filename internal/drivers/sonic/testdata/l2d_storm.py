# Replays a live capture of a port's receive counters (one JSON object per
# sample: {"t": seconds, "<port>": {<COUNTERS_DB field>: value, ...}}) through
# disunified-l2d's StormWatch (l2d.py) as if CONFIG_DB held the given
# storm-control limits, and prints each log line, each STATE_DB write and the
# final row.
#
# usage: python3 l2d_storm.py <l2d.py> <capture.ndjson> <port> <broadcast kbps> <unknown-multicast kbps>
import fnmatch
import importlib.util
import json
import sys
import types


class Conn(object):
    """A Redis database as l2d's DB.keys / DB.hgetall read it."""

    def __init__(self):
        self.h = {}

    def keys(self, pattern):
        return [k for k in self.h if fnmatch.fnmatchcase(k, pattern)]

    def hgetall(self, key):
        return dict(self.h.get(key, {}))


class Table(object):
    def __init__(self, conn, name):
        self.conn, self.name = conn, name

    def set(self, key, fvs):
        print("SET %s|%s %s" % (self.name, key, " ".join("%s=%s" % kv for kv in fvs)))
        self.conn.h["%s|%s" % (self.name, key)] = dict(fvs)

    def _del(self, key):
        print("DEL %s|%s" % (self.name, key))
        self.conn.h.pop("%s|%s" % (self.name, key), None)


counters = Conn()
sw = types.ModuleType("swsscommon.swsscommon")
sw.DBConnector = lambda name, n: counters if name == "COUNTERS_DB" else Conn()
sw.Table = Table
sw.FieldValuePairs = list
for name in ("ProducerStateTable", "SubscriberStateTable", "Select"):
    setattr(sw, name, lambda *a, **k: None)
pkg = types.ModuleType("swsscommon")
pkg.swsscommon = sw
sys.modules["swsscommon"], sys.modules["swsscommon.swsscommon"] = pkg, sw

spec = importlib.util.spec_from_file_location("l2d", sys.argv[1])
l2d = importlib.util.module_from_spec(spec)
spec.loader.exec_module(l2d)
l2d.log = lambda msg, prio=None: print("LOG " + msg)

port, bkbps, mkbps = sys.argv[3], int(sys.argv[4]), int(sys.argv[5])
db = types.SimpleNamespace(cfg=Conn(), state=Conn())
for typ, kbps in (("broadcast", bkbps), ("unknown-multicast", mkbps)):
    if kbps:
        db.cfg.h["PORT_STORM_CONTROL|%s|%s" % (port, typ)] = {"kbps": str(kbps)}
counters.h["COUNTERS_PORT_NAME_MAP"] = {port: "oid:0x1"}
w = l2d.StormWatch(db)
w.adopt()
last = 0.0
for line in open(sys.argv[2]):
    rec = json.loads(line)
    counters.h["COUNTERS:oid:0x1"] = {k: v for k, v in rec[port].items() if isinstance(v, str)}
    w.tick(rec["t"])
    last = rec["t"]
print("ROW " + json.dumps(db.state.hgetall("DUI_STORM_TABLE|" + port), sort_keys=True))
