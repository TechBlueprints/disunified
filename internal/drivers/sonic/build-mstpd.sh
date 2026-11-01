#!/bin/sh
# disunified: builds mstpd and mstpctl, statically linked, from the upstream
# release, inside a throwaway alpine container on the switch
# (docs/drivers/sonic.md 11.5). Nothing built here leaves the switch: the
# bridge redistributes no binary. Output: /out/mstpd, /out/mstpctl.
set -eu
VER=0.2.0
SHA=37cbc03ab14763a2d477d2230508ab98c2acec43fa869b985c264d44e125c386
# no libtool here: with it, -static links against musl's loader, which a
# Debian host does not have
apk add --no-cache build-base autoconf automake linux-headers >/dev/null
cd /tmp
wget -q -O mstpd.tar.gz "https://github.com/mstpd/mstpd/archive/refs/tags/$VER.tar.gz"
echo "$SHA  mstpd.tar.gz" | sha256sum -c -
tar xzf mstpd.tar.gz
cd "mstpd-$VER"
./autogen.sh >/dev/null 2>&1
./configure --prefix=/usr/local --sysconfdir=/etc >/dev/null
make -j4 LDFLAGS=-static >/dev/null
strip mstpd mstpctl
for b in mstpd mstpctl; do
  if readelf -l "$b" | grep -q INTERP; then echo "$b is not statically linked" >&2; exit 1; fi
done
cp mstpd mstpctl /out/
