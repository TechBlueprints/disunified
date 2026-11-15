# disunified-l2d and STP instances no VLAN uses (docs/drivers/sonic.md 11.35).
#
# A model of orchagent over the DX010's captured tables, with StpOrch's
# instance ids (m_stpInstToOid) beside ASIC_DB:
# - STP_PORT_STATE_TABLE SET <port>:<id> makes the instance if StpOrch has
#   none of that id (addStpPort -> addStpInstance), then the port's STP port
#   in it; DEL removes the STP port, never the instance;
# - STP_VLAN_INSTANCE_TABLE SET Vlan<v> maps the VLAN into the instance
#   (made if missing); DEL maps it back to the default and removes the
#   instance with every STP port in it (removeVlanFromStpInstance ->
#   removeStpInstance); either for a VLAN orchagent does not have stays
#   queued and is retried on every pass (it returns false);
# - VLAN_TABLE SET makes the VLAN, DEL removes it, unmapping it first.
# In "queue" mode the writes wait for orch(), which runs portsorch's before
# StpOrch's, as orchagent's passes do; otherwise each write is taken at once.
#
# The capture does not say which instance id each orphaned STP object had
# (StpOrch keeps that in memory only). The model gives each its id: the one
# the repro made is 3001 (the newest object), the others the nine ids of the
# VLANs the 15:10 deploy deleted, in oid order.
#
# Part "cleanup" (stp-orphans capture: ten orphans, Vlan4094 mapped): a
# fresh l2d whose retired list holds the ten ids, twelve passes.
# Part "race" (stp-vlan3001 capture: Vlan3001 mapped, nine orphans): a
# restart's first pass queued, Vlan3001 deleted as the CLI does, orchagent
# runs (the VLAN's DEL first, then the queued states), then passes 2 s apart
# with writes taken at once.
#
# Prints the writes that touch an instance other than 4094's and every write
# to 4094's that is not a SET of the state it already had, "PASS <n>", and at
# the end "INSTANCES <ids>" (StpOrch's), "OBJECTS <STP objects> OUTSIDE4094
# <STP ports in other instances>", "VLANS <ids>", "STP4094 <oid> <ports> <changed>",
# "APPL <leftover keys>", "STUCK <ops>", "RETIRED <ids>", then l2d's log lines
# about instances.
#
# usage: python3 l2d_orphans.py <l2d.py> <capture.json> cleanup|race <tmpdir>
import fnmatch
import importlib.util
import json
import os
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
part, tmp = sys.argv[3], sys.argv[4]
if hasattr(l2d, "STP_RETIRED"):
    l2d.STP_RETIRED = os.path.join(tmp, "stp-retired")
    l2d.STP_REPAIR = os.path.join(tmp, "stp-repair")

clock = [1000.0]
l2d.time = types.SimpleNamespace(time=lambda: clock[0], sleep=lambda s: clock.__setitem__(0, clock[0] + s),
                                 strftime=lambda *a: "")
logs = []
l2d.log = lambda msg, prio=None: logs.append(msg)

cap = json.load(open(sys.argv[2]))
dbs = {n: cap[n] for n in ("CONFIG_DB", "APPL_DB", "STATE_DB", "ASIC_DB", "COUNTERS_DB")}
brif = [p for p in cap["kernel"]["brif"] if p != "dummy"] + ["dummy"]
brport = {p: int(s) for p, s in cap["kernel"]["brport_state"].items()}
A = "ASIC_STATE:SAI_OBJECT_TYPE_"


def asic(kind):
    return {k.split(":", 2)[2]: h for k, h in dbs["ASIC_DB"].items() if k.startswith(A + kind + ":")}


def vlan_oid(vid):
    for oid, h in asic("VLAN").items():
        if h.get("SAI_VLAN_ATTR_VLAN_ID") == str(vid):
            return oid
    return None


# StpOrch's ids: every STP object in use by a VLAN, by its VLAN's id; the
# default one (no VLAN, no STP port, the lowest oid); the orphans as above.
ports_of = {}
for h in asic("STP_PORT").values():
    ports_of.setdefault(h["SAI_STP_PORT_ATTR_STP"], []).append(h)
inst, default = {}, None
for oid in sorted(asic("STP")):
    users = [h for h in asic("VLAN").values() if h.get("SAI_VLAN_ATTR_STP_INSTANCE") == oid]
    if users:
        inst[int(users[0]["SAI_VLAN_ATTR_VLAN_ID"])] = oid
    elif default is None and oid not in ports_of:
        default = oid
orphans = sorted(o for o in asic("STP") if o != default and o not in inst.values())
ids = [2, 8, 9, 10, 69, 1000, 2000, 3000, 4000]
if part == "cleanup":
    ids = ids + [3001]
for vid, oid in zip(ids, orphans):
    inst[vid] = oid
mapped_to = {v: v for v in inst if vlan_oid(v)}  # a VLAN's m_stp_id
before4094 = {k: dict(h) for k, h in asic("STP_PORT").items() if h["SAI_STP_PORT_ATTR_STP"] == inst[4094]}

seq = [0x5000]


def new_oid(kind):
    seq[0] += 1
    return "oid:0x%x%08x" % ({"STP": 0x10, "STP_PORT": 0x2e, "VLAN": 0x26}[kind], seq[0])


def bridge_port(name):
    oid = None
    for m in ("COUNTERS_PORT_NAME_MAP", "COUNTERS_LAG_NAME_MAP"):
        oid = oid or dbs["COUNTERS_DB"].get(m, {}).get(name)
    for bp, h in asic("BRIDGE_PORT").items():
        if h.get("SAI_BRIDGE_PORT_ATTR_TYPE") == "SAI_BRIDGE_PORT_TYPE_PORT" and h.get("SAI_BRIDGE_PORT_ATTR_PORT_ID") == oid:
            return bp
    return None


def stp_port(name, vid):
    bp, st = bridge_port(name), inst.get(vid)
    for oid, h in asic("STP_PORT").items():
        if h.get("SAI_STP_PORT_ATTR_BRIDGE_PORT") == bp and h.get("SAI_STP_PORT_ATTR_STP") == st:
            return oid
    return None


def add_instance(vid):
    oid = new_oid("STP")
    dbs["ASIC_DB"][A + "STP:" + oid] = {}
    inst[vid] = oid
    return oid


def remove_instance(vid):
    oid = inst.pop(vid, None)
    if oid is None:
        return
    for k, h in list(asic("STP_PORT").items()):
        if h["SAI_STP_PORT_ATTR_STP"] == oid:
            del dbs["ASIC_DB"][A + "STP_PORT:" + k]
    del dbs["ASIC_DB"][A + "STP:" + oid]


SAI = {0: "BLOCKING", 1: "BLOCKING", 2: "BLOCKING", 3: "LEARNING", 4: "FORWARDING"}


def take(table, op, key, fields):
    """One op as its orch does it; False if it stays queued (retried)."""
    row = "%s:%s" % (table, key)
    if table == "STP_PORT_STATE_TABLE":
        name, vid = key.rsplit(":", 1)
        vid = int(vid)
        if op == "DEL":
            oid = stp_port(name, vid)
            if oid:
                del dbs["ASIC_DB"][A + "STP_PORT:" + oid]
            dbs["APPL_DB"].pop(row, None)
            return True
        dbs["APPL_DB"][row] = dict(fields)
        if vid not in inst:
            add_instance(vid)
        oid = stp_port(name, vid)
        if oid is None:
            oid = new_oid("STP_PORT")
            dbs["ASIC_DB"][A + "STP_PORT:" + oid] = {"SAI_STP_PORT_ATTR_BRIDGE_PORT": bridge_port(name),
                                                    "SAI_STP_PORT_ATTR_STP": inst[vid]}
        dbs["ASIC_DB"][A + "STP_PORT:" + oid]["SAI_STP_PORT_ATTR_STATE"] = \
            "SAI_STP_PORT_STATE_" + SAI[int(fields.get("state", "0"))]
        return True
    vid = int(key[4:])
    voi = vlan_oid(vid)
    if table == "STP_VLAN_INSTANCE_TABLE":
        if voi is None:
            return False  # getPort fails: the op stays in m_toSync
        if op == "SET":
            n = int(fields["stp_instance"])
            if n not in inst:
                add_instance(n)
            dbs["ASIC_DB"][A + "VLAN:" + voi]["SAI_VLAN_ATTR_STP_INSTANCE"] = inst[n]
            mapped_to[vid] = n
            dbs["APPL_DB"][row] = dict(fields)
        else:
            dbs["ASIC_DB"][A + "VLAN:" + voi]["SAI_VLAN_ATTR_STP_INSTANCE"] = default
            remove_instance(mapped_to.pop(vid, -1))
            dbs["APPL_DB"].pop(row, None)
        return True
    if table == "VLAN_TABLE":
        if op == "SET":
            if voi is None:
                dbs["ASIC_DB"][A + "VLAN:" + new_oid("VLAN")] = {"SAI_VLAN_ATTR_VLAN_ID": str(vid)}
            dbs["APPL_DB"][row] = dict(fields)
        else:
            if voi is not None:
                if vid in mapped_to:
                    remove_instance(mapped_to.pop(vid))
                del dbs["ASIC_DB"][A + "VLAN:" + voi]
            dbs["APPL_DB"].pop(row, None)
        return True
    dbs["APPL_DB"][row] = dict(fields)  # the flush table: nothing to model
    return True


mode = ["now"]
queue, stuck, writes = [], [], []


def orch():
    """One of orchagent's passes: portsorch's tables, then StpOrch's."""
    todo = stuck + queue
    del stuck[:], queue[:]
    for t in todo:
        if t[0] == "VLAN_TABLE" and not take(*t):
            stuck.append(t)
    for t in todo:
        if t[0] != "VLAN_TABLE" and not take(*t):
            stuck.append(t)


def submit(table, op, key, fields):
    writes.append((table, op, key, fields.get("state", fields.get("stp_instance", ""))))
    queue.append((table, op, key, fields))
    if mode[0] == "now":
        orch()


class Producer(object):
    def __init__(self, table):
        self.table = table

    def set(self, key, fvs):
        submit(self.table, "SET", key, dict(fvs))

    def _del(self, key):
        submit(self.table, "DEL", key, {})


class Raw(object):
    def __init__(self, table):
        self.table = table

    def _del(self, key):
        dbs["APPL_DB"].pop("%s:%s" % (self.table, key), None)


l2d.put = lambda table, key, pairs: table.set(key, pairs)
l2d.bridge_ports = lambda: sorted(brif)
l2d.in_bridge = lambda p: p in brif
l2d.kstate = lambda p: brport.get(p) if p in brif else None

d = l2d.L2d()
db = d.db
db.appl, db.cfg, db.state, db.asic, db.counters = "APPL_DB", "CONFIG_DB", "STATE_DB", "ASIC_DB", "COUNTERS_DB"
db.keys = lambda conn, pattern: sorted(k for k in dbs[conn] if fnmatch.fnmatchcase(k, pattern))
db.hgetall = lambda conn, key: dict(dbs[conn].get(key, {}))
db.port_pst, db.vlan_pst, db.flush_pst = Producer("STP_PORT_STATE_TABLE"), Producer("STP_VLAN_INSTANCE_TABLE"), \
    Producer("STP_FASTAGEING_FLUSH_TABLE")
db.vlan_tbl_pst = Producer("VLAN_TABLE")
db.port_raw, db.vlan_raw, db.flush_raw = Raw("STP_PORT_STATE_TABLE"), Raw("STP_VLAN_INSTANCE_TABLE"), \
    Raw("STP_FASTAGEING_FLUSH_TABLE")
db.st_port = Raw("DUI_STP_PORT_TABLE")
d.stp_on = True
d.settle = clock[0] + 6  # a restart maps its VLANs after the settle

if part == "cleanup":
    d.retired = {v: clock[0] - 100 for v in ids}
    passes = 12
else:
    passes = 5


def show(n):
    for t, op, key, val in writes:
        if t == "STP_PORT_STATE_TABLE" and key.endswith(":4094") and op == "SET":
            h = before4094.get(stp_port(key.rsplit(":", 1)[0], 4094) or "", {})
            if h.get("SAI_STP_PORT_ATTR_STATE", "").endswith(SAI[int(val)]):
                continue  # the state it had: no change in the chip
        if t == "STP_PORT_STATE_TABLE" and not key.endswith(":4094") and part == "race" and n == 1:
            continue  # the restart's burst into Vlan3001's instance: counted below
        print("%s %s:%s %s" % (op, t, key, val))
    print("PASS %d" % n)


for n in range(1, passes + 1):
    del writes[:]
    if part == "race" and n == 1:
        mode[0] = "queue"
        d.sync_stp()
        print("QUEUED %d" % len([q for q in queue if q[0] == "STP_PORT_STATE_TABLE" and q[2].endswith(":3001")]))
        # the CLI's `config vlan member del` and `config vlan del`; vlanmgrd's DEL
        for k in [k for k in dbs["CONFIG_DB"] if k == "VLAN|Vlan3001" or k.startswith("VLAN_MEMBER|Vlan3001|")]:
            del dbs["CONFIG_DB"][k]
        queue.insert(0, ("VLAN_TABLE", "DEL", "Vlan3001", {}))
        orch()
        mode[0] = "now"
        print("AFTER-ORCH 3001 %s ports=%d" % ("in" if 3001 in inst else "out",
                                               len([h for h in asic("STP_PORT").values()
                                                    if h["SAI_STP_PORT_ATTR_STP"] == inst.get(3001)])))
    else:
        d.sync_stp()
    show(n)
    clock[0] += l2d.SYNC

print("INSTANCES %s" % ",".join(str(v) for v in sorted(inst)))
print("OBJECTS %d OUTSIDE4094 %d" % (len(asic("STP")), len([h for h in asic("STP_PORT").values()
                                                           if h["SAI_STP_PORT_ATTR_STP"] != inst.get(4094)])))
print("VLANS %s" % ",".join(sorted(h["SAI_VLAN_ATTR_VLAN_ID"] for h in asic("VLAN").values() if "SAI_VLAN_ATTR_VLAN_ID" in h)))
now4094 = {k: dict(h) for k, h in asic("STP_PORT").items() if h["SAI_STP_PORT_ATTR_STP"] == inst.get(4094)}
print("STP4094 %s %d %d" % (dbs["ASIC_DB"][A + "VLAN:" + vlan_oid(4094)].get("SAI_VLAN_ATTR_STP_INSTANCE") == inst.get(4094),
                           len(now4094), len([k for k in before4094 if before4094[k] != now4094.get(k)])))
print("APPL %s" % (",".join(sorted(k for k in dbs["APPL_DB"] if k.startswith(("VLAN_TABLE:", "STP_VLAN_INSTANCE_TABLE:"))
                                   or (k.startswith("STP_PORT_STATE_TABLE:") and not k.endswith(":4094")))) or "-"))
print("STUCK %s" % (",".join("%s %s:%s" % (t[1], t[0], t[2]) for t in stuck) or "-"))
print("RETIRED %s" % (",".join(str(v) for v in sorted(getattr(d, "retired", {}))) or "-"))
for msg in logs:
    if "instance" in msg.lower() or "gone" in msg:
        print("LOG " + msg)
