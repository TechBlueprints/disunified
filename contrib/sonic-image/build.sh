#!/usr/bin/env bash
# build.sh -- build SONiC 202511 with IGMP/MLD snooping (the Micas l2mcd series
# in ./series) for the virtual switch and for Broadcom, unattended.
#
#   ./build.sh                 preflight, prepare, vs, broadcom, summary
#   ./build.sh preflight       only check the host
#   ./build.sh prepare         clone, apply the series, make init, patch submodules
#   ./build.sh vs | broadcom   build one platform (prepare must have run)
#
# Run it as a normal user in the docker group, under tmux or nohup; it writes
# everything to $WORK/logs and the images to $OUT/<build id>/.
#
# Settings (environment):
#   WORK=~/sonic-igmp         where the trees and logs go (needs ~300 GB)
#   OUT=$WORK/out             where the images go, one directory per build id
#   JOBS=4                    SONIC_BUILD_JOBS (packages built in parallel)
#   DX010_SAI=legacy-th       legacy-th: DX010 on libsaibcm 13.2.1 (series 0005), image
#                             sonic-broadcom-legacy-th.bin; xgs: leave 0005 out, the
#                             DX010 takes the 14.3 SAI from sonic-broadcom.bin
#   BUILD_NUMBER=YYYYMMDD     the number in the build id <branch>.<number>-<sha>
#   FOLLOW_HEAD=0             1: build on the current 202511 tips with git am -3
#                             instead of the bases in series/BASES
#   FAILFAST=1                build l2mcd, swss and docker-l2mcd before the vs image
#   PLATFORMS="vs broadcom"   what "all" builds
#   VS_DOCKER=0               1: also build docker-sonic-vs.gz
#   RESUME=0                  1: ./build.sh vs|broadcom keeps an existing tree-<platform>
#                             and lets make carry on where it stopped
#   SKIP_PREFLIGHT=0
set -Eeuo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SERIES=$HERE/series
WORK=${WORK:-$HOME/sonic-igmp}
OUT=${OUT:-$WORK/out}
JOBS=${JOBS:-4}
DX010_SAI=${DX010_SAI:-legacy-th}
BUILD_NUMBER=${BUILD_NUMBER:-$(date -u +%Y%m%d)}
FOLLOW_HEAD=${FOLLOW_HEAD:-0}
FAILFAST=${FAILFAST:-1}
PLATFORMS=${PLATFORMS:-vs broadcom}
VS_DOCKER=${VS_DOCKER:-0}
RESUME=${RESUME:-0}
SKIP_PREFLIGHT=${SKIP_PREFLIGHT:-0}
BRANCH=igmp-202511                       # local branch name; it is the first part of the build id
URL_BUILDIMAGE=https://github.com/sonic-net/sonic-buildimage.git
export BUILD_NUMBER
export GIT_COMMITTER_NAME=${GIT_COMMITTER_NAME:-sonic-igmp-build}
export GIT_COMMITTER_EMAIL=${GIT_COMMITTER_EMAIL:-sonic-igmp-build@localhost}

LOGS=$WORK/logs
PREP=$WORK/tree-prepared
mkdir -p "$LOGS"

log()  { printf '%s  %s\n' "$(date -u +%FT%TZ)" "$*" | tee -a "$LOGS/build.log"; }
die()  { log "ERROR: $*"; exit 1; }
base_of() { awk -v r="$1" '$1 == r {print $2}' "$SERIES/BASES"; }

on_error() {
  local rc=$? line=$1
  log "FAILED (exit $rc) at build.sh line $line"
  # the newest package log in any tree is usually the one that broke
  local last
  last=$(find "$WORK" -path '*/target/*' -name '*.log' -newer "$LOGS/build.log.start" -type f -printf '%T@ %p\n' 2>/dev/null | sort -n | tail -n 1 | cut -d' ' -f2-)
  if [ -n "$last" ]; then
    log "newest package log: $last (last 60 lines follow)"
    tail -n 60 "$last" | tee -a "$LOGS/build.log"
  fi
  exit "$rc"
}
trap 'on_error $LINENO' ERR

stage() {   # stage <name> <command...>: timed, logged to logs/<name>.log
  local name=$1; shift
  local t0=$SECONDS
  log "start  $name"
  "$@" > "$LOGS/$name.log" 2>&1
  log "done   $name ($(( (SECONDS - t0) / 60 )) min)"
  printf '%s %d\n' "$name" $(( SECONDS - t0 )) >> "$LOGS/durations"
}

# ---------------------------------------------------------------------------
preflight() {
  local fail=0
  chk() { if eval "$2"; then log "ok    $1"; else log "FAIL  $1${3:+ -- $3}"; fail=1; fi; }
  warn() { if eval "$2"; then log "ok    $1"; else log "warn  $1${3:+ -- $3}"; fi; }
  warn "Debian 12 x86_64" '[ "$(. /etc/os-release; echo "$ID $VERSION_ID")" = "debian 12" ] && [ "$(uname -m)" = x86_64 ]' "other hosts work but are not what this was planned on"
  chk  "not root" '[ "$(id -u)" != 0 ]' "sonic-buildimage refuses root; use a user in the docker group"
  chk  "docker usable without sudo" 'docker info >/dev/null 2>&1' "sudo gpasswd -a $USER docker, then log in again"
  chk  "docker >= 20.10.10" '[ "$(printf "%s\n20.10.10\n" "$(docker version -f "{{.Server.Version}}" 2>/dev/null || echo 0)" | sort -V | head -n 1)" = 20.10.10 ]' "bookworm/trixie slaves need clone3"
  # j2cli's --version exits 1, so test what it prints (as Makefile.work does)
  chk  "j2 on PATH" '[ -n "$(j2 --version 2>&1 | grep j2)" ]' "pipx install jinjanator  (or apt install j2cli)"
  chk  "overlay module" '[ -d /sys/module/overlay ]' "sudo modprobe overlay"
  chk  "/dev/kvm (the vs image is installed in a KVM guest, as root inside the privileged slave)" '[ -c /dev/kvm ]' "on a Proxmox guest: CPU type host, nested virtualisation on"
  chk  "git >= 2.25" '[ "$(printf "%s\n2.25\n" "$(git --version | awk "{print \$3}")" | sort -V | head -n 1)" = 2.25 ]'
  chk  "github reachable" 'git ls-remote --exit-code "$URL_BUILDIMAGE" refs/heads/202511 >/dev/null 2>&1'
  local cores mem free
  cores=$(nproc); mem=$(awk '/MemTotal/ {printf "%d", $2 / 1048576}' /proc/meminfo)
  mkdir -p "$WORK"; free=$(df -BG --output=avail "$WORK" | tail -n 1 | tr -dc 0-9)
  warn "cores: $cores (16 planned)" '[ "$cores" -ge 8 ]'
  warn "memory: ${mem} GB (64 planned; JOBS=$JOBS)" '[ "$mem" -ge 32 ]'
  chk  "free disk at $WORK: ${free} GB (two trees need ~250-300)" '[ "$free" -ge 220 ]'
  warn "docker root has room: $(df -BG --output=avail "$(docker info -f "{{.DockerRootDir}}" 2>/dev/null || echo /)" | tail -n 1 | tr -dc 0-9) GB" '[ "$(df -BG --output=avail "$(docker info -f "{{.DockerRootDir}}" 2>/dev/null || echo /)" | tail -n 1 | tr -dc 0-9)" -ge 60 ]' "slave images live there"
  case "$DX010_SAI" in legacy-th|xgs) ;; *) log "FAIL  DX010_SAI must be legacy-th or xgs"; fail=1;; esac
  [ "$fail" = 0 ] || die "preflight failed"
}

# ---------------------------------------------------------------------------
apply_series() {   # apply_series <dir> <series name> [skip-glob]
  local dir=$1 name=$2 skip=${3:-} p list=()
  while read -r p; do
    [ -n "$skip" ] && [[ $p == $skip ]] && { echo "skipping $p"; continue; }
    list+=("$SERIES/$name/$p")
  done < "$SERIES/$name/series"
  if [ "$FOLLOW_HEAD" = 1 ]; then
    git -C "$dir" am -3 --keep-non-patch --whitespace=nowarn "${list[@]}"
  else
    git -C "$dir" am --keep-non-patch --whitespace=nowarn "${list[@]}"
  fi
}

do_prepare() {
  local bi=$PREP/sonic-buildimage base
  rm -rf "$PREP"; mkdir -p "$PREP"
  git clone --single-branch --branch 202511 "$URL_BUILDIMAGE" "$bi"
  if [ "$FOLLOW_HEAD" = 1 ]; then base=$(git -C "$bi" rev-parse origin/202511)
  else base=$(base_of sonic-buildimage); git -C "$bi" cat-file -e "$base^{commit}"; fi
  git -C "$bi" checkout -q -B "$BRANCH" "$base"
  if [ "$DX010_SAI" = xgs ]; then apply_series "$bi" sonic-buildimage '0005-*'
  else apply_series "$bi" sonic-buildimage; fi

  # make init = git submodule update --init --recursive (and relative gitdirs)
  make -C "$bi" init

  local sub dir want have
  for sub in sonic-swss sonic-swss-common sonic-utilities sonic-l2mcd sonic-mgmt-common; do
    dir=$bi/src/$sub
    have=$(git -C "$dir" rev-parse HEAD)
    want=$(base_of "$sub")
    if [ "$have" != "$want" ]; then
      if [ "$FOLLOW_HEAD" = 1 ]; then echo "$sub: pinned $have (series base $want), applying with -3"
      else echo "$sub is at $have, the series is for $want"; return 1; fi
    fi
    git -C "$dir" checkout -q -B "$BRANCH"
    apply_series "$dir" "$sub"
  done

  {
    echo "prepared   $(date -u +%FT%TZ)"
    echo "dx010_sai  $DX010_SAI"
    echo "follow     $FOLLOW_HEAD"
    echo "buildimage $(git -C "$bi" rev-parse HEAD) on $base"
    for sub in sonic-swss sonic-swss-common sonic-utilities sonic-l2mcd sonic-mgmt-common; do
      echo "$sub $(git -C "$bi/src/$sub" rev-parse HEAD) on $(base_of "$sub")"
    done
    echo "series sha256:"; (cd "$SERIES" && find . -name '*.patch' | sort | xargs sha256sum)
  } > "$PREP/MANIFEST"
  cat "$PREP/MANIFEST"
}

prepare() {
  stage prepare do_prepare
  log "prepared tree $PREP/sonic-buildimage, build id $(build_id "$PREP/sonic-buildimage")"
}

build_id() { (cd "$1" && BUILD_NUMBER=$BUILD_NUMBER bash -c '. ./functions.sh && sonic_get_version'); }

# ---------------------------------------------------------------------------
do_tree() {   # copy the prepared tree for one platform (relocatable after make init)
  local plat=$1 dst=$WORK/tree-$1
  [ -d "$PREP/sonic-buildimage" ] || { echo "run ./build.sh prepare first"; return 1; }
  if [ "$RESUME" = 1 ] && [ -d "$dst/sonic-buildimage" ]; then echo "RESUME=1: keeping $dst"; return 0; fi
  rm -rf "$dst"; mkdir -p "$dst"
  cp -a --reflink=auto "$PREP/sonic-buildimage" "$dst/"
}

do_configure() { make -C "$WORK/tree-$1/sonic-buildimage" configure PLATFORM="$1"; }

do_failfast() {   # the changed packages first, in the bookworm slave
  local bi=$WORK/tree-$1/sonic-buildimage
  BLDENV=bookworm make -C "$bi" -f Makefile.work SONIC_BUILD_JOBS="$JOBS" \
    target/debs/bookworm/libswsscommon_1.0.0_amd64.deb \
    target/debs/bookworm/l2mcd_1.0.0_amd64.deb \
    target/debs/bookworm/swss_1.0.0_amd64.deb \
    target/docker-l2mcd.gz
}

do_image() {
  local plat=$1 bi=$WORK/tree-$1/sonic-buildimage t
  case "$plat" in
    vs)       t=(target/sonic-vs.img.gz); [ "$VS_DOCKER" = 1 ] && t+=(target/docker-sonic-vs.gz) ;;
    broadcom) t=(target/sonic-broadcom.bin) ;;
  esac
  make -C "$bi" SONIC_BUILD_JOBS="$JOBS" "${t[@]}"
}

collect() {
  local plat=$1 bi=$WORK/tree-$1/sonic-buildimage id out f
  id=$(build_id "$bi"); out=$OUT/$id
  mkdir -p "$out"
  case "$plat" in
    vs) for f in sonic-vs.img.gz sonic-vs.bin docker-sonic-vs.gz; do [ -f "$bi/target/$f" ] && cp "$bi/target/$f" "$out/"; done ;;
    broadcom)
      for f in sonic-broadcom.bin sonic-broadcom-legacy-th.bin sonic-broadcom-dnx.bin; do
        [ -f "$bi/target/$f" ] && cp "$bi/target/$f" "$out/"
      done
      # install.sh inside the payload carries image_version="<id>"; best effort
      for f in "$out"/*.bin; do
        local v; v=$(grep -a -m 1 '^image_version="' "$f" 2>/dev/null | cut -d'"' -f2 || true)
        [ -z "$v" ] || [ "$v" = "$id" ] || log "warn  $(basename "$f") says image_version $v, expected $id"
      done ;;
  esac
  cp "$PREP/MANIFEST" "$out/MANIFEST"
  (cd "$out" && sha256sum -- *.gz *.bin 2>/dev/null > SHA256SUMS || true)
  echo "$id" > "$out/BUILD_ID"
  log "images for $plat in $out (build id $id)"
  sed 's/^/        /' "$out/SHA256SUMS" | tee -a "$LOGS/build.log"
}

build_platform() {
  local plat=$1
  stage "tree-$plat" do_tree "$plat"
  stage "configure-$plat" do_configure "$plat"
  if [ "$FAILFAST" = 1 ] && [ ! -f "$LOGS/failfast.done" ]; then
    stage "failfast-$plat" do_failfast "$plat"
    touch "$LOGS/failfast.done"
  fi
  stage "image-$plat" do_image "$plat"
  collect "$plat"
}

summary() {
  log "summary"
  awk '{printf "        %-22s %4d min\n", $1, $2 / 60; s += $2} END {printf "        %-22s %4d min\n", "total", s / 60}' "$LOGS/durations" | tee -a "$LOGS/build.log"
  log "DX010 image: $( [ "$DX010_SAI" = xgs ] && echo sonic-broadcom.bin || echo sonic-broadcom-legacy-th.bin ) (DX010_SAI=$DX010_SAI)"
}

# ---------------------------------------------------------------------------
touch "$LOGS/build.log.start"
cmd=${1:-all}
log "build.sh $cmd: WORK=$WORK OUT=$OUT JOBS=$JOBS DX010_SAI=$DX010_SAI BUILD_NUMBER=$BUILD_NUMBER FOLLOW_HEAD=$FOLLOW_HEAD"
case "$cmd" in
  preflight) preflight ;;
  prepare)   [ "$SKIP_PREFLIGHT" = 1 ] || preflight; prepare ;;
  vs|broadcom) build_platform "$cmd"; summary ;;
  all)
    [ "$SKIP_PREFLIGHT" = 1 ] || preflight
    : > "$LOGS/durations"; rm -f "$LOGS/failfast.done"
    prepare
    for p in $PLATFORMS; do build_platform "$p"; done
    summary ;;
  *) echo "usage: $0 [all|preflight|prepare|vs|broadcom]"; exit 2 ;;
esac
