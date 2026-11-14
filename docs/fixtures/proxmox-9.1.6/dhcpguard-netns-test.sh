#!/bin/bash
# DHCP guard semantics test, entirely inside a throwaway network namespace:
# its own bridge, veths and nft table. Nothing in the node's own namespace
# is created or changed; deleting the namespace removes everything.
# Run as root on a Proxmox VE 9.1 node: bash -s < dhcpguard-netns-test.sh.
# The rule set loaded below is the proxmox driver's own nft write for
# collect-node2-nft.txt under the trusted servers of
# controller-10.6.109/system_cfg-ecs-dhcp-snoop.txt (dhcpguard_test.go holds
# the two equal); its output is dhcpguard-netns-test.txt and the table's
# listing after the frames, nft-dhcpguard.json.
set -u
NS=dui-guard-test
X="ip netns exec $NS"
cleanup() { ip netns del $NS 2>/dev/null; }
trap cleanup EXIT
ip netns add $NS || exit 1
$X sysctl -qw net.ipv6.conf.all.disable_ipv6=1 net.ipv6.conf.default.disable_ipv6=1
$X ip link set lo up
$X ip link add vmbr0 type bridge vlan_filtering 1 stp_state 0 mcast_snooping 0
# tap100i0: a guest's NIC (trunk: VLAN 1 untagged, 2 and 69 tagged);
# fwpr120p0: a firewalled guest's member, the same; rx0: receiver, the same.
for p in tap100i0 fwpr120p0 rx0; do
  $X ip link add $p type veth peer name g$p
  $X ip link set $p master vmbr0
  $X bridge vlan add vid 2 dev $p
  $X bridge vlan add vid 69 dev $p
done
$X bridge vlan add vid 2 dev vmbr0 self
$X bridge vlan add vid 69 dev vmbr0 self
for p in vmbr0 tap100i0 fwpr120p0 rx0 gtap100i0 gfwpr120p0 grx0; do $X ip link set $p up; done
sleep 1
cat > /tmp/dui-guard-ruleset.nft <<'EOF'
table bridge disunified
delete table bridge disunified
table bridge disunified {
	comment "dui:037ae97bee8b"
	chain prerouting {
		type filter hook prerouting priority filter; policy accept;
		meta ibrname "vmbr0" meta protocol ip meta l4proto udp udp sport 67 udp dport 68 jump dhcp_guard
	}
	chain dhcp_guard {
		iifname { "tap100i0", "fwpr100p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 1"
		iifname { "tap100i0", "fwpr100p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 1"
		iifname { "tap100i0", "fwpr100p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 1"
		iifname { "tap102i0", "fwpr102p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 3"
		iifname { "tap102i0", "fwpr102p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 3"
		iifname { "tap102i0", "fwpr102p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 3"
		iifname { "tap103i0", "fwpr103p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 4"
		iifname { "tap103i0", "fwpr103p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 4"
		iifname { "tap103i0", "fwpr103p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 4"
		iifname { "tap104i0", "fwpr104p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 5"
		iifname { "tap104i0", "fwpr104p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 5"
		iifname { "tap104i0", "fwpr104p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 5"
		iifname { "tap107i0", "fwpr107p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 8"
		iifname { "tap107i0", "fwpr107p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 8"
		iifname { "tap107i0", "fwpr107p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 8"
		iifname { "tap109i0", "fwpr109p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 10"
		iifname { "tap109i0", "fwpr109p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 10"
		iifname { "tap109i0", "fwpr109p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 10"
		iifname { "tap112i0", "fwpr112p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 13"
		iifname { "tap112i0", "fwpr112p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 13"
		iifname { "tap112i0", "fwpr112p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 13"
		iifname { "tap113i0", "fwpr113p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 14"
		iifname { "tap113i0", "fwpr113p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 14"
		iifname { "tap113i0", "fwpr113p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 14"
		iifname { "tap114i0", "fwpr114p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 15"
		iifname { "tap114i0", "fwpr114p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 15"
		iifname { "tap114i0", "fwpr114p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 15"
		iifname { "tap120i0", "fwpr120p0" } ether type ip ip saddr != { 192.0.2.1 } counter drop comment "port 20"
		iifname { "tap120i0", "fwpr120p0" } vlan id 2 ip saddr != { 192.0.2.1, 192.0.2.6, 192.0.2.7 } counter drop comment "port 20"
		iifname { "tap120i0", "fwpr120p0" } vlan id 69 ip saddr != { 192.0.2.1, 192.0.2.5 } counter drop comment "port 20"
	}
}
EOF
$X nft -f /tmp/dui-guard-ruleset.nft; echo "write (no table yet) rc=$?"
$X nft -f /tmp/dui-guard-ruleset.nft; echo "write (table present) rc=$?"
rm -f /tmp/dui-guard-ruleset.nft
send() { # iface src sport vlan(0=untagged)
$X python3 - "$@" <<'PY'
import socket, struct, sys
iface, src, sport, vlan = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
def csum(b):
    s = sum(struct.unpack('!%dH' % (len(b)//2), b)); s = (s >> 16) + (s & 0xffff); s += s >> 16
    return ~s & 0xffff
payload = bytes(300)
udp = struct.pack('!HHHH', sport, 68, 8 + len(payload), 0) + payload
ip = struct.pack('!BBHHHBBH4s4s', 0x45, 0, 20 + len(udp), 0, 0, 64, 17, 0, socket.inet_aton(src), socket.inet_aton('255.255.255.255'))
ip = ip[:10] + struct.pack('!H', csum(ip)) + ip[12:]
eth = b'\xff' * 6 + bytes.fromhex('020000000099')
eth += (struct.pack('!HH', 0x8100, vlan) if vlan else b'') + struct.pack('!H', 0x0800)
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW); s.bind((iface, 0)); s.send(eth + ip + udp)
PY
}
rx() { $X cat /sys/class/net/grx0/statistics/rx_packets; }
check() { # name iface src sport vlan expect(1 pass / 0 drop)
  local b a; b=$(rx); send "$2" "$3" "$4" "$5"; sleep 0.3; a=$(rx)
  echo "$1: received=$((a-b)) expected=$6"
}
check "port 1 untagged rogue     " gtap100i0 192.0.2.66 67 0 0
check "port 1 untagged trusted   " gtap100i0 192.0.2.1 67 0 1
check "port 1 v2 trusted         " gtap100i0 192.0.2.6 67 2 1
check "port 1 v69 server of v2   " gtap100i0 192.0.2.6 67 69 0
check "port 1 other udp          " gtap100i0 192.0.2.66 1234 0 1
check "port 20 untagged rogue    " gfwpr120p0 192.0.2.66 67 0 0
check "port 20 v2 rogue          " gfwpr120p0 192.0.2.66 67 2 0
check "port 20 v69 trusted       " gfwpr120p0 192.0.2.5 67 69 1
echo "@@@ json"
$X nft -j list table bridge disunified
echo
$X nft delete table bridge disunified; echo "delete rc=$?"
echo "tables after delete: $($X nft list tables | wc -l)"
cleanup
trap - EXIT
echo "netns left: $(ip netns list | grep -c dui-guard-test)"
echo "root tables named disunified: $(nft list tables | grep -c disunified)"
