# Runs what a restart of disunified-l2d does with mstpd already running (an
# update of l2d.py restarts the unit; §11.5: the restart adopts mstpd): a
# fresh L2d's stp_up and first sync_stp against a fake /sys and /proc built
# from a collector capture -- the bridge's ports and their states from
# DUI_STP_PORT_TABLE, mstpd's report from DUI_STP_TABLE, the config from
# the capture's l2d section -- and prints every subprocess as "RUN <argv>",
# every sysfs write as "WR <path> <value>" and every APPL_DB row as
# "PUT <table> <key> field=value ...". Then it prints mstpd's liveness
# (Mstp.running) for three /proc trees: "ALIVE pidfile|scan|gone <bool>".
#
# usage: python3 l2d_adopt.py <l2d.py> <capture> <tmpdir>
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

tmp = sys.argv[3]


def mstpctl_fields(fields):
    return {k.replace("_", "-"): v for k, v in fields.items()}


bridge = mstpctl_fields(json.loads(sections["l2_bridge"])["DUI_STP_TABLE|Bridge"])
bridge["force-protocol-version"] = bridge.pop("version")
ports = []
for key, fields in sorted(json.loads(sections["l2_port"]).items()):
    d = mstpctl_fields(fields)
    d["port"] = key.split("|", 1)[1]
    ports.append(d)
vids = {int(k.split("|Vlan", 1)[1]) for k in json.loads(sections["cfg_vlan"])}


def put_file(path, body, mode=0o644):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(body)
    os.chmod(path, mode)


# /sys/class/net: Bridge in user-space STP, each port with its kernel state
# (BR_STATE_FORWARDING 3 for a forwarding port, BR_STATE_DISABLED 0 for a
# down one, BR_STATE_BLOCKING 4 for any other discarding port).
sysd = os.path.join(tmp, "sys")
put_file(os.path.join(sysd, "Bridge", "bridge", "stp_state"), "2\n")
put_file(os.path.join(sysd, "Bridge", "ifindex"), "4\n")
for d in ports:
    os.makedirs(os.path.join(sysd, "Bridge", "brif", d["port"]), exist_ok=True)
    st = 3 if d["state"] == "forwarding" else 0 if d["role"] == "Disabled" else 4
    put_file(os.path.join(sysd, d["port"], "brport", "state"), "%d\n" % st)

hook = os.path.join(tmp, "bridge-stp")
put_file(hook, "#!/bin/sh\nexit 0\n", 0o755)
conf = os.path.join(tmp, "l2.json")
put_file(conf, json.dumps(json.loads(sections["l2d"])["conf"]))


def proc_tree(name, pidfile, procs):
    """A /proc with the given pid -> comm (None: the pid's dir is there but
    the process is gone, the race pidof logged), and a pid file."""
    root = os.path.join(tmp, name)
    os.makedirs(root)
    for pid, comm in procs.items():
        os.makedirs(os.path.join(root, pid))
        if comm is not None:
            put_file(os.path.join(root, pid, "comm"), comm + "\n")
    pf = os.path.join(tmp, name + ".pid")
    if pidfile is not None:
        put_file(pf, pidfile + "\n")
    return root, pf


l2d.SYS, l2d.HOOK, l2d.CONF = sysd, hook, conf
l2d.PROC, l2d.MSTPD_PID = proc_tree("proc", "7627", {"1": "systemd", "7627": "mstpd", "9001": None})


class R(object):
    def __init__(self, rc, out=""):
        self.returncode, self.stdout, self.stderr = rc, out, ""


def run(args, check=False):
    print("RUN " + " ".join(args))
    if args[0] == l2d.MSTPCTL:
        rest = args[1:]
        if rest[:2] == ["-f", "json"]:
            if rest[2] == "showbridge":
                return R(0, json.dumps([bridge]))
            if rest[2] == "showportdetail":
                return R(0, json.dumps(ports))
        if rest[0] == "showbridge":
            return R(0, "")
        return R(0)
    return R(1)


real_wr = l2d.wr


def wr(path, value):
    print("WR %s %s" % (path, value))
    real_wr(path, value)


l2d.run, l2d.wr = run, wr
l2d.put = lambda table, key, pairs: print("PUT %s %s %s" % (table.args[1], key, " ".join("%s=%s" % kv for kv in pairs)))
l2d.log = lambda msg, prio=None: print("LOG " + msg)


class FakeDB(object):
    def __init__(self):
        for t in ("appl", "cfg", "state", "asic"):
            setattr(self, t, None)
        for t, n in (("port_pst", "STP_PORT_STATE_TABLE"), ("vlan_pst", "STP_VLAN_INSTANCE_TABLE"),
                     ("copp", "COPP_TRAP"), ("copp_group", "COPP_GROUP")):
            setattr(self, t, _Stub(None, n))

    def vlans(self):
        return set(vids)

    def known(self, name):
        return True

    def stp_ports_in_asic(self):
        return len(ports) * len(vids)

    def keys(self, conn, pattern):
        return []

    def hgetall(self, conn, key):
        return {"stp_instance": "x"}


d = l2d.L2d()
d.db = FakeDB()
d.stp_on = d.stp_up()
print("STP_ON %s" % d.stp_on)
d.settle = 0
d.sync_stp()

for name, pidfile, procs in (
        ("pidfile", "7627", {"7627": "mstpd"}),
        ("scan", "4242", {"4242": "sh", "9001": None, "7627": "mstpd"}),
        ("gone", None, {"4242": "sh", "9001": None})):
    l2d.PROC, l2d.MSTPD_PID = proc_tree("proc-" + name, pidfile, procs)
    print("ALIVE %s %s" % (name, l2d.Mstp().running()))
