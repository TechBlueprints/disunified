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
# Each module's loss-of-signal and transmit-fault flags (optic_fault.go).
section("xcvr_flag", "STATE_DB", "TRANSCEIVER_STATUS_FLAG|*")
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
# l2d's storm detection (11.21): frames above each port's storm-control limit.
section("l2_storm", "STATE_DB", "DUI_STORM_TABLE|*")
# Its DHCP rate detector's per-port counts (11.17).
section("l2_dhcp_rate", "STATE_DB", "DUI_DHCP_RATE_TABLE|*")
# MAC locking's count per interface (docs/drivers/sonic.md 11.18.2).
section("l2_maclock", "STATE_DB", "DUI_MACLOCK_TABLE|*")
# 802.1X (11.26): each controlled interface's mode, admitted hosts and
# failed attempts since boot.
section("l2_dot1x", "STATE_DB", "DUI_DOT1X_TABLE|*")
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
# DHCP guarding's count per port (docs/drivers/sonic.md 11.7.9): the guard
# table's type (ACL_TABLE_TYPE, its IN_PORTS match), whether orchagent polls
# ACL counters at all, and the packets of each counted rule (<table>:V<v>_IN_<port>).
section("cfg_acl_type", "CONFIG_DB", "ACL_TABLE_TYPE|DUI_*")
section("cfg_flex_acl", "CONFIG_DB", "FLEX_COUNTER_TABLE|ACL")


def acl_counters(name):
    s(name)
    out = {}
    try:
        rules = DB.get_all("COUNTERS_DB", "ACL_COUNTER_RULE_MAP")
    except Exception:
        rules = {}
    for rule, oid in rules.items():
        if not rule.startswith("DUI_") or "_IN_" not in rule:
            continue
        try:
            c = DB.get_all("COUNTERS_DB", "COUNTERS:" + oid)
        except Exception:
            c = {}
        out[rule] = {"oid": oid, "SAI_ACL_COUNTER_ATTR_PACKETS": c.get("SAI_ACL_COUNTER_ATTR_PACKETS", "")}
    dump(out)


acl_counters("acl_counters")
# IGMP snooping (the l2mcd series, docs/drivers/sonic.md 8.10): a snooping
# VLAN's row, its unknown-multicast setting and its static groups, as the
# CLI and the driver write them. Absent on an image without the series.
section("cfg_l2mc", "CONFIG_DB", "L2MC|*", "L2MC_SUPPRESS|*", "L2MC_STATIC_GROUP|*", "L2MC_STATIC_MEMBER|*", "L2MC_MROUTER|*")


def bridge_ports(name):
    """Each port's and PortChannel's bridge port in the chip, by name:
    {"admin": SAI_BRIDGE_PORT_ATTR_ADMIN_STATE}. orchagent sets a bridge
    port admin down only to remove it; one left down is a removal orchagent
    refused, which the kit's L2MC orch before its sonic-swss 0004 made
    (docs/drivers/sonic.md 8.15): the port learns nothing."""
    s(name)
    names = {}
    for m in ("COUNTERS_PORT_NAME_MAP", "COUNTERS_LAG_NAME_MAP"):
        try:
            names.update({oid: n for n, oid in DB.get_all("COUNTERS_DB", m).items()})
        except Exception:
            pass
    out = {}
    for k in table("ASIC_DB", ["ASIC_STATE:SAI_OBJECT_TYPE_BRIDGE_PORT:*"]).items():
        f = k[1]
        n = names.get(f.get("SAI_BRIDGE_PORT_ATTR_PORT_ID", ""))
        if f.get("SAI_BRIDGE_PORT_ATTR_TYPE") == "SAI_BRIDGE_PORT_TYPE_PORT" and n:
            out[n] = {"admin": f.get("SAI_BRIDGE_PORT_ATTR_ADMIN_STATE", "")}
    dump(out)


bridge_ports("asic_bridge_port")

# Port isolation, port security and egress shaping (docs/drivers/sonic.md
# 11.12): the driver's isolation group, the provisioned FDB entries (only
# provisioning writes APPL_DB FDB_TABLE; learnt MACs are STATE_DB's), the
# driver's shaper profiles and every port's QoS map.
section("appl_iso", "APPL_DB", "ISOLATION_GROUP_TABLE:DUI_*")
section("appl_fdb", "APPL_DB", "FDB_TABLE:*")
section("cfg_qos", "CONFIG_DB", "SCHEDULER|DUI_*", "PORT_QOS_MAP|*")
# The router interfaces' addresses as orchagent knows them (intfmgrd's rows
# from CONFIG_DB, and the driver's for a SLAAC address, ipv6.go): a row is
# what gives an address its ip2me route in the chip.
section("appl_intf", "APPL_DB", "INTF_TABLE:*")
# LLDP-MED: the TIA TLVs lldpd sends per port (the driver's custom TLVs,
# OUI 00-12-BB): one "<port> <capabilities>|- <network policy>|-" line per
# local interface, each value as lldpcli reports it; empty when lldpcli
# cannot be read.
s("lldp_med")
try:
    _out = subprocess.run(["docker", "exec", "lldp", "lldpcli", "-f", "json", "show", "interfaces", "details"],
                          stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=10).stdout
    _ifs = (json.loads(_out or b"{}").get("lldp") or {}).get("interface") or {}
    if isinstance(_ifs, list):
        _ifs = {k: v for d in _ifs for k, v in d.items()}
    for _n in sorted(_ifs):
        _u = (_ifs[_n] or {}).get("unknown-tlvs") or {}
        _u = _u.get("unknown-tlv") or []
        if isinstance(_u, dict):
            _u = [_u]
        _med = {str(t.get("subtype")): str(t.get("value", "")) for t in _u if str(t.get("oui", "")).lower() == "00,12,bb"}
        sys.stdout.write("%s %s %s\n" % (_n, _med.get("1") or "-", _med.get("2") or "-"))
except Exception:
    pass

# The DHCP servers the driver runs on routed networks (l3.go, §11.13):
# one "<iface> <active state> <sha256 of its udhcpd config>" line per
# config file the driver keeps; empty when there is none.
s("dhcpd")
try:
    import hashlib
    _dir = "/etc/sonic/disunified-dhcpd"
    for _f in sorted(os.listdir(_dir)) if os.path.isdir(_dir) else []:
        if not _f.endswith(".conf"):
            continue
        _n = _f[:-5]
        with open(os.path.join(_dir, _f), "rb") as _h:
            _sum = hashlib.sha256(_h.read()).hexdigest()
        _st = subprocess.run(["systemctl", "is-active", "disunified-dhcpd-" + _n], stdout=subprocess.PIPE,
                             stderr=subprocess.DEVNULL, timeout=10).stdout.decode().strip() or "unknown"
        sys.stdout.write("%s %s %s\n" % (_n, _st, _sum))
except Exception:
    pass

# The BGP objects bgpd runs (bgp.go, docs/drivers/sonic.md 11.14): one line
# per top-level object of `show running-config bgpd` -- "router bgp <asn>
# [vrf <name>]", "route-map <name>", "ip prefix-list <name>", ... -- its
# name only, never its body (a neighbor's password is in there); "error"
# when bgpd cannot be read.
s("frr_bgp")
try:
    _r = subprocess.run(["docker", "exec", "bgp", "vtysh", "-c", "show running-config bgpd"], stdout=subprocess.PIPE,
                        stderr=subprocess.DEVNULL, timeout=15)
    if _r.returncode != 0:
        raise IOError("vtysh")
    _seen = set()
    for _line in _r.stdout.decode("utf-8", "replace").split("\n"):
        if not _line or _line[0].isspace():
            continue
        _t = _line.split()
        _k = None
        if _t[:2] == ["router", "bgp"] and len(_t) >= 3:
            _k = _t[:5] if len(_t) >= 5 and _t[3] == "vrf" else _t[:3]
        elif _t[0] == "route-map" and len(_t) >= 2:
            _k = _t[:2]
        elif _t[0] in ("ip", "ipv6") and len(_t) >= 3 and _t[1] in ("prefix-list", "access-list"):
            _k = _t[:3]
        elif _t[0] == "access-list" and len(_t) >= 2:
            _k = _t[:2]
        elif _t[:3] == ["bgp", "as-path", "access-list"] and len(_t) >= 4:
            _k = _t[:4]
        elif _t[0] == "bgp" and len(_t) >= 3 and _t[1] in ("community-list", "extcommunity-list", "large-community-list"):
            _k = _t[:4] if _t[2] in ("standard", "expanded") and len(_t) >= 4 else _t[:3]
        if _k and " ".join(_k) not in _seen:
            _seen.add(" ".join(_k))
            sys.stdout.write(" ".join(_k) + "\n")
except Exception:
    sys.stdout.write("error\n")

# The driver's ospfd (ospf.go, docs/drivers/sonic.md 11.14): "running" when
# ospfd runs in the bgp container, "conf <sha256>" of the driver's file
# (never its content: it holds the authentication keys); "error" when the
# container cannot be read.
s("frr_ospf")
try:
    _r = subprocess.run(["docker", "exec", "bgp", "sh", "-c",
                         "pgrep -x ospfd >/dev/null && echo running; "
                         "f=/etc/frr/disunified-ospfd.conf; [ -f $f ] && echo conf $(sha256sum < $f | cut -d' ' -f1); true"],
                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=15)
    if _r.returncode != 0:
        raise IOError("docker exec")
    sys.stdout.write(_r.stdout.decode("utf-8", "replace"))
except Exception:
    sys.stdout.write("error\n")

# zebra's IPv4 default route (kroute.go, docs/drivers/sonic.md 6.4.1): what
# FRR selected and believes installed ("route", reduced from `show ip route
# 0.0.0.0/0 json`), the running config's own lines for it ("static"), and
# whether zebra installs routes through kernel nexthop objects
# ("kernel_nexthops": no "no zebra nexthop kernel enable" line). Only those
# lines are taken from the running config, which also holds the daemons'
# passwords. "error" when the bgp container cannot be read.
s("frr_route")
try:
    _r = subprocess.run(["docker", "exec", "bgp", "sh", "-c",
                         "vtysh -c 'show ip route 0.0.0.0/0 json' && echo @@@@ && vtysh -c 'show running-config'"],
                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=15)
    if _r.returncode != 0:
        raise IOError("docker exec")
    _js, _sep, _cfg = _r.stdout.decode("utf-8", "replace").partition("\n@@@@\n")
    if not _sep:
        raise IOError("vtysh")
    _rt = []
    for _e in (json.loads(_js or "{}") or {}).get("0.0.0.0/0", []):
        _rt.append({"protocol": _e.get("protocol", ""), "selected": bool(_e.get("selected")),
                    "installed": bool(_e.get("installed")), "nexthops": [
                        {"ip": _h.get("ip", ""), "interface": _h.get("interfaceName", ""),
                         "active": bool(_h.get("active")), "fib": bool(_h.get("fib"))}
                        for _h in _e.get("nexthops", [])]})
    _lines = [_l.strip() for _l in _cfg.split("\n")]
    dump({"route": _rt, "static": [_l for _l in _lines if _l.startswith("ip route 0.0.0.0/0 ")],
          "kernel_nexthops": "no zebra nexthop kernel enable" not in _lines})
except Exception:
    sys.stdout.write("error\n")

# --- Linux ---
# linux-bcm-genl's psample proc (the image kit's sonic-buildimage 0014): the
# SAI samples a port only with it, and l2d detects unknown-unicast storms
# from those samples (docs/drivers/sonic.md 11.24).
s("genl_psample")
sys.stdout.write("yes\n" if os.path.exists("/proc/bcm/genl/psample/rate") else "no\n")
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
    # hostapd for 802.1X (11.26), built on the switch as mstpd is
    _l2["hostapd"] = ""
    if os.access("/usr/local/sbin/hostapd", os.X_OK):
        _l2["hostapd"] = subprocess.run(["/usr/local/sbin/hostapd", "-v"], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                        timeout=5).stdout.decode().strip().split("\n")[0]
    # whether the RADIUS secrets file exists (its content is root's and never read here)
    _l2["dot1x_secret"] = os.path.exists("/etc/sonic/disunified/dot1x-radius")
    _l2["hostapd_build"] = "running" if _unit("is-active", "disunified-hostapd-build") == "active" else ""
    if not _l2["hostapd_build"]:
        try:
            with open("/var/lib/disunified/hostapd-build/status") as _f:
                _l2["hostapd_build"] = _f.read().strip()
        except Exception:
            pass
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
# The fan floor (fan.go, docs/drivers/sonic.md 11.14): on a platform whose
# fan control is fancontrol, the airflow the boot script detected and, for
# the platform's fancontrol, its fancontrol-<airflow> (copied over it at
# boot) and pmon's running /etc/fancontrol, the per-PWM values of the lines
# the fan modes set, each as its distinct values joined by ",". Empty on a
# platform without the files (the VS).
s("fancontrol")
_fc = {}
try:
    _plat = (DB.get_all("CONFIG_DB", "DEVICE_METADATA|localhost") or {}).get("platform", "")
    _dir = "/usr/share/sonic/device/%s" % _plat
    if _plat and os.path.isfile(os.path.join(_dir, "fancontrol")):
        _fc["platform"] = _plat
        try:
            with open(os.path.join(_dir, "fan_airflow")) as _f:
                _fc["airflow"] = _f.read().strip()
        except Exception:
            pass

        def _floor(body):
            _v = {}
            for _l in body.splitlines():
                _k, _eq, _rest = _l.partition("=")
                if _eq and _k in ("MINTEMP", "MINSTART", "MINSTOP", "MINPWM", "MAXTEMP", "MAXPWM"):
                    _v[_k] = ",".join(sorted(set(_t.rpartition("=")[2] for _t in _rest.split())))
            return _v
        _names = ["fancontrol"] + (["fancontrol-" + _fc["airflow"]] if _fc.get("airflow") else [])
        for _n in _names:
            try:
                with open(os.path.join(_dir, _n)) as _f:
                    _fc[_n] = _floor(_f.read())
            except Exception:
                pass
        try:
            _r = subprocess.run(["docker", "exec", "pmon", "cat", "/etc/fancontrol"], stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL, timeout=10)
            if _r.returncode == 0:
                _fc["pmon"] = _floor(_r.stdout.decode("utf-8", "replace"))
        except Exception:
            pass
except Exception:
    pass
dump(_fc)
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
# rsyslog.conf's SONiC templates: rsyslog-config.sh renders the Linux
# hostname into them as text, and a hostname change re-renders nothing, so
# syslog keeps the old name until rsyslog-config runs again (device.go).
# Only the template lines (the hostname and the build id); empty when the
# file cannot be read.
s("rsyslog_tmpl")
try:
    with open("/etc/rsyslog.conf") as _f:
        for _line in _f:
            if _line.startswith("$template SONiC"):
                sys.stdout.write(_line if _line.endswith("\n") else _line + "\n")
except Exception:
    pass
text("version", path="/etc/sonic/sonic_version.yml")
text("uptime", path="/proc/uptime")
text("stat", path="/proc/stat", first_line=True)
text("loadavg", path="/proc/loadavg")
text("meminfo", path="/proc/meminfo")
text("addr", ["ip", "-j", "addr", "show"])
text("route", ["ip", "-j", "route", "show", "default"])
text("neigh", ["ip", "-j", "neigh", "show"])
# IPv6 router advertisements (ipv6.go, docs/drivers/sonic.md §11.22): each
# in-band netdev's accept_ra -- SONiC forwards on every router interface, so
# there only 2 takes RAs -- and the IPv6 routes the kernel learned from RAs:
# the default (proto ra) and the on-link prefixes, which carry an expiry (a
# configured address's prefix route never does). "error" when unreadable.
s("ipv6_ra")
try:
    _ra = {}
    _d = "/proc/sys/net/ipv6/conf"
    for _n in sorted(os.listdir(_d)):
        if _n.startswith(("Vlan", "Ethernet", "PortChannel", "Loopback")):
            try:
                with open(os.path.join(_d, _n, "accept_ra")) as _f:
                    _ra[_n] = _f.read().strip()
            except Exception:
                pass
    _out = subprocess.run(["ip", "-6", "-j", "route", "show"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                          timeout=10).stdout
    _rt = [{"dst": _r.get("dst", ""), "dev": _r.get("dev", ""), "protocol": _r.get("protocol", "")}
           for _r in json.loads(_out or b"[]") if _r.get("protocol") == "ra" or "expires" in _r]
    dump({"accept_ra": _ra, "routes": _rt})
except Exception:
    sys.stdout.write("error\n")
# chrony's own view of its sources (docs/drivers/sonic.md §10.6), read
# through chronyd's localhost command port as the login user: the sources
# (CSV), and how many are online and offline -- a boot hook can leave every
# source offline, which `sources` cannot tell from unreachable (both read
# `?` with reach 0); the driver nudges chrony from `activity`.
text("chrony_sources", ["chronyc", "-n", "-c", "sources"])
text("chrony_activity", ["chronyc", "-n", "activity"])
s("end")
