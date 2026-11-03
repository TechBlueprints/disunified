#!/usr/bin/env bash
# vs-inject.sh -- end-to-end check of the l2mcd parser fixes on a VS: each of the
# harness's regression packets (/tmp/corpus/<case>, whole IPv4 packets) sent by
# test host h1 (vs-hosts.sh) as a tagged frame into a snooping VLAN on
# Ethernet8; after each, l2mcd's counters and whether it still runs. Inside the
# VM only.
set -uo pipefail
VID=100
stats() { docker exec l2mcd sh -c "rm -f /tmp/g10; l2mcd_ctl -s /tmp/g10 -c 10 >/dev/null 2>&1; sleep 1; grep Recv-Stats /tmp/g10" | sed -E 's/.*igmp:([0-9]+).*malformed-igmp:([0-9]+).*/igmp \1 malformed \2/'; }
sudo config vlan add $VID; sudo config vlan member add $VID Ethernet8
sudo config igmp-snooping enable $VID; sudo config igmp-snooping version $VID 3
sleep 3
pid0=$(docker exec l2mcd pgrep -x l2mcd)
echo "l2mcd pid $pid0; before: $(stats)"
for f in /tmp/corpus/*; do
  sudo ip netns exec h1 python3 -c "
import socket, sys
pkt = open('$f', 'rb').read()
d = pkt[16:20]
mac = bytes([1, 0, 0x5e, d[1] & 0x7f, d[2], d[3]])
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW)
s.bind(('h1eth', 0))
s.send(mac + bytes.fromhex('020000000011') + b'\x81\x00' + (${VID}).to_bytes(2, 'big') + b'\x08\x00' + pkt)"
  sleep 1
  printf '%-28s %4d bytes  %s\n' "$(basename "$f")" "$(stat -c %s "$f")" "$(stats)"
done
pid1=$(docker exec l2mcd pgrep -x l2mcd)
[ "$pid0" = "$pid1" ] && echo "l2mcd still running, same pid $pid1" || echo "l2mcd RESTARTED: $pid0 -> $pid1"
docker exec l2mcd supervisorctl status l2mcd l2mcmgrd
sudo grep -h "rx: dropped a malformed" /var/log/syslog | tail -n 3 | cut -c1-200
show igmp-snooping groups vlan $VID | tail -n 3
sudo config igmp-snooping disable $VID; sudo config vlan member del $VID Ethernet8; sudo config vlan del $VID
