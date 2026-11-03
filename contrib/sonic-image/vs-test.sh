#!/usr/bin/env bash
# vs-test.sh -- the virtual-switch checks of disunified docs/drivers/sonic.md
# §8.9 (V1-V3, V6, V7) on a SONiC VS running the IGMP-snooping build.
# Run on the switch as admin (it uses sudo). Every output is kept under
# ~/igmp-vs-test/<step>.txt for fixtures; the summary is printed at the end.
# It only touches a test VLAN (VID, default 100) and the l2mcd feature, and
# removes what it created. Set LIVE_PORT to the one front-panel port with a
# link if a join test is wanted afterwards (V4 is run separately).
set -uo pipefail

VID=${VID:-100}
P1=${P1:-Ethernet8}        # test VLAN member that takes a static group
P2=${P2:-Ethernet12}       # test VLAN member that becomes an mrouter port
GROUP=${GROUP:-239.1.2.3}
OUT=$HOME/igmp-vs-test
mkdir -p "$OUT"
PASS=0; FAIL=0
ok()   { echo "  ok    $*"; PASS=$((PASS + 1)); }
bad()  { echo "  FAIL  $*"; FAIL=$((FAIL + 1)); }
cap()  { local f=$OUT/$1; shift; { echo "\$ $*"; "$@"; } > "$f" 2>&1; }   # cap <file> <cmd...>
asic_vlan() {   # the ASIC_DB VLAN object key for VID $1
  local k
  for k in $(sonic-db-cli ASIC_DB keys 'ASIC_STATE:SAI_OBJECT_TYPE_VLAN:*'); do
    [ "$(sonic-db-cli ASIC_DB hget "$k" SAI_VLAN_ATTR_VLAN_ID)" = "$1" ] && { echo "$k"; return; }
  done
}
waitfor() {     # waitfor <seconds> <cmd...>: until cmd succeeds
  local t=$1; shift
  for _ in $(seq 1 "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
  return 1
}

echo "== V1 boot"
cap v1-version.txt show version
cap v1-docker.txt docker ps --format '{{.Names}} {{.Status}}'
cap v1-feature.txt show feature status
grep -q 'igmp-202511' "$OUT/v1-version.txt" && ok "build $(sed -n 's/^SONiC Software Version: SONiC\.//p' "$OUT/v1-version.txt")" || bad "not the igmp-202511 build"
grep -E '^l2mcd ' "$OUT/v1-feature.txt" | grep -q disabled && ok "FEATURE l2mcd ships disabled" || bad "l2mcd feature: $(grep -E '^l2mcd ' "$OUT/v1-feature.txt")"
for c in swss syncd database bgp teamd pmon lldp; do
  grep -q "^$c " "$OUT/v1-docker.txt" || [ "$c" = bgp ] || bad "container $c not running"
done
ok "containers: $(awk '{print $1}' "$OUT/v1-docker.txt" | sort | tr '\n' ' ')"
cap v1-syslog-l2mc.txt sudo grep -i -E 'l2mc|igmp' /var/log/syslog
if grep -i -E 'error|fail' "$OUT/v1-syslog-l2mc.txt" | grep -v -i 'no error' >/dev/null; then bad "L2MC/IGMP errors in syslog before enabling (see v1-syslog-l2mc.txt)"; else ok "no L2MC/IGMP errors in syslog with the feature off"; fi

echo "== V2 feature on"
sudo config feature state l2mcd enabled
if waitfor 180 bash -c 'docker ps --format "{{.Names}}" | grep -qx l2mcd && docker exec l2mcd supervisorctl status l2mcd l2mcmgrd | grep -c RUNNING | grep -qx 2'; then
  ok "l2mcd container up, l2mcd and l2mcmgrd RUNNING"
else
  bad "l2mcd container or its programs not running"
fi
cap v2-supervisor.txt docker exec l2mcd supervisorctl status
cap v2-copp-appl.txt sonic-db-cli APPL_DB hgetall 'COPP_TABLE:l2mcd_group'
grep -q 'igmp_query' "$OUT/v2-copp-appl.txt" && ok "APPL_DB COPP_TABLE:l2mcd_group carries the IGMP traps" || bad "no l2mcd_group in APPL_DB"
traps=""
for k in $(sonic-db-cli ASIC_DB keys 'ASIC_STATE:SAI_OBJECT_TYPE_HOSTIF_TRAP:*'); do
  traps="$traps $(sonic-db-cli ASIC_DB hget "$k" SAI_HOSTIF_TRAP_ATTR_TRAP_TYPE)"
done
echo "$traps" | tr ' ' '\n' | sort > "$OUT/v2-asic-traps.txt"
n=$(grep -c 'IGMP_TYPE' "$OUT/v2-asic-traps.txt")
[ "$n" = 5 ] && ok "ASIC_DB has the 5 IGMP hostif traps" || bad "ASIC_DB IGMP traps: $n of 5"
grep -q 'MLD' "$OUT/v2-asic-traps.txt" && bad "MLD traps installed" || ok "no MLD traps (as the ECS)"

echo "== V3 snooping on test Vlan$VID"
sudo config vlan add "$VID"
sudo config vlan member add "$VID" "$P1"
sudo config vlan member add "$VID" "$P2"
sudo config igmp-snooping enable "$VID"
cap v3-config-l2mc.txt sonic-db-cli CONFIG_DB hgetall "L2MC|Vlan$VID"
grep -q "'enabled': 'true'" "$OUT/v3-config-l2mc.txt" && ok "CONFIG_DB L2MC|Vlan$VID enabled" || bad "CONFIG_DB L2MC row"
waitfor 30 bash -c "sonic-db-cli APPL_DB keys 'L2MC_VLAN_TABLE:Vlan$VID*' | grep -q ." && ok "APPL_DB L2MC_VLAN_TABLE:Vlan$VID" || bad "no APPL_DB L2MC_VLAN_TABLE entry"
cap v3-appl-l2mc-vlan.txt bash -c "for k in \$(sonic-db-cli APPL_DB keys 'L2MC_VLAN_TABLE:*'); do echo \$k; sonic-db-cli APPL_DB hgetall \$k; done"
sleep 3
vk=$(asic_vlan "$VID")
cap v3-asic-vlan.txt sonic-db-cli ASIC_DB hgetall "$vk"
grep -q "SAI_VLAN_ATTR_CUSTOM_IGMP_SNOOPING_ENABLE': 'true'" "$OUT/v3-asic-vlan.txt" && ok "ASIC_DB Vlan$VID SAI_VLAN_ATTR_CUSTOM_IGMP_SNOOPING_ENABLE true" || bad "VLAN snooping attribute not in ASIC_DB"
cap v3-show-vlan.txt show igmp-snooping vlan "$VID"

echo "== V6 static group, mrouter port, optimised flood"
sudo config igmp-snooping static-group add "$VID" "$P1" "$GROUP"
sudo config igmp-snooping mrouter add "$VID" "$P2"
sudo config igmp-snooping optimised_multicast_flood enable "$VID"
sleep 5
cap v6-show-groups.txt show igmp-snooping groups vlan "$VID"
cap v6-appl-member.txt bash -c "for k in \$(sonic-db-cli APPL_DB keys 'L2MC_MEMBER_TABLE:*'; sonic-db-cli APPL_DB keys 'L2MC_MROUTER_TABLE:*'); do echo \$k; sonic-db-cli APPL_DB hgetall \$k; done"
grep -q "$GROUP" "$OUT/v6-appl-member.txt" && ok "APPL_DB L2MC_MEMBER_TABLE has $GROUP on $P1" || bad "no APPL_DB member for $GROUP"
grep -q "L2MC_MROUTER_TABLE:Vlan$VID" "$OUT/v6-appl-member.txt" && ok "APPL_DB L2MC_MROUTER_TABLE has $P2" || bad "no APPL_DB mrouter entry"
cap v6-asic-l2mc.txt bash -c "for t in L2MC_ENTRY L2MC_GROUP L2MC_GROUP_MEMBER; do for k in \$(sonic-db-cli ASIC_DB keys \"ASIC_STATE:SAI_OBJECT_TYPE_\$t:*\"); do echo \$k; sonic-db-cli ASIC_DB hgetall \$k; done; done"
grep -q 'SAI_OBJECT_TYPE_L2MC_ENTRY' "$OUT/v6-asic-l2mc.txt" && grep -q "$GROUP" "$OUT/v6-asic-l2mc.txt" && ok "ASIC_DB L2MC_ENTRY for $GROUP" || bad "no ASIC_DB L2MC_ENTRY"
grep -q 'SAI_OBJECT_TYPE_L2MC_GROUP_MEMBER' "$OUT/v6-asic-l2mc.txt" && ok "ASIC_DB L2MC_GROUP_MEMBER present" || bad "no ASIC_DB L2MC_GROUP_MEMBER"
cap v6-asic-vlan.txt sonic-db-cli ASIC_DB hgetall "$vk"
grep -q 'SAI_VLAN_ATTR_UNKNOWN_IPV4_MCAST_OUTPUT_GROUP_ID' "$OUT/v6-asic-vlan.txt" && ok "optimised flood sets SAI_VLAN_ATTR_UNKNOWN_IPV4_MCAST_OUTPUT_GROUP_ID" || bad "optimised flood: no unknown-IPv4-mcast output group on the VLAN"

echo "== V7 lifecycle"
sudo systemctl restart l2mcd
waitfor 180 bash -c 'docker exec l2mcd supervisorctl status l2mcd l2mcmgrd | grep -c RUNNING | grep -qx 2' && ok "l2mcd container restarted" || bad "l2mcd did not come back"
sleep 10
cap v7-after-restart.txt bash -c "sonic-db-cli APPL_DB keys 'L2MC_*'"
grep -q "$GROUP" "$OUT/v7-after-restart.txt" && ok "static group rebuilt after the container restart" || bad "static group missing after restart"
out=$(sudo config vlan del "$VID" 2>&1); echo "$out" > "$OUT/v7-vlan-del-refused.txt"
sonic-db-cli CONFIG_DB exists "VLAN|Vlan$VID" | grep -q 1 && ok "config vlan del refused while snooping is on: $(echo "$out" | tail -n 1)" || bad "Vlan$VID was deleted with snooping on"
out=$(sudo config vlan member del "$VID" "$P1" 2>&1); echo "$out" > "$OUT/v7-member-del-refused.txt"
sonic-db-cli CONFIG_DB exists "VLAN_MEMBER|Vlan$VID|$P1" | grep -q 1 && ok "member with a static group not removable: $(echo "$out" | tail -n 1)" || bad "$P1 removed despite its static group"
sudo config igmp-snooping disable "$VID"
sleep 5
cap v7-after-disable.txt bash -c "sonic-db-cli CONFIG_DB keys 'L2MC*'; sonic-db-cli APPL_DB keys 'L2MC_*'; sonic-db-cli ASIC_DB keys 'ASIC_STATE:SAI_OBJECT_TYPE_L2MC_*'"
grep -q . "$OUT/v7-after-disable.txt" && grep -v '^\$' "$OUT/v7-after-disable.txt" | grep -q 'L2MC' && bad "L2MC state left after disable (v7-after-disable.txt)" || ok "disable removed every L2MC row and ASIC object"
sudo config vlan member del "$VID" "$P1"; sudo config vlan member del "$VID" "$P2"; sudo config vlan del "$VID"
sonic-db-cli CONFIG_DB exists "VLAN|Vlan$VID" | grep -q 0 && ok "test Vlan$VID removed" || bad "test Vlan$VID still there"
cap v7-syslog-l2mc.txt sudo grep -i -E 'l2mc|igmp' /var/log/syslog
if grep -i -E 'err|fail' "$OUT/v7-syslog-l2mc.txt" | grep -v -i -E 'no error|errdisable' > "$OUT/v7-syslog-errors.txt"; then
  echo "  note  $(wc -l < "$OUT/v7-syslog-errors.txt") L2MC/IGMP error-looking syslog lines (v7-syslog-errors.txt)"
fi

echo
echo "vs-test: $PASS ok, $FAIL failed; outputs in $OUT"
exit $(( FAIL > 0 ))
