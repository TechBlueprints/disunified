#!/bin/sh
# Started by EOS's own process manager (docs/drivers/arista-on-switch.md §4):
#
#   daemon disunified
#      exec /mnt/flash/disunified/run.sh
#      no shutdown
#
# Everything the bridge keeps is beside this script on the flash: the
# binary, config.yaml, env (the controller API key; optional) and state/.
# The reply log goes where config.yaml's log_dir says (a tmpfs).
dir=$(cd "$(dirname "$0")" && pwd)
if [ -f "$dir/env" ]; then
    set -a
    . "$dir/env"
    set +a
fi
cd "$dir" || exit 1
exec "$dir/disunified" -config "$dir/config.yaml"
