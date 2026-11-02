# Runs disunified-l2d's COPP install (l2d.py's copp_install) with swsscommon
# stubbed and prints each CONFIG_DB row it writes, in order, as
# "PUT <table>|<key> field=value ...".
#
# usage: python3 l2d_copp.py <l2d.py> [copp_cfg.json]
import importlib.util
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
l2d.put = lambda table, key, pairs: print("PUT %s|%s %s" % (table.args[1], key, " ".join("%s=%s" % kv for kv in pairs)))
l2d.COPP_CFG = sys.argv[2] if len(sys.argv) > 2 else "/nonexistent/copp_cfg.json"
l2d.L2d().copp_install()
