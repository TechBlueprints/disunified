#!/usr/bin/env bash
# Run the harness inside the bookworm slave container, in its own network
# namespace with dummy links named like the switch's ports. Nothing reaches a
# real network: the container has no network (--network none) and transmits
# are captured by the harness.
#   run.sh build <src>            build into out/<tag>
#   run.sh regress <tag> [case]   run the regression cases (each its own process)
#   run.sh fuzz <tag> igmp|mld <seconds> [jobs]
#   run.sh shell <tag>
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
img=${SLAVE_IMAGE:-sonic-slave-bookworm-clint:76be43efe8d}
in_ctr() {
  # root in the container for the links and l2mcd's packet sockets; files go back to the caller
  docker run --rm --network none --cap-add NET_ADMIN --cap-add NET_RAW --user root \
    -v "$here:/fz" -w /fz -e HARNESS_DEBUG -e HARNESS_LOGARGS "$img" bash -c "
      trap 'chown -R $(id -u):$(id -g) /fz' EXIT
      for p in Ethernet4 Ethernet8 Vlan100; do ip link add \$p type dummy && ip link set \$p up; done
      $1"
}
cmd=$1; shift
case "$cmd" in
  build)
    src=$1; tag=$(basename "$src")
    in_ctr "./build.sh $src out/$tag" ;;
  regress)
    tag=$1; only=${2:-}
    cases="igmp-valid igmp-valid-aux igmp-65535-sources igmp-65535-groups igmp-truncated-record
           igmp-aux-claimed igmp-type-0 igmp-type-200 igmp-totlen-beyond-packet igmp-totlen-forwarded
           igmp-ihl-beyond-totlen igmp-short-message igmp-v3-query-sources igmp-v1-query-group port-list-repeated mld-valid mld-65535-sources
           mld-65535-groups mld-zero-sources-with-aux mld-type-0 mld-type-200 mld-no-hbh
           mld-hbh-oversized mld-payload-beyond-packet mld-v2-query-sources"
    [ -n "$only" ] && cases=$only
    cases=$(echo $cases)
    in_ctr "mkdir -p corpus/igmp corpus/mld
      for f in found/crash-*; do [ -f \$f ] && cp \$f corpus/igmp/found-\${f##*/}; done
      for c in $cases; do
        # leaks: only what packets allocate (the harness hides setup from LSan)
        if HARNESS_CORPUS=corpus out/$tag/regress \$c > out/$tag/regress-\$c.log 2>&1; then
          r=\"clean \$(grep -o 'dropped as malformed.*' out/$tag/regress-\$c.log)\"
        else r=\$(grep -m1 -oE 'ERROR: AddressSanitizer: [a-z-]+ on address|ERROR: AddressSanitizer: SEGV|runtime error: [^:]*|setup failed|open sockets: [0-9]+, then [0-9]+' out/$tag/regress-\$c.log || echo 'exit '\$?)
          at=\$(grep -m1 -E '^ +#[0-9]+ .* in [a-z_0-9]+ /fz/src' out/$tag/regress-\$c.log | sed -E 's|.* in ([a-z_0-9]+) /fz/src-[a-z]+/([^ ]+).*|\\1 \\2|'); r=\"\$r @ \$at\"; fi
        printf '%-28s %s\n' \$c \"\$r\"
      done" ;;
  fuzz)
    tag=$1; which=$2; secs=$3; jobs=${4:-4}
    in_ctr "mkdir -p corpus/$which corpus-$which-seen out/$tag/crash-$which out/$tag/work-$which
      cd out/$tag/work-$which
      # new inputs go to the first directory; the seeds stay as the regression cases wrote them
      HARNESS_LOGARGS=\${HARNESS_LOGARGS:-/dev/null} nice -n 19 ../fuzz-$which -max_total_time=$secs -max_len=2048 \
        -jobs=$jobs -workers=$jobs -use_value_profile=1 -print_final_stats=1 \
        -artifact_prefix=../crash-$which/ ../../../corpus-$which-seen ../../../corpus/$which > ../fuzz-$which.log 2>&1 || true
      tail -n 5 ../fuzz-$which.log; ls ../crash-$which | head" ;;
  shell)
    in_ctr "bash" ;;
esac
