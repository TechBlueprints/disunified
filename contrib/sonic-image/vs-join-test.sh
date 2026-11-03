#!/usr/bin/env bash
# vs-join-test.sh -- disunified docs/drivers/sonic.md §8.9 V4 (joins and leaves)
# and V5 (querier) on a SONiC VS with the IGMP-snooping build, using the test
# hosts of vs-hosts.sh: h1 behind Ethernet8, h2 behind Ethernet12, both tagged in
# a test VLAN. The IGMP is the hosts' own kernels' (a socket joining a group),
# nothing is crafted, and nothing leaves the VM. Outputs under ~/igmp-vs-test.
# Removes what it creates.
set -uo pipefail

VID=${VID:-100}
GROUP=${GROUP:-239.1.2.3}
OUT=$HOME/igmp-vs-test
mkdir -p "$OUT"
PASS=0; FAIL=0
ok()   { echo "  ok    $*"; PASS=$((PASS + 1)); }
bad()  { echo "  FAIL  $*"; FAIL=$((FAIL + 1)); }
cap()  { local f=$OUT/$1; shift; { echo "\$ $*"; "$@"; } > "$f" 2>&1; }
waitfor() { local t=$1; shift; for _ in $(seq 1 "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
in_h() { local h=$1; shift; sudo ip netns exec "$h" "$@"; }

# A host that joins GROUP for $2 seconds (IGMPv3 by the kernel's default).
join() {
  in_h "$1" python3 -c "
import socket, struct, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP, struct.pack('4s4s', socket.inet_aton('$GROUP'), socket.inet_aton('$3')))
time.sleep($2)" &
}
# IGMP queries a host sees on its port within $2 seconds: "<count> <sources>".
queries() {
  in_h "$1" python3 -c "
import socket, time
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3))
s.bind(('$1eth', 0)); s.settimeout(1)
n, srcs, end = 0, set(), time.time() + $2
while time.time() < end:
    try: f = s.recv(2048)
    except socket.timeout: continue
    off = 14
    if f[12:14] == b'\x81\x00': off = 18
    if f[off-2:off] != b'\x08\x00' or f[off+9] != 2: continue
    ihl = (f[off] & 15) * 4
    if f[off+ihl] == 0x11: n += 1; srcs.add(socket.inet_ntoa(f[off+12:off+16]))
print(n, ' '.join(sorted(srcs)))"
}

if ! ip link show eth3 >/dev/null 2>&1 || ! ip link show eth4 >/dev/null 2>&1; then
  echo "no test hosts: run vs-hosts.sh up first"; exit 2
fi
show feature status | grep -E '^l2mcd ' | grep -q enabled || sudo config feature state l2mcd enabled
waitfor 120 bash -c 'docker exec l2mcd supervisorctl status l2mcd l2mcmgrd | grep -c RUNNING | grep -qx 2' || { echo "l2mcd not running"; exit 2; }

sudo config vlan add "$VID"
sudo config vlan member add "$VID" Ethernet8
sudo config vlan member add "$VID" Ethernet12
sudo config igmp-snooping enable "$VID"
# The CLI's default is IGMPv2, and l2mcd at v2 ignores v3 reports -- which is
# what a Linux host sends while no querier has told it otherwise. At v3 it takes
# v1/v2/v3 reports; h2 is forced to v2 so both kinds are tested.
sudo config igmp-snooping version "$VID" 3
for n in 1 2; do
  in_h "h$n" ip link add link "h${n}eth" name "v$VID" type vlan id "$VID"
  in_h "h$n" ip addr add "192.0.2.1$n/24" dev "v$VID"
  in_h "h$n" ip link set "v$VID" up
done
in_h h2 sysctl -qw "net.ipv4.conf.v$VID.force_igmp_version=2"
sleep 3

echo "== V4 join and leave"
join h1 40 192.0.2.11
if waitfor 30 bash -c "show igmp-snooping groups vlan $VID | grep -q '$GROUP'"; then ok "h1's join: $GROUP learned"; else bad "h1's join not learned"; fi
cap v4-show-groups.txt show igmp-snooping groups vlan "$VID"
grep -q Ethernet8 "$OUT/v4-show-groups.txt" && ok "on Ethernet8 (h1's port)" || bad "not on Ethernet8"
grep -q Ethernet12 "$OUT/v4-show-groups.txt" && bad "also on Ethernet12, where nobody joined" || ok "not on Ethernet12"
cap v4-appl-member.txt bash -c "for k in \$(sonic-db-cli APPL_DB keys 'L2MC_MEMBER_TABLE:*'); do echo \$k; sonic-db-cli APPL_DB hgetall \$k; done"
grep -q "$GROUP" "$OUT/v4-appl-member.txt" && grep -q Ethernet8 "$OUT/v4-appl-member.txt" && ok "APPL_DB L2MC_MEMBER_TABLE: $(grep -m1 "$GROUP" "$OUT/v4-appl-member.txt")" || bad "no APPL_DB member"
cap v4-asic.txt bash -c "for t in L2MC_ENTRY L2MC_GROUP L2MC_GROUP_MEMBER; do for k in \$(sonic-db-cli ASIC_DB keys \"ASIC_STATE:SAI_OBJECT_TYPE_\$t:*\"); do echo \$k; sonic-db-cli ASIC_DB hgetall \$k; done; done"
grep -q "$GROUP" "$OUT/v4-asic.txt" && ok "ASIC_DB L2MC_ENTRY for $GROUP" || bad "no ASIC_DB L2MC_ENTRY"
[ "$(grep -c 'SAI_OBJECT_TYPE_L2MC_GROUP_MEMBER' "$OUT/v4-asic.txt")" = 1 ] && ok "one L2MC_GROUP_MEMBER" || bad "L2MC_GROUP_MEMBERs: $(grep -c 'SAI_OBJECT_TYPE_L2MC_GROUP_MEMBER' "$OUT/v4-asic.txt")"
join h2 25 192.0.2.12
waitfor 30 bash -c "show igmp-snooping groups vlan $VID | grep -q Ethernet12" && ok "h2 joins too (IGMPv2): Ethernet12 added" || bad "h2's v2 join not learned"
cap v4-show-groups-2.txt show igmp-snooping groups vlan "$VID"
wait   # both sockets close: the kernels send their leaves
t0=$SECONDS
if waitfor 60 bash -c "! show igmp-snooping groups vlan $VID | grep -q '$GROUP'"; then
  ok "after both leaves the group is gone ($(( SECONDS - t0 )) s)"
else
  bad "group still listed 60 s after the leaves"
fi
sleep 5
cap v4-after-leave.txt bash -c "sonic-db-cli APPL_DB keys 'L2MC_MEMBER_TABLE:*'; sonic-db-cli ASIC_DB keys 'ASIC_STATE:SAI_OBJECT_TYPE_L2MC_ENTRY:*'"
grep -v '^\$' "$OUT/v4-after-leave.txt" | grep -q L2MC && bad "L2MC rows left after the leaves" || ok "APPL_DB and ASIC_DB entries removed"

echo "== V5 querier"
q=$(queries h2 12)
[ "${q%% *}" = 0 ] && ok "no queries while the querier is off" || bad "queries with the querier off: $q"
sudo config igmp-snooping query-interval "$VID" 12   # must exceed the 10 s max response time
sudo config igmp-snooping querier enable "$VID"
q=$(queries h2 30)
echo "$q" > "$OUT/v5-queries.txt"
[ "${q%% *}" -ge 2 ] && ok "querier on: $q (count, sources) seen by h2 in 30 s" || bad "querier on: only $q"
sudo config igmp-snooping querier disable "$VID"
sleep 6
q=$(queries h2 12)
[ "${q%% *}" = 0 ] && ok "querier off again: silent" || bad "still querying: $q"

echo "== cleanup"
for n in 1 2; do in_h "h$n" ip link del "v$VID"; done
sudo config igmp-snooping disable "$VID"
sudo config vlan member del "$VID" Ethernet8; sudo config vlan member del "$VID" Ethernet12; sudo config vlan del "$VID"
sonic-db-cli CONFIG_DB exists "VLAN|Vlan$VID" | grep -q 0 && ok "test Vlan$VID removed" || bad "test Vlan$VID still there"
echo
echo "vs-join-test: $PASS ok, $FAIL failed; outputs in $OUT"
exit $(( FAIL > 0 ))
