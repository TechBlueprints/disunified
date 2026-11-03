#!/usr/bin/env bash
# cli-help-capture.sh <outdir> -- run on a SONiC switch: save `config ... --help`
# for every command the disunified sonic driver uses, one file per command, so
# two builds can be diffed (202505 vs the 202511 IGMP build).
out=${1:-$HOME/cli-help}
mkdir -p "$out"
cmds=(
  "" save hostname
  vlan "vlan add" "vlan del" "vlan member" "vlan member add" "vlan member del"
  interface "interface ip" "interface ip add" "interface ip remove" "interface startup" "interface shutdown"
  "interface speed" "interface mtu" "interface fec" "interface autoneg" "interface breakout"
  "interface storm-control" "interface storm-control add" "interface storm-control del"
  portchannel "portchannel add" "portchannel del" "portchannel member" "portchannel member add" "portchannel member del"
  mirror_session "mirror_session span" "mirror_session span add" "mirror_session remove"
  spanning-tree "spanning-tree enable" "spanning-tree disable" "spanning-tree priority" "spanning-tree interface"
  snmp "snmp community" "snmp community add" "snmp community del"
  route "route add" "route del"
  dns "dns nameserver" "dns nameserver add" "dns nameserver del"
  syslog "syslog add" "syslog del" ntp "ntp add" "ntp del"
  clock "clock timezone"
  igmp-snooping mld-snooping feature "feature state"
)
for c in "${cmds[@]}"; do
  f=$out/config_${c// /_}.txt
  sudo config $c --help > "$f" 2>&1
  echo "rc=$?" >> "$f"
done
show version | grep -E 'SONiC Software Version' > "$out/VERSION"
echo "captured ${#cmds[@]} help texts in $out"
