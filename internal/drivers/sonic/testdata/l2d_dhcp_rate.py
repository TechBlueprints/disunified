# Runs disunified-l2d's DHCP rate detector (l2d.py's DhcpRate) without a
# switch: its packet filter through a small classic-BPF interpreter and its
# classifier on real frames, its per-second accounting, and its CoPP writes
# against a recording stand-in for the databases. Prints one line per check:
# "ok <what>" or "FAIL <what>: <detail>".
#
# usage: python3 l2d_dhcp_rate.py <l2d.py> <frames fixture>
import importlib.util
import struct
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
sw.FieldValuePairs = list
pkg = types.ModuleType("swsscommon")
pkg.swsscommon = sw
sys.modules["swsscommon"], sys.modules["swsscommon.swsscommon"] = pkg, sw

spec = importlib.util.spec_from_file_location("l2d", sys.argv[1])
l2d = importlib.util.module_from_spec(spec)
spec.loader.exec_module(l2d)
logs = []
l2d.log = lambda m, prio=0: logs.append(m)
l2d.COPP_CFG = "/nonexistent"  # the stock trap ids
frames = [bytes.fromhex(l) for l in open(sys.argv[2]).read().split("\n") if l and not l.startswith("#")]


def check(what, cond, detail=""):
    print(("ok " if cond else "FAIL ") + what + ("" if cond else ": " + str(detail)))


def bpf(prog, pkt):
    """The subset of classic BPF the filter uses (net/core/filter.c)."""
    a = x = pc = 0
    while True:
        code, jt, jf, k = prog[pc]
        pc += 1
        if code == 0x28:    # ldh abs
            if k + 2 > len(pkt): return 0
            a = struct.unpack("!H", pkt[k:k + 2])[0]
        elif code == 0x30:  # ldb abs
            if k + 1 > len(pkt): return 0
            a = pkt[k]
        elif code == 0x48:  # ldh ind
            if x + k + 2 > len(pkt): return 0
            a = struct.unpack("!H", pkt[x + k:x + k + 2])[0]
        elif code == 0xB1:  # ldxb 4*([k]&0xf)
            if k + 1 > len(pkt): return 0
            x = (pkt[k] & 0x0F) * 4
        elif code == 0x15:  # jeq
            pc += jt if a == k else jf
        elif code == 0x45:  # jset
            pc += jt if a & k else jf
        elif code == 0x05:  # ja
            pc += k
        elif code == 0x06:  # ret
            return k
        else:
            raise ValueError("opcode %#x" % code)


def untag(f):
    return f[:12] + f[16:]


def setbyte(f, i, v):
    return f[:i] + bytes([v]) + f[i + 1:]


F = l2d.DhcpRate.FILTER
P = 18 + 20 + 8  # the BOOTP header in the tagged fixture frames
for i, f in enumerate(frames):
    check("frame %d tagged passes the filter" % i, bpf(F, f) == 256, bpf(F, f))
    check("frame %d untagged passes the filter" % i, bpf(F, untag(f)) == 256)
    check("frame %d is a client request" % i, l2d.DhcpRate.classify(f) and l2d.DhcpRate.classify(untag(f)))
f = frames[0]
check("a reply (to port 68) is filtered out", bpf(F, f[:18 + 20 + 2] + b"\x00\x44" + f[18 + 20 + 4:]) == 0)
check("a fragment is filtered out", bpf(F, setbyte(f, 18 + 7, 1)) == 0)
check("TCP is filtered out", bpf(F, setbyte(f, 18 + 9, 6)) == 0)
check("ARP is filtered out", bpf(F, f[:16] + b"\x08\x06" + f[18:]) == 0)
check("a BOOTREPLY is not counted", not l2d.DhcpRate.classify(setbyte(f, P, 2)))
check("a relayed request (giaddr set) is not counted", not l2d.DhcpRate.classify(setbyte(f, P + 24, 192)))
check("a truncated frame is not counted", not l2d.DhcpRate.classify(f[:P + 10]))


class Tbl(object):
    def __init__(self, name, rec):
        self.name, self.rec = name, rec

    def set(self, key, fvs):
        self.rec.append(("set", self.name, key, dict(fvs)))

    def _del(self, key):
        self.rec.append(("del", self.name, key))


class FakeDB(object):
    def __init__(self):
        self.rec, self.rows = [], {}
        self.cfg, self.state = "CONFIG_DB", "STATE_DB"
        self.copp_group = Tbl("COPP_GROUP", self.rec)
        self.copp = Tbl("COPP_TRAP", self.rec)

    def hgetall(self, conn, key):
        return dict(self.rows.get(key, {}))

    def keys(self, conn, pattern):
        return [k for k in self.rows if k.startswith(pattern.rstrip("*"))]

    def apply(self):
        for r in self.rec:
            key = (r[1] + "|" + r[2]) if r[1].startswith("COPP") else r[2]
            if r[0] == "set":
                self.rows[key] = r[3]
            else:
                self.rows.pop(key, None)


db = FakeDB()
db.rows["FEATURE|dhcp_relay"] = {"state": "enabled"}
g = l2d.DhcpRate(db)
g.table = Tbl("DUI_DHCP_RATE_TABLE", db.rec)
class FakeSock(object):
    def close(self):
        pass


g.open = lambda: setattr(g, "sock", FakeSock())
conf = l2d.DhcpRate.load({"enabled": True, "threshold": 300, "rate": 600})
g.configure(conf)
sets = [(r[1], r[2]) for r in db.rec if r[0] == "set"]
check("the group goes before the trap", sets == [("COPP_GROUP", "queue3_group1"), ("COPP_TRAP", "dhcp_relay")], sets)
check("the group is the ECS's shape at the configured rate",
      db.rec[0][3] == {"cbs": "600", "cir": "600", "meter_type": "packets", "mode": "sr_tcm", "queue": "3",
                       "red_action": "drop", "trap_action": "trap", "trap_priority": "3"}, db.rec[0][3])
check("the trap keeps the image's ids", db.rec[1][3] == {"trap_ids": "dhcp,dhcpv6", "trap_group": "queue3_group1"},
      db.rec[1][3])
db.apply()
del db.rec[:]
g.configure(conf)
check("a second sync writes nothing", db.rec == [], db.rec)

# accounting: 450 requests in one second, 150 in the next, 301, then 300 on
# another port: 150 + 1 over the threshold, peak 450, Ethernet4 nothing.
for sec, port, n in ((100, "Ethernet0", 450), (101, "Ethernet0", 150), (102, "Ethernet0", 301), (103, "Ethernet4", 300)):
    for _ in range(n):
        g.note(port, sec)
g.close_second()
rows = [r for r in db.rec if r[1] == "DUI_DHCP_RATE_TABLE"]
last = rows[-1] if rows else None
check("the count is the excess over 300 in each second",
      last == ("set", "DUI_DHCP_RATE_TABLE", "Ethernet0", {"last": "102", "over": "151", "peak": "450"}), last)
check("a port at the threshold is not counted", all(r[2] != "Ethernet4" for r in rows), rows)
check("one log line per episode", sum("received" in m for m in logs) == 1, logs)

del db.rec[:]
off = l2d.DhcpRate.load({"enabled": False})
l2d.time.time = lambda: 1000.0
g.configure(off)
check("stopping deletes the trap row first", [(r[0], r[1]) for r in db.rec] == [("del", "COPP_TRAP")], db.rec)
db.apply()
del db.rec[:]
l2d.time.time = lambda: 1003.0
g.configure(off)
check("the group goes after coppmgr moved the trap back", [(r[0], r[1]) for r in db.rec] == [("del", "COPP_GROUP")],
      db.rec)
