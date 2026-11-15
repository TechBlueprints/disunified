# disunified-l2d and a PortChannel member's stray chip STP ports
# (docs/drivers/sonic.md 11.34).
#
# Part 1, the cause: a port's "<state> -> disabled" notification as it leaves
# Bridge (the syslog excerpt's own l2d lines for Ethernet0 and Ethernet48)
# reaches on_link once with the port gone from brif (leaving) and once with
# it still listed (a link drop). Prints "LEAVE <port> <puts> <known>" and
# "DROP <port> <puts> <known>": the STP_PORT_STATE writes the event made and
# whether l2d still counts the port as its own.
#
# Part 2, the repair: the DX010's captured tables (Ethernet0 a member of
# PortChannel1 holding a bridge port and an STP port in Vlan4094's instance,
# PortChannel1 none) under a small model of StpOrch over them: a SET for
# <port>:<vid> creates the port's chip STP port in the VLAN's instance unless
# it is a PortChannel one of whose members holds one there (the chip's
# "Lag member N exists as STP PORT ... in STG n", printed as REFUSED), else
# updates it; a DEL removes it if there. A fresh l2d (a restart) runs three
# passes 2 s apart. Prints each write ("SET k state", "DEL k"), REFUSED
# lines, "PASS <n> <writes>", and at the end the chip's STP ports of
# Ethernet0 and PortChannel1 per VLAN ("CHIP <port> <vid>=<state>,...") and
# the APPL_DB state keys of interfaces outside Bridge ("STALE <keys>").
#
# usage: python3 l2d_strays.py <l2d.py> <capture.json> <syslog excerpt>
import fnmatch
import importlib.util
import json
import re
import sys
import types


class _Stub(object):
    def __init__(self, *args, **kwargs):
        self.args = args

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

clock = [1000.0]
l2d.time = types.SimpleNamespace(time=lambda: clock[0], sleep=lambda s: clock.__setitem__(0, clock[0] + s),
                                 strftime=lambda *a: "")
logs = []
l2d.log = lambda msg, prio=None: logs.append(msg)

cap = json.load(open(sys.argv[2]))
dbs = {n: cap[n] for n in ("CONFIG_DB", "APPL_DB", "STATE_DB", "ASIC_DB", "COUNTERS_DB")}
brif = list(cap["kernel"]["brif"])
brport = {p: int(s) for p, s in cap["kernel"]["brport_state"].items()}
writes = []
oids = [0]


def asic(kind):
    return {k.split(":", 2)[2]: h for k, h in dbs["ASIC_DB"].items() if k.startswith("ASIC_STATE:SAI_OBJECT_TYPE_%s:" % kind)}


def iface_oid(name):
    for m in ("COUNTERS_PORT_NAME_MAP", "COUNTERS_LAG_NAME_MAP"):
        if name in dbs["COUNTERS_DB"].get(m, {}):
            return dbs["COUNTERS_DB"][m][name]
    return None


def bridge_port(name):
    oid = iface_oid(name)
    for bp, h in asic("BRIDGE_PORT").items():
        if h.get("SAI_BRIDGE_PORT_ATTR_TYPE") == "SAI_BRIDGE_PORT_TYPE_PORT" and h.get("SAI_BRIDGE_PORT_ATTR_PORT_ID") == oid:
            return bp
    return None


def stg(vid):
    for h in asic("VLAN").values():
        if h.get("SAI_VLAN_ATTR_VLAN_ID") == str(vid):
            return h.get("SAI_VLAN_ATTR_STP_INSTANCE")
    return None


def stp_port(name, vid):
    bp, st = bridge_port(name), stg(vid)
    for oid, h in asic("STP_PORT").items():
        if h.get("SAI_STP_PORT_ATTR_BRIDGE_PORT") == bp and h.get("SAI_STP_PORT_ATTR_STP") == st:
            return oid
    return None


SAI = {0: "BLOCKING", 1: "BLOCKING", 2: "BLOCKING", 3: "LEARNING", 4: "FORWARDING"}


def stporch(op, key, fields):
    """StpOrch's doStpPortStateTask over the model (202511 stporch.cpp)."""
    name, vid = key.rsplit(":", 1)
    vid = int(vid)
    row = "STP_PORT_STATE_TABLE:" + key
    if op == "DEL":
        oid = stp_port(name, vid)
        if oid:
            del dbs["ASIC_DB"]["ASIC_STATE:SAI_OBJECT_TYPE_STP_PORT:" + oid]
        dbs["APPL_DB"].pop(row, None)
        return
    dbs["APPL_DB"][row] = dict(fields)
    oid = stp_port(name, vid)
    if oid is None:
        if stg(vid) is None:
            return  # no instance in the model: nothing to check against
        members = [k.split("|", 2)[2] for k in dbs["CONFIG_DB"] if fnmatch.fnmatchcase(k, "PORTCHANNEL_MEMBER|%s|*" % name)]
        held = [m for m in members if stp_port(m, vid)]
        if held:
            print("REFUSED %s: lag member %s exists as STP PORT in the instance of Vlan%d" % (key, held[0], vid))
            return
        oids[0] += 1
        oid = "oid:0x2e9999%08x" % oids[0]
        dbs["ASIC_DB"]["ASIC_STATE:SAI_OBJECT_TYPE_STP_PORT:" + oid] = {
            "SAI_STP_PORT_ATTR_BRIDGE_PORT": bridge_port(name), "SAI_STP_PORT_ATTR_STP": stg(vid)}
    dbs["ASIC_DB"]["ASIC_STATE:SAI_OBJECT_TYPE_STP_PORT:" + oid]["SAI_STP_PORT_ATTR_STATE"] = \
        "SAI_STP_PORT_STATE_" + SAI[int(fields.get("state", "0"))]


class Producer(object):
    def __init__(self, table):
        self.table = table

    def set(self, key, fvs):
        fields = dict(fvs)
        writes.append("SET %s:%s %s" % (self.table, key, fields.get("state", fields.get("stp_instance", ""))))
        if self.table == "STP_PORT_STATE_TABLE":
            stporch("SET", key, fields)
        else:
            dbs["APPL_DB"]["%s:%s" % (self.table, key)] = fields

    def _del(self, key):
        writes.append("DEL %s:%s" % (self.table, key))
        if self.table == "STP_PORT_STATE_TABLE":
            stporch("DEL", key, {})


class Raw(object):
    def __init__(self, table):
        self.table = table

    def _del(self, key):
        dbs["APPL_DB"].pop("%s:%s" % (self.table, key), None)


l2d.put = lambda table, key, pairs: table.set(key, pairs)
l2d.bridge_ports = lambda: sorted(brif)
l2d.in_bridge = lambda p: p in brif
l2d.kstate = lambda p: brport.get(p) if p in brif else None


def fresh():
    d = l2d.L2d()
    db = d.db
    db.appl, db.cfg, db.state, db.asic, db.counters = "APPL_DB", "CONFIG_DB", "STATE_DB", "ASIC_DB", "COUNTERS_DB"
    db.keys = lambda conn, pattern: sorted(k for k in dbs[conn] if fnmatch.fnmatchcase(k, pattern))
    db.hgetall = lambda conn, key: dict(dbs[conn].get(key, {}))
    db.port_pst, db.vlan_pst, db.flush_pst = Producer("STP_PORT_STATE_TABLE"), Producer("STP_VLAN_INSTANCE_TABLE"), \
        Producer("STP_FASTAGEING_FLUSH_TABLE")
    db.port_raw, db.vlan_raw, db.flush_raw = Raw("STP_PORT_STATE_TABLE"), Raw("STP_VLAN_INSTANCE_TABLE"), \
        Raw("STP_FASTAGEING_FLUSH_TABLE")
    db.st_port = Raw("DUI_STP_PORT_TABLE")
    d.stp_on = True
    d.settle = 0
    return d


# Part 1: the leaving port's notification against a link drop.
line_re = re.compile(r"disunified-l2d\[\d+\]: (Ethernet\d+): (\w+) -> disabled$")
NUM = dict((v, k) for k, v in l2d.KNAME.items())
seen = set()
for line in open(sys.argv[3]):
    m = line_re.search(line.rstrip("\n"))
    if not m or m.group(1) in seen:
        continue
    port, old = m.group(1), NUM[m.group(2)]
    seen.add(port)
    for case, member in (("LEAVE", False), ("DROP", True)):
        d = fresh()
        d.ifindex = 7
        d.ports[port] = old
        d.written = {(port, 4094): str(l2d.KSTATE[old])}
        del writes[:]
        was = list(brif)
        if member and port not in brif:
            brif.append(port)
        if not member and port in brif:
            brif.remove(port)
        save = json.dumps(dbs)
        d.on_link(l2d.RTM_NEWLINK, port, d.ifindex, 0)
        print("%s %s %d %s" % (case, port, len([w for w in writes if w.startswith("SET STP_PORT_STATE_TABLE:%s:" % port)]),
                               port in d.ports))
        brif[:] = was
        for k, v in json.loads(save).items():
            dbs[k] = v
del writes[:]

# Part 2: a restart with the captured stray, three passes.
d = fresh()
for n in range(1, 4):
    del writes[:]
    d.sync_stp()
    for w in writes:
        if w.startswith(("SET STP_PORT_STATE_TABLE:Ethernet0:", "SET STP_PORT_STATE_TABLE:PortChannel1:", "DEL ")):
            print(w)
    print("PASS %d %d" % (n, len(writes)))
    clock[0] += l2d.SYNC
for name in ("Ethernet0", "PortChannel1"):
    held = []
    for h in asic("VLAN").values():
        if "SAI_VLAN_ATTR_VLAN_ID" not in h:
            continue  # the default VLAN's object carries no id
        vid = int(h["SAI_VLAN_ATTR_VLAN_ID"])
        oid = stp_port(name, vid)
        if oid:
            held.append("%d=%s" % (vid, asic("STP_PORT")[oid].get("SAI_STP_PORT_ATTR_STATE", "?").replace("SAI_STP_PORT_STATE_", "")))
    print("CHIP %s %s" % (name, ",".join(sorted(held)) or "-"))
print("STALE %s" % (",".join(sorted(k for k in dbs["APPL_DB"] if k.startswith("STP_PORT_STATE_TABLE:") and
                                     k.split(":")[1] not in brif)) or "-"))
for msg in logs:
    if "no bridge port" in msg:
        print("LOG " + msg)
