#!/bin/bash
# switch-to-unifi Proxmox collector. Runs on the node over SSH, once per
# poll, and prints tagged sections ("@@@ <name>" lines) the Go side parses.
# Everything is read-only. $1 = the bridge to present (default vmbr0).
# Every command below exists on Proxmox VE 9 (Debian 13); a missing optional
# tool (lldpcli, sensors) leaves its section empty rather than failing.
BR="${1:-vmbr0}"
s() { echo "@@@ $1"; }

s hostname;   hostname
s pveversion; pveversion 2>/dev/null
s dmi;        for f in sys_vendor product_name product_serial board_serial bios_version; do echo "$f=$(cat /sys/class/dmi/id/$f 2>/dev/null)"; done
s uptime;     cat /proc/uptime
s stat;       head -n 1 /proc/stat
s loadavg;    cat /proc/loadavg
s addr;       ip -j addr show dev "$BR"
s neigh;      ip -j neigh show dev "$BR"
s meminfo;    cat /proc/meminfo
s links;      ip -j -s -d link show
s brlink;     bridge -j -d link show
s vlan;       bridge -j -compressvlans vlan show
s fdb;        bridge -j -s fdb show br "$BR" dynamic
s bridge
for f in stp_state multicast_snooping priority root_id bridge_id vlan_filtering default_pvid; do
  echo "$f=$(cat /sys/class/net/$BR/bridge/$f 2>/dev/null)"
done
echo "address=$(cat /sys/class/net/$BR/address 2>/dev/null)"
echo "mtu=$(cat /sys/class/net/$BR/mtu 2>/dev/null)"
echo "vids=$(sed -n "/^iface $BR /,/^\$/p" /etc/network/interfaces 2>/dev/null | awk '/bridge-vids/{ $1=""; print }')"

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
s bonding;    for b in /proc/net/bonding/*; do [ -e "$b" ] || continue; echo "## $(basename $b)"; cat "$b"; done
s ethtool;    for n in $PHYS; do echo "## $n"; ethtool "$n" 2>/dev/null; done
s ethtoolm;   for n in $PHYS; do echo "## $n"; ethtool -m "$n" 2>/dev/null; done
s hwmon
for h in /sys/class/hwmon/hwmon*; do
  [ -e "$h/name" ] || continue
  echo "## $(cat $h/name)"
  for f in "$h"/temp*_input "$h"/temp*_label "$h"/temp*_max "$h"/temp*_crit "$h"/fan*_input "$h"/fan*_max "$h"/fan*_label "$h"/pwm[0-9]; do
    [ -e "$f" ] || continue
    echo "$(basename "$f")=$(cat "$f" 2>/dev/null)"
  done
done
s carrier;    for d in /sys/class/net/*; do [ -e "$d/carrier_changes" ] && echo "$(basename "$d")=$(cat "$d/carrier_changes" 2>/dev/null)"; done
s vmlist;     cat /etc/pve/.vmlist 2>/dev/null
s qemu
for f in /etc/pve/nodes/*/qemu-server/*.conf; do
  [ -e "$f" ] || continue
  echo "## $f"
  sed -n '/^\[/q;p' "$f" | grep -E '^(name|net[0-9]+|hotplug|template):'
done
s lxc
for f in /etc/pve/nodes/*/lxc/*.conf; do
  [ -e "$f" ] || continue
  echo "## $f"
  sed -n '/^\[/q;p' "$f" | grep -E '^(hostname|net[0-9]+|template):'
done
s lldp;       lldpcli -f json0 show neighbors details 2>/dev/null
s chrony;     grep -hE '^(server|pool) ' /etc/chrony/chrony.conf 2>/dev/null
s ntpunifi;   cat /etc/chrony/sources.d/switch-to-unifi.sources 2>/dev/null
s end
