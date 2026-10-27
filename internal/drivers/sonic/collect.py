# disunified SONiC collector. Runs on the switch over SSH, once per poll, as
# `python3 -` with this file on stdin, and prints tagged sections
# ("@@@ <name>" lines) the Go side parses. Read-only; needs no sudo.
#
# Each Redis section is one JSON object {key: {field: value}}, one key per
# line, read through swsscommon's SonicV2Connector (present on every SONiC
# host); if that is missing, through the host's redis module on the unix
# socket with the db ids from database_config.json.
#
# Reduced on the switch: only the tables below are read. CONFIG_DB carries
# secrets elsewhere (TACPLUS_SERVER passkeys, RADIUS secrets, the SNMP
# community names), so nothing here dumps a whole database, and the SNMP
# section is key names only. Never widen this to a full redis dump.
import json
import os
import subprocess
import sys


class Swss(object):
    def __init__(self):
        from swsscommon.swsscommon import SonicV2Connector
        self.db = SonicV2Connector(use_unix_socket_path=True)
        self.connected = set()

    def _conn(self, name):
        if name not in self.connected:
            self.db.connect(name)
            self.connected.add(name)

    def keys(self, name, pattern):
        self._conn(name)
        return list(self.db.keys(name, pattern) or [])

    def get_all(self, name, key):
        self._conn(name)
        return dict(self.db.get_all(name, key) or {})


class Raw(object):
    """Fallback: the redis module on the unix socket."""

    def __init__(self):
        import redis  # noqa: F401 -- fail here, not on first use
        with open("/var/run/redis/sonic-db/database_config.json") as f:
            cfg = json.load(f)
        self.ids = {n: d["id"] for n, d in cfg["DATABASES"].items()}
        self.clients = {}

    def _c(self, name):
        if name not in self.clients:
            import redis
            self.clients[name] = redis.Redis(unix_socket_path="/var/run/redis/redis.sock", db=self.ids[name], decode_responses=True)
        return self.clients[name]

    def keys(self, name, pattern):
        return list(self._c(name).keys(pattern))

    def get_all(self, name, key):
        return dict(self._c(name).hgetall(key))


try:
    DB = Swss()
except Exception:
    DB = Raw()


def s(name):
    sys.stdout.write("@@@ " + name + "\n")


def dump(entries):
    """One JSON object, one key per line, sorted."""
    keys = sorted(entries)
    sys.stdout.write("{")
    for i, k in enumerate(keys):
        sys.stdout.write("\n" if i == 0 else ",\n")
        sys.stdout.write(json.dumps(k) + ": " + json.dumps(entries[k], sort_keys=True))
    sys.stdout.write("\n}\n" if keys else "}\n")


def table(db, patterns, keys_only=False):
    out = {}
    for p in patterns:
        try:
            ks = DB.keys(db, p)
        except Exception:
            continue
        for k in ks:
            out[k] = {} if keys_only else DB.get_all(db, k)
    return out


def section(name, db, *patterns, **kw):
    s(name)
    dump(table(db, patterns, kw.get("keys_only", False)))


def text(name, argv=None, path=None, first_line=False):
    s(name)
    try:
        if path:
            with open(path) as f:
                body = f.readline() if first_line else f.read()
        else:
            body = subprocess.run(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=10).stdout.decode("utf-8", "replace")
    except Exception:
        body = ""
    sys.stdout.write(body if body.endswith("\n") or body == "" else body + "\n")


# --- Redis ---
section("metadata", "CONFIG_DB", "DEVICE_METADATA|localhost")
section("state_metadata", "STATE_DB", "DEVICE_METADATA|localhost")
section("cfg_port", "CONFIG_DB", "PORT|*")
section("appl_port", "APPL_DB", "PORT_TABLE:*")
section("state_port", "STATE_DB", "PORT_TABLE|*")
section("cfg_vlan", "CONFIG_DB", "VLAN|*")
section("cfg_vlan_member", "CONFIG_DB", "VLAN_MEMBER|*")
section("cfg_lag", "CONFIG_DB", "PORTCHANNEL|*")
section("cfg_lag_member", "CONFIG_DB", "PORTCHANNEL_MEMBER|*")
section("appl_lag", "APPL_DB", "LAG_TABLE:*")
section("cfg_mgmt", "CONFIG_DB", "MGMT_INTERFACE|*", "MGMT_PORT|*", "VLAN_INTERFACE|*", "INTERFACE|*", "LOOPBACK_INTERFACE|*")
section("cfg_stp", "CONFIG_DB", "STP|*", "STP_VLAN|*", "STP_PORT|*", "STP_VLAN_PORT|*")
section("cfg_mirror", "CONFIG_DB", "MIRROR_SESSION|*")
section("cfg_ntp", "CONFIG_DB", "NTP_SERVER|*")
section("cfg_syslog", "CONFIG_DB", "SYSLOG_SERVER|*")
section("cfg_snmp", "CONFIG_DB", "SNMP_COMMUNITY|*", keys_only=True)
section("cfg_route", "CONFIG_DB", "STATIC_ROUTE|*")
section("cfg_dns", "CONFIG_DB", "DNS_NAMESERVER|*")
section("cfg_storm", "CONFIG_DB", "PORT_STORM_CONTROL|*")
section("cfg_breakout", "CONFIG_DB", "BREAKOUT_CFG|*")
section("cfg_feature", "CONFIG_DB", "FEATURE|*")
section("lldp_loc", "APPL_DB", "LLDP_LOC_CHASSIS")
section("lldp", "APPL_DB", "LLDP_ENTRY_TABLE:*")
section("fdb", "STATE_DB", "FDB_TABLE|*")
section("counters_map", "COUNTERS_DB", "COUNTERS_PORT_NAME_MAP")
section("counters", "COUNTERS_DB", "COUNTERS:oid:0x10*")
section("rates", "COUNTERS_DB", "RATES:oid:0x10*")
section("xcvr", "STATE_DB", "TRANSCEIVER_INFO|*")
section("dom", "STATE_DB", "TRANSCEIVER_DOM_SENSOR|*")
section("fan", "STATE_DB", "FAN_INFO|*")
section("psu", "STATE_DB", "PSU_INFO|*")
section("temp", "STATE_DB", "TEMPERATURE_INFO|*")
section("chassis", "STATE_DB", "CHASSIS_INFO|*")
section("asic_temp", "STATE_DB", "ASIC_TEMPERATURE_INFO")

# --- Linux ---
# Dynamic port breakout needs the platform's platform.json (absent on the VS).
s("platform_json")
try:
    _plat = (DB.get_all("CONFIG_DB", "DEVICE_METADATA|localhost") or {}).get("platform", "")
    sys.stdout.write(("yes" if _plat and os.path.exists("/usr/share/sonic/device/%s/platform.json" % _plat) else "no") + "\n")
except Exception:
    sys.stdout.write("no\n")
# The bridge's own block in the login user's authorized_keys -- only the
# lines between its markers, never the operator's keys.
s("authorized_keys")
try:
    _in = False
    with open(os.path.expanduser("~/.ssh/authorized_keys")) as _f:
        for _line in _f:
            _line = _line.rstrip("\n")
            if _line == "# disunified-managed-begin":
                _in = True
            elif _line == "# disunified-managed-end":
                _in = False
            elif _in and _line.strip():
                sys.stdout.write(_line + "\n")
except Exception:
    pass
text("hostname", ["hostname"])
text("version", path="/etc/sonic/sonic_version.yml")
text("uptime", path="/proc/uptime")
text("stat", path="/proc/stat", first_line=True)
text("loadavg", path="/proc/loadavg")
text("meminfo", path="/proc/meminfo")
text("addr", ["ip", "-j", "addr", "show"])
text("route", ["ip", "-j", "route", "show", "default"])
text("neigh", ["ip", "-j", "neigh", "show"])
s("end")
