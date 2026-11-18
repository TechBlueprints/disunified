#!/bin/sh
# Builds the bridge for an Arista EOS switch itself into dist/eos/disunified/
# (docs/drivers/arista-on-switch.md §4): a static binary, run.sh and an
# example config, to be copied to /mnt/flash/disunified/ on the switch.
#
#   scripts/build-eos.sh [amd64|386]     (default amd64)
#
# EOS's userland is i686 on many platforms, but a static binary needs only
# the kernel: amd64 where `bash uname -m` on the switch says x86_64 (the
# DCS-7160 on 4.26.14M), 386 where it says i686 (not tried).
set -eu

arch=${1:-amd64}
case "$arch" in
amd64|386) ;;
*) echo "usage: $0 [amd64|386]" >&2; exit 2 ;;
esac

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

version=${VERSION:-"0.0.0+$(git log -1 --format=%cd --date=format:%Y%m%d).$(git rev-parse --short HEAD)"}
out="$root/dist/eos/disunified"
rm -rf "$out"
mkdir -p "$out"

CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.buildVersion=$version" \
    -o "$out/disunified" ./cmd/disunified
install -m 0755 deploy/eos/run.sh "$out/run.sh"
install -m 0644 deploy/eos/config.example.yaml "$out/config.example.yaml"

echo "built $out ($version, linux/$arch)"
echo "copy disunified and run.sh to /mnt/flash/disunified/ on the switch; see docs/drivers/arista-on-switch.md"
