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
#   gone are dropped from APPL_DB directly, never through the producer.
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
# Everything the bridge reads back is published in STATE_DB:
# DUI_STP_TABLE|Bridge, DUI_STP_PORT_TABLE|<port>, DUI_LOOP_GUARD_TABLE|<port>.
#
# `disunified-l2d --teardown` undoes it: every VLAN back to the default STP
# instance, every port forwarding in the kernel, stp_state 0, mstpd stopped,
# guarded ports restored. A stop of the service does NOT tear down (mstpd
# keeps running and the next start adopts it), so a restart never opens a
# window with no spanning tree.
import collections
import json
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


def run(args, check=False):
    p = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True)
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
    }


class DB(object):
    def __init__(self):
        self.appl = sw.DBConnector("APPL_DB", 0)
        self.cfg = sw.DBConnector("CONFIG_DB", 0)
        self.state = sw.DBConnector("STATE_DB", 0)
        self.asic = sw.DBConnector("ASIC_DB", 0)
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


def bridge_ports():
    try:
        return sorted(os.listdir("%s/%s/brif" % (SYS, BRIDGE)))
    except OSError:
        return []


def kstate(port):
    try:
        return int(rd("%s/%s/brport/state" % (SYS, port), "0"))
    except ValueError:
        return 0


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
        return run(["pidof", "mstpd"]).returncode == 0

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
        self.written = {}  # (port, vid) -> stporch state in APPL_DB
        self.mapped = set()  # vids whose VLAN is in its own instance
        self.flush_due = {}  # port -> time a flush is due (coalesced)
        self.root_port = ""
        self.gw_port = ""
        self.fdb_port = {}  # (vlan, mac) -> port
        self.moves = collections.defaultdict(collections.deque)
        self.blocked = {}  # port -> unblock time
        self.fdb_sub = None
        self.nl = None

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
        for p in self.ports:
            st = kstate(p)
            self.ports[p] = st
            self.write_state(p, st, vids)
        if time.time() >= self.settle:
            for vid in sorted(vids - self.mapped):
                self.map_vlan(vid)
        for vid in self.mapped:
            if not self.db.hgetall(self.db.appl, "STP_VLAN_INSTANCE_TABLE:Vlan%d" % vid):
                put(self.db.vlan_pst, "Vlan%d" % vid, [("stp_instance", str(vid))])

    def pending_vlan_delete(self, vids):
        return any(v not in vids for v in self.mapped)

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
        vids = self.mapped | {v for (_, v) in self.written}
        if state != self.ports[name]:
            log("%s: %s -> %s" % (name, KNAME.get(self.ports[name], "?"), KNAME.get(state, "?")))
            self.ports[name] = state
            self.write_state(name, state, vids)
        else:
            # same state: a bridge-port sysfs write, i.e. mstpd's FDB flush
            self.flush_due.setdefault(name, time.time() + 0.02)

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
            put(self.db.st_port, p, [(f.replace("-", "_"), str(d.get(f, ""))) for f in (
                "role", "state", "external-port-cost", "admin-edge-port", "auto-edge-port",
                "oper-edge-port", "bpdu-guard-port", "bpdu-guard-error", "bpdu-filter-port",
                "num-rx-bpdu", "num-tx-bpdu", "num-rx-tcn", "num-transition-fwd",
                "num-transition-blk", "point-to-point")])

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
        for p in (self.db.lag_members(port) if port.startswith("PortChannel") else [port]):
            put(self.db.ifport_pst, p, [("admin_status", "down")])
        self.blocked[port] = until
        put(self.db.st_guard, port, [("state", "blocked"), ("until", "%d" % until), ("vlan", vlan),
                                    ("mac", mac), ("peer", peer)])
        log("loop protection: %s down for %ds (%s %s flapping with %s)" % (port, hold, vlan, mac, peer),
            syslog.LOG_WARNING)

    def unblock(self, port):
        for p in (self.db.lag_members(port) if port.startswith("PortChannel") else [port]):
            admin = self.db.hgetall(self.db.cfg, "PORT|" + p).get("admin_status", "down")
            put(self.db.ifport_pst, p, [("admin_status", admin)])
        self.blocked.pop(port, None)
        self.db.st_guard._del(port)
        log("loop protection: %s restored" % port)

    def guard_tick(self):
        now = time.time()
        for p, t in list(self.blocked.items()):
            if t <= now or not self.conf["loop_guard"]["enabled"]:
                self.unblock(p)

    def adopt_guard_state(self):
        for k in self.db.keys(self.db.state, "DUI_LOOP_GUARD_TABLE|*"):
            p = k.split("|", 1)[1]
            try:
                self.blocked[p] = float(self.db.hgetall(self.db.state, k).get("until", "0"))
            except ValueError:
                self.blocked[p] = 0

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
        elif self.stp_on and not self.mstp.running():
            log("mstpd died: restarting it", syslog.LOG_ERR)
            self.stp_on = self.stp_up()
        if self.stp_on:
            self.sync_stp()
            self.publish()
        self.gw_port = self.gateway_port()
        self.guard_tick()

    def main(self):
        syslog.openlog("disunified-l2d", syslog.LOG_PID, syslog.LOG_DAEMON)
        signal.signal(signal.SIGHUP, lambda *_: setattr(self, "reload", True))
        signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
        while not os.path.isdir("%s/%s/brif" % (SYS, BRIDGE)) or \
                not self.db.hgetall(self.db.appl, "PORT_TABLE:PortInitDone"):
            time.sleep(1)
        self.adopt_guard_state()
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
            r, _, _ = select.select([self.nl, self.fdb_sub.getFd()], [], [], 0.02)
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


def teardown():
    syslog.openlog("disunified-l2d", syslog.LOG_PID, syslog.LOG_DAEMON)
    d = L2d()
    d.stp_down("teardown")
    d.adopt_guard_state()
    for p in list(d.blocked):
        d.unblock(p)


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
        L2d().main()
