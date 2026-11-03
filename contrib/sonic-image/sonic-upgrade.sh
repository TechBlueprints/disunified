#!/usr/bin/env bash
# sonic-upgrade.sh -- install a SONiC image so that a failed boot rolls itself back.
# Run on the switch as admin (uses sudo). Nothing here reboots on its own except the
# dead-man inside the new image.
#
#   sonic-upgrade.sh arm <image.bin> <sha256> [minutes] [trace]
#       verify the image, sonic-installer install it, keep the running image as the
#       default and make the new one boot ONCE (set-next-boot), and place a dead-man
#       timer in the new image's writable overlay: <minutes> (default 20) after the
#       new image boots it reboots -- into the default, i.e. the old image -- unless
#       `confirm` ran first. A hang or power cycle also comes back on the old image.
#       Then reboot by hand: sudo reboot
#   sonic-upgrade.sh status
#   sonic-upgrade.sh confirm      on the new image, once it is proven: disarm the
#                                 dead-man, remove it, make the new image the default
#   sonic-upgrade.sh rollback     on the new image: make the old image the default
#                                 again and reboot into it now
#
# State: /host/sonic-upgrade.state (old= new=). Never runs sonic-installer cleanup.
set -euo pipefail
# A reader that goes away (`| head`) must not stop arm/confirm/rollback halfway
# (seen: `confirm | head -n 1` left the image the default but not marked
# confirmed): SIGPIPE is ignored, sonic-installer's chatter goes to the log, and
# what this script prints cannot fail it.
trap '' PIPE
LOG=/host/sonic-upgrade.log

STATE=/host/sonic-upgrade.state
DISARM=/host/sonic-upgrade.disarmed
UNIT=sonic-upgrade-deadman

current() { sudo sonic-installer list | sed -n 's/^Current: //p'; }
images()  { sudo sonic-installer list | sed -n '/^Available:/,$p' | tail -n +2 | sed '/^$/d'; }
say()     { echo "sonic-upgrade: $*" 2>/dev/null || true; logger -t sonic-upgrade "$*" 2>/dev/null || true; }
inst()    { sudo sonic-installer "$@" 2>&1 | sudo tee -a "$LOG" > /dev/null; }   # set-default / set-next-boot, quietly

# The DX010 trial pieces (`arm ... trace`): placed in the new image's overlay,
# removed by confirm. They keep a hardware watchdog on the box through the first
# boot and record, synced to /host so a hard reset cannot lose it, what the boot
# did: a trace every 2 s, watchdog-control's disarm with its output and timing,
# and the CPLD watchdog fed by a loop that a kernel hang stops.
CPLD=/sys/devices/platform/dx010_cpld
TRIAL_UNITS="sonic-upgrade-trace.service sonic-upgrade-wdpet.service"
trial_files() {
  local rw=$1
  sudo mkdir -p "$rw/usr/local/sbin" "$rw/etc/systemd/system/sysinit.target.wants" \
                "$rw/etc/systemd/system/multi-user.target.wants" "$rw/etc/systemd/system/watchdog-control.service.d"
  # 1. the trace: one line every 2 s, synced
  sudo tee "$rw/usr/local/sbin/sonic-upgrade-trace.sh" > /dev/null <<'EOF'
#!/bin/bash
# sonic-upgrade trial trace: one line every 2 s to /host, synced, so a hard reset
# cannot lose it. The last line is the last moment the box was running.
log=/host/sonic-upgrade-trace.log
c=/sys/devices/platform/dx010_cpld
k() { cat /proc/sys/kernel/$1 2>/dev/null; }
echo "=== boot $(date -u +%FT%TZ) $(uname -r) $(cat /proc/cmdline | grep -o 'loop=[^ ]*') nmi_watchdog=$(k nmi_watchdog) hardlockup_panic=$(k hardlockup_panic) softlockup_panic=$(k softlockup_panic) panic=$(k panic) panic_on_oops=$(k panic_on_oops)" >> $log
f() { if [ -e $c/$1 ]; then [ -w $c/$1 ] && echo rw || echo ro; else echo no; fi; }
reg141() {   # the CPLD watchdog enable, read as the platform helper reads (flock'd)
  [ -e $c/getreg ] || return 0
  timeout 2 python3 -c "
import fcntl
with open('$c/getreg', 'w+') as g:
    fcntl.flock(g, fcntl.LOCK_EX); g.write('0x141\n'); g.flush(); g.seek(0); print(g.read().strip())
" 2>/dev/null
}
while :; do
  st=""
  for u in rc-local platform-modules-dx010 watchdog-control swss syncd sonic-upgrade-wdpet; do
    st="$st ${u%%.*}=$(timeout 2 systemctl is-active $u 2>/dev/null || true)"
  done
  cpld="setreg=$(f setreg) getreg=$(f getreg) wd141=$(reg141)"
  sdk=$(tail -c 300000 /var/log/syslog 2>/dev/null | grep -oE 'BDE dev 0[^,]*|SOC unit 0 attached[^,]*|_interrupt_connect[^ ]*|soc_init[^ ]*|sai_api_initialize|Init done|Start rpc server' | tail -n 1)
  kerr=$(dmesg --level=emerg,alert,crit,err 2>/dev/null | tail -n 1 | cut -c1-120)
  echo "$(date -u +%T) up=$(cut -d' ' -f1 /proc/uptime)$st $cpld wdu=$(pgrep -c -f 'watchdogutil' || true) pet=$(tail -n 1 /host/sonic-upgrade-wdpet.log 2>/dev/null | cut -c1-8) load=$(cut -d' ' -f1 /proc/loadavg) sdk=[$sdk] kerr=[$kerr]" >> $log
  sync $log
  sleep 2
done
EOF
  sudo tee "$rw/etc/systemd/system/sonic-upgrade-trace.service" > /dev/null <<'EOF'
[Unit]
Description=sonic-upgrade trial: boot trace to /host
DefaultDependencies=no
After=local-fs.target

[Service]
Type=simple
ExecStart=/usr/local/sbin/sonic-upgrade-trace.sh

[Install]
WantedBy=sysinit.target
EOF
  # 2. the CPLD watchdog, fed while the kernel runs
  sudo tee "$rw/usr/local/sbin/sonic-upgrade-wdpet.sh" > /dev/null <<'EOF'
#!/bin/bash
# sonic-upgrade trial: arm the DX010 CPLD watchdog for 120 s once the CPLD
# driver is up, and feed it every 20 s (registers as the platform's watchdog.py
# writes them: 0x141 enable, 0x142-0x144 the timeout in ms, 0x145 keepalive).
# A kernel hang stops the feeding: the CPLD resets the box within 120 s, and GRUB's
# default image boots. `stop` (shutdown, confirm) leaves it armed for 300 s, which
# the next boot's watchdog-control turns off.
cpld=${SONIC_UPGRADE_CPLD:-/sys/devices/platform/dx010_cpld}
log=/host/sonic-upgrade-wdpet.log
say() { echo "$(date -u +%T) $*" >> $log; sync $log; }
w() { echo "$1 $2" > $cpld/setreg; }
timeout_ms() {   # timeout_ms <seconds>
  local ms=$(( $1 * 1000 ))
  w 0x144 "$(printf '0x%x' $(( (ms >> 16) & 255 )))"
  w 0x143 "$(printf '0x%x' $(( (ms >> 8) & 255 )))"
  w 0x142 "$(printf '0x%x' $(( ms & 255 )))"
}
if [ "${1:-}" = stop ]; then
  if [ -e $cpld/setreg ] && timeout_ms 300 && w 0x141 0x1 && w 0x145 0x1; then
    say "stopped: CPLD watchdog left armed for 300 s (the next boot's watchdog-control turns it off)"
  else
    say "stopped: could not write the CPLD"
  fi
  exit 0
fi
n=0
until [ -e $cpld/setreg ]; do
  [ $(( n % 10 )) -eq 0 ] && say "waiting for $cpld (up $(cut -d' ' -f1 /proc/uptime) s)"
  n=$(( n + 1 )); sleep 1
done
if w 0x141 0x0 && timeout_ms 120 && w 0x141 0x1 && w 0x145 0x1; then
  say "armed: CPLD watchdog 120 s, fed every 20 s (up $(cut -d' ' -f1 /proc/uptime) s)"
else
  say "FAILED to arm the CPLD watchdog through $cpld/setreg"
fi
while :; do
  sleep 20
  if w 0x141 0x1 && w 0x145 0x1; then say "fed"; else say "FAILED to feed"; fi
done
EOF
  sudo tee "$rw/etc/systemd/system/sonic-upgrade-wdpet.service" > /dev/null <<'EOF'
[Unit]
Description=sonic-upgrade trial: keep the DX010 CPLD watchdog armed and fed
DefaultDependencies=no
# stopped before the CPLD driver goes, so `stop` can still leave it at 300 s
After=local-fs.target platform-modules-dx010.service
Conflicts=shutdown.target
Before=shutdown.target

[Service]
Type=simple
ExecStart=/usr/local/sbin/sonic-upgrade-wdpet.sh
ExecStop=/usr/local/sbin/sonic-upgrade-wdpet.sh stop

[Install]
WantedBy=sysinit.target
EOF
  # 3. watchdog-control's disarm, observed: the same command, its output and time
  #    to its own synced file; its constructor turns the CPLD off, and the feeder
  #    turns it back on within 20 s
  sudo tee "$rw/usr/local/sbin/sonic-upgrade-wdu.sh" > /dev/null <<'EOF'
#!/bin/bash
log=/host/sonic-upgrade-wdu.log
t0=$(date +%s)
echo "$(date -u +%FT%TZ) watchdogutil disarm, as watchdog-control runs it" >> $log; sync $log
timeout 300 /usr/local/bin/watchdogutil disarm >> $log 2>&1
rc=$?
echo "$(date -u +%FT%TZ) rc=$rc after $(( $(date +%s) - t0 )) s" >> $log; sync $log
EOF
  sudo tee "$rw/etc/systemd/system/watchdog-control.service.d/sonic-upgrade-trial.conf" > /dev/null <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/local/sbin/sonic-upgrade-wdu.sh
EOF
  # 4. panics: SONiC's own soft-lockup/oops/hung-task panics, plus hard lockups
  #    (NMI watchdog); named to sort after the image's 99-sysctl.conf (panic=10)
  sudo mkdir -p "$rw/etc/sysctl.d"
  sudo tee "$rw/etc/sysctl.d/zz-sonic-upgrade-trial.conf" > /dev/null <<'EOF'
kernel.softlockup_panic = 1
kernel.hardlockup_panic = 1
kernel.panic = 20
kernel.panic_on_oops = 1
EOF
  sudo chmod 755 "$rw/usr/local/sbin/sonic-upgrade-trace.sh" "$rw/usr/local/sbin/sonic-upgrade-wdpet.sh" "$rw/usr/local/sbin/sonic-upgrade-wdu.sh"
  for u in $TRIAL_UNITS; do
    sudo ln -sf "/etc/systemd/system/$u" "$rw/etc/systemd/system/sysinit.target.wants/$u"
  done
}

# Undo trial_files on the running image: stop the feeder (which leaves the CPLD
# armed for 300 s), turn the CPLD watchdog off, remove the pieces. The logs stay.
is_zero() { [[ "$1" =~ ^(0x)?0+$ ]]; }
cpld141() {   # the CPLD watchdog enable register, read back as the platform helper does
  sudo timeout 5 python3 -c "
import fcntl
with open('$CPLD/getreg', 'w+') as g:
    fcntl.flock(g, fcntl.LOCK_EX); g.write('0x141\n'); g.flush(); g.seek(0); print(g.read().strip())
" 2>/dev/null
}
trial_remove() {
  [ -f /etc/systemd/system/sonic-upgrade-wdpet.service ] || return 0
  sudo systemctl stop $TRIAL_UNITS 2>/dev/null || true
  if [ -e "$CPLD/setreg" ]; then
    local v i
    for i in 1 2 3; do
      echo "0x141 0x0" | sudo tee "$CPLD/setreg" > /dev/null
      v=$(cpld141)
      is_zero "$v" && break
      sleep 1
    done
    if ! is_zero "$v"; then
      sudo timeout 60 /usr/local/bin/watchdogutil disarm || true   # its constructor writes 0x141 = 0 too
      v=$(cpld141)
    fi
    if ! is_zero "$v"; then
      say "the CPLD watchdog did not turn off (0x141 reads '${v:-nothing}'): not confirming;"
      say "the feeder left it armed for 300 s, so the box resets into the default image ($old) unless 0x141 is cleared"
      exit 4
    fi
    say "CPLD watchdog off (0x141 = $v, read back)"
  fi
  for u in $TRIAL_UNITS; do
    sudo rm -f "/etc/systemd/system/$u" "/etc/systemd/system/sysinit.target.wants/$u"
  done
  sudo rm -f /etc/systemd/system/watchdog-control.service.d/sonic-upgrade-trial.conf \
             /usr/local/sbin/sonic-upgrade-trace.sh /usr/local/sbin/sonic-upgrade-wdpet.sh /usr/local/sbin/sonic-upgrade-wdu.sh \
             /etc/sysctl.d/zz-sonic-upgrade-trial.conf
  sudo rmdir /etc/systemd/system/watchdog-control.service.d 2>/dev/null || true
  sudo systemctl daemon-reload
  sudo /sbin/sysctl -q -w kernel.hardlockup_panic=0
  sudo /sbin/sysctl -q --system
  say "trial pieces removed (units, drop-in, sysctl; hardlockup_panic=$(cat /proc/sys/kernel/hardlockup_panic) panic=$(cat /proc/sys/kernel/panic)); logs kept: /host/sonic-upgrade-{trace,wdpet,wdu}.log"
}

arm() {
  local bin=$1 sum=$2 min=${3:-20} trace=${4:-} old new before rw
  [ -f "$bin" ] || { echo "no image file $bin"; exit 2; }
  echo "$sum  $bin" | sha256sum -c - || { echo "sha256 mismatch"; exit 2; }
  old=$(current); before=$(images)
  # an image armed before (and rolled back) is installed already: arming it again
  # would boot the config it migrated then, not today's -- install it afresh
  local binver; binver=$(sudo sonic-installer binary-version "$bin" 2>/dev/null || true)
  if [ -n "$binver" ] && echo "$before" | grep -qx "$binver"; then
    echo "$binver is installed already (an earlier arm). Remove it so it is installed"
    echo "afresh with the current config, then arm again:"
    echo "  sudo sonic-installer remove -y $binver"
    exit 2
  fi
  df -h /host | tail -n 1
  # the image's install.sh, run from a running SONiC, deletes every image but the
  # running one and the new one (a factory image included): say so first
  local gone
  gone=$(echo "$before" | grep -vx "$old" || true)
  [ -z "$gone" ] || say "the install will delete: $(echo "$gone" | tr '\n' ' ')(install.sh keeps only $old and the new image)"
  say "installing $bin beside $old"
  inst install -y "$bin"
  sudo tail -n 3 "$LOG" 2>/dev/null || true
  new=$(comm -13 <(echo "$before" | sort) <(images | sort) | head -n 1)
  [ -n "$new" ] || { echo "could not find the newly installed image (sonic-installer list)"; images; exit 3; }
  inst set-default "$old"
  inst set-next-boot "$new"
  rw=/host/image-${new#SONiC-OS-}/rw
  [ -d "/host/image-${new#SONiC-OS-}" ] || { echo "no /host/image-${new#SONiC-OS-} for $new"; exit 3; }
  sudo mkdir -p "$rw/etc/systemd/system/timers.target.wants"
  sudo tee "$rw/etc/systemd/system/$UNIT.service" > /dev/null <<EOF
[Unit]
Description=sonic-upgrade dead-man: back to the default image unless confirmed
ConditionPathExists=!$DISARM

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'logger -t sonic-upgrade "dead-man fired: not confirmed within $min min, rebooting into the default image ($old)"; /bin/systemctl reboot; sleep 900; /bin/systemctl reboot --force --force'
EOF
  sudo tee "$rw/etc/systemd/system/$UNIT.timer" > /dev/null <<EOF
[Unit]
Description=sonic-upgrade dead-man timer ($min min after boot)

[Timer]
OnBootSec=${min}min
AccuracySec=10s

[Install]
WantedBy=timers.target
EOF
  sudo ln -sf "/etc/systemd/system/$UNIT.timer" "$rw/etc/systemd/system/timers.target.wants/$UNIT.timer"
  sudo rm -f "$DISARM"

  # Carry what an image upgrade would otherwise lose and what the bridge needs to
  # get back in: the SSH host keys (a new image has none and generates new ones,
  # which the bridge's known_hosts would refuse), admin's authorized_keys (the
  # bridge's and the operator's keys) and admin's password (re-applied at boot).
  sudo mkdir -p "$rw/etc/ssh"
  sudo cp -a /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub "$rw/etc/ssh/"
  # mstpd and mstpctl, when the bridge built them on this switch (disunified-l2d,
  # docs/drivers/sonic.md 11.5): static binaries, so the new image needs no
  # rebuild -- which takes docker, DNS and the internet -- before spanning tree
  # comes back; the bridge reinstalls the rest of l2d in seconds.
  for b in mstpd mstpctl; do
    if [ -x "/usr/local/sbin/$b" ]; then sudo install -D -m 0755 "/usr/local/sbin/$b" "$rw/usr/local/sbin/$b"; fi
  done
  # The disunified bridge, when it runs on this switch (docs/drivers/sonic-on-switch.md
  # in the disunified repo): SONiC installs every deb in the new image's
  # platform/<platform>/ on its first boot (rc.local's firsttime block), so the
  # package comes back with the image; its config and state are in /host.
  local deb plat
  deb=$(ls -1 /host/disunified/disunified_*.deb 2>/dev/null | tail -n 1 || true)
  if [ -n "$deb" ]; then
    plat=$(sed -n 's/^onie_platform=//p' /host/machine.conf)
    if [ -n "$plat" ] && [ -d "/host/image-${new#SONiC-OS-}/platform/$plat" ]; then
      sudo install -m 0644 "$deb" "/host/image-${new#SONiC-OS-}/platform/$plat/"
      say "the bridge's $(basename "$deb") goes in with the new image's first boot (platform/$plat)"
    else
      say "WARNING: no platform/$plat directory in the new image; the bridge's deb is NOT carried -- dpkg -i it after the boot"
    fi
  fi
  sudo install -d -o admin -g admin -m 700 "$rw/home/admin" "$rw/home/admin/.ssh"
  sudo install -o admin -g admin -m 600 /home/admin/.ssh/authorized_keys "$rw/home/admin/.ssh/authorized_keys"
  sudo sh -c 'getent shadow admin | cut -d: -f2 > /host/sonic-upgrade.admin-hash && chmod 600 /host/sonic-upgrade.admin-hash'
  sudo mkdir -p "$rw/usr/local/sbin" "$rw/etc/systemd/system/multi-user.target.wants"
  sudo tee "$rw/usr/local/sbin/sonic-upgrade-carry.sh" > /dev/null <<'EOF'
#!/bin/sh
# set by sonic-upgrade.sh arm: give admin back the password it had on the old image
f=/host/sonic-upgrade.admin-hash
[ -s "$f" ] || exit 0
h=$(cat "$f")
[ "$(getent shadow admin | cut -d: -f2)" = "$h" ] || usermod -p "$h" admin
logger -t sonic-upgrade "carried admin's password, host keys and authorized_keys from the old image"
EOF
  sudo chmod 755 "$rw/usr/local/sbin/sonic-upgrade-carry.sh"
  sudo tee "$rw/etc/systemd/system/sonic-upgrade-carry.service" > /dev/null <<EOF
[Unit]
Description=sonic-upgrade: carry admin's password from the old image
Before=ssh.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/sonic-upgrade-carry.sh

[Install]
WantedBy=multi-user.target
EOF
  sudo ln -sf /etc/systemd/system/sonic-upgrade-carry.service "$rw/etc/systemd/system/multi-user.target.wants/sonic-upgrade-carry.service"
  if [ "$trace" = trace ]; then
    trial_files "$rw"
    say "trial pieces placed: boot trace, CPLD watchdog fed every 20 s (120 s), watchdog-control's disarm logged"
  fi
  printf 'old=%s\nnew=%s\narmed=%s\nminutes=%s\n' "$old" "$new" "$(date -u +%FT%TZ)" "$min" | sudo tee "$STATE" > /dev/null
  say "armed: default stays $old, next boot (once) $new, dead-man ${min} min after it boots"
  sudo sonic-installer list
  echo "now: sudo reboot   (then, on $new: sonic-upgrade.sh confirm, or let the dead-man return to $old)"
}

status() {
  sudo sonic-installer list
  [ -f "$STATE" ] && cat "$STATE" || echo "no $STATE"
  if sudo grep -q '^confirmed=' "$STATE" 2>/dev/null; then
    echo "confirmed: the dead-man and the carry-over are removed"
  elif [ -f "$DISARM" ]; then
    echo "disarmed ($DISARM)"
  elif [ -f "/etc/systemd/system/$UNIT.timer" ]; then
    echo "armed: the dead-man runs on this image"
    systemctl list-timers "$UNIT.timer" --all --no-pager 2>/dev/null | sed -n 1,3p || true
  else
    echo "armed: the dead-man waits in the new image (not running on this one)"
  fi
}

confirm() {
  # shellcheck disable=SC1090
  . <(sudo cat "$STATE")
  [ "$(current)" = "$new" ] || { echo "running $(current), not $new: nothing to confirm"; exit 2; }
  trial_remove    # first: if the CPLD will not turn off, the dead-man must still be armed
  sudo touch "$DISARM"
  sudo systemctl stop "$UNIT.timer" 2>/dev/null || true
  sudo rm -f "/etc/systemd/system/$UNIT.service" "/etc/systemd/system/$UNIT.timer" \
             "/etc/systemd/system/timers.target.wants/$UNIT.timer"
  sudo rm -f /etc/systemd/system/sonic-upgrade-carry.service \
             /etc/systemd/system/multi-user.target.wants/sonic-upgrade-carry.service \
             /usr/local/sbin/sonic-upgrade-carry.sh /host/sonic-upgrade.admin-hash
  sudo systemctl daemon-reload
  inst set-default "$new"
  sudo rm -f "$DISARM"
  printf 'confirmed=%s\n' "$(date -u +%FT%TZ)" | sudo tee -a "$STATE" > /dev/null
  say "confirmed $new as the default; $old stays installed"
  if ls /host/disunified/disunified_*.deb >/dev/null 2>&1; then
    say "bridge: disunified $(dpkg-query -W -f '${Version}' disunified 2>/dev/null || echo 'NOT INSTALLED'), service $(systemctl is-active disunified 2>/dev/null || true)"
  fi
  sudo sonic-installer list
}

rollback() {
  # shellcheck disable=SC1090
  . <(sudo cat "$STATE")
  inst set-default "$old"
  say "rolling back to $old now"
  sudo reboot
}

case "${1:-}" in
  arm) shift; [ $# -ge 2 ] || { echo "usage: $0 arm <image.bin> <sha256> [minutes] [trace]"; exit 2; }; arm "$@" ;;
  status) status ;;
  confirm) confirm ;;
  rollback) rollback ;;
  *) sed -n '2,22p' "$0"; exit 2 ;;
esac
