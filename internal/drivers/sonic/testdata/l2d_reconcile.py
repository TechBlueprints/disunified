# Runs disunified-l2d's settings reconcile (l2d.py) twice against mstpd's
# reported state as a collector capture holds it -- STATE_DB DUI_STP_TABLE
# and DUI_STP_PORT_TABLE, which l2d publishes from mstpctl's JSON -- and a
# config file, and prints each mstpctl call and log line. The second pass
# runs on the state the first pass's calls produced.
#
# usage: python3 l2d_reconcile.py <l2d.py> <capture> <l2.json>
import importlib.util
import json
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

sections, name = {}, None
for line in open(sys.argv[2]):
    if line.startswith("@@@ "):
        name = line[4:].strip()
        sections[name] = ""
    elif name:
        sections[name] += line


def mstpctl_fields(fields):
    return {k.replace("_", "-"): v for k, v in fields.items()}


b = mstpctl_fields(json.loads(sections["l2_bridge"])["DUI_STP_TABLE|Bridge"])
b["force-protocol-version"] = b.pop("version")
ports = []
for key, fields in sorted(json.loads(sections["l2_port"]).items()):
    d = mstpctl_fields(fields)
    d["port"] = key.split("|", 1)[1]
    ports.append(d)
by_port = {d["port"]: d for d in ports}


class FakeMstp(object):
    """mstpctl's set commands, applied to the reported state."""

    def ctl(self, *args):
        print("CALL " + " ".join(args))
        cmd = args[0]
        if cmd == "settreeprio":
            b["bridge-id"] = "%X.%s" % (int(args[3]), b["bridge-id"].split(".", 1)[1])
        elif cmd == "setforcevers":
            b["force-protocol-version"] = args[2]
        else:
            field = dict((c, f) for f, c in l2d.PORT_SETTINGS)[cmd]
            by_port[args[2]][field] = args[3]
        return True


l2d.log = lambda msg, prio=None: print("LOG " + msg)
l2d.CONF = sys.argv[3]
d = l2d.L2d()
d.mstp = FakeMstp()
d.conf = l2d.load_conf()
print("PASS 1")
d.reconcile(b, ports)
print("PASS 2")
d.reconcile(b, ports)
