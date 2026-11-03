#!/bin/sh
# Builds the disunified .deb for linux/amd64 or linux/arm64 into dist/
# (docs/drivers/sonic-on-switch.md §4, §6).
#
#   scripts/build-deb.sh [amd64|arm64]     (default amd64)
#
# The version is $VERSION when set, else the tag on HEAD without its "v"
# (v1.2.3 -> 1.2.3; a prerelease v1.2.3-rc1 -> 1.2.3~rc1, which Debian sorts
# below 1.2.3), else <the last tag>+<yyyymmdd>.<sha> after a tag, else
# 0.0.0+<yyyymmdd>.<sha> (the commit's date). The binary's -build-version
# carries the same string.
#
# Packing needs dpkg-deb (Linux). Without it -- on a Mac -- the tree is
# staged and the script prints the dpkg-deb command to run on a host that
# has it (the switch itself, or a Debian container).
set -eu

arch=${1:-amd64}
case "$arch" in
amd64|arm64) ;;
*) echo "usage: $0 [amd64|arm64]" >&2; exit 2 ;;
esac

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

if [ -z "${VERSION:-}" ]; then
    stamp="$(git log -1 --format=%cd --date=format:%Y%m%d).$(git rev-parse --short HEAD)"
    if tag=$(git describe --tags --exact-match 2>/dev/null); then
        VERSION=$tag
    elif tag=$(git describe --tags --abbrev=0 2>/dev/null); then
        VERSION="$tag+$stamp"
    else
        VERSION="0.0.0+$stamp"
    fi
fi
version=${VERSION#v}
case "$version" in
*-*) version=$(printf '%s' "$version" | sed 's/-/~/') ;;   # a prerelease sorts below its release
esac

name="disunified_${version}_${arch}"
stage="$root/dist/deb/$name"
rm -rf "$stage"
mkdir -p "$stage/DEBIAN" "$stage/usr/sbin" "$stage/usr/lib/systemd/system" "$stage/usr/share/doc/disunified"

CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.buildVersion=$version" \
    -o "$stage/usr/sbin/disunified" ./cmd/disunified
chmod 0755 "$stage/usr/sbin/disunified"

install -m 0644 deploy/deb/disunified.service "$stage/usr/lib/systemd/system/disunified.service"
install -m 0644 deploy/config.example.yaml "$stage/usr/share/doc/disunified/config.example.yaml"
install -m 0644 deploy/env.example "$stage/usr/share/doc/disunified/env.example"
{
    echo "disunified -- https://github.com/TechBlueprints/disunified"
    echo "Independent project; not affiliated with or endorsed by Ubiquiti Inc."
    echo "\"UniFi\" and \"Ubiquiti\" are trademarks of Ubiquiti Inc."
    echo
    cat LICENSE
} > "$stage/usr/share/doc/disunified/copyright"
chmod 0644 "$stage/usr/share/doc/disunified/copyright"

sed -e "s/@VERSION@/$version/" -e "s/@ARCH@/$arch/" deploy/deb/control > "$stage/DEBIAN/control"
chmod 0644 "$stage/DEBIAN/control"
for s in postinst prerm postrm; do
    install -m 0755 "deploy/deb/$s" "$stage/DEBIAN/$s"
done

deb="$root/dist/$name.deb"
if command -v dpkg-deb >/dev/null 2>&1; then
    dpkg-deb --root-owner-group -b "$stage" "$deb"
    echo "built $deb"
    echo "on the switch: install it, and make it the copy an image install carries forward:"
    echo "  sudo dpkg -i $name.deb"
    echo "  rm -f /host/disunified/disunified_*.deb && cp $name.deb /host/disunified/"
else
    echo "staged $stage"
    echo "dpkg-deb is not on this host; pack the staged tree (modes kept) where it is, e.g.:"
    echo "  COPYFILE_DISABLE=1 tar -C $root/dist/deb -czf $name.tar.gz $name   # then, on a Debian host or the switch:"
    echo "  tar -xzf $name.tar.gz && dpkg-deb --root-owner-group -b $name $name.deb"
fi
