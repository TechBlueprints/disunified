# disunified-l2d: spanning tree and loop protection on a SONiC switch the
# disunified bridge presents to UniFi. The bridge's sonic driver installs it
# as /usr/local/sbin/disunified-l2d and runs it as root under systemd
# (disunified-l2d.service); docs/drivers/sonic.md section 11.5 is the design.
#
# RSTP: mstpd runs the protocol on the kernel bridge "Bridge" (vlanmgrd's;
# every switched port is a member). This daemon starts mstpd, hands it the
# bridge (stp_state 1 -> /sbin/bridge-stp answers 0 -> user-space STP),
# keeps mstpd's settings at the controller's (CONF, checked against mstpd's
# own report every 2 s and set with mstpctl where they differ), and mirrors each
# port state mstpd sets in the kernel into the ASIC through the stock
# orchagent's StpOrch (APPL_DB STP_* tables), within milliseconds, from
# rtnetlink's bridge-port notifications.
#
# StpOrch facts this file is built around (sonic-swss 202505 stporch.cpp):
# - an instance id maps to a SAI STP object created on first use, and a VLAN
#   moved into it blocks every port that has no state there yet, so every
#   bridge port gets a state in a VLAN's instance before the VLAN is mapped
#   (checked in ASIC_DB);
# - deleting a VLAN removes every port state of the VLAN's instance, so each
#   VLAN has its own instance (instance id = VLAN id) and the others keep
#   theirs;
# - a port-state key naming a port orchagent does not know stops that
#   table's queue for good (`return`, not `continue`), so only ports present
#   in APPL_DB and STATE_DB are written, and keys of ports or VLANs that are
#   gone are dropped from APPL_DB directly, never through the producer;
# - a state for a port with no bridge port makes addStpPort create one (and
#   the STP port), so nothing is written for a port leaving Bridge: taken
#   after portsorch removed the bridge port, it left a PortChannel member
#   with STP ports of its own, and the chip then refuses the PortChannel's
#   (repair_strays, docs/drivers/sonic.md 11.34).
#
# A topology change: mstpd flushes the kernel FDB of a port through sysfs,
# and the kernel announces every bridge-port sysfs write with an
# RTM_NEWLINK; a notification whose state did not change is taken as that
# flush, and the port's VLANs are flushed in the ASIC
# (STP_FASTAGEING_FLUSH_TABLE).
#
# Loop protection (Ubiquiti's MAC_MOVE_GUARD semantics on their own SONiC
# switches): a MAC moving between ports `threshold` times within `interval`
# seconds takes the guarded port it moved to down (APPL_DB PORT_TABLE
# admin_status, so CONFIG_DB and the saved config keep the operator's
# setting) for `hold` seconds -- never the STP root port nor the port the
# default gateway is learned on.
#
# BPDU guard: mstpd shuts a guarded port's netdev when a BPDU arrives; l2d
# then holds the port down in the chip the same way (APPL_DB admin_status)
# until the operator's Re-enable in UniFi (the controller's port-recovery,
# which the driver posts as STATE_DB DUI_PORT_RECOVERY|<port>), the guard or
# STP going off for the port, or spanning tree off (§11.28). The same
# Re-enable ends a loop-protection hold early.
#
# Storm detection (StormWatch, section 11.21): a port whose incoming broadcast
# or multicast exceeds its storm-control limit is counted, never taken down.
#
# The DHCP rate (UniFi's "Incoming DHCP packets exceed 300/sec"): class
# DhcpRate counts each port's DHCP client requests at the CPU and reports
# the excess over 300 in any one second; it acts on nothing (11.17).
#
# MAC locking (UniFi's MAC Address Filter; MacLockGuard below, 11.18.2): a
# secured port's unknown sources go to the CPU instead of being dropped
# unseen, the kernel bridge drops them there, and each frame from a source
# not on the port's list is counted for the port, which stays up.
#
# 802.1X (UniFi's 802.1X Control; Dot1xGuard below, 11.26): a hostapd per
# controlled port relays its hosts' EAP to the RADIUS servers; each host it
# authorises is admitted with a static FDB entry, and each failed attempt is
# counted for the port.
#
# Everything the bridge reads back is published in STATE_DB:
# DUI_STP_TABLE|Bridge, DUI_STP_PORT_TABLE|<port>, DUI_LOOP_GUARD_TABLE|<port>,
# DUI_BPDU_GUARD_TABLE|<port>,
# DUI_STORM_TABLE|<port>, DUI_DHCP_RATE_TABLE|<port>, DUI_MACLOCK_TABLE|<interface>,
# DUI_DOT1X_TABLE|<interface>.
#
# `disunified-l2d --teardown` undoes it: every VLAN back to the default STP
# instance, every port forwarding in the kernel, stp_state 0, mstpd stopped,
# guarded and BPDU-guarded ports restored, secured ports back to their CONFIG_DB learn mode.
# A stop of the service does NOT tear down (mstpd
# keeps running and the next start adopts it), so a restart never opens a
# window with no spanning tree.
import collections
import ctypes
import hashlib
import hmac
import json
import math
import os
import select
import signal
import socket
import struct
import subprocess
import sys
import syslog
import time

from swsscommon import swsscommon as sw

BRIDGE = "Bridge"
CONF = "/etc/sonic/disunified/l2.json"
MSTPD = "/usr/local/sbin/mstpd"
MSTPCTL = "/usr/local/sbin/mstpctl"
MSTPD_PID = "/run/mstpd.pid"  # mstpd writes it when it daemonizes
PROC = "/proc"
HOOK = "/usr/sbin/bridge-stp"
SYS = "/sys/class/net"
SYNC = 2.0  # seconds between full reconciliations
# BPDUs to the CPU while mstpd runs: copporch maps trap id "stp" to
# SAI_HOSTIF_TRAP_TYPE_STP; the image's copp_cfg.json has no stp entry, and
# always_enabled installs it without FEATURE|stp. Removed again with STP
# off, so the switch floods BPDUs and stays transparent to its neighbours'
# spanning tree.
COPP_STP = [("trap_ids", "stp"), ("trap_group", "queue4_group1"), ("always_enabled", "true")]
# The trap's group. The image defines it in /etc/sonic/copp_cfg.json and
# coppmgr merges that file with CONFIG_DB (an identical row is a duplicate
# it ignores), so the switch runs the same either way; CONFIG_DB needs its
# own copy because sonic-yang resolves the trap's trap_group leafref against
# CONFIG_DB alone, and `config interface breakout` validates the whole
# CONFIG_DB (docs/drivers/sonic.md 11.8: the trap on its own failed every
# breakout). Written before the trap and left in place -- a group without
# a trap is valid.
COPP_GROUP_NAME = "queue4_group1"
COPP_GROUP_DEFAULT = [("cbs", "6000"), ("cir", "6000"), ("meter_type", "packets"), ("mode", "sr_tcm"),
                      ("queue", "4"), ("red_action", "drop"), ("trap_action", "trap"), ("trap_priority", "4")]
COPP_CFG = "/etc/sonic/copp_cfg.json"


def copp_group():
    """The group as the image defines it, else as 202505 and 202511 ship it."""
    try:
        with open(COPP_CFG) as f:
            return sorted(json.load(f)["COPP_GROUP"][COPP_GROUP_NAME].items())
    except (IOError, OSError, KeyError, ValueError, AttributeError):
        return COPP_GROUP_DEFAULT

# A bridge port's settings: mstpctl's JSON field and the command that sets it.
PORT_SETTINGS = (
    ("bpdu-filter-port", "setportbpdufilter"),
    ("admin-edge-port", "setportadminedge"),
    ("auto-edge-port", "setportautoedge"),
    ("bpdu-guard-port", "setbpduguard"),
)

# kernel BR_STATE_* -> StpOrch stp_state (stporch.h)
KSTATE = {0: 0, 1: 2, 2: 3, 3: 4, 4: 1}
KNAME = {0: "disabled", 1: "listening", 2: "learning", 3: "forwarding", 4: "blocking"}

AF_BRIDGE = 7
RTMGRP_LINK = 1
RTM_NEWLINK, RTM_DELLINK = 16, 17
IFLA_IFNAME, IFLA_MASTER, IFLA_PROTINFO = 3, 10, 12
IFLA_BRPORT_STATE = 1


def put(table, key, pairs):
    table.set(key, sw.FieldValuePairs(list(pairs)))


def log(msg, prio=syslog.LOG_INFO):
    syslog.syslog(prio, msg)


def run(args, check=False, stdin=None):
    p = subprocess.run(args, input=stdin, stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True)
    if check and p.returncode != 0:
        log("%s: exit %d: %s" % (" ".join(args), p.returncode, p.stderr.strip()), syslog.LOG_WARNING)
    return p


def rd(path, default=""):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return default


def wr(path, value):
    with open(path, "w") as f:
        f.write(value)


def load_conf():
    try:
        with open(CONF) as f:
            c = json.load(f)
    except (OSError, ValueError):
        c = {}
    stp = c.get("stp") or {}
    guard = c.get("loop_guard") or {}
    lock = c.get("mac_lock") or {}
    return {
        "stp": {
            "enabled": bool(stp.get("enabled")),
            "version": stp.get("version") or "rstp",
            "priority": int(stp.get("priority", 32768)),
            "ports": stp.get("ports") or {},
        },
        "loop_guard": {
            "enabled": bool(guard.get("enabled")),
            "threshold": int(guard.get("threshold", 10)),
            "interval": float(guard.get("interval", 5)),
            "hold": float(guard.get("hold", 150)),
            "ports": set(guard.get("ports") or []),
        },
        "dhcp_rate": DhcpRate.load(c.get("dhcp_rate")),
        "mac_lock": {
            "ports": {p: frozenset(m.lower() for m in (macs or [])) for p, macs in (lock.get("ports") or {}).items()},
        },
        "dot1x": c.get("dot1x") or {},
    }


class DB(object):
    def __init__(self):
        self.appl = sw.DBConnector("APPL_DB", 0)
        self.cfg = sw.DBConnector("CONFIG_DB", 0)
        self.state = sw.DBConnector("STATE_DB", 0)
        self.asic = sw.DBConnector("ASIC_DB", 0)
        self.counters = sw.DBConnector("COUNTERS_DB", 0)
        self.port_pst = sw.ProducerStateTable(self.appl, "STP_PORT_STATE_TABLE")
        self.vlan_pst = sw.ProducerStateTable(self.appl, "STP_VLAN_INSTANCE_TABLE")
        self.flush_pst = sw.ProducerStateTable(self.appl, "STP_FASTAGEING_FLUSH_TABLE")
        self.ifport_pst = sw.ProducerStateTable(self.appl, "PORT_TABLE")
        self.port_raw = sw.Table(self.appl, "STP_PORT_STATE_TABLE")
        self.vlan_raw = sw.Table(self.appl, "STP_VLAN_INSTANCE_TABLE")
        self.flush_raw = sw.Table(self.appl, "STP_FASTAGEING_FLUSH_TABLE")
        self.st_bridge = sw.Table(self.state, "DUI_STP_TABLE")
        self.st_port = sw.Table(self.state, "DUI_STP_PORT_TABLE")
        self.st_guard = sw.Table(self.state, "DUI_LOOP_GUARD_TABLE")
        self.st_bpdu = sw.Table(self.state, "DUI_BPDU_GUARD_TABLE")
        self.st_recover = sw.Table(self.state, "DUI_PORT_RECOVERY")
        self.copp = sw.Table(self.cfg, "COPP_TRAP")
        self.copp_group = sw.Table(self.cfg, "COPP_GROUP")

    @staticmethod
    def keys(conn, pattern):
        return list(conn.keys(pattern) or [])

    @staticmethod
    def hgetall(conn, key):
        return dict(conn.hgetall(key) or {})

    def vlans(self):
        """VLAN ids orchagent has created (CONFIG_DB and the ASIC agree)."""
        cfg = set()
        for k in self.keys(self.cfg, "VLAN|Vlan*"):
            try:
                cfg.add(int(k.split("|Vlan", 1)[1]))
            except ValueError:
                pass
        asic = set()
        for k in self.keys(self.asic, "ASIC_STATE:SAI_OBJECT_TYPE_VLAN:*"):
            v = self.hgetall(self.asic, k).get("SAI_VLAN_ATTR_VLAN_ID")
            if v:
                asic.add(int(v))
        return cfg & asic

    def known(self, name):
        """True if orchagent knows the port or PortChannel and it is ready."""
        if name.startswith("PortChannel"):
            return bool(self.hgetall(self.appl, "LAG_TABLE:" + name)) and bool(self.hgetall(self.state, "LAG_TABLE|" + name))
        if name.startswith("Ethernet"):
            return bool(self.hgetall(self.appl, "PORT_TABLE:" + name)) and \
                self.hgetall(self.state, "PORT_TABLE|" + name).get("state") == "ok"
        return False

    def stp_ports_in_asic(self):
        return len(self.keys(self.asic, "ASIC_STATE:SAI_OBJECT_TYPE_STP_PORT:*"))

    def lag_members(self, lag):
        out = []
        for k in self.keys(self.cfg, "PORTCHANNEL_MEMBER|%s|*" % lag):
            out.append(k.split("|", 2)[2])
        return out

    def lag_of(self, port):
        for k in self.keys(self.cfg, "PORTCHANNEL_MEMBER|*|%s" % port):
            return k.split("|", 2)[1]
        return ""

    def asic_stp_ports(self):
        """Interface name -> the vids (None: an instance no VLAN is in) of
        the chip STP ports ASIC_DB holds for its bridge port."""
        names = {}
        for m in ("COUNTERS_PORT_NAME_MAP", "COUNTERS_LAG_NAME_MAP"):
            for name, oid in self.hgetall(self.counters, m).items():
                names[oid] = name
        owner = {}
        for k in self.keys(self.asic, "ASIC_STATE:SAI_OBJECT_TYPE_BRIDGE_PORT:*"):
            h = self.hgetall(self.asic, k)
            if h.get("SAI_BRIDGE_PORT_ATTR_TYPE") == "SAI_BRIDGE_PORT_TYPE_PORT":
                owner[k.split(":", 2)[2]] = names.get(h.get("SAI_BRIDGE_PORT_ATTR_PORT_ID"), "")
        vid_of = {}
        for k in self.keys(self.asic, "ASIC_STATE:SAI_OBJECT_TYPE_VLAN:*"):
            h = self.hgetall(self.asic, k)
            if h.get("SAI_VLAN_ATTR_STP_INSTANCE") and h.get("SAI_VLAN_ATTR_VLAN_ID"):
                vid_of[h["SAI_VLAN_ATTR_STP_INSTANCE"]] = int(h["SAI_VLAN_ATTR_VLAN_ID"])
        out = collections.defaultdict(list)
        for k in self.keys(self.asic, "ASIC_STATE:SAI_OBJECT_TYPE_STP_PORT:*"):
            h = self.hgetall(self.asic, k)
            name = owner.get(h.get("SAI_STP_PORT_ATTR_BRIDGE_PORT"), "")
            if name:
                out[name].append(vid_of.get(h.get("SAI_STP_PORT_ATTR_STP")))
        return out


def bridge_ports():
    try:
        return sorted(os.listdir("%s/%s/brif" % (SYS, BRIDGE)))
    except OSError:
        return []


def in_bridge(port):
    """True while the port is a member of Bridge. Leaving it, the kernel
    unlinks brif/<port> before it announces the port disabled (del_nbp:
    sysfs_remove_link, then br_stp_disable_port's RTM_NEWLINK), so a state
    notification for a port no longer listed is its departure, not a link
    drop (docs/drivers/sonic.md 11.34)."""
    return os.path.exists("%s/%s/brif/%s" % (SYS, BRIDGE, port))


def kstate(port):
    """The port's kernel STP state; None once it is no bridge port."""
    try:
        return int(rd("%s/%s/brport/state" % (SYS, port), ""))
    except ValueError:
        return None


def nl_open():
    s = socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, socket.NETLINK_ROUTE)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4 << 20)
    s.bind((0, RTMGRP_LINK))
    s.setblocking(False)
    return s


def nl_attrs(buf, off, end):
    while off + 4 <= end:
        ln, ty = struct.unpack_from("HH", buf, off)
        if ln < 4:
            return
        yield ty & 0x3FFF, buf[off + 4:off + ln]
        off += (ln + 3) & ~3


def nl_events(data):
    """Bridge-port notifications in one netlink datagram: (msgtype, name, state)."""
    off = 0
    while off + 16 <= len(data):
        ln, ty = struct.unpack_from("IH", data, off)
        if ln < 16:
            return
        if ty in (RTM_NEWLINK, RTM_DELLINK) and ln >= 32:
            fam = data[off + 16]
            if fam == AF_BRIDGE:
                name, master, state = None, None, None
                for at, val in nl_attrs(data, off + 32, off + ln):
                    if at == IFLA_IFNAME:
                        name = val.rstrip(b"\0").decode()
                    elif at == IFLA_MASTER and len(val) >= 4:
                        master = struct.unpack("I", val[:4])[0]
                    elif at == IFLA_PROTINFO:
                        for bt, bv in nl_attrs(val, 0, len(val)):
                            if bt == IFLA_BRPORT_STATE and bv:
                                state = bv[0]
                yield ty, name, master, state
        off += (ln + 3) & ~3


class Mstp(object):
    """mstpd and mstpctl."""

    def running(self):
        """Whether mstpd runs: its pid file, else a scan of /proc, read here.
        Not pidof: sysvinit's pidof logs a process that exits between its
        /proc listing and its read of <pid>/stat ("can't read from
        <pid>/stat") straight to syslog at ERR, past any stderr redirect --
        three times in 36 h on the DX010, every one a false alarm."""
        if rd("%s/%s/comm" % (PROC, rd(MSTPD_PID, "-"))) == "mstpd":
            return True
        try:
            pids = [p for p in os.listdir(PROC) if p.isdigit()]
        except OSError:
            return False
        return any(rd("%s/%s/comm" % (PROC, p)) == "mstpd" for p in pids)  # a gone pid reads ""

    def start(self):
        if self.running():
            return True
        if not os.access(MSTPD, os.X_OK):
            log("mstpd is not installed at %s" % MSTPD, syslog.LOG_ERR)
            return False
        run([MSTPD, "-v", "2"], check=True)  # daemonizes, logs to syslog
        for _ in range(20):
            if self.running():
                return True
            time.sleep(0.1)
        return False

    def stop(self):
        run(["pkill", "-x", "mstpd"])

    def ctl(self, *args):
        return run([MSTPCTL] + list(args), check=True).returncode == 0

    def json(self, *args):
        p = run([MSTPCTL, "-f", "json"] + list(args))
        if p.returncode != 0:
            return None
        try:
            return json.loads(p.stdout)
        except ValueError:
            return None

    def bridge(self):
        b = self.json("showbridge", BRIDGE)
        if isinstance(b, list):
            b = b[0] if b else None
        return b

    def ports(self):
        p = self.json("showportdetail", BRIDGE) or []
        return [p] if isinstance(p, dict) else p

    def has_bridge(self):
        return run([MSTPCTL, "showbridge", BRIDGE]).returncode == 0


class DhcpRate(object):
    """UniFi's "Incoming DHCP packets exceed 300/sec" (the port's DHCP rate
    anomaly), detected and counted, never acted on: in the UI's model the
    port keeps running (docs/drivers/sonic.md 11.17).

    What is counted: DHCPv4 requests a client sends straight to the network
    (BOOTREQUEST, giaddr 0), per ingress front-panel port, per second, as
    they reach the CPU. A server's or a relay's replies are BOOTREPLY and a
    relay's forwarded requests carry its giaddr, so neither makes a server's
    or a relay's port look like a flood. A second in which a port's count
    exceeds the threshold adds the excess to that port's cumulative `over`
    in STATE_DB DUI_DHCP_RATE_TABLE|<port>, which the bridge reads as the
    port's DHCP-rate count.

    The CPU sees DHCP only through CoPP: on the image it shares
    queue4_group3 with LLDP and UDLD at 100 packets/s for the whole switch,
    so no port could ever be seen above 300/s. While the detector runs, the
    image's dhcp_relay trap moves to a group of its own, as Ubiquiti's ECS
    keeps DHCP (queue 3, alone), at a rate a single port's 300/s fits in;
    the group and the trap row go again when it stops, and coppmgr puts the
    trap back in the image's group."""

    TABLE = "DUI_DHCP_RATE_TABLE"
    TRAP = "dhcp_relay"  # copp_cfg.json's trap key, gated by FEATURE|dhcp_relay
    GROUP = "queue3_group1"
    QUIET = 10  # seconds under the threshold that end an episode (for the log)

    # A classic BPF program: IPv4 UDP to port 67, untagged or 802.1Q, not a
    # fragment; the first 256 bytes (the BOOTP header) are kept.
    FILTER = [
        (0x28, 0, 0, 12),       # 0  ldh [12]          ethertype
        (0x15, 0, 7, 0x0800),   # 1  jeq IPv4 ? 2 : 9
        (0x30, 0, 0, 23),       # 2  ldb [23]          protocol
        (0x15, 0, 16, 17),      # 3  jeq UDP ? 4 : drop
        (0x28, 0, 0, 20),       # 4  ldh [20]          flags + fragment
        (0x45, 14, 0, 0x1FFF),  # 5  jset offset ? drop : 6
        (0xB1, 0, 0, 14),       # 6  ldxb 4*([14]&0xf) header length
        (0x48, 0, 0, 16),       # 7  ldh [x+16]        UDP destination
        (0x05, 0, 0, 9),        # 8  ja 18
        (0x15, 0, 10, 0x8100),  # 9  jeq 802.1Q ? 10 : drop
        (0x28, 0, 0, 16),       # 10 ldh [16]          inner ethertype
        (0x15, 0, 8, 0x0800),   # 11 jeq IPv4 ? 12 : drop
        (0x30, 0, 0, 27),       # 12 ldb [27]
        (0x15, 0, 6, 17),       # 13 jeq UDP ? 14 : drop
        (0x28, 0, 0, 24),       # 14 ldh [24]
        (0x45, 4, 0, 0x1FFF),   # 15 jset offset ? drop : 16
        (0xB1, 0, 0, 18),       # 16 ldxb 4*([18]&0xf)
        (0x48, 0, 0, 20),       # 17 ldh [x+20]
        (0x15, 0, 1, 67),       # 18 jeq 67 ? 19 : drop
        (0x06, 0, 0, 256),      # 19 accept 256 bytes
        (0x06, 0, 0, 0),        # 20 drop
    ]

    @staticmethod
    def load(c):
        c = c or {}
        return {
            "enabled": bool(c.get("enabled")),
            "threshold": int(c.get("threshold", 300)),
            "rate": int(c.get("rate", 600)),
        }

    def __init__(self, db):
        self.db = db
        self.table = sw.Table(db.state, self.TABLE)
        self.conf = self.load(None)
        self.sock = None
        self.filter_buf = None
        self.sec = 0
        self.counts = collections.Counter()  # port -> requests this second
        self.stats = {}  # port -> {"over", "peak", "last"}
        self.episode = {}  # port -> last second over the threshold
        self.group_delete_at = 0.0
        self.untrapped_logged = False

    @staticmethod
    def classify(frame):
        """True for a client's own DHCP request: BOOTREQUEST, giaddr 0."""
        off = 18 if frame[12:14] == b"\x81\x00" else 14
        if len(frame) < off + 20:
            return False
        p = off + (frame[off] & 0x0F) * 4 + 8  # the BOOTP header
        return len(frame) >= p + 28 and frame[p] == 1 and frame[p + 24:p + 28] == b"\0\0\0\0"

    def configure(self, conf):
        """Called on every sync: start, stop, keep CoPP as the config says."""
        was = self.conf["enabled"]
        self.conf = conf
        if conf["enabled"]:
            self.copp_install()
            if self.sock is None:
                self.open()
        elif was or self.sock is not None or \
                self.db.hgetall(self.db.cfg, "COPP_TRAP|" + self.TRAP).get("trap_group") == self.GROUP:
            self.stop("disabled")  # also a trap row left behind by an l2d that died
        if self.group_delete_at and time.time() >= self.group_delete_at:
            self.db.copp_group._del(self.GROUP)
            self.group_delete_at = 0.0

    def copp_install(self):
        group = [("cbs", str(self.conf["rate"])), ("cir", str(self.conf["rate"])), ("meter_type", "packets"),
                 ("mode", "sr_tcm"), ("queue", "3"), ("red_action", "drop"), ("trap_action", "trap"),
                 ("trap_priority", "3")]
        if self.db.hgetall(self.db.cfg, "COPP_GROUP|" + self.GROUP) != dict(group):
            put(self.db.copp_group, self.GROUP, group)  # before the trap: its leafref
        try:
            with open(COPP_CFG) as f:
                ids = json.load(f)["COPP_TRAP"][self.TRAP]["trap_ids"]
        except (IOError, OSError, KeyError, ValueError, TypeError):
            ids = "dhcp,dhcpv6"
        trap = [("trap_ids", ids), ("trap_group", self.GROUP)]
        if self.db.hgetall(self.db.cfg, "COPP_TRAP|" + self.TRAP) != dict(trap):
            put(self.db.copp, self.TRAP, trap)
            log("dhcp rate: %s trap in %s (%d packets/s)" % (self.TRAP, self.GROUP, self.conf["rate"]))
        self.group_delete_at = 0.0
        if self.db.hgetall(self.db.cfg, "FEATURE|dhcp_relay").get("state") != "enabled" and not self.untrapped_logged:
            self.untrapped_logged = True
            log("dhcp rate: FEATURE|dhcp_relay is not enabled, so DHCP is not trapped to the CPU; nothing to count",
                syslog.LOG_WARNING)

    def copp_remove(self, now=False):
        if self.db.hgetall(self.db.cfg, "COPP_TRAP|" + self.TRAP).get("trap_group") == self.GROUP:
            self.db.copp._del(self.TRAP)  # coppmgr restores the image's own row
            log("dhcp rate: %s trap back in the image's group" % self.TRAP)
        if now:
            time.sleep(1)
            self.db.copp_group._del(self.GROUP)
        else:
            self.group_delete_at = time.time() + 2  # after coppmgr moved the trap

    def open(self):
        s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0003))
        prog = b"".join(struct.pack("HBBI", *ins) for ins in self.FILTER)
        self.filter_buf = ctypes.create_string_buffer(prog)
        fprog = struct.pack("HL", len(self.FILTER), ctypes.addressof(self.filter_buf))
        s.setsockopt(socket.SOL_SOCKET, 26, fprog)  # SO_ATTACH_FILTER
        s.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 1 << 20)
        s.setblocking(False)
        try:  # what arrived before the filter was attached
            while True:
                s.recv(1)
        except BlockingIOError:
            pass
        self.sock = s
        self.adopt()
        log("dhcp rate: counting DHCP requests per port (threshold %d/s)" % self.conf["threshold"])

    def adopt(self):
        """Counts survive an l2d restart (STATE_DB keeps them until a reboot)."""
        for k in self.db.keys(self.db.state, self.TABLE + "|*"):
            f = self.db.hgetall(self.db.state, k)
            try:
                self.stats[k.split("|", 1)[1]] = {"over": int(f.get("over", 0)), "peak": int(f.get("peak", 0)),
                                                  "last": int(f.get("last", 0))}
            except ValueError:
                pass

    def stop(self, why):
        if self.sock is not None:
            self.sock.close()
            self.sock = None
        self.counts.clear()
        self.copp_remove(now=(why == "teardown"))
        log("dhcp rate: stopped (%s)" % why)

    def fileno(self):
        return self.sock.fileno() if self.sock is not None else None

    def readable(self):
        now = int(time.time())
        for _ in range(4096):
            try:
                frame, addr = self.sock.recvfrom(256)
            except BlockingIOError:
                break
            except OSError:
                break
            ifname, pkttype = addr[0], addr[2]
            if pkttype == socket.PACKET_OUTGOING or not ifname.startswith("Ethernet"):
                continue
            if self.classify(frame):
                self.note(ifname, now)

    def note(self, port, sec):
        if sec != self.sec:
            self.close_second()
            self.sec = sec
        self.counts[port] += 1

    def tick(self):
        if self.sock is not None and int(time.time()) != self.sec:
            self.close_second()
            self.sec = int(time.time())

    def close_second(self):
        limit = self.conf["threshold"]
        for port, n in self.counts.items():
            if n <= limit:
                continue
            st = self.stats.setdefault(port, {"over": 0, "peak": 0, "last": 0})
            st["over"] += n - limit
            st["peak"] = max(st["peak"], n)
            st["last"] = self.sec
            put(self.table, port, [(k, str(v)) for k, v in sorted(st.items())])
            if self.sec - self.episode.get(port, 0) > self.QUIET:
                log("dhcp rate: %s received %d DHCP requests in one second (threshold %d)" % (port, n, limit),
                    syslog.LOG_WARNING)
            self.episode[port] = self.sec
        self.counts.clear()


class L2d(object):
    def __init__(self):
        self.db = DB()
        self.mstp = Mstp()
        self.conf = load_conf()
        self.reload = False
        self.resync = False
        self.settle = 0.0  # no VLAN is mapped before this time (mstpd converging)
        self.asic_floor = 0  # STP ports ASIC_DB was seen to hold once a VLAN was mapped
        self.stp_on = False  # mstpd owns Bridge
        self.ifindex = 0
        self.ports = {}  # bridge port known to orchagent -> last kernel state seen
        # bridge port -> changes of its kernel state (disabled, blocking,
        # learning, forwarding) seen since boot: published as state_changes,
        # the controller's stp_state_change_count (§11.23). mstpd's own
        # transition counters miss a port that stays discarding across a
        # link drop (Backup, Alternate) and count one at start for a port
        # that never came up.
        self.changes = {}
        self.written = {}  # (port, vid) -> stporch state in APPL_DB
        self.strays = {}  # interface outside Bridge with chip STP ports -> first seen
        self.stray_scan = 0.0  # the next look at ASIC_DB for them without a cue
        self.mapped = set()  # vids whose VLAN is in its own instance
        self.flush_due = {}  # port -> time a flush is due (coalesced)
        self.root_port = ""
        self.gw_port = ""
        self.fdb_port = {}  # (vlan, mac) -> port
        self.moves = collections.defaultdict(collections.deque)
        self.blocked = {}  # port -> unblock time
        self.bpdu_held = {}  # bridge port BPDU guard holds down -> since (UTC)
        self.bpdu_stale = {}  # released port -> its state_changes at the release
        self.fdb_sub = None
        self.nl = None
        self.storm = StormWatch(self.db)  # storm detection (11.21): self.storm.tick() in main
        self.dhcp = DhcpRate(self.db)  # the DHCP rate detector (its own class)
        # Detectors in this list (beside the two above) are run by sync(),
        # main() and teardown() through one interface: sync(conf) every pass,
        # fds() and readable(fd) for the main loop's select, teardown().
        self.maclock = MacLockGuard(self)
        self.ucast = UcastSampler(self)  # unknown-unicast storms' samples (11.24)
        self.dot1x = Dot1xGuard(self)  # 802.1X port authentication (11.26)
        self.detectors = [self.maclock, self.ucast, self.dot1x]

    # --- spanning tree -------------------------------------------------

    def stp_up(self):
        if not os.access(HOOK, os.X_OK):
            log("%s is missing: not enabling STP (the kernel would run its own)" % HOOK, syslog.LOG_ERR)
            return False
        self.copp_install()
        if not self.mstp.start():
            return False
        brdir = "%s/%s/bridge" % (SYS, BRIDGE)
        if rd(brdir + "/stp_state") == "0":
            wr(brdir + "/stp_state", "1")
        st = rd(brdir + "/stp_state")
        if st != "2":
            log("Bridge stp_state is %s, not 2 (user space): turning it off" % st, syslog.LOG_ERR)
            wr(brdir + "/stp_state", "0")
            return False
        if not self.mstp.has_bridge():
            self.mstp.ctl("addbridge", BRIDGE)
        for _ in range(50):
            if self.mstp.has_bridge():
                break
            time.sleep(0.1)
        else:
            log("mstpd did not take Bridge", syslog.LOG_ERR)
            return False
        self.ifindex = int(rd("%s/%s/ifindex" % (SYS, BRIDGE), "0"))
        self.reconcile(self.mstp.bridge() or {}, self.mstp.ports())
        # Every port starts discarding; the root port forwards once the root's
        # next BPDU arrives (hello 2 s). Mapping a VLAN before that would block
        # the uplink in the ASIC for those seconds.
        self.settle = time.time() + 6
        log("spanning tree on: %s, priority %d" % (self.conf["stp"]["version"], self.conf["stp"]["priority"]))
        return True

    def want_bridge(self):
        """The force version and the priority nibble the config asks for."""
        s = self.conf["stp"]
        version = s["version"] if s["version"] in ("stp", "rstp", "mstp") else "rstp"
        return version, max(0, min(15, s["priority"] // 4096))

    def want_port(self, port):
        """A bridge port's yes/no settings, keyed as mstpctl's JSON reports them."""
        if not (port.startswith("Ethernet") or port.startswith("PortChannel")):
            # vlanmgrd's "dummy" keeps Bridge up; it carries nothing.
            return {"bpdu-filter-port": True, "admin-edge-port": True, "auto-edge-port": True, "bpdu-guard-port": False}
        pc = self.conf["stp"]["ports"].get(port) or {}
        enabled = pc.get("enabled", True)
        edge = pc.get("edge", "auto")
        return {
            "bpdu-filter-port": not enabled,
            "admin-edge-port": not enabled or edge == "enabled",
            "auto-edge-port": edge != "disabled",
            "bpdu-guard-port": bool(enabled and pc.get("bpdu_guard", False)),
        }

    def reconcile(self, bridge, ports):
        """Sets in mstpd whatever differs from the config, judged by mstpd's own
        report (showbridge, showportdetail: every bridge port, a down one too),
        so a setting reaches mstpd however the config changed -- one write or
        two in a row -- and whatever mstpd went through. A refused mstpctl call
        is retried on the next pass. Returns True if anything was set."""
        changes = []
        version, nibble = self.want_bridge()
        have = bridge.get("force-protocol-version", "")
        if have != version and self.mstp.ctl("setforcevers", BRIDGE, version):
            changes.append("version %s -> %s" % (have or "?", version))
        try:
            have_nibble = int(bridge.get("bridge-id", "").split(".")[0], 16)
        except ValueError:
            have_nibble = -1
        if have_nibble != nibble and self.mstp.ctl("settreeprio", BRIDGE, "0", str(nibble)):
            changes.append("priority %s -> %d" % (have_nibble * 4096 if have_nibble >= 0 else "?", nibble * 4096))
        for d in ports:
            port = d.get("port", "")
            if not port:
                continue
            want = self.want_port(port)
            for field, cmd in PORT_SETTINGS:
                on = want[field]
                if (d.get(field) == "yes") != on and self.mstp.ctl(cmd, BRIDGE, port, "yes" if on else "no"):
                    changes.append("%s %s %s" % (port, field[:-5].replace("-", " "), "on" if on else "off"))
        if changes:
            log("settings: " + "; ".join(changes))
        return bool(changes)

    def write_state(self, port, state, vids):
        s = str(KSTATE.get(state, 1))
        changed = False
        for vid in vids:
            if self.written.get((port, vid)) != s:
                put(self.db.port_pst, "%s:%d" % (port, vid), [("state", s)])
                self.written[(port, vid)] = s
                changed = True
        return changed

    def map_vlan(self, vid):
        """Map a VLAN into its instance once every bridge port has a state there."""
        if any((p, vid) not in self.written for p in self.ports):
            return
        expected = len(self.written)
        deadline = time.time() + 10
        while self.db.stp_ports_in_asic() < expected:
            if time.time() > deadline:
                log("ASIC holds %d STP ports, expected %d: Vlan%d not mapped yet" %
                    (self.db.stp_ports_in_asic(), expected, vid), syslog.LOG_WARNING)
                return
            time.sleep(0.05)
        put(self.db.vlan_pst, "Vlan%d" % vid, [("stp_instance", str(vid))])
        self.mapped.add(vid)
        self.asic_floor = expected
        log("Vlan%d in STP instance %d (%d ports)" % (vid, vid, len(self.ports)))

    def sync_stp(self):
        vids = self.db.vlans()
        members = set(bridge_ports())
        # An ASIC holding fewer STP ports than it was seen to hold means
        # orchagent restarted (or a VLAN went): rebuild from scratch. Fresh
        # writes it has not processed yet are no sign of that.
        if self.mapped and self.db.stp_ports_in_asic() < self.asic_floor and not self.pending_vlan_delete(vids):
            log("ASIC lost STP ports (orchagent restart?): rewriting every state", syslog.LOG_WARNING)
            self.written.clear()
            self.mapped.clear()
            self.asic_floor = 0
        for vid in list(self.mapped | {v for (_, v) in self.written}):
            if vid not in vids:
                self.forget_vlan(vid)
        for p in list(self.ports):
            if p not in members or not self.db.known(p):
                self.forget_port(p)
        for p in sorted(members):
            if p in self.ports or not self.db.known(p):
                continue
            self.ports[p] = None
        self.repair_strays(members, vids)
        # A PortChannel one of whose members holds chip STP ports waits for
        # the repair: the chip would refuse its STP ports until then.
        waiting = {self.db.lag_of(p) for p in self.strays if p.startswith("Ethernet")}
        for p in list(self.ports):
            st = kstate(p)
            if st is None:  # left Bridge since bridge_ports() was read
                self.forget_port(p)
                continue
            self.count_change(p, self.ports[p], st)
            self.ports[p] = st
            if p not in waiting:
                self.write_state(p, st, vids)
        if time.time() >= self.settle:
            for vid in sorted(vids - self.mapped):
                self.map_vlan(vid)
        for vid in self.mapped:
            if not self.db.hgetall(self.db.appl, "STP_VLAN_INSTANCE_TABLE:Vlan%d" % vid):
                put(self.db.vlan_pst, "Vlan%d" % vid, [("stp_instance", str(vid))])

    def pending_vlan_delete(self, vids):
        return any(v not in vids for v in self.mapped)

    def repair_strays(self, members, vids):
        """Chip STP ports of an interface that is no bridge port (11.34).
        StpOrch makes them when a state for a port reaches it after portsorch
        removed the port's bridge port: addStpPort adds a bridge port and an
        STP port in the instance. A PortChannel member that has them keeps
        its PortChannel from getting STP ports of its own ("Lag member N
        exists as STP PORT ... in STG n"), so the PortChannel's VLANs are
        not forwarded on that member in the chip, whatever mstpd says.

        Found from ASIC_DB (looked at whenever APPL_DB holds a state key for
        an interface outside Bridge -- the late write leaves one -- and once
        a minute), removed through StpOrch once seen on two passes (a DEL is
        a no-op for an instance it holds no STP port in), and the
        PortChannel's states written again. The bridge port StpOrch made
        stays: no orchagent path removes it but a reboot. State keys of an
        interface outside Bridge without chip STP ports are only dropped
        from APPL_DB."""
        now = time.time()
        rows = collections.defaultdict(set)
        for k in self.db.keys(self.db.appl, "STP_PORT_STATE_TABLE:*"):
            parts = k.split(":")
            if len(parts) == 3 and parts[1] not in members:
                try:
                    rows[parts[1]].add(int(parts[2]))
                except ValueError:
                    pass
        if not rows and not self.strays and now < self.stray_scan:
            return
        self.stray_scan = now + 60
        chip = self.db.asic_stp_ports()
        strays = {p for p in chip if p not in members and p.startswith(("Ethernet", "PortChannel"))}
        for p in list(self.strays):
            if p not in strays:
                del self.strays[p]
        for p in sorted(set(rows) - strays):
            if p not in self.ports:  # one leaving this pass is forget_port's
                for v in sorted(rows[p]):
                    self.db.port_raw._del("%s:%d" % (p, v))
        for p in sorted(strays):
            first = self.strays.setdefault(p, now)
            if now - first < SYNC * 0.75 or not self.db.known(p):
                continue
            held = len(chip[p])
            before = self.db.stp_ports_in_asic()
            for v in sorted(rows.get(p, set()) | set(vids) | {v for v in chip[p] if v is not None}):
                self.db.port_pst._del("%s:%d" % (p, v))
            deadline = time.time() + 2
            while self.db.stp_ports_in_asic() > before - held and time.time() < deadline:
                time.sleep(0.05)
            left = len(self.db.asic_stp_ports().get(p, []))
            self.asic_floor = max(0, self.asic_floor - (held - left))
            lag = self.db.lag_of(p) if p.startswith("Ethernet") else ""
            if lag:
                for key in [k for k in self.written if k[0] == lag]:
                    del self.written[key]
            if left:
                self.strays[p] = time.time()  # try again in two passes
                log("%s is no bridge port and still holds %d of its %d STP ports in the chip" % (p, left, held),
                    syslog.LOG_WARNING)
            else:
                del self.strays[p]
                log("%s is no bridge port%s but held %d STP ports in the chip: removed%s; the bridge port "
                    "orchagent made for it stays until a reboot" %
                    (p, " (a member of %s)" % lag if lag else "", held,
                     ", %s's states written again" % lag if lag else ""), syslog.LOG_WARNING)

    def forget_vlan(self, vid):
        # orchagent removed the instance with the VLAN; drop our keys without
        # telling it (a producer DEL for a missing VLAN would never complete).
        for (p, v) in list(self.written):
            if v == vid:
                del self.written[(p, v)]
                self.db.port_raw._del("%s:%d" % (p, v))
        self.db.vlan_raw._del("Vlan%d" % vid)
        self.db.flush_raw._del("Vlan%d" % vid)
        self.mapped.discard(vid)
        self.asic_floor = min(self.asic_floor, len(self.written))
        log("Vlan%d is gone: its STP instance keys dropped" % vid)

    def forget_port(self, port):
        # portsorch removed the port's STP ports with its bridge port.
        for (p, v) in list(self.written):
            if p == port:
                del self.written[(p, v)]
                self.db.port_raw._del("%s:%d" % (p, v))
        self.ports.pop(port, None)
        self.bpdu_release(port, "no longer a bridge port")
        self.bpdu_forget(port)
        self.changes.pop(port, None)
        self.db.st_port._del(port)
        self.asic_floor = min(self.asic_floor, len(self.written))

    def on_link(self, ty, name, master, state):
        if not name or name == BRIDGE:
            return
        if ty == RTM_DELLINK or (master is not None and master != self.ifindex):
            if name in self.ports:
                self.forget_port(name)
            return
        if name not in self.ports:
            self.resync = True  # a port joined Bridge: give it states now, not in 2 s
            return
        if state is None:
            return
        if not in_bridge(name):
            # The port is leaving Bridge (it left its last VLAN: it joins a
            # PortChannel or becomes a routed port). Its "disabled" must not
            # reach StpOrch: processed after portsorch removed the bridge
            # port, StpOrch's addStpPort makes a new one for it, with an STP
            # port in every instance, and a PortChannel it joins then gets
            # none (the chip refuses a LAG's STP port while a member has
            # one: 11.34).
            self.forget_port(name)
            return
        vids = self.mapped | {v for (_, v) in self.written}
        if state != self.ports[name]:
            log("%s: %s -> %s" % (name, KNAME.get(self.ports[name], "?"), KNAME.get(state, "?")))
            self.count_change(name, self.ports[name], state)
            self.ports[name] = state
            self.write_state(name, state, vids)
        else:
            # same state: a bridge-port sysfs write, i.e. mstpd's FDB flush
            self.flush_due.setdefault(name, time.time() + 0.02)

    def count_change(self, port, old, new):
        """One more state change for the port; the first state seen is none.
        The count carries over an l2d restart from STATE_DB (a reboot or STP
        off clears it, as a switch's count since boot)."""
        if old is None or old == new:
            return
        self.changes[port] = self.state_changes(port) + 1

    def state_changes(self, port):
        if port not in self.changes:
            was = self.db.hgetall(self.db.state, "DUI_STP_PORT_TABLE|%s" % port).get("state_changes", "")
            self.changes[port] = int(was) if was.isdigit() else 0
        return self.changes[port]

    def run_flushes(self):
        now = time.time()
        due = [p for p, t in self.flush_due.items() if t <= now]
        if not due:
            return
        for p in due:
            del self.flush_due[p]
        if not self.mapped:
            return  # nothing in the ASIC follows mstpd yet
        for vid in sorted(self.mapped):
            put(self.db.flush_pst, "Vlan%d" % vid, [("state", "true")])
        log("topology change on %s: FDB flushed in %d VLANs" % (",".join(sorted(due)), len(self.mapped)))

    def copp_install(self):
        """The STP trap and, before it, the group its trap_group names."""
        put(self.db.copp_group, COPP_GROUP_NAME, copp_group())
        put(self.db.copp, "stp", COPP_STP)

    def stp_down(self, why):
        """Every VLAN back to the default instance; the kernel forwards on every port."""
        for k in self.db.keys(self.db.appl, "STP_VLAN_INSTANCE_TABLE:*"):
            self.db.vlan_pst._del(k.split(":", 1)[1])
        deadline = time.time() + 10
        while self.db.stp_ports_in_asic() and time.time() < deadline:
            time.sleep(0.1)
        for k in self.db.keys(self.db.appl, "STP_PORT_STATE_TABLE:*"):
            self.db.port_raw._del(k.split(":", 1)[1])
        for k in self.db.keys(self.db.appl, "STP_FASTAGEING_FLUSH_TABLE:*"):
            self.db.flush_raw._del(k.split(":", 1)[1])
        self.stp_on = False
        for p in list(self.bpdu_held):
            self.bpdu_release(p, "spanning tree off")
        for p in list(self.bpdu_stale):
            self.bpdu_forget(p)
        self.mstp.stop()
        for p in bridge_ports():
            run(["/usr/sbin/bridge", "link", "set", "dev", p, "state", "3"])
        if rd("%s/%s/bridge/stp_state" % (SYS, BRIDGE)) != "0":
            wr("%s/%s/bridge/stp_state" % (SYS, BRIDGE), "0")
        self.db.copp._del("stp")
        for k in self.db.keys(self.db.state, "DUI_STP_PORT_TABLE|*"):
            self.db.st_port._del(k.split("|", 1)[1])
        self.db.st_bridge._del(BRIDGE)  # a set merges: drop the running view first
        put(self.db.st_bridge, BRIDGE, [("enabled", "no"), ("reason", why)])
        self.written.clear()
        self.mapped.clear()
        self.ports.clear()
        self.changes.clear()
        self.asic_floor = 0
        self.stp_on = False
        log("spanning tree off: %s" % why)

    def publish(self):
        if not self.stp_on:
            return
        b = self.mstp.bridge()
        if not b:
            put(self.db.st_bridge, BRIDGE, [("enabled", "yes"), ("mstpd", "unreachable")])
            return
        ports = self.mstp.ports()
        if self.reconcile(b, ports):
            b, ports = self.mstp.bridge() or b, self.mstp.ports()
        rp = (b.get("root-port") or "").split(" ")[0]
        self.root_port = rp
        put(self.db.st_bridge, BRIDGE, [
            ("enabled", "yes"), ("mstpd", "running"),
            ("version", b.get("force-protocol-version", "")),
            ("bridge_id", b.get("bridge-id", "")),
            ("root_id", b.get("designated-root", "")),
            ("root_port", rp),
            ("root_path_cost", b.get("path-cost", "")),
            ("topology_change_count", b.get("topology-change-count", "")),
            ("time_since_topology_change", b.get("time-since-topology-change", "")),
            ("topology_change_port", b.get("last-topology-change-port", "")),
        ])
        for d in ports:
            p = d.get("port", "")
            if p not in self.ports:
                continue
            fields = [(f.replace("-", "_"), str(d.get(f, ""))) for f in (
                "role", "state", "external-port-cost", "admin-edge-port", "auto-edge-port",
                "oper-edge-port", "bpdu-guard-port", "bpdu-guard-error", "bpdu-filter-port",
                "num-rx-bpdu", "num-tx-bpdu", "num-rx-tcn", "num-transition-fwd",
                "num-transition-blk", "point-to-point")]
            fields.append(("state_changes", str(self.state_changes(p))))
            self.bpdu_check(p, d)
            fields.append(("bpdu_guard_held", "yes" if p in self.bpdu_held else "no"))
            put(self.db.st_port, p, fields)

    # --- loop protection ------------------------------------------------

    def gateway_port(self):
        p = run(["ip", "-4", "route", "show", "default"])
        gw = ""
        for line in p.stdout.splitlines():
            f = line.split()
            if "via" in f and "dev" in f and not f[f.index("dev") + 1].startswith("eth"):
                gw = f[f.index("via") + 1]
                break
        if not gw:
            return ""
        n = run(["ip", "-4", "neigh", "show", gw]).stdout.split()
        mac = n[n.index("lladdr") + 1].lower() if "lladdr" in n else ""
        for (vlan, m), port in self.fdb_port.items():
            if m == mac:
                return port
        return ""

    def on_fdb(self, key, op, fvs):
        try:
            vlan, mac = key.split(":", 1)
        except ValueError:
            return
        k = (vlan, mac.lower())
        if op != "SET":
            self.fdb_port.pop(k, None)
            return
        port = dict(fvs).get("port", "")
        old = self.fdb_port.get(k)
        self.fdb_port[k] = port
        if not old or not port or old == port:
            return
        g = self.conf["loop_guard"]
        now = time.time()
        q = self.moves[k]
        q.append(now)
        while q and q[0] < now - g["interval"]:
            q.popleft()
        if len(q) < g["threshold"] or not g["enabled"]:
            return
        q.clear()
        for cand in (port, old):
            if cand in self.blocked or cand not in g["ports"]:
                continue
            if cand in (self.root_port, self.gw_port):
                log("loop: %s %s moved %s<->%s %d times in %gs; %s carries the uplink, not blocked" %
                    (vlan, mac, old, port, g["threshold"], g["interval"], cand), syslog.LOG_WARNING)
                continue
            self.block(cand, vlan, mac, old if cand == port else port)
            return

    def block(self, port, vlan, mac, peer):
        hold = self.conf["loop_guard"]["hold"]
        until = time.time() + hold
        self.blocked[port] = until
        self.hold_down(port)
        put(self.db.st_guard, port, [("state", "blocked"), ("until", "%d" % until), ("vlan", vlan),
                                    ("mac", mac), ("peer", peer)])
        log("loop protection: %s down for %ds (%s %s flapping with %s)" % (port, hold, vlan, mac, peer),
            syslog.LOG_WARNING)

    def unblock(self, port, why="restored"):
        self.blocked.pop(port, None)
        self.db.st_guard._del(port)
        self.restore(port)
        log("loop protection: %s %s" % (port, why))

    def guard_tick(self):
        now = time.time()
        g = self.conf["loop_guard"]
        for p, t in list(self.blocked.items()):
            if t <= now:
                self.unblock(p)
            elif not g["enabled"] or p not in g["ports"]:
                self.unblock(p, "restored (loop protection off)")

    def adopt_guard_state(self):
        for k in self.db.keys(self.db.state, "DUI_LOOP_GUARD_TABLE|*"):
            p = k.split("|", 1)[1]
            try:
                self.blocked[p] = float(self.db.hgetall(self.db.state, k).get("until", "0"))
            except ValueError:
                self.blocked[p] = 0
        for k in self.db.keys(self.db.state, "DUI_BPDU_GUARD_TABLE|*"):
            p = k.split("|", 1)[1]
            row = self.db.hgetall(self.db.state, k)
            if row.get("state") == "held":
                self.bpdu_held[p] = row.get("since", "")
            elif row.get("state") == "released":
                self.bpdu_stale[p] = int(row.get("changes", "0") or 0)

    # --- ports held down in the chip ------------------------------------
    #
    # Loop protection and BPDU guard are the two protections that take a
    # port down in UniFi's model (§11.20); each hold is APPL_DB PORT_TABLE
    # admin_status down, so CONFIG_DB, the saved config and the driver's
    # plan (which reads CONFIG_DB) keep the operator's setting. portmgrd
    # rewrites APPL_DB's admin_status from CONFIG_DB whenever anything
    # writes the port's CONFIG_DB row (a description, an MTU, a speed), so
    # every pass puts a held port down again (reassert).

    def members(self, port):
        return self.db.lag_members(port) if port.startswith("PortChannel") else [port]

    def holding(self):
        """Every interface some hold keeps down."""
        out = set()
        for p in list(self.blocked) + list(self.bpdu_held):
            out.update(self.members(p))
        return out

    def hold_down(self, port):
        for p in self.members(port):
            put(self.db.ifport_pst, p, [("admin_status", "down")])

    def restore(self, port):
        """A released port's admin state back to CONFIG_DB's (an interface
        another hold still covers stays down), and its netdev up when the
        operator's setting is up: mstpd's BPDU guard shut the netdev itself
        (bridge port and member), which nothing else brings back."""
        still = self.holding()
        for p in self.members(port):
            if p in still:
                continue
            admin = self.db.hgetall(self.db.cfg, "PORT|" + p).get("admin_status", "down")
            put(self.db.ifport_pst, p, [("admin_status", admin)])
            if admin == "up":
                run(["ip", "link", "set", "dev", p, "up"])
        if port.startswith("PortChannel") and port not in still and \
                self.db.hgetall(self.db.cfg, "PORTCHANNEL|" + port).get("admin_status", "up") == "up":
            run(["ip", "link", "set", "dev", port, "up"])

    def reassert(self):
        """portmgrd's copy also runs `ip link set up` on the netdev; a held
        port's netdev goes down again too, so mstpd sees the port disabled
        as it did after the trip (seen 2026-11-15: with the netdev up mstpd
        cleared its error and ran the port as Designated, sending BPDUs into
        a disabled chip port)."""
        for p in sorted(self.holding()):
            if self.db.hgetall(self.db.appl, "PORT_TABLE:" + p).get("admin_status") != "down":
                put(self.db.ifport_pst, p, [("admin_status", "down")])
                log("%s: admin_status was rewritten under a hold: down again" % p, syslog.LOG_WARNING)
            try:
                up = int(rd("%s/%s/flags" % (SYS, p), "0"), 16) & 1
            except ValueError:
                up = 0
            if up:
                run(["ip", "link", "set", "dev", p, "down"])

    # --- BPDU guard -------------------------------------------------------
    #
    # mstpd's BPDU guard shuts only the port's netdev ("Received BPDU on
    # BPDU Guarded Port - Port Down"); the chip's port stays up, so a
    # cabled peer keeps its link (§11.23.3). A UniFi switch "shuts down the
    # port and requires manual re-enable" (the UI's tooltip), so l2d holds a
    # tripped port down in the chip until one of: the operator's Re-enable
    # (the controller's port-recovery command, posted by the driver as
    # STATE_DB DUI_PORT_RECOVERY|<port>), BPDU guard or STP switched off for
    # the port, the port leaving the bridge, spanning tree off, teardown.
    # No timer: the ECS's bpduguard cause has auto_recovery_interval 0.
    #
    # mstpd clears its error only when the port's link comes up again, so a
    # port released while it has no link (an empty cage) still reads
    # bpdu-guard-error yes. Such a port is "released": its error is not a
    # new trip until the port's kernel state has changed since (a link-up,
    # which is the only way out of disabled). The hold and the release
    # survive an l2d restart in DUI_BPDU_GUARD_TABLE; the driver reads
    # bpdu_guard_held from DUI_STP_PORT_TABLE.

    def bpdu_hold(self, port):
        since = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        self.bpdu_held[port] = since
        self.bpdu_stale.pop(port, None)
        self.hold_down(port)
        self.db.st_bpdu._del(port)  # a set merges: no field of a release stays
        put(self.db.st_bpdu, port, [("state", "held"), ("since", since)])
        log("bpdu guard: %s received a BPDU: held down in the chip until re-enabled" % port, syslog.LOG_WARNING)

    def bpdu_release(self, port, why):
        if port not in self.bpdu_held:
            return
        del self.bpdu_held[port]
        n = self.state_changes(port)
        self.bpdu_stale[port] = n
        put(self.db.st_bpdu, port, [("state", "released"), ("changes", str(n))])
        self.restore(port)
        log("bpdu guard: %s released (%s)" % (port, why))

    def bpdu_forget(self, port):
        self.bpdu_stale.pop(port, None)
        self.db.st_bpdu._del(port)

    def bpdu_check(self, port, d):
        """One port of mstpd's report (read after reconcile set mstpd's
        settings): a new trip holds the port; a held port whose guard
        mstpd no longer runs is released. Released only then, never on
        the config alone: a release while mstpd still guards the port
        lets the link come up into the guard, which shuts the netdev
        again with the guard about to go off (live, 2026-11-15)."""
        if port in self.bpdu_held and d.get("bpdu-guard-port") != "yes":
            self.bpdu_release(port, "BPDU guard off")
            return
        if d.get("bpdu-guard-port") != "yes" or d.get("bpdu-guard-error") != "yes":
            if port in self.bpdu_stale and d.get("bpdu-guard-error") != "yes":
                self.bpdu_forget(port)
            return
        if port in self.bpdu_held:
            return
        if port in self.bpdu_stale and self.state_changes(port) <= self.bpdu_stale[port]:
            return  # the error of the trip already released: no link since
        self.bpdu_hold(port)

    def bpdu_tick(self):
        """Releases what no longer has a reason to be held."""
        for p in list(self.bpdu_held):
            if not self.stp_on:
                self.bpdu_release(p, "spanning tree off")
            elif p not in self.ports:
                self.bpdu_release(p, "no longer a bridge port")
        # A released port keeps its mark until mstpd's error clears or the
        # port leaves the bridge, guard on or off: a guard switched on again
        # over the old error must not hold an unlinked port.
        for p in list(self.bpdu_stale):
            if p not in self.ports:
                self.bpdu_forget(p)

    def recoveries(self):
        """The operator's Re-enable (UniFi's port-recovery): releases a BPDU
        guard hold or a loop-protection hold on the named port, a member of
        a held PortChannel included."""
        for k in self.db.keys(self.db.state, "DUI_PORT_RECOVERY|*"):
            name = k.split("|", 1)[1]
            self.db.st_recover._del(name)
            hit = False
            for p in list(self.bpdu_held):
                if name == p or name in self.members(p):
                    self.bpdu_release(p, "re-enabled by the controller")
                    hit = True
            for p in list(self.blocked):
                if name == p or name in self.members(p):
                    self.unblock(p, "re-enabled by the controller")
                    hit = True
            if not hit:
                log("port recovery for %s: nothing holds it" % name)

    # --- main loop -------------------------------------------------------

    def sync(self):
        # Cleared before the config is read: a reload that arrives while this
        # sync runs wakes the next one instead of being lost (the driver writes
        # the config twice for one push: the bridge, then the ports).
        self.reload = False
        self.conf = load_conf()
        want = self.conf["stp"]["enabled"]
        if want and not self.stp_on:
            self.stp_on = self.stp_up()
        elif not want and (self.stp_on or rd("%s/%s/bridge/stp_state" % (SYS, BRIDGE)) == "2"):
            self.stp_down("disabled by the controller")
        elif self.stp_on and os.path.isdir("%s/%s/brif" % (SYS, BRIDGE)) and \
                rd("%s/%s/bridge/stp_state" % (SYS, BRIDGE)) != "2":
            # swss recreated Bridge (a restart of swss, a config reload): the
            # new netdev has stp_state 0 and mstpd dropped the old bridge, so
            # every port forwards unguarded until the bridge is adopted again
            # (seen live 2026-11-02: an orchagent crash left the switch with
            # no spanning tree for ten minutes). Start over as a fresh enable:
            # the ASIC's STP ports are new too.
            log("Bridge was recreated (swss restart?): re-enabling spanning tree", syslog.LOG_WARNING)
            self.written.clear()
            self.mapped.clear()
            self.asic_floor = 0
            self.ports.clear()
            self.stp_on = self.stp_up()
        elif self.stp_on and not self.mstp.running():
            log("mstpd died: restarting it", syslog.LOG_ERR)
            self.stp_on = self.stp_up()
        if self.stp_on:
            self.sync_stp()
        # Releases first, so the port rows published next say so.
        self.recoveries()
        self.bpdu_tick()
        if self.stp_on:
            self.publish()
        self.gw_port = self.gateway_port()
        self.guard_tick()
        self.reassert()
        self.dhcp.configure(self.conf["dhcp_rate"])
        for d in self.detectors:
            d.sync(self.conf)

    def main(self):
        syslog.openlog("disunified-l2d", syslog.LOG_PID, syslog.LOG_DAEMON)
        signal.signal(signal.SIGHUP, lambda *_: setattr(self, "reload", True))
        signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
        while not os.path.isdir("%s/%s/brif" % (SYS, BRIDGE)) or \
                not self.db.hgetall(self.db.appl, "PORT_TABLE:PortInitDone"):
            time.sleep(1)
        self.adopt_guard_state()
        self.storm.adopt()
        self.nl = nl_open()
        self.fdb_sub = sw.SubscriberStateTable(self.db.state, "FDB_TABLE")
        sel = sw.Select()
        sel.addSelectable(self.fdb_sub)
        log("started")
        nxt = 0.0
        while True:
            now = time.time()
            if now >= nxt or self.reload or self.resync:
                self.resync = False
                self.sync()
                nxt = time.time() + SYNC
            extra = {fd: d for d in self.detectors for fd in d.fds()}
            fds = [self.nl, self.fdb_sub.getFd()] + ([self.dhcp] if self.dhcp.sock is not None else []) + list(extra)
            r, _, _ = select.select(fds, [], [], 0.02)
            if self.dhcp in r:
                self.dhcp.readable()
            self.dhcp.tick()
            for fd in r:
                if isinstance(fd, int) and fd in extra:
                    extra[fd].readable(fd)
            if self.nl in r:
                try:
                    while True:
                        for ev in nl_events(self.nl.recv(1 << 16)):
                            if self.stp_on:
                                self.on_link(*ev)
                except BlockingIOError:
                    pass
                except OSError:  # ENOBUFS: notifications lost, resync now
                    nxt = 0.0
            # Drain the FDB subscription every turn: one read can cache
            # several entries, which leave the fd quiet until they are popped.
            while True:
                state, _ = sel.select(0)
                if state != sw.Select.OBJECT:
                    break
                key, op, fvs = self.fdb_sub.pop()
                self.on_fdb(key, op, fvs)
            self.run_flushes()
            self.storm.tick()


class StormWatch(object):
    """Storm detection (docs/drivers/sonic.md 11.21): a port whose incoming
    broadcast or multicast traffic exceeds its storm-control threshold.

    UniFi's storm anomalies say that incoming packets of a type exceeded
    the port's Storm Control threshold; the port is not taken down (11.20),
    and the switch keeps limiting the traffic (PORT_STORM_CONTROL's
    policers, which count nothing: 11.19). This reads the port's own
    receive counters, which count every frame the MAC took in, before the
    policer: IF_IN_BROADCAST_PKTS for broadcast, IF_IN_MULTICAST_PKTS for
    multicast (known groups too -- no counter tells unknown multicast
    apart). Unknown unicast has no counter of its own and is not watched.

    The threshold is in bytes (kbps x 125); the counters are frames. A
    frame is at least MIN_FRAME bytes, so more than limit/MIN_FRAME frames
    a second is certainly over the threshold, whatever their size: the
    rate is compared against that, over a WINDOW of samples, with MARGIN
    for the counters' own 1 s poll (a window's ends can be up to one poll
    stale); a storm ends when the rate is back under the limit. While a
    port is over, the frames above limit/MIN_FRAME are
    added to its count -- a lower bound on what storm control dropped --,
    which STATE_DB DUI_STORM_TABLE|<port> publishes (`broadcast`,
    `multicast`, monotonic; kept across restarts of this daemon) and the
    bridge reports as the port's storm drops.

    Unknown unicast (11.24) has no counter: UcastSampler feeds it the
    port's sFlow samples that the switch flooded (add_sample), each
    standing for `rate` frames of its size. The rate is taken in bytes,
    over the shortest of U_WINDOWS holding U_MIN samples; a storm starts
    when the rate less U_SIGMA standard errors is over the limit and ends
    when the rate plus them is under it, so the sampling's own noise never
    makes one. While a port is over, the share of each second's flooded
    frames above the byte limit is added to `unicast`."""

    TYPES = (  # PORT_STORM_CONTROL type, counter, STATE_DB field
        ("broadcast", "SAI_PORT_STAT_IF_IN_BROADCAST_PKTS", "broadcast"),
        ("unknown-multicast", "SAI_PORT_STAT_IF_IN_MULTICAST_PKTS", "multicast"),
    )
    MIN_FRAME = 64  # bytes: the smallest Ethernet frame
    TICK = 1.0  # seconds between samples (the counters' poll interval)
    WINDOW = 5  # samples a rate is taken over
    MARGIN = 1.25  # over = above MARGIN x the frame limit; under = below it
    UCAST = ("unknown-unicast", "unicast")  # PORT_STORM_CONTROL type, STATE_DB field
    U_WINDOWS = (5, 10, 20, 30)  # seconds a sampled unicast rate is taken over
    U_MIN = 30  # samples the shorter windows need before they are used
    U_START = 10  # samples a storm needs at least, whatever the window
    U_SIGMA = 3.0  # standard errors (1/sqrt(samples)) a start or an end must clear

    def __init__(self, db):
        self.cfg = db.cfg
        self.counters = sw.DBConnector("COUNTERS_DB", 0)
        self.st = sw.Table(db.state, "DUI_STORM_TABLE")
        self.state = db.state
        self.samples = {}  # port -> deque of (time, {field: frames})
        self.count = {}  # port -> {field: frames above the limit}
        self.over = set()  # (port, field) currently storming
        self.published = set()  # ports with a DUI_STORM_TABLE row
        self.nxt = 0.0
        self.ucast_on = False  # UcastSampler is delivering samples
        self.usamples = {}  # port -> deque of (time, frames, bytes) of flooded unicast
        self.ulast = {}  # port -> time of its last unicast evaluation

    def adopt(self):
        for k in DB.keys(self.state, "DUI_STORM_TABLE|*"):
            port = k.split("|", 1)[1]
            row = DB.hgetall(self.state, k)
            self.published.add(port)
            self.count[port] = {}
            for field in [f for _, _, f in self.TYPES] + ([self.UCAST[1]] if self.UCAST[1] in row else []):
                try:
                    self.count[port][field] = float(row.get(field, "0"))
                except ValueError:
                    self.count[port][field] = 0.0

    def limits(self):
        """port -> {STATE_DB field: kbps} from CONFIG_DB PORT_STORM_CONTROL;
        unknown unicast only while UcastSampler delivers samples."""
        types = [(typ, field) for typ, _, field in self.TYPES] + ([self.UCAST] if self.ucast_on else [])
        out = {}
        for k in DB.keys(self.cfg, "PORT_STORM_CONTROL|*"):
            parts = k.split("|")
            if len(parts) != 3:
                continue
            for typ, field in types:
                if parts[2] == typ:
                    try:
                        kbps = int(DB.hgetall(self.cfg, k).get("kbps", "0"))
                    except ValueError:
                        kbps = 0
                    if kbps > 0:
                        out.setdefault(parts[1], {})[field] = kbps
        return out

    def add_sample(self, port, now, rate, size):
        """A flooded unicast frame sampled 1:rate on port (UcastSampler)."""
        self.usamples.setdefault(port, collections.deque()).append((now, float(rate), float(rate) * size))

    def tick(self, now=None):
        now = time.time() if now is None else now
        if now < self.nxt:
            return
        self.nxt = now + self.TICK
        lim = self.limits()
        oids = None
        for port in list(self.count):
            if port not in lim:
                self.forget(port)
        for port in list(self.usamples):
            if self.UCAST[1] not in lim.get(port, {}):
                self.usamples.pop(port, None)
                self.ulast.pop(port, None)
                self.over.discard((port, self.UCAST[1]))
        for port, kinds in lim.items():
            changed = False
            ckinds = {f: k for f, k in kinds.items() if f != self.UCAST[1]}
            if ckinds:
                if oids is None:
                    oids = DB.hgetall(self.counters, "COUNTERS_PORT_NAME_MAP")
                oid = oids.get(port)
                row = DB.hgetall(self.counters, "COUNTERS:" + oid) if oid else {}
                try:
                    frames = {field: int(row[ctr]) for _, ctr, field in self.TYPES}
                except (KeyError, ValueError):
                    frames = None
                if frames is not None:
                    changed = self.sample(port, ckinds, now, frames)
            if self.UCAST[1] in kinds:
                changed = self.usample(port, kinds[self.UCAST[1]], now) or changed
            if changed:
                counts = self.count[port]
                put(self.st, port, [(f, "%d" % counts[f]) for f in sorted(counts)])
                self.published.add(port)

    def counts_of(self, port, unicast=False):
        counts = self.count.setdefault(port, {f: 0.0 for _, _, f in self.TYPES})
        for _, _, f in self.TYPES:
            counts.setdefault(f, 0.0)
        if unicast:
            counts.setdefault(self.UCAST[1], 0.0)
        return counts

    def usample(self, port, kbps, now):
        """Unknown unicast: the sampled flooded bytes against the byte limit.
        Returns whether the port's row must be written."""
        field = self.UCAST[1]
        counts = self.counts_of(port, unicast=True)
        changed = port not in self.published
        q = self.usamples.setdefault(port, collections.deque())
        while q and q[0][0] <= now - self.U_WINDOWS[-1]:
            q.popleft()
        limit = kbps * 125.0  # bytes/s, as orchagent programs the policer
        for w in self.U_WINDOWS:
            sel = [x for x in q if x[0] > now - w]
            if len(sel) >= self.U_MIN:
                break
        n = len(sel)
        rate = sum(x[2] for x in sel) / w
        spread = self.U_SIGMA / math.sqrt(n) if n else 1.0
        key = (port, field)
        if key not in self.over and n >= self.U_START and rate * (1 - spread) > limit:
            self.over.add(key)
            log("storm control: %s unicast storm, %.0f bytes/s of flooded unicast over %d s (%d samples), limit %d kbps (%.0f bytes/s)" %
                (port, rate, w, n, kbps, limit), syslog.LOG_WARNING)
        elif key in self.over and rate * (1 + spread) < limit:
            self.over.discard(key)
            log("storm control: %s unicast storm over, %d frames above the limit in all" % (port, counts[field]))
        if key in self.over:
            last = self.ulast.get(port, now - self.TICK)
            fresh = sum(x[1] for x in q if last < x[0] <= now)
            if rate > limit:
                counts[field] += fresh * (1 - limit / rate)
            changed = True
        self.ulast[port] = now
        return changed

    def sample(self, port, kinds, now, frames):
        """The counter-based types (broadcast, multicast); returns whether
        the port's row must be written."""
        q = self.samples.setdefault(port, collections.deque(maxlen=self.WINDOW + 1))
        last = q[-1] if q else None
        q.append((now, frames))
        if last is None or any(frames[f] < last[1][f] for f in frames):
            q.clear()  # first sample, or the counters were cleared
            q.append((now, frames))
            return False
        counts = self.counts_of(port)
        changed = port not in self.published
        t0, f0 = q[0]
        for field, kbps in kinds.items():
            allowed = kbps * 125.0 / self.MIN_FRAME  # frames/s that fit under the limit
            rate = (frames[field] - f0[field]) / (now - t0) if len(q) > self.WINDOW and now > t0 else 0.0
            key = (port, field)
            if key not in self.over and rate > allowed * self.MARGIN:
                self.over.add(key)
                log("storm control: %s %s storm, %.0f frames/s over %d s, limit %d kbps (%.0f frames/s at %d bytes)" %
                    (port, field, rate, self.WINDOW, kbps, allowed, self.MIN_FRAME), syslog.LOG_WARNING)
            elif key in self.over and rate < allowed:
                self.over.discard(key)
                log("storm control: %s %s storm over, %d frames above the limit in all" % (port, field, counts[field]))
            if key in self.over:
                dt = now - last[0]
                counts[field] += max(0.0, frames[field] - last[1][field] - allowed * dt)
                changed = True
        for field in counts:
            if field not in kinds and field != self.UCAST[1]:
                self.over.discard((port, field))
        return changed

    def forget(self, port):
        """Storm control is gone from the port: so is its row."""
        self.samples.pop(port, None)
        self.usamples.pop(port, None)
        self.ulast.pop(port, None)
        self.count.pop(port, None)
        self.over = {k for k in self.over if k[0] != port}
        if port in self.published:
            self.st._del(port)
            self.published.discard(port)



PSAMPLE_RATE_PROC = "/proc/bcm/genl/psample/rate"  # linux-bcm-genl (image kit sonic-buildimage 0014)
GENL_ID_CTRL, CTRL_CMD_GETFAMILY = 0x10, 3
CTRL_ATTR_FAMILY_ID, CTRL_ATTR_FAMILY_NAME, CTRL_ATTR_MCAST_GROUPS = 1, 2, 7
PSAMPLE_IIF, PSAMPLE_OIF, PSAMPLE_ORIGSIZE, PSAMPLE_RATE, PSAMPLE_DATA = 0, 1, 2, 5, 6
PSAMPLE_NOT_FORWARDED = 0xFFFF  # bcm-genl: neither a front-panel port nor the CPU (flooded, or dropped)


def nla(buf, off=0, end=None):
    """Netlink attributes -> {type: payload} (the first of each type)."""
    end = len(buf) if end is None else end
    out = {}
    while off + 4 <= end:
        ln, ty = struct.unpack_from("HH", buf, off)
        if ln < 4:
            break
        out.setdefault(ty & 0x3FFF, buf[off + 4:off + ln])
        off += (ln + 3) & ~3
    return out


def psample_messages(buf, family):
    """(iif, oif, rate, origsize, data) of each psample message in a recv."""
    off = 0
    while off + 16 <= len(buf):
        ln, ty = struct.unpack_from("IH", buf, off)
        if ln < 16:
            break
        if ty == family:
            a = nla(buf, off + 20, off + ln)
            u = lambda k, f, d: struct.unpack(f, a[k][:struct.calcsize(f)])[0] if k in a else d
            yield (u(PSAMPLE_IIF, "H", 0), u(PSAMPLE_OIF, "H", 0), u(PSAMPLE_RATE, "I", 0),
                   u(PSAMPLE_ORIGSIZE, "I", 0), a.get(PSAMPLE_DATA, b""))
        off += (ln + 3) & ~3


class UcastSampler(object):
    """Unknown-unicast storm detection's eyes (docs/drivers/sonic.md 11.24).

    Nothing on the DX010 counts the unicast frames it floods: the SAI
    refuses the ACL field for a missed destination lookup and answers the
    storm policers' statistics with zeros (11.24.1). The chip's ingress
    sFlow sampler sees every received frame, before storm control drops
    it, and the SAI delivers its samples through linux-bcm-genl to the
    psample generic-netlink family. So for each port with unknown-unicast
    storm control this samples the port through SONiC's own sFlow
    configuration, reads the samples, and hands StormWatch each one the
    switch flooded: a unicast destination, sent to no front-panel port and
    not the CPU, and not in the FDB for its VLAN.

    The configuration is CONFIG_DB's, as `config sflow` writes it:
    SFLOW_SESSION|all down first (no other port is sampled), SFLOW|global
    up, SFLOW_SESSION|<port> up at the port's rate, and FEATURE|sflow on
    (sflowmgrd turns the rows into sfloworch's; coppmgrd installs the
    sample trap with its group only through the feature, never for an
    always_enabled trap whose group has no other trap, 11.24.3). A
    feature this daemon turned on is marked in OWNED and turned off again
    with the last session; one the operator turned on is left alone, and
    so are sessions it already has. The sample rate gives TARGET samples
    a second at the limit with 64-byte frames (at least 256, sonic-sflow's
    floor), so the CPU sees samples in proportion to the traffic; CoPP
    caps them (the sflow trap's own queue, 1000/s)."""

    TARGET = 25  # samples a second at the limit with 64-byte frames
    RATE_MIN, RATE_MAX = 256, 8388608  # sonic-sflow.yang's sample_rate range
    OWNED = "/etc/sonic/disunified/l2d-sflow"  # this daemon turned the sflow feature on

    def __init__(self, l2d):
        self.l2d = l2d
        self.db = l2d.db
        self.storm = l2d.storm
        self.t_session = sw.Table(self.db.cfg, "SFLOW_SESSION")
        self.t_global = sw.Table(self.db.cfg, "SFLOW")
        self.t_feature = sw.Table(self.db.cfg, "FEATURE")
        self.sock = None
        self.family = 0
        self.rates = {}  # watched port -> sample rate
        self.pvid = {}  # port -> its untagged VLAN id
        self.own = set()  # the switch's own MACs
        self.names = {}  # ifindex -> port name
        self.noted = set()
        self.stopping = 0.0  # when the sflow rows were deleted

    def note(self, key, msg, prio=syslog.LOG_WARNING):
        if key not in self.noted:
            self.noted.add(key)
            log(msg, prio)

    def want(self):
        """port -> sample rate, for each front-panel port with unknown-unicast storm control."""
        out = {}
        for k in DB.keys(self.db.cfg, "PORT_STORM_CONTROL|*|unknown-unicast"):
            port = k.split("|")[1]
            try:
                kbps = int(DB.hgetall(self.db.cfg, k).get("kbps", "0"))
            except ValueError:
                kbps = 0
            if port.startswith("Ethernet") and kbps > 0:
                rate = int(kbps * 125.0 / StormWatch.MIN_FRAME / self.TARGET)
                out[port] = min(self.RATE_MAX, max(self.RATE_MIN, rate))
        return out

    def owned(self):
        return os.path.exists(self.OWNED)

    def sync(self, conf):
        want = self.want()
        if not want:
            self.stop()
            return
        if not os.path.exists(PSAMPLE_RATE_PROC):
            self.note("genl", "unknown-unicast storm control on %d port(s), but this image has no linux-bcm-genl "
                      "(%s): unknown-unicast storms are not detected (docs/drivers/sonic.md 11.24)" % (len(want), PSAMPLE_RATE_PROC))
            self.stop()
            return
        feature = DB.hgetall(self.db.cfg, "FEATURE|sflow").get("state") == "enabled"
        if feature and not self.owned():
            self.note("operator", "the sflow feature is the operator's: unknown-unicast storms are detected from what "
                      "it samples on the storm-controlled ports, no session is written", syslog.LOG_INFO)
        else:
            self.write_sessions(want, feature)
        self.rates = want
        if self.sock is None:
            self.open()
        self.refresh()
        self.storm.ucast_on = self.sock is not None

    def write_sessions(self, want, feature):
        if DB.hgetall(self.db.cfg, "SFLOW_SESSION|all").get("admin_state") != "down":
            put(self.t_session, "all", [("admin_state", "down")])  # first: no other port is sampled
        for port, rate in sorted(want.items()):
            row = [("admin_state", "up"), ("sample_rate", str(rate))]
            if DB.hgetall(self.db.cfg, "SFLOW_SESSION|" + port) != dict(row):
                put(self.t_session, port, row)
                log("storm control: sampling %s at 1:%d for unknown unicast" % (port, rate))
        for k in DB.keys(self.db.cfg, "SFLOW_SESSION|Ethernet*"):
            port = k.split("|", 1)[1]
            if port not in want:
                self.t_session._del(port)
                log("storm control: %s no longer sampled" % port)
        if DB.hgetall(self.db.cfg, "SFLOW|global").get("admin_state") != "up":
            put(self.t_global, "global", [("admin_state", "up")])
        if not feature:
            with open(self.OWNED, "w") as f:
                f.write(time.strftime("%Y-%m-%dT%H:%M:%S%z") + "\n")
            self.t_feature.hset("sflow", "state", "enabled")
            log("storm control: sflow feature on for unknown-unicast sampling")

    def stop(self):
        self.storm.ucast_on = False
        self.rates = {}
        if self.sock is not None:
            self.sock.close()
            self.sock = None
        if not self.owned():
            return
        rows = DB.keys(self.db.cfg, "SFLOW_SESSION|*") + DB.keys(self.db.cfg, "SFLOW|*")
        if rows:
            for k in rows:
                table, key = k.split("|", 1)
                (self.t_session if table == "SFLOW_SESSION" else self.t_global)._del(key)
            self.stopping = time.time()
            return  # sflowmgrd takes its rows out of APPL_DB before the feature goes
        if DB.keys(self.db.appl, "SFLOW_*") and time.time() - self.stopping < 10:
            return
        self.t_feature.hset("sflow", "state", "disabled")
        try:
            os.unlink(self.OWNED)
        except OSError:
            pass
        log("storm control: unknown-unicast sampling stopped, sflow feature off")

    def open(self):
        try:
            sk = socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, 16)  # NETLINK_GENERIC
            sk.bind((0, 0))
            name = b"psample\0"
            a = struct.pack("HH", 4 + len(name), CTRL_ATTR_FAMILY_NAME) + name
            a += b"\0" * ((4 - len(a) % 4) % 4)
            msg = struct.pack("BBH", CTRL_CMD_GETFAMILY, 1, 0) + a
            sk.send(struct.pack("IHHII", 16 + len(msg), GENL_ID_CTRL, 1, 1, 0) + msg)
            at = nla(sk.recv(65536), 20)
            family = struct.unpack("H", at[CTRL_ATTR_FAMILY_ID][:2])[0]
            groups = {}
            for g in nla(at[CTRL_ATTR_MCAST_GROUPS]).values():
                ga = nla(g)
                groups[ga[1].rstrip(b"\0").decode()] = struct.unpack("I", ga[2][:4])[0]
            sk.setsockopt(270, 1, groups["packets"])  # SOL_NETLINK, NETLINK_ADD_MEMBERSHIP
            sk.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 1 << 20)
            sk.setblocking(False)
        except (OSError, KeyError, struct.error) as e:
            self.note("psample", "cannot read psample samples (%s): unknown-unicast storms are not detected" % e)
            return
        self.sock, self.family = sk, family
        log("storm control: reading psample samples for unknown unicast")

    def refresh(self):
        """The ports' untagged VLANs and the switch's MACs, as the classifier needs them."""
        lag_of = {}
        for k in DB.keys(self.db.cfg, "PORTCHANNEL_MEMBER|*"):
            _, lag, member = k.split("|", 2)
            lag_of[member] = lag
        untagged = {}
        for k in DB.keys(self.db.cfg, "VLAN_MEMBER|*"):
            _, vlan, member = k.split("|", 2)
            if DB.hgetall(self.db.cfg, k).get("tagging_mode") == "untagged" and vlan.startswith("Vlan"):
                untagged[member] = int(vlan[4:])
        self.pvid = {p: untagged.get(lag_of.get(p, p)) for p in self.rates}
        mac = DB.hgetall(self.db.cfg, "DEVICE_METADATA|localhost").get("mac", "")
        self.own = {mac.lower()} if mac else set()

    def fds(self):
        return [self.sock.fileno()] if self.sock is not None else []

    def name(self, ifindex):
        if ifindex not in self.names:
            try:
                self.names[ifindex] = socket.if_indextoname(ifindex)
            except OSError:
                return ""
        return self.names[ifindex]

    def readable(self, fd):
        now = time.time()
        while self.sock is not None:
            try:
                buf = self.sock.recv(1 << 16)
            except BlockingIOError:
                return
            except OSError:  # ENOBUFS: samples lost, a lower bound still
                continue
            for iif, oif, rate, size, data in psample_messages(buf, self.family):
                self.classify(now, self.name(iif), oif, rate, size, data)

    def classify(self, now, port, oif, rate, size, data):
        """Hand StormWatch a sample the switch flooded as unknown unicast."""
        if port not in self.rates or len(data) < 14 or data[0] & 1:
            return  # not watched, short, or a group destination
        if oif != PSAMPLE_NOT_FORWARDED:
            return  # forwarded to a front-panel port (a known destination), or to the CPU
        dmac = ":".join("%02x" % b for b in data[:6])
        if dmac in self.own:
            return  # the switch's own MAC: routed or for the CPU, never flooded
        vid = self.pvid.get(port)
        if data[12:14] == b"\x81\x00" and len(data) >= 16:
            vid = struct.unpack("!H", data[14:16])[0] & 0xFFF
        if vid and ("Vlan%d" % vid, dmac) in self.l2d.fdb_port:
            return  # a known destination, dropped for another reason
        self.storm.add_sample(port, now, rate if rate > 1 else self.rates[port], size)

    def teardown(self):
        deadline = time.time() + 12
        while self.owned() and time.time() < deadline:
            self.stop()
            time.sleep(1)


MACLOCK_CHAIN = "DUI_MACLOCK"
MACLOCK_TABLE = "DUI_MACLOCK_TABLE"
EBTABLES = "/usr/sbin/ebtables"
EBTABLES_RESTORE = "/usr/sbin/ebtables-restore"
ETH_P_ALL = 3
PACKET_OUTGOING = 4
LEARN_TRAP = "cpu_trap"
LEARN_HARDWARE = "hardware"
MACLOCK_LOG_EVERY = 600.0  # seconds between log lines for one port and source


def eb_state(name):
    """(chain exists, its rule count, the hooks of INPUT and FORWARD that
    jump to it), from the kernel: a chain that is gone or shorter than
    written (an operator's flush; SONiC's database.sh runs a plain
    ebtables-restore of /etc/ebtables.filter.cfg, which empties the whole
    filter table) is written again."""
    txt = run([EBTABLES, "-L"]).stdout
    exists, count, jumps, chain = False, 0, set(), ""
    for line in txt.splitlines():
        if line.startswith("Bridge chain: "):
            chain = line[len("Bridge chain: "):].split(",")[0]
            if chain == name:
                exists = True
                try:
                    count = int(line.split("entries: ")[1].split(",")[0])
                except (IndexError, ValueError):
                    count = -1
        elif line.strip() == "-j " + name and chain in ("INPUT", "FORWARD"):
            jumps.add(chain)
    return exists, count, jumps


def eb_restore(lines):
    """Applies lines (ebtables commands without the program name) to the
    filter table in one ebtables-restore --noflush, which the switch's
    ebtables (1.8, nf_tables) commits as one transaction: a frame meets the
    chains as they were before or as they are after, never in between, and
    a refused line refuses the whole batch (docs/drivers/sonic.md 11.26.12).
    One ebtables call per rule left a rewritten chain empty, i.e. passing
    every frame, until its last rule was back."""
    return run([EBTABLES_RESTORE, "--noflush"], check=True,
               stdin="\n".join(["*filter"] + lines) + "\n").returncode == 0


def eb_chain(chain, rules, jumps, first):
    """The restore lines that leave chain (policy RETURN) holding exactly
    rules, jumped to from INPUT and FORWARD: ahead of every other rule
    (first) or after them. jumps: the hooks that already jump to it. The
    declaration creates the chain or keeps it; the flush is explicit
    because ebtables-legacy-restore keeps a declared chain's rules."""
    out = [":%s RETURN" % chain, "-F " + chain]
    out += ["-A %s %s" % (chain, " ".join(r)) for r in rules]
    for c in ("INPUT", "FORWARD"):
        if c not in jumps:
            out.append(("-I %s 1 -j %s" if first else "-A %s -j %s") % (c, chain))
    return out


def eb_unchain(chain, jumps, exists):
    """The restore lines that remove chain and its jumps."""
    out = ["-D %s -j %s" % (c, chain) for c in sorted(jumps)]
    if exists:
        out += ["-F " + chain, "-X " + chain]
    return out


def mac_text(b):
    return ":".join("%02x" % c for c in bytearray(b))


class MacLockGuard(object):
    """MAC locking: UniFi's MAC Address Filter on a port, and its anomaly
    (256; as the UI words it, a MAC detected on the port that is not on its
    allowed list) -- docs/drivers/sonic.md 11.18.2.

    The driver secures a port with CONFIG_DB learn_mode drop and one static
    FDB entry per allowed MAC: the chip forwards the listed sources and drops
    every other one without a trace. With the port in this daemon's config
    (mac_lock.ports: bridge port -> allowed MACs) it switches the port's
    learning to cpu_trap in APPL_DB, as Ubiquiti's own SONiC switches run a
    secured port: an unknown source is still neither learnt nor forwarded by
    the chip, but the frame goes to the CPU, policed by the default CoPP
    group, and arrives on the port's netdev. The kernel bridge drops it there
    (chain DUI_MACLOCK, from INPUT and FORWARD: the listed sources pass, the
    rest of the port's frames are dropped) and a packet socket on the netdev
    sees it first. Each frame whose source is not on the list is counted for
    the interface it arrived on (STATE_DB DUI_MACLOCK_TABLE|<interface>:
    drops, cumulative since boot, and the last such source); the port stays
    up. CONFIG_DB keeps drop, so a reboot, a swss restart or a stopped daemon
    leaves the port as closed as before, only uncounted; the chain is written
    before the learn mode changes and the learn mode goes back before the
    chain is removed."""

    def __init__(self, l2d):
        self.l2d = l2d
        self.db = l2d.db
        self.want = {}  # bridge port -> frozenset of allowed MACs
        self.trapped = set()  # bridge ports this daemon switched to cpu_trap
        self.socks = {}  # netdev -> (socket, bridge port)
        self.written = None  # the chain's rules as last written; None = not checked
        self.own = ""
        self.lag_pst = sw.ProducerStateTable(self.db.appl, "LAG_TABLE")
        self.st = sw.Table(self.db.state, MACLOCK_TABLE)
        self.drops = {}  # netdev -> count, carried over a restart from STATE_DB
        self.last = {}  # netdev -> last source counted
        self.dirty = set()
        self.logged = {}  # (netdev, mac) -> time logged
        for k in self.db.keys(self.db.state, MACLOCK_TABLE + "|*"):
            row = self.db.hgetall(self.db.state, k)
            try:
                self.drops[k.split("|", 1)[1]] = int(row.get("drops", "0"))
            except ValueError:
                pass

    # --- configuration -----------------------------------------------------

    @staticmethod
    def learn_key(port):
        return ("LAG_TABLE:" if port.startswith("PortChannel") else "PORT_TABLE:") + port

    def learn_set(self, port, mode):
        pst = self.lag_pst if port.startswith("PortChannel") else self.db.ifport_pst
        put(pst, port, [("learn_mode", mode)])

    def configured_mode(self, port):
        table = "PORTCHANNEL|" if port.startswith("PortChannel") else "PORT|"
        return self.db.hgetall(self.db.cfg, table + port).get("learn_mode") or "hardware"

    def netdevs(self, port):
        return self.db.lag_members(port) if port.startswith("PortChannel") else [port]

    def rules(self):
        out = []
        for port in sorted(self.want):
            for mac in sorted(self.want[port]):
                out.append(["-i", port, "-s", mac, "-j", "RETURN"])
            out.append(["-i", port, "-j", "DROP"])
        return out

    def chain_state(self):
        return eb_state(MACLOCK_CHAIN)

    def write_chain(self, rules):
        exists, count, jumps = self.chain_state()
        if not rules:
            ok = True
            if exists or jumps:
                ok = eb_restore(eb_unchain(MACLOCK_CHAIN, jumps, exists))
                if ok:
                    log("MAC locking: kernel bridge rules removed")
            self.written = [] if ok else None
            return ok
        if exists and count == len(rules) and jumps == {"INPUT", "FORWARD"} and rules == self.written:
            return True
        # One transaction: the chain is never empty, and so never passes
        # every frame, while it is rewritten (11.26.12).
        ok = eb_restore(eb_chain(MACLOCK_CHAIN, rules, jumps, False))
        self.written = rules if ok else None
        if ok:
            log("MAC locking: kernel bridge rules for %s (%d allowed sources)" %
                (",".join(sorted(self.want)), sum(len(m) for m in self.want.values())))
        return ok

    def sync(self, conf):
        want = {p: m for p, m in conf["mac_lock"]["ports"].items() if self.db.known(p)}
        if not self.own:
            self.own = (self.db.hgetall(self.db.cfg, "DEVICE_METADATA|localhost").get("mac") or "").lower()
        # A port leaving the config gets its configured learn mode back
        # before its rules go.
        for port in sorted(self.trapped - set(want)):
            if self.db.known(port):
                self.learn_set(port, self.configured_mode(port))
            self.trapped.discard(port)
            log("MAC locking: %s no longer watched (learn mode %s)" % (port, self.configured_mode(port)))
        self.want = want
        if want or self.written is None or self.written:
            if not self.write_chain(self.rules()):
                return  # no learn mode change without the kernel's drop in place
        for port in sorted(want):
            if self.db.hgetall(self.db.appl, self.learn_key(port)).get("learn_mode") != LEARN_TRAP:
                self.learn_set(port, LEARN_TRAP)
                if port not in self.trapped:
                    log("MAC locking: %s watched, %d allowed sources" % (port, len(want[port])))
            self.trapped.add(port)
        self.sync_socks()
        self.publish()

    def sync_socks(self):
        wanted = {n: port for port in self.want for n in self.netdevs(port)
                  if os.path.isdir("%s/%s" % (SYS, n))}
        for n in list(self.socks):
            if wanted.get(n) != self.socks[n][1]:
                self.socks.pop(n)[0].close()
        for n, port in wanted.items():
            if n in self.socks:
                continue
            try:
                s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(ETH_P_ALL))
                s.bind((n, 0))
                s.setblocking(False)
            except OSError as e:
                log("MAC locking: no packet socket on %s: %s" % (n, e), syslog.LOG_WARNING)
                continue
            self.socks[n] = (s, port)

    def publish(self):
        """The counts, at most once a pass: the trap's rate is CoPP's, the
        database writes are this daemon's."""
        for n in sorted(self.dirty):
            put(self.st, n, [("drops", str(self.drops.get(n, 0))), ("last_mac", self.last.get(n, ""))])
        self.dirty.clear()

    # --- detection ---------------------------------------------------------

    def fds(self):
        return [s.fileno() for s, _ in self.socks.values()]

    def readable(self, fd):
        for n, (s, port) in list(self.socks.items()):
            if s.fileno() == fd:
                self.drain(n, s, port)
                return

    def drain(self, n, s, port):
        """Reads what is queued, at most a burst per turn; each inbound frame
        from a unicast source that is not the switch's and not on the port's
        list is one drop."""
        for _ in range(256):
            try:
                data, addr = s.recvfrom(64)
            except (BlockingIOError, InterruptedError):
                return
            except OSError:
                return
            if addr[2] == PACKET_OUTGOING or len(data) < 12 or bytearray(data)[6] & 1:
                continue  # the switch's own frames; a group address is no source
            src = mac_text(data[6:12])
            if src == self.own or src in self.want.get(port, ()):
                continue
            self.drops[n] = self.drops.get(n, 0) + 1
            self.last[n] = src
            self.dirty.add(n)
            now = time.time()
            if now - self.logged.get((n, src), 0) > MACLOCK_LOG_EVERY:
                self.logged[(n, src)] = now
                log("MAC locking: %s on %s is not on its list; dropped (%d on this port since boot)" %
                    (src, n, self.drops[n]), syslog.LOG_WARNING)

    def teardown(self):
        """Secured ports back to their CONFIG_DB learn mode, then the rules go.
        The counts stay: they are since boot."""
        for k in self.db.keys(self.db.appl, "PORT_TABLE:*") + self.db.keys(self.db.appl, "LAG_TABLE:*"):
            port = k.split(":", 1)[1]
            if self.db.hgetall(self.db.appl, k).get("learn_mode") == LEARN_TRAP:
                self.learn_set(port, self.configured_mode(port))
        self.want = {}
        self.written = None
        self.write_chain([])
        for s, _ in self.socks.values():
            s.close()
        self.socks = {}


DOT1X_CHAIN = "DUI_DOT1X"
DOT1X_TABLE = "DUI_DOT1X_TABLE"
DOT1X_SECRET = "/etc/sonic/disunified/dot1x-radius"  # one RADIUS secret per line, the servers' order; root 0600
DOT1X_RUN = "/run/disunified-dot1x"
HOSTAPD = "/usr/local/sbin/hostapd"
# EAPOL to the CPU while a port authenticates: copporch maps trap id "eapol"
# to SAI_HOSTIF_TRAP_TYPE_EAPOL. The image's copp_cfg.json lists it under the
# macsec feature only, which is off, so the trap is installed with
# always_enabled, as STP's is (verified live on the DX010: ASIC_DB gains the
# trap, a looped port's EAPOL-Start reaches its netdev tagged with the VLAN).
COPP_EAPOL_KEY = "dui_eapol"
COPP_EAPOL = [("trap_ids", "eapol"), ("trap_group", COPP_GROUP_NAME), ("always_enabled", "true")]
# A port's control modes that run an authenticator. force_unauthorized is
# held closed with no authenticator; force_authorized is not in the config.
DOT1X_AUTH_MODES = ("auto", "multi_host", "mac_based")
DOT1X_RESTART_EVERY = 10.0  # seconds between starts of one port's hostapd
# MAC authentication (mac_based, sonic.md 11.26.8): a host that sends no
# EAPOL is authenticated by its MAC address, which l2d sends to the RADIUS
# servers itself (hostapd's wired driver has no MAC authentication: its
# macaddr_acl is consulted only for 802.11 authentication frames).
MAB_TIMEOUT = 3.0  # seconds before a request is sent again
MAB_TRIES = 2  # sends per server before the next server
MAB_QUIET = 60.0  # seconds a refused or unanswered source is left alone (hostapd's quiet period)
MAB_PENDING = 32  # requests in flight at most (a burst of new sources is policed by CoPP too)
# A MAC-authenticated host's last frame is also written as l2d runs (the
# row's mab_heard), at most this many seconds behind, so an l2d that dies
# without stopping (SIGKILL, a crash) leaves the next one a time no later
# than the host's last frame and less than this before it: that l2d gives
# each host this long on top of its idle timeout (sonic.md 11.26.13).
MAB_HEARD_STEP = 5.0
ETH_P_PAE = 0x888E
ETH_P_8021Q = 0x8100
RADIUS_ACCESS_REQUEST, RADIUS_ACCESS_ACCEPT, RADIUS_ACCESS_REJECT = 1, 2, 3
RADIUS_MESSAGE_AUTHENTICATOR = 80


def mab_user(mac):
    """The user name and password for a MAC: as the controller's UI says of
    its RADIUS users, "For 802.1X, addresses must be entered upper case
    without separators" (Settings -> RADIUS -> Local Credentials)."""
    return mac.replace(":", "").replace("-", "").upper()


def radius_attr(typ, value):
    return struct.pack("BB", typ, len(value) + 2) + value


def radius_hide(password, secret, auth):
    """User-Password as RFC 2865 5.2 hides it."""
    p = password + b"\0" * (-len(password) % 16)
    if not p:
        p = b"\0" * 16
    out, prev = b"", auth
    for i in range(0, len(p), 16):
        key = hashlib.md5(secret + prev).digest()
        prev = bytes(a ^ b for a, b in zip(p[i:i + 16], key))
        out += prev
    return out


def mab_request(ident, auth, mac, secret, nas_ip, port, own):
    """A MAC authentication Access-Request: the MAC as user name and
    password, the usual MAB attributes (Service-Type Call-Check, the host
    and the switch in RFC 3580's station form, NAS-Port-Type Ethernet) and a
    Message-Authenticator (RFC 3579), as hostapd sends one for its MAC ACL."""
    user = mab_user(mac).encode()
    station = lambda m: "-".join(m.upper().split(":")).encode()
    attrs = radius_attr(RADIUS_MESSAGE_AUTHENTICATOR, b"\0" * 16)
    attrs += radius_attr(1, user)  # User-Name
    attrs += radius_attr(2, radius_hide(user, secret, auth))  # User-Password
    attrs += radius_attr(6, struct.pack("!I", 10))  # Service-Type Call-Check
    if nas_ip:
        try:
            attrs += radius_attr(4, socket.inet_aton(nas_ip))  # NAS-IP-Address
        except OSError:
            pass
    attrs += radius_attr(32, b"disunified")  # NAS-Identifier, as hostapd's nas_identifier
    attrs += radius_attr(61, struct.pack("!I", 15))  # NAS-Port-Type Ethernet
    attrs += radius_attr(87, port.encode())  # NAS-Port-Id
    attrs += radius_attr(31, station(mac))  # Calling-Station-Id
    if own:
        attrs += radius_attr(30, station(own))  # Called-Station-Id
    pkt = struct.pack("!BBH", RADIUS_ACCESS_REQUEST, ident, 20 + len(attrs)) + auth + attrs
    ma = hmac.new(secret, pkt, hashlib.md5).digest()
    return pkt[:22] + ma + pkt[38:]


def radius_answer(data, ident, auth, secret):
    """An Access-Accept or Access-Reject for the request (ident, auth):
    "accept", "reject", or None when it is not one, or is not signed with
    the secret -- the Response Authenticator and the Message-Authenticator,
    which a reply must carry (RFC 3579 3.2, and the BlastRADIUS fix)."""
    if len(data) < 20:
        return None
    code, rid, length = struct.unpack("!BBH", data[:4])
    if rid != ident or length < 20 or length > len(data) or \
            code not in (RADIUS_ACCESS_ACCEPT, RADIUS_ACCESS_REJECT):
        return None
    data = data[:length]
    if hashlib.md5(data[:4] + auth + data[20:] + secret).digest() != data[4:20]:
        return None
    pos, ma = 20, -1
    while pos + 2 <= length:
        typ, alen = data[pos], data[pos + 1]
        if alen < 2 or pos + alen > length:
            return None
        if typ == RADIUS_MESSAGE_AUTHENTICATOR and alen == 18:
            ma = pos
        pos += alen
    if ma < 0:
        return None
    zeroed = data[:4] + auth + data[20:ma + 2] + b"\0" * 16 + data[ma + 18:]
    if not hmac.compare_digest(hmac.new(secret, zeroed, hashlib.md5).digest(), data[ma + 2:ma + 18]):
        return None
    return "accept" if code == RADIUS_ACCESS_ACCEPT else "reject"


def eapol_rules(port):
    """EAPOL is accepted before anything else: the bridge hands a link-local
    frame to the port's packet sockets (hostapd's) only once ebtables INPUT
    has passed it (seen live: a DROP for the port starved hostapd)."""
    return [["-i", port, "-p", "0x888e", "-j", "ACCEPT"],
            ["-i", port, "-p", "802_1Q", "--vlan-encap", "0x888e", "-j", "ACCEPT"]]


class Dot1xGuard(object):
    """IEEE 802.1X port authentication (UniFi's 802.1X Control) -- docs/
    drivers/sonic.md 11.26.

    The driver holds a controlled port closed in CONFIG_DB (learn_mode drop:
    no source is learnt or forwarded by the chip, from boot) and lists it in
    this daemon's config (dot1x.ports: interface -> control mode). For each
    port in auto, multi_host or mac_based this daemon runs a hostapd of its
    own (wired driver, a RADIUS client and nothing else; built on the switch
    by the driver, as mstpd is) and installs the EAPOL trap. hostapd relays
    the host's EAP to the RADIUS servers and reports the outcome on its
    control socket:

    - AP-STA-CONNECTED <mac>: the host is admitted -- a static FDB entry in
      every VLAN the port carries (APPL_DB FDB_TABLE, as the MAC filter
      writes its allowed MACs) and a RETURN for the source in the kernel
      chain DUI_DOT1X. The controller's modes, as a UniFi switch is
      understood to run them (sonic.md 11.26.9): auto ("Credentials") is
      port-based -- once one host has authenticated, the port is open to
      every host (the chain RETURNs the whole port and its learning is
      hardware in APPL_DB), until the last one logs off or the link drops,
      when the port closes again and forgets what it learnt; multi_host
      ("Multiple Credentials") and mac_based authenticate each host on its
      own, so only the admitted ones pass;
    - AP-STA-DISCONNECTED <mac>: the admission goes;
    - CTRL-EVENT-EAP-FAILURE2 <mac>: a failed attempt, counted for the port
      (STATE_DB DUI_DOT1X_TABLE|<port> failures, since boot; the anomaly 128
      the driver reports).

    The chain DUI_DOT1X (from INPUT and FORWARD, ahead of MAC locking's)
    accepts the port's EAPOL, passes the admitted sources and drops the rest
    of the port's frames, so an unauthorised host cannot reach the switch's
    own CPU through a trapped frame (ARP, DHCP, IGMP) either. A port leaving
    the config, or losing its link, loses its admissions."""

    def __init__(self, l2d):
        self.l2d = l2d
        self.db = l2d.db
        self.fdb_pst = sw.ProducerStateTable(self.db.appl, "FDB_TABLE")
        self.lag_pst = sw.ProducerStateTable(self.db.appl, "LAG_TABLE")
        self.st = sw.Table(self.db.state, DOT1X_TABLE)
        self.want = {}  # interface -> control mode
        self.servers = []  # [(ip, port, secret)]
        self.nas_ip = ""
        self.own = ""
        self.started = {}  # interface -> the hostapd config text it was started with
        self.last_start = {}  # interface -> time of the last start
        self.socks = {}  # interface -> event socket (attached)
        self.admitted = {}  # interface -> set of MACs
        self.fdb = {}  # interface -> set of FDB_TABLE keys written
        self.failures = {}  # interface -> count, carried over a restart
        self.last_fail = {}
        self.dirty = set()
        self.written = None  # chain rules as last written
        # interface -> MACs an l2d before this one admitted: their FDB
        # entries stay until the port's running hostapd says whether it
        # still authorises them (attach), or the port is gone.
        self.stale = {}
        self.stale_mab = {}  # of those, the ones admitted by their MAC address
        # (interface, mac) -> when an l2d before this one last saw that
        # MAC-authenticated host (the row's mab_seen, written as it stopped;
        # after a crash its mab_heard, with a grace of MAB_HEARD_STEP), so
        # its idle timer carries on from there.
        self.stale_seen = {}
        self.stale_grace = {}
        # MAC authentication (mac_based ports, 11.26.8)
        self.idle = {}  # interface -> idle timeout (s) of a MAC-authenticated host; 0 = none
        self.mab = {}  # interface -> MACs admitted by their MAC address
        self.mab_socks = {}  # interface -> packet socket reading its trapped frames
        self.learn = {}  # interface -> the learn mode this daemon set in APPL_DB (cpu_trap, or hardware for an open auto port)
        self.learn_adopted = False  # an earlier l2d's learn modes taken over (the first sync)
        self.auth_ports, self.mab_ports = set(), set()
        self.flusher = None
        self.seen = {}  # (interface, mac) -> last frame seen from a MAC-authenticated host
        # (interface, mac) -> seconds added to the idle timeout for a host
        # kept over a crash, until its next frame (its mab_heard may be up
        # to MAB_HEARD_STEP behind its last frame)
        self.grace = {}
        self.heard = {}  # interface -> {mac: time} as last written to the row's mab_heard
        self.quiet = {}  # (interface, mac) -> until when a refused or unanswered source is left alone
        self.pending = {}  # RADIUS identifier -> the request in flight
        self.inflight = {}  # (interface, mac) -> RADIUS identifier
        self.next_id = 0
        self.rsocks = {}  # address family -> the RADIUS client's UDP socket
        self.noanswer = 0.0  # last time "no RADIUS server answered" was logged
        for k in self.db.keys(self.db.state, DOT1X_TABLE + "|*"):
            row = self.db.hgetall(self.db.state, k)
            port = k.split("|", 1)[1]
            try:
                self.failures[port] = int(row.get("failures", "0"))
            except ValueError:
                pass
            self.last_fail[port] = row.get("last_failed_mac", "")
            macs = set(splitlist(row.get("authorized", "")))
            if macs:
                self.stale[port] = macs
                mab = set(splitlist(row.get("mab", ""))) & macs
                if mab:
                    self.stale_mab[port] = mab
            heard = parse_seen(row.get("mab_heard", ""))
            if heard:
                self.heard[port] = heard
            if "mab_seen" in row:  # the l2d before stopped: its hosts' last frames as they were
                for mac, t in parse_seen(row["mab_seen"]).items():
                    self.stale_seen[(port, mac)] = t
                self.st.hdel(port, "mab_seen")  # read once: a later crash must not find it stale
            else:  # it died without stopping: what it wrote as it ran, at most MAB_HEARD_STEP behind
                for mac, t in heard.items():
                    self.stale_seen[(port, mac)] = t
                    self.stale_grace[(port, mac)] = MAB_HEARD_STEP

    # --- configuration -----------------------------------------------------

    def fdb_keys(self, port, mac):
        return ["%s:%s" % (v, mac) for v in self.vlans_of(port)]

    def vlans_of(self, port):
        out = []
        for k in self.db.keys(self.db.cfg, "VLAN_MEMBER|Vlan*|" + port):
            out.append(k.split("|")[1])
        return sorted(out)

    def secrets(self):
        try:
            with open(DOT1X_SECRET) as f:
                return [line.rstrip("\n") for line in f]
        except OSError:
            return []

    def hostapd_conf(self, port):
        lines = [
            "# disunified-l2d: 802.1X authenticator for %s (docs/drivers/sonic.md 11.26)" % port,
            "interface=%s" % port,
            "driver=wired",
            "ctrl_interface=%s/ctrl" % DOT1X_RUN,
            "logger_syslog=-1",
            "logger_syslog_level=2",
            "ieee8021x=1",
            "eapol_version=2",
            "eap_server=0",
            "use_pae_group_addr=1",
            "eap_reauth_period=3600",
            "nas_identifier=disunified",
        ]
        if self.nas_ip:
            lines.append("own_ip_addr=%s" % self.nas_ip)
        for ip, port_no, secret in self.servers:
            lines += ["auth_server_addr=%s" % ip, "auth_server_port=%d" % port_no,
                      "auth_server_shared_secret=%s" % secret]
        return "\n".join(lines) + "\n"

    def sync(self, conf):
        c = conf.get("dot1x") or {}
        want = {p: m for p, m in (c.get("ports") or {}).items()
                if m != "force_authorized" and self.db.known(p) and not p.startswith("PortChannel")}
        if not self.own:
            self.own = (self.db.hgetall(self.db.cfg, "DEVICE_METADATA|localhost").get("mac") or "").lower()
        secrets = self.secrets()
        servers = []
        for i, s in enumerate(c.get("servers") or []):
            if i < len(secrets) and s.get("ip"):
                servers.append((s["ip"], int(s.get("port") or 1812), secrets[i]))
        if c.get("servers") and not servers:
            log("802.1X: no RADIUS secret on the switch (%s); no authenticator started" % DOT1X_SECRET,
                syslog.LOG_WARNING)
        self.servers, self.nas_ip = servers, c.get("nas_ip") or ""
        self.idle = {}
        for p, t in (c.get("idle_timeout") or {}).items():
            try:
                self.idle[p] = max(0, int(t))
            except (TypeError, ValueError):
                pass
        mab = {p for p, m in want.items() if m == "mac_based" and servers}
        # A port that stops authenticating by MAC, or stops being an open
        # Credentials port, gets its CONFIG_DB learn mode back first (the
        # kernel's drop is still there), then loses its hosts.
        self.auth_ports = {p for p, m in want.items() if m in DOT1X_AUTH_MODES and servers}
        self.mab_ports = mab
        # a port leaving the config: its row keeps its count, not its mode
        self.dirty |= set(self.want) - set(want)
        self.want = {p: m for p, m in self.want.items() if p in want}
        self.learn_sync(close_only=True)
        for port in sorted(set(self.mab) - mab):
            for mac in sorted(self.mab.get(port, ())):
                self.revoke(port, mac, "no longer authenticated by MAC address")
            self.mab.pop(port, None)
        # A port leaving the config: its authenticator stops, its admissions go.
        for port in sorted(set(self.started) - {p for p, m in want.items() if m in DOT1X_AUTH_MODES and servers}):
            self.stop(port)
        for port in sorted(set(self.admitted) - set(want)):
            self.revoke_all(port, "no longer controlled")
        self.want = want
        # A restart: the learn modes an earlier l2d set on the ports it
        # controlled (CONFIG_DB holds them at drop) are this daemon's to keep
        # or undo -- judged once the ports' modes are known, so a port that
        # stays as it was gets no write.
        if not self.learn_adopted:
            self.learn_adopted = True
            for p in sorted(want):
                have = self.db.hgetall(self.db.appl, "PORT_TABLE:" + p).get("learn_mode")
                if p not in self.learn and have in (LEARN_TRAP, LEARN_HARDWARE) and self.configured_mode(p) == "drop":
                    self.learn[p] = have
        auth = sorted(p for p, m in want.items() if m in DOT1X_AUTH_MODES and servers)
        for port in sorted(set(self.stale) - set(auth)):
            self.drop_stale(port, set())
        # An earlier l2d's admissions (a restart) are settled before this
        # daemon first writes the chain, so that write already holds the
        # hosts kept and none it drops: a port without link loses them, a
        # MAC-authenticated host is kept with its idle time; the 802.1X
        # hosts wait for the port's hostapd (attach).
        for port in auth:
            if not self.carrier(port) and self.stale.get(port):
                self.drop_stale(port, set(), "link down")
            self.adopt_mab(port)
        self.learn_sync(close_only=True)  # what they held open in the chip closes first
        # The chain before any authenticator runs: nothing is admitted
        # through the kernel that the chip would not admit either.
        self.write_chain(self.rules())
        self.copp(bool(auth))
        for port in auth:
            if not self.carrier(port) and self.admitted.get(port):
                self.revoke_all(port, "link down")
            self.ensure(port)
        # MAC authentication: an unknown source's frames go to the CPU
        # (cpu_trap, MAC locking's mode; the chain above drops them in the
        # kernel), where the first one starts a request. An open
        # Credentials port learns in hardware. Only with the chain in place.
        if self.written is not None:
            self.learn_sync()
        self.sync_mab_socks(mab)
        self.mab_tick(time.time())
        self.publish()

    # --- MAC authentication ----------------------------------------------------

    def configured_mode(self, port):
        return self.db.hgetall(self.db.cfg, "PORT|" + port).get("learn_mode") or "hardware"

    def learn_wanted(self):
        """The learn mode each port should have in APPL_DB: cpu_trap where
        hosts authenticate by MAC address, hardware on a Credentials port
        some host has opened; a port MAC locking watches is its."""
        maclock = getattr(self.l2d, "maclock", None)
        out = {}
        for p in self.auth_ports:
            if maclock and p in maclock.want:
                continue
            if p in self.mab_ports:
                out[p] = LEARN_TRAP
            elif self.want.get(p) == "auto" and self.holders(p):
                out[p] = LEARN_HARDWARE
        return out

    def holders(self, port):
        """The hosts admitted on the port, with an earlier l2d's admissions
        not settled yet (stale): over a restart they keep what they had --
        their RETURN, an open Credentials port -- until the port's hostapd
        (attach) or its idle timer says otherwise."""
        return self.admitted.get(port, set()) | self.stale.get(port, set())

    def learn_sync(self, close_only=False):
        want = self.learn_wanted()
        for p in sorted(self.learn):
            if want.get(p) == self.learn[p]:
                continue
            prev = self.learn.pop(p)
            maclock = getattr(self.l2d, "maclock", None)
            if not (maclock and p in maclock.want) and self.db.known(p):
                put(self.db.ifport_pst, p, [("learn_mode", self.configured_mode(p))])
            if prev == LEARN_HARDWARE:
                self.flush(p)  # what it learnt while open goes with it
                log("802.1X: %s closed again (no host authenticated); its learnt addresses flushed" % p)
        if close_only:
            return
        for p, mode in sorted(want.items()):
            if self.db.hgetall(self.db.appl, "PORT_TABLE:" + p).get("learn_mode") != mode:
                put(self.db.ifport_pst, p, [("learn_mode", mode)])
            if self.learn.get(p) != mode:
                self.learn[p] = mode
                if mode == LEARN_TRAP:
                    log("802.1X: %s authenticates by MAC address (unknown sources trapped to the CPU)" % p)
                else:
                    log("802.1X: %s open to every host (Credentials: %s authenticated)" %
                        (p, ",".join(sorted(self.holders(p)))))

    def flush(self, port):
        """The port's dynamic FDB entries, through fdborch's flush request
        (op PORT: dynamic entries only; the static ones stay)."""
        try:
            if self.flusher is None:
                self.flusher = sw.NotificationProducer(self.db.appl, "FLUSHFDBREQUEST")
            self.flusher.send("PORT", port, sw.FieldValuePairs([]))
        except Exception as e:  # the next aging pass forgets them anyway
            log("802.1X: FDB flush of %s failed: %s" % (port, e), syslog.LOG_WARNING)

    def sync_mab_socks(self, ports):
        for port in list(self.mab_socks):
            if port not in ports:
                self.mab_socks.pop(port).close()
        for port in sorted(ports):
            if port in self.mab_socks or not os.path.isdir("%s/%s" % (SYS, port)):
                continue
            try:
                s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(ETH_P_ALL))
                s.bind((port, 0))
                s.setblocking(False)
            except OSError as e:
                log("802.1X: no packet socket on %s: %s" % (port, e), syslog.LOG_WARNING)
                continue
            self.mab_socks[port] = s

    def rsock(self, ip):
        fam = socket.AF_INET6 if ":" in ip else socket.AF_INET
        s = self.rsocks.get(fam)
        if s is None:
            s = socket.socket(fam, socket.SOCK_DGRAM)
            s.setblocking(False)
            self.rsocks[fam] = s
        return s

    def drain_frames(self, port, s):
        """Each inbound frame from a unicast source that is neither the
        switch, nor admitted, nor waiting on an answer, nor quiet starts a
        request; an admitted MAC-authenticated host's frames mark it
        active."""
        now = time.time()
        for _ in range(256):
            try:
                data, addr = s.recvfrom(64)
            except (BlockingIOError, InterruptedError):
                return
            except OSError:
                return
            if addr[2] == PACKET_OUTGOING or len(data) < 14 or bytearray(data)[6] & 1:
                continue
            src = mac_text(data[6:12])
            if src == self.own:
                continue
            etype = struct.unpack("!H", data[12:14])[0]
            if etype == ETH_P_8021Q and len(data) >= 18:
                etype = struct.unpack("!H", data[16:18])[0]
            if etype == ETH_P_PAE:
                continue  # an 802.1X host: hostapd's
            key = (port, src)
            if src in self.admitted.get(port, ()):
                if src in self.mab.get(port, ()):
                    self.mark_seen(port, src, now)
                continue
            if key in self.inflight or self.quiet.get(key, 0) > now or len(self.pending) >= MAB_PENDING:
                continue
            self.mab_start(port, src, now)

    def mab_start(self, port, mac, now):
        if not self.servers:
            return
        for _ in range(256):
            ident, self.next_id = self.next_id, (self.next_id + 1) % 256
            if ident not in self.pending:
                break
        else:
            return
        self.pending[ident] = {"port": port, "mac": mac, "server": 0, "tries": 0, "due": now, "auth": b""}
        self.inflight[(port, mac)] = ident
        log("802.1X: %s on %s sent no EAPOL; asking the RADIUS server about its MAC address" % (mac, port))
        self.mab_send(ident, now)

    def mab_send(self, ident, now):
        req = self.pending[ident]
        if req["tries"] >= MAB_TRIES:
            req["server"], req["tries"], req["auth"] = req["server"] + 1, 0, b""
        if req["server"] >= len(self.servers):
            self.mab_done(ident)
            self.quiet[(req["port"], req["mac"])] = now + MAB_QUIET
            if now - self.noanswer > MAB_QUIET:
                self.noanswer = now
                log("802.1X: no RADIUS server answered for %s on %s; tried again in %ds" %
                    (req["mac"], req["port"], MAB_QUIET), syslog.LOG_WARNING)
            return
        ip, port_no, secret = self.servers[req["server"]]
        if not req["auth"]:
            req["auth"] = os.urandom(16)
        pkt = mab_request(ident, req["auth"], req["mac"], secret.encode(), self.nas_ip, req["port"], self.own)
        try:
            self.rsock(ip).sendto(pkt, (ip, port_no))
        except OSError as e:
            log("802.1X: RADIUS request to %s:%d not sent: %s" % (ip, port_no, e), syslog.LOG_WARNING)
        req["tries"] += 1
        req["due"] = now + MAB_TIMEOUT

    def mab_done(self, ident):
        req = self.pending.pop(ident, None)
        if req:
            self.inflight.pop((req["port"], req["mac"]), None)
        return req

    def drain_radius(self, s):
        now = time.time()
        for _ in range(64):
            try:
                data, addr = s.recvfrom(4096)
            except (BlockingIOError, InterruptedError, OSError):
                break
            if len(data) < 20:
                continue
            req = self.pending.get(bytearray(data)[1])
            if req is None or req["server"] >= len(self.servers):
                continue
            ip, port_no, secret = self.servers[req["server"]]
            answer = radius_answer(data, bytearray(data)[1], req["auth"], secret.encode())
            if answer is None:
                log("802.1X: a reply from %s that is not a signed answer to the request for %s; ignored" %
                    (addr[0], req["mac"]), syslog.LOG_WARNING)
                continue
            self.mab_done(bytearray(data)[1])
            port, mac = req["port"], req["mac"]
            if self.want.get(port) != "mac_based":
                continue
            if answer == "accept":
                self.admit(port, mac, "by its MAC address")
                self.mab.setdefault(port, set()).add(mac)
                self.mark_seen(port, mac, now)
            else:
                self.failures[port] = self.failures.get(port, 0) + 1
                self.last_fail[port] = mac
                self.quiet[(port, mac)] = now + MAB_QUIET
                self.dirty.add(port)
                log("802.1X: %s refused by its MAC address on %s (%d failures on this port since boot)" %
                    (mac, port, self.failures[port]), syslog.LOG_WARNING)
        self.publish()

    def mab_tick(self, now):
        for ident in sorted(self.pending):
            if ident in self.pending and now >= self.pending[ident]["due"]:
                self.mab_send(ident, now)
        for port in sorted(self.mab):
            t = self.idle.get(port, 0)
            if t <= 0:
                continue
            for mac in sorted(self.mab[port]):
                if now - self.seen.get((port, mac), now) > t + self.grace.get((port, mac), 0):
                    self.revoke(port, mac, "idle for %ds" % t)
        for key in [k for k, until in self.quiet.items() if until <= now]:
            del self.quiet[key]
        self.write_heard()

    def mark_seen(self, port, mac, now):
        """A frame from a MAC-authenticated host: its idle timer starts again,
        and the row's mab_heard is written once it is MAB_HEARD_STEP behind
        (so it never is more than that, whenever l2d dies)."""
        key = (port, mac)
        self.seen[key] = now
        self.grace.pop(key, None)
        if now - self.heard.get(port, {}).get(mac, float("-inf")) >= MAB_HEARD_STEP:
            self.write_heard(port)

    def write_heard(self, port=None):
        """The row's mab_heard: each MAC-authenticated host's last frame,
        rewritten for one port (a frame or an admission) or, every pass, for
        each port whose hosts changed; removed with the port's last host."""
        for p in [port] if port else sorted(set(self.mab) | set(self.heard)):
            cur = {m: self.seen[(p, m)] for m in self.mab.get(p, ()) if (p, m) in self.seen}
            have = self.heard.get(p)
            if port is None and have is not None and set(have) == set(cur):
                continue
            if cur:
                put(self.st, p, [("mab_heard", format_seen(cur))])
                self.heard[p] = cur
            elif have is not None:
                self.st.hdel(p, "mab_heard")
                self.heard.pop(p, None)

    def carrier(self, port):
        return rd("%s/%s/carrier" % (SYS, port), "0") == "1"

    def copp(self, on):
        have = self.db.hgetall(self.db.cfg, "COPP_TRAP|" + COPP_EAPOL_KEY)
        if on and have != dict(COPP_EAPOL):
            put(self.db.copp_group, COPP_GROUP_NAME, copp_group())  # its leafref first
            put(self.db.copp, COPP_EAPOL_KEY, COPP_EAPOL)
            log("802.1X: EAPOL trapped to the CPU")
        elif not on and have:
            self.db.copp._del(COPP_EAPOL_KEY)
            log("802.1X: EAPOL trap removed")

    # --- the authenticators --------------------------------------------------

    def pidfile(self, port):
        return "%s/%s.pid" % (DOT1X_RUN, port)

    def pid(self, port):
        try:
            pid = int(rd(self.pidfile(port)))
        except ValueError:
            return 0
        cmd = rd("%s/%d/cmdline" % (PROC, pid))
        return pid if HOSTAPD in cmd and ("%s.conf" % port) in cmd else 0

    def ensure(self, port):
        text = self.hostapd_conf(port)
        pid = self.pid(port)
        path = "%s/%s.conf" % (DOT1X_RUN, port)
        if pid and self.started.get(port) is None and rd(path) + "\n" == text:
            self.started[port] = text  # adopted from an l2d before this one
        if pid and self.started.get(port) == text:
            if port not in self.socks:
                self.attach(port)
            return
        if pid:
            self.stop(port)  # its settings changed
        now = time.time()
        if now - self.last_start.get(port, 0) < DOT1X_RESTART_EVERY:
            return
        self.last_start[port] = now
        if not os.access(HOSTAPD, os.X_OK):
            return
        os.makedirs(DOT1X_RUN + "/ctrl", mode=0o700, exist_ok=True)
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            f.write(text)
        p = run([HOSTAPD, "-B", "-P", self.pidfile(port), path], check=True)
        if p.returncode == 0:
            self.started[port] = text
            log("802.1X: authenticator started on %s (%s, %d RADIUS servers)" %
                (port, self.want.get(port), len(self.servers)))
            self.attach(port)

    def stop(self, port):
        self.detach(port)
        pid = self.pid(port)
        if pid:
            try:
                os.kill(pid, signal.SIGTERM)
            except OSError:
                pass
            log("802.1X: authenticator on %s stopped" % port)
        self.started.pop(port, None)
        for path in (self.pidfile(port), "%s/%s.conf" % (DOT1X_RUN, port)):
            try:
                os.unlink(path)
            except OSError:
                pass

    def ctrl(self, port, cmd):
        """One request on hostapd's control socket, its reply ("" on failure)."""
        me = "%s/req-%s" % (DOT1X_RUN, port)
        s = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
        try:
            try:
                os.unlink(me)
            except OSError:
                pass
            s.bind(me)
            s.settimeout(2.0)
            s.connect("%s/ctrl/%s" % (DOT1X_RUN, port))
            s.send(cmd.encode())
            return s.recv(8192).decode(errors="replace")
        except OSError:
            return ""
        finally:
            s.close()
            try:
                os.unlink(me)
            except OSError:
                pass

    def attach(self, port):
        me = "%s/ev-%s" % (DOT1X_RUN, port)
        s = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
        try:
            try:
                os.unlink(me)
            except OSError:
                pass
            s.bind(me)
            s.connect("%s/ctrl/%s" % (DOT1X_RUN, port))
            s.settimeout(2.0)
            s.send(b"ATTACH")
            if not s.recv(64).startswith(b"OK"):
                raise OSError("ATTACH refused")
            s.setblocking(False)
        except OSError as e:
            s.close()
            log("802.1X: no events from %s's authenticator yet: %s" % (port, e), syslog.LOG_WARNING)
            return
        self.socks[port] = s
        # Who hostapd already authorised (an authenticator adopted after a
        # restart of this daemon): admitted again.
        have = set()
        up = self.carrier(port)
        reply = self.ctrl(port, "STA-FIRST")
        while reply and reply != "FAIL\n" and len(have) < 4096:
            mac = reply.split("\n", 1)[0].strip().lower()
            if not mac or mac in have:
                break
            have.add(mac)
            if "[AUTHORIZED]" in reply and mac != self.own:
                if up:
                    self.admit(port, mac)
                else:  # the link went while no l2d watched: as a running one does
                    self.ctrl(port, "DEAUTHENTICATE " + mac)
            reply = self.ctrl(port, "STA-NEXT " + mac)
        self.adopt_mab(port)
        self.drop_stale(port, self.admitted.get(port, set()))

    def adopt_mab(self, port):
        """An earlier l2d's MAC-authenticated hosts on the port (they are not
        hostapd's): kept while the port still authenticates by MAC and has
        its link, each with the idle time it had when that l2d stopped; one
        idle for longer than the port's timeout goes. Every host is decided
        before any is admitted, so no chain write holds one that goes."""
        macs = self.stale_mab.pop(port, set()) & self.stale.get(port, set())
        if not macs:
            return
        now = time.time()
        t = self.idle.get(port, 0)
        if self.want.get(port) != "mac_based" or port not in self.mab_ports or not self.carrier(port):
            for mac in macs:
                self.stale_seen.pop((port, mac), None)
            return  # left to drop_stale (a link down, or hostapd's list)
        keep, gone = {}, []
        grace = {}
        for mac in sorted(macs):
            seen = self.stale_seen.pop((port, mac), now)
            grace[mac] = self.stale_grace.pop((port, mac), 0)
            if t > 0 and now - seen > t + grace[mac]:
                gone.append(mac)
            else:
                keep[mac] = min(seen, now)
        for mac in gone:
            self.stale[port].discard(mac)
            self.drop_stale_mac(port, mac, "idle for %ds, over a restart" % t)
        if gone and self.written is not None:
            self.write_chain(self.rules())
        for mac, seen in sorted(keep.items()):
            self.admit(port, mac, "by its MAC address, kept over a restart")
            self.stale[port].discard(mac)
            self.mab.setdefault(port, set()).add(mac)
            self.seen[(port, mac)] = seen
            if grace[mac]:
                self.grace[(port, mac)] = grace[mac]
        if not self.stale.get(port):
            self.stale.pop(port, None)

    def drop_stale(self, port, keep, why="not authorised after a restart"):
        """An earlier l2d's admissions on the port that hostapd no longer
        authorises: their FDB entries go, then what they held open (the
        chain's RETURN, a Credentials port's learning)."""
        gone = sorted(self.stale.pop(port, set()) - keep)
        for mac in gone:
            self.drop_stale_mac(port, mac, why)
        if gone and self.written is not None:  # before the first write, that write follows
            self.learn_sync(close_only=True)
            self.write_chain(self.rules())

    def drop_stale_mac(self, port, mac, why):
        for key in self.fdb_keys(port, mac):
            self.fdb_pst._del(key)
        self.stale_seen.pop((port, mac), None)
        log("802.1X: %s no longer authorised on %s (%s)" % (mac, port, why))
        self.dirty.add(port)

    def detach(self, port):
        s = self.socks.pop(port, None)
        if s is not None:
            s.close()
            try:
                os.unlink("%s/ev-%s" % (DOT1X_RUN, port))
            except OSError:
                pass

    # --- admissions ------------------------------------------------------------

    def admit(self, port, mac, how=""):
        if mac == self.own:
            return  # the switch's own frames, looped back to it
        macs = self.admitted.setdefault(port, set())
        new = mac not in macs
        macs.add(mac)
        keys = self.fdb.setdefault(port, set())
        for key in self.fdb_keys(port, mac):
            if key not in keys:
                # one an earlier l2d wrote stays as it is (a restart rewrites nothing)
                if self.db.hgetall(self.db.appl, "FDB_TABLE:" + key) != {"port": port, "type": "static"}:
                    put(self.fdb_pst, key, [("port", port), ("type", "static")])
                keys.add(key)
        self.write_chain(self.rules())
        self.dirty.add(port)
        if new:
            log("802.1X: %s authorised on %s%s" % (mac, port, " " + how if how else ""))
        if self.written is not None:
            self.learn_sync()  # a Credentials port opens with its first host

    def revoke(self, port, mac, why):
        macs = self.admitted.get(port, set())
        if mac not in macs:
            return
        macs.discard(mac)
        self.mab.get(port, set()).discard(mac)
        self.seen.pop((port, mac), None)
        self.grace.pop((port, mac), None)
        keys = self.fdb.get(port, set())
        for key in self.fdb_keys(port, mac):
            if key in keys:
                self.fdb_pst._del(key)
                keys.discard(key)
        self.learn_sync(close_only=True)  # a Credentials port closes with its last host
        self.write_chain(self.rules())
        self.dirty.add(port)
        log("802.1X: %s no longer authorised on %s (%s)" % (mac, port, why))

    def revoke_all(self, port, why):
        for mac in sorted(self.admitted.get(port, set())):
            self.revoke(port, mac, why)
            if self.started.get(port):
                self.ctrl(port, "DEAUTHENTICATE " + mac)
        self.admitted.pop(port, None)
        self.dirty.add(port)

    def rules(self):
        out = []
        for port in sorted(self.want):
            out += eapol_rules(port)
            if self.want[port] == "auto" and port in self.auth_ports and self.holders(port):
                out.append(["-i", port, "-j", "RETURN"])  # opened by a host: every host passes
                continue
            for mac in sorted(self.holders(port)):
                out.append(["-i", port, "-s", mac, "-j", "RETURN"])
            out.append(["-i", port, "-j", "DROP"])
        return out

    def write_chain(self, rules):
        if rules == self.written == []:
            return
        exists, count, jumps = eb_state(DOT1X_CHAIN)
        if rules and rules == self.written and count == len(rules) and jumps == {"INPUT", "FORWARD"}:
            return
        if rules and rules == self.written:
            log("802.1X: kernel bridge rules were removed or changed under this daemon; written again",
                syslog.LOG_WARNING)
        if not rules:
            ok = True
            if exists or jumps:
                ok = eb_restore(eb_unchain(DOT1X_CHAIN, jumps, exists))
                if ok and exists:
                    log("802.1X: kernel bridge rules removed")
            self.written = [] if ok else None
            return
        # One transaction (11.26.12): an admission, a revocation or a start
        # never leaves a closed port's DROP out of the chain, as a flush and
        # one append per rule did for ~20 ms; the jumps go first, ahead of
        # MAC locking's chain. A refused batch leaves the chain as it was.
        ok = eb_restore(eb_chain(DOT1X_CHAIN, rules, jumps, True))
        self.written = rules if ok else None

    def publish(self):
        for port in sorted(self.dirty | set(self.want)):
            row = [("mode", self.want.get(port, "")),
                   ("authorized", ",".join(sorted(self.admitted.get(port, ())))),
                   ("failures", str(self.failures.get(port, 0))),
                   ("last_failed_mac", self.last_fail.get(port, "")),
                   ("mab", ",".join(sorted(self.mab.get(port, ()))))]
            have = self.db.hgetall(self.db.state, DOT1X_TABLE + "|" + port)
            if any(have.get(k) != v for k, v in row):  # mab_heard is write_heard's
                put(self.st, port, row)
        self.dirty.clear()

    # --- events ----------------------------------------------------------------

    def fds(self):
        return [s.fileno() for s in list(self.socks.values()) + list(self.mab_socks.values()) +
                list(self.rsocks.values())]

    def readable(self, fd):
        for port, s in list(self.socks.items()):
            if s.fileno() == fd:
                self.drain(port, s)
                return
        for port, s in list(self.mab_socks.items()):
            if s.fileno() == fd:
                self.drain_frames(port, s)
                return
        for s in list(self.rsocks.values()):
            if s.fileno() == fd:
                self.drain_radius(s)
                return

    def drain(self, port, s):
        for _ in range(64):
            try:
                msg = s.recv(4096).decode(errors="replace").strip()
            except (BlockingIOError, InterruptedError):
                break
            except OSError:
                self.detach(port)  # hostapd went away; ensure() restarts it
                break
            self.event(port, msg)
        self.publish()

    def event(self, port, msg):
        if msg.startswith("<") and ">" in msg:
            msg = msg.split(">", 1)[1]
        fields = msg.split()
        if len(fields) < 2:
            return
        ev, mac = fields[0], fields[1].lower()
        if ev == "AP-STA-CONNECTED":
            self.admit(port, mac)
        elif ev == "AP-STA-DISCONNECTED":
            self.revoke(port, mac, "logged off or timed out")
        elif ev == "CTRL-EVENT-EAP-FAILURE2" and mac != self.own:
            self.failures[port] = self.failures.get(port, 0) + 1
            self.last_fail[port] = mac
            self.dirty.add(port)
            log("802.1X: %s failed to authenticate on %s (%d on this port since boot)" %
                (mac, port, self.failures[port]), syslog.LOG_WARNING)

    def stopping(self):
        """This daemon is stopping (a restart): each MAC-authenticated host's
        last frame goes into its port's row (mab_seen), so the next l2d's
        idle timer carries on from it; hostapd, the chain and the FDB
        entries stay for that l2d to adopt."""
        for port in sorted(self.mab):
            seen = {m: self.seen[(port, m)] for m in self.mab[port] if (port, m) in self.seen}
            if seen:
                put(self.st, port, [("mab_seen", format_seen(seen))])

    def teardown(self):
        """Every authenticator stopped, every admission removed, the chain and
        the trap gone; the ports stay closed (CONFIG_DB keeps learn_mode drop).
        The counts stay: they are since boot."""
        self.auth_ports, self.mab_ports = set(), set()
        self.learn_sync(close_only=True)
        for s in list(self.mab_socks.values()) + list(self.rsocks.values()):
            s.close()
        self.mab_socks, self.rsocks, self.pending, self.inflight = {}, {}, {}, {}
        for port in sorted(set(self.admitted)):
            for mac in sorted(self.admitted.get(port, ())):
                for key in self.fdb_keys(port, mac):
                    self.fdb_pst._del(key)
        try:
            names = [n[:-4] for n in os.listdir(DOT1X_RUN) if n.endswith(".pid")]
        except OSError:
            names = []
        for port in sorted(set(names) | set(self.started)):
            self.stop(port)
        self.admitted, self.want = {}, {}
        self.written = None
        self.write_chain([])
        self.copp(False)
        for port in sorted(set(self.failures)):
            put(self.st, port, [("mode", ""), ("authorized", ""), ("failures", str(self.failures[port])),
                                ("last_failed_mac", self.last_fail.get(port, "")), ("mab", "")])
        self.mab = {}
        self.write_heard()


def splitlist(s):
    return [x for x in (s or "").split(",") if x]


def parse_seen(s):
    """mab_seen / mab_heard: <mac>=<epoch>,..."""
    out = {}
    for item in splitlist(s):
        mac, _, t = item.partition("=")
        try:
            out[mac] = float(t)
        except ValueError:
            pass
    return out


def format_seen(seen):
    return ",".join("%s=%.1f" % (m, seen[m]) for m in sorted(seen))


def teardown():
    syslog.openlog("disunified-l2d", syslog.LOG_PID, syslog.LOG_DAEMON)
    d = L2d()
    d.adopt_guard_state()
    d.stp_down("teardown")  # releases BPDU guard holds
    for p in list(d.blocked):
        d.unblock(p)
    d.dhcp.stop("teardown")
    for det in d.detectors:
        det.teardown()


def deadman():
    """The bridge's first enable was not confirmed in time: stop, tear down,
    and record it in the config so neither a restart nor a reboot brings
    spanning tree back before the controller turns it off and on."""
    run(["systemctl", "stop", "disunified-l2d"])
    teardown()
    try:
        with open(CONF) as f:
            c = json.load(f)
    except (OSError, ValueError):
        c = {}
    c.setdefault("stp", {})["enabled"] = False
    c["deadman"] = time.strftime("%Y-%m-%dT%H:%M:%S%z")
    tmp = CONF + ".tmp"
    with open(tmp, "w") as f:
        json.dump(c, f, indent=2)
        f.write("\n")
    os.replace(tmp, CONF)
    log("dead-man: spanning tree undone, config marked", syslog.LOG_WARNING)
    run(["systemctl", "start", "disunified-l2d"])  # loop protection, if configured, carries on


if __name__ == "__main__":
    if sys.argv[1:] == ["--teardown"]:
        teardown()
    elif sys.argv[1:] == ["--deadman"]:
        deadman()
    else:
        d = L2d()
        try:
            d.main()  # SIGTERM ends it with SystemExit
        finally:
            d.dot1x.stopping()
