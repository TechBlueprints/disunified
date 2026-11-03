#!/usr/bin/env bash
# vs-hosts.sh up|down -- two test hosts for a SONiC VS that has no NIC behind
# Ethernet8/Ethernet12: veth pairs named eth3/eth4 (the VS lane map's names for
# those ports) whose far ends sit in network namespaces h1 and h2. Nothing leaves
# the VM. `up` restarts swss so the virtual switch binds the new interfaces
# (about two minutes); the ports then come up at the veth's speed. Run on the VS
# as admin. Not persistent: a reboot removes them.
set -euo pipefail
case "${1:-}" in
  up)
    for n in 1 2; do
      e=eth$((n + 2))
      ip link show "$e" >/dev/null 2>&1 && continue
      sudo ip netns add "h$n" 2>/dev/null || true
      sudo ip link add "$e" type veth peer name "h${n}eth"
      sudo ip link set "h${n}eth" netns "h$n"
      sudo ip link set "$e" up
      sudo ip netns exec "h$n" ip link set lo up
      sudo ip netns exec "h$n" ip link set "h${n}eth" up
    done
    sudo systemctl restart swss
    for _ in $(seq 1 60); do
      show interfaces status Ethernet8,Ethernet12 2>/dev/null | grep -c ' up  *up ' | grep -qx 2 && { echo "Ethernet8, Ethernet12 up"; exit 0; }
      sleep 5
    done
    echo "the ports did not come up"; exit 1 ;;
  down)
    for n in 1 2; do sudo ip netns del "h$n" 2>/dev/null || true; done   # takes the veth pairs with it
    echo "test hosts removed (restart swss to rebind)" ;;
  *) echo "usage: $0 up|down"; exit 2 ;;
esac
