#!/bin/sh
# disunified: the kernel's user-space STP hook (/usr/sbin/bridge-stp), called
# as `bridge-stp <bridge> start|stop` when a bridge's stp_state changes. An
# exit of 0 to `start` hands the bridge to user space (stp_state 2); anything
# else makes the kernel run its own 802.1D STP on it. SONiC's "Bridge" always
# goes to user space, where disunified-l2d runs mstpd and adds the bridge
# itself (the kernel holds the RTNL lock while this runs, so nothing here may
# talk to the bridge). Installed by the bridge's sonic driver.
case "$1:$2" in
Bridge:start|Bridge:stop) exit 0 ;;
esac
exit 1
