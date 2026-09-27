#!/bin/bash
# disunified Podman collector. Runs on the host over SSH, once per poll,
# and prints tagged sections ("@@@ <name>" lines) the Go side parses.
# Everything is read-only. $1 = the uplink NIC to present (default: the
# NIC carrying the default route); $2 = the slot file (see the driver).
# Every command below exists on a rootful Podman 5 host (AlmaLinux 10 /
# any systemd distro with iproute2 and python3); a missing optional tool
# (ethtool) leaves its section empty.
#
# Container records are reduced to the fields the driver reads before
# they leave the host: `podman inspect` carries every environment variable
# of every container, and that is where secrets live. Nothing below prints
# Config.Env, Args, Cmd or mounts.
s() { echo "@@@ $1"; }

s hostname;   hostname
s osrelease;  cat /etc/os-release 2>/dev/null
s podman;     podman version --format '{{.Client.Version}}' 2>/dev/null
s dmi;        for f in sys_vendor product_name product_serial product_uuid board_serial bios_version; do echo "$f=$(cat /sys/class/dmi/id/$f 2>/dev/null)"; done
s machineid;  cat /etc/machine-id 2>/dev/null
s uptime;     cat /proc/uptime
s stat;       head -n 1 /proc/stat
s loadavg;    cat /proc/loadavg
s meminfo;    cat /proc/meminfo
s route;      ip -j route show default
UP="${1:-$(ip -j route show default | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r[0]["dev"] if r else "")' 2>/dev/null)}"
s uplink;     echo "$UP"
s addr;       ip -j addr show
s neigh;      ip -j neigh show
s links;      ip -j -s -d link show
s fdb;        bridge -j -s fdb show dynamic

PHYS=""
for d in /sys/class/net/*; do
  n=$(basename "$d"); [ -e "$d/device" ] || continue; [ -e "$d/wireless" ] && continue
  PHYS="$PHYS $n"
done
s phys
for n in $PHYS; do
  d=/sys/class/net/$n
  echo "$n master=$(basename "$(readlink $d/master 2>/dev/null)" 2>/dev/null) speed=$(cat $d/speed 2>/dev/null) duplex=$(cat $d/duplex 2>/dev/null) carrier=$(cat $d/carrier 2>/dev/null) permaddr=$(ethtool -P $n 2>/dev/null | awk '{print $3}')"
done
s ethtool;    for n in $PHYS; do echo "## $n"; ethtool "$n" 2>/dev/null; done
# NetworkManager's view of the uplink: the connection profile and its IPv4
# settings, for the controller's IP Settings (control.address).
s nmconn;     nmcli -g GENERAL.CONNECTION device show "$UP" 2>/dev/null
s nmipv4;     CON=$(nmcli -g GENERAL.CONNECTION device show "$UP" 2>/dev/null); [ -n "$CON" ] && nmcli -g ipv4.method,ipv4.addresses,ipv4.gateway,ipv4.dns con show "$CON" 2>/dev/null
s carrier;    for d in /sys/class/net/*; do [ -e "$d/carrier_changes" ] && echo "$(basename "$d")=$(cat "$d/carrier_changes" 2>/dev/null)"; done

# Podman's own view: networks (driver, interface, subnets), every
# container (running or not) and the running ones' network endpoints.
s networks
podman network inspect $(podman network ls --format '{{.Name}}' 2>/dev/null) 2>/dev/null | python3 -c '
import json, sys
out = []
for n in json.load(sys.stdin):
    out.append({"name": n.get("name"), "id": n.get("id"), "driver": n.get("driver"), "interface": n.get("network_interface"),
                "subnets": [s.get("subnet") for s in n.get("subnets") or []], "internal": n.get("internal", False)})
print(json.dumps(out))'
s ps
podman ps -a --format json 2>/dev/null | python3 -c '
import json, sys
out = []
for c in json.load(sys.stdin):
    out.append({"id": c.get("Id"), "names": c.get("Names"), "state": c.get("State"), "status": c.get("Status"),
                "created": c.get("Created"), "image": c.get("Image"), "networks": c.get("Networks") or [], "exited": c.get("Exited"), "pod": c.get("PodName")})
print(json.dumps(out))'
RUNNING="$(podman ps --format '{{.ID}}' 2>/dev/null)"
s inspect
[ -n "$RUNNING" ] && podman inspect $RUNNING 2>/dev/null | python3 -c '
import json, sys
out = []
for c in json.load(sys.stdin):
    ns = c.get("NetworkSettings") or {}
    nets = {}
    for name, e in (ns.get("Networks") or {}).items():
        nets[name] = {"mac": e.get("MacAddress"), "ip": e.get("IPAddress"), "prefix": e.get("IPPrefixLen"), "gateway": e.get("Gateway"),
                      "network_id": e.get("NetworkID"), "aliases": e.get("Aliases") or [], "interface": e.get("InterfaceName")}
    out.append({"id": c.get("Id"), "name": c.get("Name"), "sandbox": ns.get("SandboxKey"), "networks": nets,
                "started": (c.get("State") or {}).get("StartedAt"), "running": (c.get("State") or {}).get("Running"),
                "hostname": (c.get("Config") or {}).get("Hostname"), "image": (c.get("ImageName") or c.get("Config", {}).get("Image"))})
print(json.dumps(out))'
# Each running container's interfaces from inside its network namespace:
# the endpoint's own MAC, carrier and counters, for bridge and macvlan
# endpoints alike (a macvlan endpoint has no host-side interface at all).
s netns
for id in $RUNNING; do
  ns=$(podman inspect --format '{{.NetworkSettings.SandboxKey}}' "$id" 2>/dev/null)
  [ -n "$ns" ] || continue
  echo "## $id"
  nsenter --net="$ns" ip -j -s -d link show 2>/dev/null
done
s slots;      cat "${2:-/var/lib/disunified/podman-slots.json}" 2>/dev/null
s end
