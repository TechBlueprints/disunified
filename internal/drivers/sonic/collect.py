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
# The SAI's answers about the switch (orchagent's SWITCH_CAPABILITY): 202511's
# `config mirror_session span add` refuses a direction whose
# PORT_INGRESS/EGRESS_MIRROR_CAPABLE flag is not 'true' (docs/drivers/sonic.md
# 11.8), so the driver reads them to know when to write the session itself.
section("state_switch_cap", "STATE_DB", "SWITCH_CAPABILITY|switch")
section("cfg_vlan", "CONFIG_DB", "VLAN|*")
section("cfg_vlan_member", "CONFIG_DB", "VLAN_MEMBER|*")
section("cfg_lag", "CONFIG_DB", "PORTCHANNEL|*")
section("cfg_lag_member", "CONFIG_DB", "PORTCHANNEL_MEMBER|*")
section("appl_lag", "APPL_DB", "LAG_TABLE:*")
# Each member's LACP state: tlm_teamd mirrors `teamdctl <PortChannel> state
# dump` into STATE_DB (partner system, actor/partner state, selection), so
# no `docker exec` into the teamd container is needed.
section("state_lag_member", "STATE_DB", "LAG_MEMBER_TABLE|*")
section("cfg_mgmt", "CONFIG_DB", "MGMT_INTERFACE|*", "MGMT_PORT|*", "VLAN_INTERFACE|*", "INTERFACE|*", "LOOPBACK_INTERFACE|*", "PORTCHANNEL_INTERFACE|*")
section("cfg_stp", "CONFIG_DB", "STP|*", "STP_VLAN|*", "STP_PORT|*", "STP_VLAN_PORT|*")
section("cfg_mirror", "CONFIG_DB", "MIRROR_SESSION|*")
section("cfg_ntp", "CONFIG_DB", "NTP_SERVER|*")
section("cfg_ntp_global", "CONFIG_DB", "NTP|global")
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
# disunified-l2d's view (docs/drivers/sonic.md 11.5): mstpd's bridge and
# ports, and the ports loop protection holds down.
section("l2_bridge", "STATE_DB", "DUI_STP_TABLE|*")
section("l2_port", "STATE_DB", "DUI_STP_PORT_TABLE|*")
section("l2_guard", "STATE_DB", "DUI_LOOP_GUARD_TABLE|*")
# orchagent's resource counters: the FDB's used and available entries.
section("crm", "COUNTERS_DB", "CRM:STATS")
# The bridge's own ACL tables (docs/drivers/sonic.md 11.7.5) -- DHCP
# guarding and the controller's IP rules, all named DUI_*; nothing else in
# ACL_TABLE or ACL_RULE is read. orchagent's verdict on each is in STATE_DB;
# CRM counts the ingress ACL tables left per bind point and each table's
# entries.
section("cfg_acl_table", "CONFIG_DB", "ACL_TABLE|DUI_*")
section("cfg_acl_rule", "CONFIG_DB", "ACL_RULE|DUI_*")
section("state_acl", "STATE_DB", "ACL_TABLE_TABLE|DUI_*", "ACL_RULE_TABLE|DUI_*")
section("crm_acl", "COUNTERS_DB", "CRM:ACL_STATS:INGRESS:*", "CRM:ACL_TABLE_STATS:*")

# --- Linux ---
# Dynamic port breakout needs the platform's platform.json (absent on the
# VS): "no", or its interfaces block reduced to what breakout needs -- each
# parent port's index, lanes and breakout modes (mode -> child aliases) --
# one port per line like the Redis sections.
s("platform_json")
try:
    _plat = (DB.get_all("CONFIG_DB", "DEVICE_METADATA|localhost") or {}).get("platform", "")
    _path = "/usr/share/sonic/device/%s/platform.json" % _plat
    if not _plat or not os.path.exists(_path):
        raise IOError("no platform.json")
    with open(_path) as _f:
        _intf = json.load(_f).get("interfaces") or {}
    dump({n: {"index": str(v.get("index", "")), "lanes": str(v.get("lanes", "")), "breakout_modes": v.get("breakout_modes") or {}}
          for n, v in _intf.items() if isinstance(v, dict)})
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
# The SNMP communities /etc/sonic/snmp.yml names: the snmp container's
# start.sh copies them into CONFIG_DB SNMP_COMMUNITY on every start
# (snmp_yml_to_configdb.py), so one deleted from CONFIG_DB alone comes
# back. Names only, like cfg_snmp: community -> the yml key that holds it.
# "{}" when the file is absent; "error" when it cannot be read.
s("snmp_yml")
try:
    _y = {}
    if os.path.exists("/etc/sonic/snmp.yml"):
        import yaml
        with open("/etc/sonic/snmp.yml") as _f:
            _y = yaml.safe_load(_f) or {}
    _c = {}
    for _k in ("snmp_rocommunity", "snmp_rocommunities", "snmp_rwcommunity", "snmp_rwcommunities"):
        _v = _y.get(_k)
        for _n in (_v if isinstance(_v, list) else [_v] if _v is not None else []):
            _c[str(_n)] = {"key": _k}
    dump(_c)
except Exception:
    sys.stdout.write("error\n")
# disunified-l2d's installation, one JSON object: the installed files'
# sha256 ("" = absent), mstpd's version, the units' states, whether the
# switch can build mstpd (docker), the kernel bridge's stp_state and the
# daemon's config (no secrets in it).
s("l2d")
try:
    import hashlib

    def _sha(path):
        try:
            with open(path, "rb") as f:
                return hashlib.sha256(f.read()).hexdigest()
        except Exception:
            return ""

    def _unit(*args):
        try:
            return subprocess.run(["systemctl"] + list(args), stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                  timeout=5).stdout.decode().strip()
        except Exception:
            return ""

    _l2 = {"files": {p: _sha(p) for p in ("/usr/local/sbin/disunified-l2d", "/usr/sbin/bridge-stp",
                                          "/etc/systemd/system/disunified-l2d.service")}}
    _l2["mstpd"] = ""
    if os.access("/usr/local/sbin/mstpd", os.X_OK) and os.access("/usr/local/sbin/mstpctl", os.X_OK):
        _l2["mstpd"] = subprocess.run(["/usr/local/sbin/mstpd", "-V"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                      timeout=5).stdout.decode().strip()
    _l2["service"] = _unit("is-active", "disunified-l2d")
    _l2["deadman"] = _unit("is-active", "disunified-l2d-deadman.timer")
    _l2["build"] = "running" if _unit("is-active", "disunified-mstpd-build") == "active" else ""
    if not _l2["build"]:
        try:
            with open("/var/lib/disunified/mstpd-build/status") as _f:
                _l2["build"] = _f.read().strip()
        except Exception:
            pass
    _l2["docker"] = os.access("/usr/bin/docker", os.X_OK)
    try:
        with open("/sys/class/net/Bridge/bridge/stp_state") as _f:
            _l2["stp_state"] = _f.read().strip()
    except Exception:
        _l2["stp_state"] = ""
    _l2["conf"] = None
    try:
        with open("/etc/sonic/disunified/l2.json") as _f:
            _l2["conf"] = json.load(_f)
    except Exception:
        pass
    sys.stdout.write(json.dumps(_l2, sort_keys=True) + "\n")
except Exception:
    sys.stdout.write("error\n")
# The hardware monitors (sysfs hwmon), each chip reduced to its name, its
# device ("<i2c bus>-<addr>" for a PMBus supply) and its *_input/*_label
# readings: pmon's PSU_INFO keeps one supply temperature and the output
# power, while the supply's own chip has every sensor, its fan's RPM and
# the input power.
s("hwmon")
_h = {}
try:
    for _d in sorted(os.listdir("/sys/class/hwmon")):
        _p = os.path.join("/sys/class/hwmon", _d)
        _e = {}
        for _n in sorted(os.listdir(_p)):
            if _n == "name" or _n.endswith(("_input", "_label")):
                try:
                    with open(os.path.join(_p, _n)) as _f:
                        _e[_n] = _f.read().strip()
                except Exception:
                    pass
        _e["device"] = os.path.basename(os.path.realpath(os.path.join(_p, "device")))
        _h[_d] = _e
except Exception:
    pass
dump(_h)
# The SSH connection this run came in on, as the switch sees it: the
# client's address and the switch address it dialled -- the path the IP
# ACL rules must not cut (acl.go). Empty outside an SSH session.
s("ssh")
_ssh = (os.environ.get("SSH_CONNECTION") or "").split()
sys.stdout.write((_ssh[0] + " " + _ssh[2] + "\n") if len(_ssh) == 4 else "\n")
text("hostname", ["hostname"])
text("version", path="/etc/sonic/sonic_version.yml")
text("uptime", path="/proc/uptime")
text("stat", path="/proc/stat", first_line=True)
text("loadavg", path="/proc/loadavg")
text("meminfo", path="/proc/meminfo")
text("addr", ["ip", "-j", "addr", "show"])
text("route", ["ip", "-j", "route", "show", "default"])
text("neigh", ["ip", "-j", "neigh", "show"])
# chrony's own view of its sources (docs/drivers/sonic.md §10.6), read
# through chronyd's localhost command port as the login user: the sources
# (CSV), and how many are online and offline -- a boot hook can leave every
# source offline, which `sources` cannot tell from unreachable (both read
# `?` with reach 0); the driver nudges chrony from `activity`.
text("chrony_sources", ["chronyc", "-n", "-c", "sources"])
text("chrony_activity", ["chronyc", "-n", "activity"])
s("end")
