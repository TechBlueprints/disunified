# sonic — facts that cost time

Read `internal/drivers/CLAUDE.md` first. Specification and plan:
`docs/drivers/sonic.md` (§5 the read side, §6 the control side, §6.6 the
live results). Built 2026-10-27 on the podman driver's pattern against the
SONiC virtual switch (`202505.1238524-ff3c0c9f6`, hardware SKU
`Seastone-DX010`, L2 preset), adopted as `USWF07D`; every control feature
verified live on that build; no hardware yet.

- **One exec per poll: `python3 -` with `collect.py` on stdin**, reading
  Redis through `swsscommon.SonicV2Connector(use_unix_socket_path=True)` as
  the login user (no sudo). Only the listed tables; `SNMP_COMMUNITY` key
  names only. CONFIG_DB holds TACACS/RADIUS secrets: never widen this to a
  whole-database dump. Each Redis section prints one key per line, sorted,
  so a recapture diffs cleanly.
- **Separators differ per database**: CONFIG_DB and STATE_DB `|`
  (`PORT|Ethernet0`), APPL_DB and COUNTERS_DB `:` (`PORT_TABLE:Ethernet0`).
  APPL_DB `PORT_TABLE` also holds two markers that are not ports
  (`PortConfigDone`, `PortInitDone`); ports come from CONFIG_DB `PORT` only.
- **Speeds are Mb/s** (`100000` = 100G) in CONFIG and APPL. STATE_DB's
  `speed` is `4294967295` on the VS (and the CLI prints `4294967.3G`): never
  read it. The VS leaves `supported_speeds` empty and `fec` `N/A`, so speed
  caps fall back to the configured speed and FEC is unknown.
- **The L2 preset's CONFIG_DB PORT has no `mtu`, `fec` or `description`**;
  MTU comes from APPL_DB (9100). A missing CONFIG field is the default, not
  an error.
- **A cage is every PORT entry with the same `index`.** Broken-out child
  ports keep their parent's index, so folding by index covers every
  `BREAKOUT_CFG` mode; `Lanes` is the number of folded interfaces (1 for an
  unsplit cage), as the Arista driver does.
- **LLDP capability bits are the LLDP-MIB BITS string, MSB first**: `"28 00"`
  is bridge (0x20) + router (0x08). SONiC's own `LLDP_LOC_CHASSIS` reads
  `28 00`; the capture's value is the test.
- **The LLDP chassis id is eth0's MAC, not the system MAC** in
  `DEVICE_METADATA|localhost` (`sonic-cfggen` generates the latter on the
  VS). The system MAC is the device MAC; eth0's is `MgmtMAC`.
- **SONiC has no VLAN 1** (the CLI refuses it; 2..4094 only), and UniFi's
  untagged Default network is VLAN 1. Option `native_vlan` (default 4094,
  2..4094, anything else refused in `Open`) names the stand-in: an
  *untagged* member of `Vlan<native_vlan>` reports native VLAN 1, an
  address learned in it is on VLAN 1, and `Snapshot.VLANs` shows 1 in its
  place. A *tagged* membership of that id is reported as the id itself
  (the hazard §5.2a names: it would collide with a real UniFi VLAN of that
  number). A port in no VLAN at all is a routed port on SONiC and reports
  an empty `PortVLAN` (native 0). The lab guest runs every port untagged in
  `Vlan4094`. The control side must map the other way (VLAN 1 -> untagged
  in `Vlan<native_vlan>`).
- **FDB**: STATE_DB `FDB_TABLE|Vlan<id>:<mac>` -> `{port, type}`, no
  timestamps; a PortChannel's MACs go to its lowest member's port.
- **Uplink hint** when LLDP is silent (the lab guest): the up port on which
  the default gateway's MAC was learned. On the VS the gateway is reached
  over eth0 but its MAC is also learned in `Vlan4094` on Ethernet0, so
  port 1 is the hint.
- **eth0 is the OOB port** (`OOBInterfaces`); with the bridge dialling it the
  loop warns. Its `Up` is **carrier** (`operstate == "UP"` or `LOWER_UP`),
  never the admin `UP` flag: an unplugged port is admin UP, NO-CARRIER.
- **`DHCP` describes the address the bridge dials** (`Collector.Dial`, the
  host of the `ssh` target as given, resolved each poll): true only if that
  address is `dynamic` in `ip -j addr`, or sits on eth0 with no
  `MGMT_INTERFACE`. A stale lease on eth0 says nothing about a static
  in-band address. `Addresses` keeps every configured address (a down
  interface's too), the dialled one first. The gateway is the default
  route with the lowest metric on an interface with carrier; in-band, FRR
  installs the saved static route (`proto 196`, metric 20) beside eth0's
  DHCP route (metric 202). Fixtures: `collect.txt` (in-band, dialled on
  Vlan4094) and `collect-eth0-dhcp.txt` (the earlier state, dialled on
  eth0's lease).
- **SSH host key: SONiC 202505 ships only an RSA host key**, and its sshd
  (Debian 12) refuses the SHA-1 `ssh-rsa` signature. `internal/sshrun`
  restricts the offered host-key algorithms to the known_hosts key types;
  for an `ssh-rsa` entry it now also offers `rsa-sha2-512`/`rsa-sha2-256`
  (without that the handshake fails: "no common algorithm for host key").
- **Control side (§6.2), `control.go`.** Writes are `sudo config …` (and
  `sudo sonic-db-cli CONFIG_DB hset/hdel 'PORT|<p>' description` -- there
  is no `config interface description` on 202505), run as one `bash -s`
  batch under `set -e`, ending in `sudo config save -y`. `config` needs root
  **even for `--help`**; `Start` reads each group's help through sudo and
  fails on a missing sub-command (`config-help.txt` in the fixture dir is
  the captured help). The plan works on a copy of CONFIG_DB (`swState`) and
  applies each write's effect as it plans, so later steps see earlier ones;
  after a successful batch that copy becomes the state, so a second apply
  before the next collect writes nothing.
- **SONiC refuses VLAN config on a LAG member and on a mirror destination**,
  and allows one untagged VLAN per port: members and destinations are
  stripped first and restored after; a native change is `del` then `add
  -u`. The FixtureRunner models these refusals, so a wrong order fails a
  test. A LAG member reports its PortChannel's VLANs (and MTU); an emptied
  PortChannel is stripped and deleted.
- **A PortChannel's MTU must equal its members'** (seen live: "Port MTU of
  Ethernet16 is different than the PortChannel1 MTU size"). A new
  PortChannel is 9100 and `config interface mtu PortChannel<n>` is
  "Invalid port", so right after `config portchannel add` the driver runs
  `sonic-db-cli CONFIG_DB hset 'PORTCHANNEL|PortChannel<n>' mtu <members'
  MTU>` (APPL_DB `LAG_TABLE` follows), then the members join. When jumbo
  flips, the PortChannel's hset goes first and the members' `config
  interface mtu` after it. The FixtureRunner refuses a mismatched member.
- **Multi-VLAN forms** (probed): `config vlan member add|del -m a,b <port>`
  (a VLAN list for ONE port; a port list is rejected) and `-e -m <list>`
  ("every existing VLAN except these"); `config vlan add -m a,b`. Measured
  on the VS: a config call costs ~250 ms, a 31-port Allow All is ~8 s, and
  the first full push (31 MTUs + 31 VLAN commands) ~16 s -- which the
  loop's old shared 20 s budget cut short live. Writes now have their own
  budget (`informloop.Config.ApplyTimeout`, config `control.apply_timeout`,
  default 90 s); a collect keeps 20 s. Allow All is one `add -e -m
  <native_vlan> <port>` per port, never one call per membership. `-e`
  takes whatever VLANs exist at that moment: it is
  used only when the switch holds no VLAN outside the controller's list
  (EnsureVLANs runs first); otherwise the explicit `-m` list.
  `EnsureVLANs` refuses a list holding `native_vlan` whole; `ApplyPorts`
  skips a port that names it.
- **`hdel … description` leaves APPL_DB's copy behind**: clearing is `hset
  … description ''` (APPL_DB follows) then `hdel`; the description is read
  from CONFIG_DB only.
- **Device level (§6.3), `device.go`.** STP is PVST only and only with
  `FEATURE|stp`; IGMP and DHCP snooping are refused with a log. NTP and
  syslog: add the controller's, remove only what the bridge added
  (`<state_dir>/sonic-managed.json`, with the managed SNMP community);
  hostnames are resolved by the bridge (`Resolve` hook in tests). The SNMP
  community is a credential: commands are logged with `<community>`.
- **SSH keys** go in a marked block of the login user's authorized_keys
  (`# disunified-managed-begin/-end`), rewritten whole by one `bash -s --
  disunified-authorized-keys` script (awk drops the old block, mktemp + mv);
  `collect.py` prints only that block, never the operator's lines. A pushed
  key that is not `<type> <base64>` is refused (the controller fixture's
  `<pubkey>` placeholders are).
- **Manage → Restart arrives as a reply `_type: reboot`** (`reboot_type:
  soft`), not as a `cmd` (10.6.106, build 1238524, 2026-10-27); the session
  handles it like the reboot cmd: `sudo reboot` with `control.reboot`
  (verified live: back in ~2 min, the bridge reconnected), emulated
  otherwise.
- **The reported address follows the dial target.** The informs' `ip` /
  `connect_request_ip` came from the configured target at start and stayed
  stale after a move (the controller showed the old address). The loop now
  sets them from an applied static address, and after every collect from
  `devicemodel.AddressReporter`, which this Collector implements as its
  live dial target (`CurrentAddress`); `main` uses it at start too, so a
  restart after a move reports the persisted address.
- **SSH identity: a `.pub` in `ssh_key` selects one agent key.** The Mac's
  1Password agent holds an employer key first; `sshrun` offered it and Go
  gave up on every key when it refused to sign. `sshrun` now tries agent
  keys one handshake at a time, and `ssh_key: <file>.pub` offers only that
  agent identity (verified live: "authenticated with … Personal key").
- **Address (§6.4), `address.go`:** static only; add the new address (and
  route/DNS), dial it with `Redial` (same user, key, known_hosts) and run
  `true`; on no answer the new address (and route) are removed again.
  known_hosts must carry the switch's key under the new address. Default
  route lives in `STATIC_ROUTE|default|0.0.0.0/0` (`cfg_route`), DNS in
  `DNS_NAMESERVER` (`cfg_dns`).
- **`promote_secondaries` is 0 on SONiC** (`/proc/sys/net/ipv4/conf/<iface>/promote_secondaries`):
  deleting a subnet's primary address deletes its secondaries, and a new
  address in the same subnet is a secondary. A live move removed the old
  address and lost the new one with it -- no IPv4 on Vlan4094, switch
  unreachable. The move's batch over the new address is now `echo 1 | sudo
  tee …/promote_secondaries` (no `sysctl` on admin's PATH), `config
  interface ip remove <old>`, `ip -4 -o addr show dev <iface> | grep -q '
  <new> '` (fails the batch before save), stale route del, `config save
  -y`; only then the runner switches and `dial` is persisted in
  `sonic-managed.json`. A failed dial falls back once to the pending /
  persisted / config-file address (`dial.go`). The FixtureRunner models
  the sysctl (`IgnorePromote` for the failure path).
- On the VS a `config reload` logs "eth1: mtu greater than device maximum"
  for a port MTU of 9100/9216: a virtio artifact, not an error to SONiC.
- **Jumbo** is switch-wide in UniFi (`switch.jumboframes` + `switch.mtu`)
  and a port MTU on SONiC: `PortDesired.MTU` (set by the loop when the key
  is in the push; 0 = leave alone) is the pushed `switch.mtu` when jumbo is
  on (9216 on every recorded push; SONiC accepts it, probed, APPL_DB
  follows) and 1500 when off. PortChannels follow the same value. The
  driver implements `devicemodel.JumboSwitcher`, so the loop's "always
  forwards jumbo" note is not printed for it.
- **NTP/syslog hostnames are pinned.** SONiC takes addresses only, and a
  pool answers a different one on every lookup: re-resolving each push
  churned `ntp add <new>; ntp del <old>` every cycle (live). The first
  resolution is kept in `sonic-managed.json` (`pinned`, keyed `ntp|<host>`
  / `syslog|<host>`) while the hostname stays in the push and the address
  stays configured; it is re-resolved when the address is gone from the
  switch (by hand, or at a bridge start) or the hostname left and came back.
  A name that does not resolve is logged once and skipped.
- **SNMP needs the site's SNMP setting on to be pushed.** Enabling SNMP on
  the device (location/contact) only stores `snmp_location`/`snmp_contact`
  on the device record; the controller pushes `switch.snmp.*` in
  system_cfg only when the site-wide SNMP setting (Settings -> CyberSecure)
  is on. It was off on site, so the community path is built and
  fixture-tested but unverified live. No recorded push carries
  location/contact keys (none of the controller captures has any
  `switch.snmp.*` key), so `config snmp location|contact` (both exist on
  202505, diffed against `SNMP|LOCATION` / `SNMP|CONTACT`; the VS default
  is `SNMP|LOCATION {Location: public}`) is not mapped yet -- capture a push
  with the site's SNMP on first.
- **STP port settings only with `FEATURE|stp`.** `collect.py` prints
  `cfg_storm` (`PORT_STORM_CONTROL`) and `platform_json`: `no`, or
  platform.json's `interfaces` block reduced to each parent's `index`,
  `lanes` and `breakout_modes` (older captures say `yes`).
- **Breakout (`breakout.go`) -- built 2026-10-27, HARDWARE-UNTESTED.** A
  cage speed its current `BREAKOUT_CFG` mode does not run picks the mode
  that runs it (its default speed preferred): DX010 100G/40G `1x100G[40G]`,
  50G `2x50G`, 25G/10G `4x25G[10G]`; a speed the mode runs is a lane-speed
  change on every lane. The write is `config interface breakout -f -y
  <parent> '<mode>' </dev/null`: `-f` because every port here has a VLAN
  membership (without it SONiC refuses with the dependency list), **no
  `-l`**: it loads `/etc/sonic/port_breakout_config_db.json`, which no
  SONiC build generates (absent on the VS) and whose absence fails the
  breakout; `/dev/null` because the breakout can still `click.confirm`
  about tables without YANG, and inside the `bash -s` batch that prompt
  would read the rest of the batch. The mode is quoted (`[` globs). The
  children (portconfig.py `get_config`: `Ethernet<n+laneoffset>`, the
  parent's index, the mode's default speed, no `admin_status` = down, RS-FEC
  at 50G+ per lane) get the cage's whole intent from the same plan (state,
  name, MTU, VLANs, storm control). Refused with a log on the VS, for a
  speed no mode runs, and for a cage in an aggregate or a mirror session
  (`-f` would drop them). A split cage's default name stays the parent's
  alias (`Eth2/1` -> `Eth2`); every mode's speeds are claimed. The tests
  build the DX010 variant from the VS capture plus the upstream
  `dx010-platform.json` / `dx010-hwsku.json` beside it.
- **Time zone (`zones.go`).** The controller pushes a POSIX rule
  (`system.timezone=MST7MDT,M3.2.0,M11.1.0`), and `config clock timezone`
  accepts only names from `timedatectl list-timezones`. The driver reads
  the switch's zone list with each zone's rule (`tail -n 1` of its TZif
  file; ~0.7 s, once per run, `zones.txt` is that capture) and writes the
  zone named like the rule (`MST7MDT`), else by its abbreviation, else the
  first match; a current zone with the same rule (America/Denver) is kept.
  A zone name in the push is taken as is. `DEVICE_METADATA timezone` is the
  read side (`System.Timezone`; `UTC` on the VS). Verify live.
- **Hostname** with `control.hostname` only: `config hostname <name>` then
  `config save -y` (hostcfgd sets the Linux hostname). SONiC's YANG caps
  the whole name at 63 characters, not per label. Verify live.
- **Locate (`locate.go`) -- HARDWARE-UNTESTED.** No LED in the `config`
  CLI; the platform API's `Chassis.set_status_led("off"|"green")` (DX010:
  `/sys/devices/platform/leds_dx010/leds/dx010:green:stat`) returns False
  on failure, so the command exits on its result. Toggled once a second
  while locating, green after; the VS's platform API raises
  `FileNotFoundError` (no `/etc/sonic/vs_chassis_metadata.json`): logged
  once per run, then report-only. pmon's system-health may set the LED
  too. `Start` also checks `config clock timezone` and `config hostname`.
- **Scrub**: `scripts/sanitize-sonic.py`. IPv6 link-local addresses are
  rewritten too (their EUI-64 half is a real MAC). The FDB keys put a MAC
  right after `Vlan4094:`; the MAC pattern must not refuse a preceding `:`.
  The capture's own `version` section keeps the build host string from
  `sonic_version.yml` (a CI host, not site data).
