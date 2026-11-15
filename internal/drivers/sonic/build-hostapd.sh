#!/bin/sh
# disunified: builds hostapd, statically linked, from the upstream release,
# inside a throwaway alpine container on the switch (docs/drivers/sonic.md
# 11.26), as build-mstpd.sh builds mstpd. Nothing built here leaves the
# switch: the bridge redistributes no binary. Output: /out/hostapd,
# /out/hostapd_cli.
#
# The build is an IEEE 802.1X authenticator for wired ports and nothing
# else: the wired driver (EAPOL on a port's netdev, PAE group address), a
# RADIUS client, the control interface disunified-l2d reads its events
# from. No nl80211, no TLS library (the EAP methods run on the RADIUS
# server, the switch only relays them), no WPS or DPP. EAP-MD5 and the
# integrated RADIUS server are built in for one reason: they let the
# switch run a RADIUS server of its own for a test (driver=none), so the
# authenticator can be proven without a server on the LAN.
set -eu
VER=2.11
# sha512 of hostapd-2.11.tar.gz from https://w1.fi/releases/, the value
# Alpine's aports pins for the same file (main/hostapd/APKBUILD)
SHA=1382af2a243e3a555507b55942f41d1233cc942826d26bc4f4dbe5e75560b3fe22e426948762fe162453e934685a69ef0c05723b7fecef71811fc1987e32b048
apk add --no-cache build-base linux-headers >/dev/null
cd /tmp
# a copy placed beside the script is used as is (the switch may have no
# route to w1.fi); either way the hash decides
if [ -f "/out/hostapd-$VER.tar.gz" ]; then
  cp "/out/hostapd-$VER.tar.gz" hostapd.tar.gz
else
  wget -q -O hostapd.tar.gz "https://w1.fi/releases/hostapd-$VER.tar.gz"
fi
echo "$SHA  hostapd.tar.gz" | sha512sum -c -
tar xzf hostapd.tar.gz
cd "hostapd-$VER/hostapd"
cat > .config <<'EOF'
CONFIG_DRIVER_WIRED=y
CONFIG_DRIVER_NONE=y
CONFIG_EAP=y
CONFIG_EAP_MD5=y
CONFIG_RADIUS_SERVER=y
CONFIG_CTRL_IFACE=y
CONFIG_TLS=internal
CONFIG_INTERNAL_LIBTOMMATH=y
CONFIG_NO_VLAN=y
CONFIG_NO_ACCOUNTING=y
EOF
make -j4 LDFLAGS=-static hostapd hostapd_cli >/dev/null
strip hostapd hostapd_cli
for b in hostapd hostapd_cli; do
  if readelf -l "$b" | grep -q INTERP; then echo "$b is not statically linked" >&2; exit 1; fi
done
cp hostapd hostapd_cli /out/
