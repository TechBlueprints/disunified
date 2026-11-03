#!/usr/bin/env bash
# Build the l2mcd parser harness and fuzzers with clang, ASan and UBSan.
# Run inside the sonic-slave-bookworm container (clang-14, libFuzzer, libevent,
# libhiredis): build.sh <l2mcd source dir> <out dir>
set -euo pipefail
src=$(cd "$1" && pwd)
out=$2
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$out"

defs="-DDEBUG -DBRCD_DCE_L3_PORT -DBRCD_DCE_L3_PIM_PORT -DBRCD_DCE_L36_PIM -DBRCD_DCE_IPV6
      -DBRCD_LIB_MLD_DCE_PORT -DBRCD_DCE_SHARED_RIB -DBRCD_DCE_NEW_RIB -DSR_SWITCH_ROUTER
      -D__NO_IPSEC__ -D__MP__ -D__IPV6__ -DBRCD_DCE_L3V6_PORT -DBRCD_DCE_MLD_PORT
      -D__FOUNDRY_BGP__ -D__PIM__"
# -fno-sanitize=null,object-size: the daemon's offsetof is the &((T *)0)->m idiom
# (M_AVLL_OFFSETOF), which UBSan reports on every AVL tree creation; a real NULL
# dereference still faults (ASan reports SEGV) and ASan does the bounds checking.
# -fno-sanitize=shift-base: L2MCD_IFIDX_TYPE_BF shifts an interface type into the
# sign bit of an int on every VLAN interface index (include/l2mcd.h), not
# packet-dependent.
san="-fsanitize=address,undefined -fno-sanitize=null,object-size,shift-base -fsanitize-address-use-after-scope -fno-sanitize-recover=all -fno-omit-frame-pointer"
cflags="-g -O1 -w $defs -I$src -I$src/include $san"
# every daemon C file except l2mcd_system.c (the harness includes it) and the
# C++ main/sync (stubs.c stands in for the sync layer)
objs=()
rm -rf "$out/obj"; mkdir -p "$out/obj"
for f in common/applog.c common/malloc_stubs.c common/hash_grow_generic.c common/wheel_timer.c \
         common/avl.c common/timer_api.c common/linklist_api.c common/l2mcd_mcast_port.c \
         common/l2mcd_data_struct.c common/l2mcd_mcast_addr.c port/main.c port/l2mcd_cli.c \
         port/l2mcd_utils.c port/l2mcd_portdb.c port/l2mcd_mld_mcast.c port/mld_pkt.c \
         port/l2mcd_mld_pim_snoop.c port/mld_vdb.c port/l2mcd_mld_port.c port/l2mcd_mld_utils.c \
         src/igmp.c src/igmp_cu.c src/igmp_fsm.c src/igmp_io.c src/mld_main.c src/mld_cu.c src/mld_io.c; do
  o="$out/obj/$(echo "$f" | tr / _).o"
  clang $cflags -fsanitize=fuzzer-no-link -c "$src/$f" -o "$o" &
  objs+=("$o")
done
wait
clang $cflags -fsanitize=fuzzer-no-link -c "$here/stubs.c" -o "$out/obj/stubs.o"
objs+=("$out/obj/stubs.o")
link="-Wl,--wrap=sendto -Wl,--wrap=vsyslog -levent -lhiredis -lpthread -lm"
clang $cflags -o "$out/regress" "$here/harness.c" "${objs[@]}" $link
clang $cflags -fsanitize=fuzzer -DFUZZ_IGMP -o "$out/fuzz-igmp" "$here/harness.c" "${objs[@]}" $link
clang $cflags -fsanitize=fuzzer -DFUZZ_MLD -o "$out/fuzz-mld" "$here/harness.c" "${objs[@]}" $link
echo "built: $out/regress $out/fuzz-igmp $out/fuzz-mld"
