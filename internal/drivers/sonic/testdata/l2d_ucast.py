# Replays a live capture of a port's psample (sFlow) samples through
# disunified-l2d's UcastSampler and StormWatch (l2d.py) as if CONFIG_DB held
# the given unknown-unicast storm-control limit, and prints each log line,
# each CONFIG_DB sFlow write, each STATE_DB write and the final row.
#
# The capture is one JSON object per line: {"t": seconds} marks a second of
# the run (StormWatch ticks then), {"t", "iif", "oif", "rate", "size",
# "data"} is one psample message as l2d reads it (the ingress and egress
# ifindex resolved to port names, "data" the frame's first bytes in hex),
# and {"cfg": {...}} / {"fdb": [[vlan, mac], ...]} / {"own": mac} set what
# CONFIG_DB, the FDB and DEVICE_METADATA held during the run.
#
# usage: python3 l2d_ucast.py <l2d.py> <capture.ndjson> <port> <unknown-unicast kbps>
import fnmatch
import importlib.util
import json
import os
import sys
import tempfile
import types


class Conn(object):
    def __init__(self):
        self.h = {}

    def keys(self, pattern):
        return [k for k in self.h if fnmatch.fnmatchcase(k, pattern)]

    def hgetall(self, key):
        return dict(self.h.get(key, {}))


class Table(object):
    def __init__(self, conn, name, sep="|"):
        self.conn, self.name, self.sep = conn, name, sep

    def set(self, key, fvs):
        print("SET %s%s%s %s" % (self.name, self.sep, key, " ".join("%s=%s" % kv for kv in fvs)))
        self.conn.h["%s%s%s" % (self.name, self.sep, key)] = dict(fvs)

    def hset(self, key, field, value):
        print("HSET %s%s%s %s=%s" % (self.name, self.sep, key, field, value))
        self.conn.h.setdefault("%s%s%s" % (self.name, self.sep, key), {})[field] = value

    def _del(self, key):
        print("DEL %s%s%s" % (self.name, self.sep, key))
        self.conn.h.pop("%s%s%s" % (self.name, self.sep, key), None)


appl = Conn()
sw = types.ModuleType("swsscommon.swsscommon")
sw.DBConnector = lambda name, n: Conn()
sw.Table = Table
sw.ProducerStateTable = lambda conn, name: Table(appl, name, ":")
sw.FieldValuePairs = list
for name in ("SubscriberStateTable", "Select"):
    setattr(sw, name, lambda *a, **k: None)
pkg = types.ModuleType("swsscommon")
pkg.swsscommon = sw
sys.modules["swsscommon"], sys.modules["swsscommon.swsscommon"] = pkg, sw

spec = importlib.util.spec_from_file_location("l2d", sys.argv[1])
l2d = importlib.util.module_from_spec(spec)
spec.loader.exec_module(l2d)
l2d.log = lambda msg, prio=None: print("LOG " + msg)
proc = tempfile.NamedTemporaryFile(delete=False)
l2d.PSAMPLE_RATE_PROC = proc.name  # the image has linux-bcm-genl
owned = proc.name + ".owned"
l2d.UcastSampler.OWNED = owned

port, kbps = sys.argv[3], int(sys.argv[4])
db = types.SimpleNamespace(cfg=Conn(), state=Conn(), appl=appl)
db.cfg.h["PORT_STORM_CONTROL|%s|unknown-unicast" % port] = {"kbps": str(kbps)}
d = types.SimpleNamespace(db=db, fdb_port={})
d.storm = l2d.StormWatch(db)
s = l2d.UcastSampler(d)
s.open = lambda: setattr(s, "sock", types.SimpleNamespace(fileno=lambda: 3, close=lambda: None))
s.sync({})
for line in open(sys.argv[2]):
    rec = json.loads(line)
    if "cfg" in rec:
        db.cfg.h.update(rec["cfg"])
        s.sync({})
    elif "fdb" in rec:
        d.fdb_port = {(v, m): "x" for v, m in rec["fdb"]}
    elif "own" in rec:
        db.cfg.h["DEVICE_METADATA|localhost"] = {"mac": rec["own"]}
        s.refresh()
    elif "iif" in rec:
        s.classify(rec["t"], rec["iif"], rec["oif"], rec["rate"], rec["size"], bytes.fromhex(rec["data"]))
    else:
        d.storm.tick(rec["t"])
print("ROW " + json.dumps(db.state.hgetall("DUI_STORM_TABLE|" + port), sort_keys=True))
db.cfg.h.pop("PORT_STORM_CONTROL|%s|unknown-unicast" % port)
s.sync({})  # the rows go
s.sync({})  # then the feature (APPL_DB holds no sflow row here)
d.storm.tick(1e12)
print("AFTER " + json.dumps(sorted(k for k in db.cfg.h if k.startswith("SFLOW"))) + " " + db.cfg.h.get("FEATURE|sflow", {}).get("state", ""))
os.unlink(proc.name)
