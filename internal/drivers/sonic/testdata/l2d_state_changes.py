# Replays disunified-l2d's own journal of a port's kernel state changes (the
# "<port>: <old> -> <new>" lines it logs from netlink) into a fresh L2d's
# on_link, then a sync_stp pass that reads the last state again and a
# publish, and prints the count each step leaves and the state_changes field
# published for the port: "STEP <n> <count>", "PUBLISH <port> <value>".
# A second L2d then starts with STATE_DB holding that value, sees one more
# change and publishes again ("RESTART <value>"): the count carries over an
# l2d restart.
#
# usage: python3 l2d_state_changes.py <l2d.py> <journal>
import importlib.util
import re
import sys
import types


class _Stub(object):
    def __init__(self, *args, **kwargs):
        pass

    def __getattr__(self, name):
        return lambda *args, **kwargs: None


sw = types.ModuleType("swsscommon.swsscommon")
for name in ("DBConnector", "ProducerStateTable", "Table", "SubscriberStateTable", "Select", "FieldValuePairs"):
    setattr(sw, name, _Stub)
pkg = types.ModuleType("swsscommon")
pkg.swsscommon = sw
sys.modules["swsscommon"], sys.modules["swsscommon.swsscommon"] = pkg, sw

spec = importlib.util.spec_from_file_location("l2d", sys.argv[1])
l2d = importlib.util.module_from_spec(spec)
spec.loader.exec_module(l2d)

NUM = dict((v, k) for k, v in l2d.KNAME.items())
line_re = re.compile(r"disunified-l2d\[\d+\]: (\S+): (\w+) -> (\w+)$")
events = []
for line in open(sys.argv[2]):
    m = line_re.search(line.rstrip("\n"))
    if m:
        events.append((m.group(1), NUM[m.group(2)], NUM[m.group(3)]))

state_db = {}  # key -> fields, what STATE_DB holds
published = {}


def put(table, key, pairs):
    published[key] = dict(pairs)


l2d.put = put
l2d.log = lambda msg, prio=None: None


class FakeMstp(object):
    def __init__(self, owner):
        self.owner = owner

    def bridge(self):
        return {"root-port": ""}

    def ports(self):
        return [{"port": p} for p in sorted(self.owner.ports)]


def fresh():
    x = l2d.L2d()
    x.db.hgetall = lambda conn, key: state_db.get(key, {})
    x.mstp = FakeMstp(x)
    x.reconcile = lambda b, ports: False
    x.stp_on = True
    x.ifindex = 7
    return x


d = fresh()
port = events[0][0]
d.ports[port] = events[0][1]  # the state the first line leaves: l2d already knew it
for n, (p, old, new) in enumerate(events, 1):
    d.on_link(l2d.RTM_NEWLINK, p, d.ifindex, new)
    print("STEP %d %d" % (n, d.changes.get(p, 0)))
last = events[-1][2]
l2d.kstate = lambda p: last
d.db.vlans = lambda: set()
d.db.known = lambda p: True
l2d.bridge_ports = lambda: [port]
d.write_state = lambda p, st, vids: False
d.sync_stp()
d.publish()
print("PUBLISH %s %s" % (port, published[port].get("state_changes")))

state_db["DUI_STP_PORT_TABLE|%s" % port] = {"state_changes": published[port]["state_changes"]}
r = fresh()
r.ports[port] = last
r.on_link(l2d.RTM_NEWLINK, port, r.ifindex, 0 if last else 4)
published.clear()
r.publish()
print("RESTART %s" % published[port].get("state_changes"))

# A port that never changed state publishes 0 (mstpd's own count reads 1 for
# a port that never came up).
q = fresh()
q.ports["Ethernet68"] = 0
published.clear()
q.publish()
print("NEVER %s" % published["Ethernet68"].get("state_changes"))
