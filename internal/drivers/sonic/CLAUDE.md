# sonic — facts that cost time

Read `internal/drivers/CLAUDE.md` first. Specification and plan:
`docs/drivers/sonic.md` (§5 the read side, §6 the control side, §6.6 the
live results). Built 2026-10-27 on the podman driver's pattern against the
SONiC virtual switch (`202505.1238524-ff3c0c9f6`, hardware SKU
`Seastone-DX010`, L2 preset), adopted as `USWF07D`; the control features
verified live on that build except: a forced speed change (the live 100G was already the cage's speed, so no speed command ran), the driver's own aggregate creation (live it stopped at the PortChannel MTU refusal and was finished by hand; the fixed sequence is fixture- and replay-tested), a gateway change and the undo of a move whose new address does not answer (never run live) and SNMP (not pushed with the site's SNMP off); syslog verified live once the site's remote logging was switched on (`syslog.remote.*`, 2026-10-27); §7.5's four are fixture-tested
only. The lab session's real pushes are
`docs/fixtures/controller-10.6.106/replies-sonic.ndjson`, replayed with
control on in `TestReplaySonicLabSession`. **The DX010 arrived 2026-10-31**
(build `202505.1238526-ff3c0c9f6`, the VS's commit; adopted, placed);
`docs/drivers/sonic.md` §10 is what its first run fixed, and
`docs/fixtures/sonic-202505.1238526-ff3c0c9f6/collect.txt` is its capture
(`hardware_test.go`; the control tests stay on the VS capture, and the
DX010 rig borrows the VS's `config-help.txt`/`zones.txt`).

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
  caps fall back to the configured speed and FEC is unknown. The DX010
  fills them: `supported_speeds 40000,100000`, `supported_fecs none,rs`,
  `fec rs` on a linked port (absent on an empty cage), and the SAI FEC
  codeword counters. Its `ASIC_TEMPERATURE_INFO` reads all zeros (the
  board's `TEMPERATURE_INFO` has the ASIC sensor); PSU `max_power` is 900.
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
  place. A *tagged* membership of that id reads as tagged VLAN 1 too (the
  stand-in never reaches the controller); the control side never writes
  one, and a push that tags VLAN 1 on a port whose native VLAN is another
  is refused with a log (SONiC carries UniFi's VLAN 1 only untagged). A port in no VLAN at all is a routed port on SONiC and reports
  an empty `PortVLAN` (native 0). The lab guest runs every port untagged in
  `Vlan4094`. The control side must map the other way (VLAN 1 -> untagged
  in `Vlan<native_vlan>`).
- **FDB**: STATE_DB `FDB_TABLE|Vlan<id>:<mac>` -> `{port, type}`, no
  timestamps (an entry's learn time is the loop's tracker's,
  `docs/mac-table-times.md`); a PortChannel's MACs go to its lowest
  member's port.
- **Uplink hint** when LLDP is silent (the lab guest): the up port on which
  the default gateway's MAC was learned. On the VS the gateway is reached
  over eth0 but its MAC is also learned in `Vlan4094` on Ethernet0, so
  port 1 is the hint.
- **eth0 is the OOB port** (`OOBInterfaces`); with the bridge dialling it the
  loop warns. Its `Up` is **carrier** (`operstate == "UP"` or `LOWER_UP`),
  never the admin `UP` flag: an unplugged port is admin UP, NO-CARRIER.
  `AdminDown` is the missing `UP` flag: the DX010's eth0 is cabled but taken
  down on purpose (it shares the system MAC with every Vlan interface, and
  on the LAN beside the in-band address it cost 20--40 % of the uplink's
  packets) and keeps its last lease; the loop does not warn about that
  address (§10.6). **`ip link set eth0 down` does not survive a reboot**:
  after one, with eth0 cabled to a disabled port, dhclient re-applied its
  recorded lease minutes after boot -- `dynamic` address plus a `linkdown`
  default route with no metric that beat Vlan4094's metric 20
  (`ignore_routes_with_linkdown=0`), black-holing everything off the LAN.
  The unit had `config interface ip add eth0 169.254.1.1/16`
  (MGMT_INTERFACE: no DHCP on eth0 ever) from 2026-11-01 to 2026-11-07, when
  it was removed again for 202511's breakout (a gateway-less MGMT_INTERFACE
  row fails sonic-yang; a DHCP eth0 has `metric 202` there, §11.11); the
  loop does not warn about
  an IPv4 link-local OOB address without carrier (with carrier: the
  "connected" note). Fixture: `collect-20261101.txt`; the sanitiser keeps
  169.254/16.
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
- **LACP fast rate and min-links 1** on every PortChannel the push names, as
  UniFi's own SONiC switches run theirs (`docs/drivers/sonic.md` §9,
  2026-10-28): the create is `config portchannel add PortChannel<n>
  --fast-rate true --min-links 1` (202505's `add_portchannel`: `--fast-rate`
  true|false, default false; `--min-links` 1..1024, default 1; both written
  to `PORTCHANNEL`). One that holds anything else (made by hand, or by the
  driver before that day) gets `sonic-db-cli CONFIG_DB hset
  'PORTCHANNEL|PortChannel<n>' fast_rate true min_links 1` once. **teammgrd
  reads `fast_rate` and `min_links` only when it creates the team**
  (sonic-swss 202505 `cfgmgr/teammgr.cpp`, "cannot be changed after the LAG
  is created"), so the hset reaches a running LAG at its next start
  (reboot, `config reload`); the saved config holds it at once. Untested
  live; no capture holds a PortChannel (`cfg_lag` already prints every
  field, so a recapture with one on hardware needs no collector change).
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
- **A VLAN no push names is deleted (`vlan_prune.go`, `docs/drivers/sonic.md`
  §11.29, 2026-11-15)**, at the end of `ApplyPorts`, once the ports, the
  IGMP rows and a routed network's address have left it. The push lists a
  VLAN only while a port carries it or the switch routes it (a routed
  network no port carries is in `switch.routing_interface` only); the ECS
  removes such a VLAN with its members. Kept: `native_vlan`, the device's
  and every attached lane switch's list (`EnsureLaneVLANs`: a lane
  switch's list no longer replaces the device's), routed VLANs, and any
  VLAN with a member, an address, a VRF/link-local interface row, a relay
  or a snooping row the driver did not take out. Only from lists this run
  was given (`siteKnown`, each child's, `c.l3` where FRR runs); not with
  `control.ports: off`. The DHCP guard is re-derived after a delete. A
  known-empty list is not "unknown": `planFor` keeps `[]` (the old nil
  fell back to every VLAN on the switch for Allow All).
- **`hdel … description` leaves APPL_DB's copy behind**: clearing is `hset
  … description ''` (APPL_DB follows) then `hdel`; the description is read
  from CONFIG_DB only.
- **Device level (§6.3), `device.go`.** STP is PVST only and only with
  `FEATURE|stp`; IGMP snooping only through l2mcd (below). NTP and
  syslog: the controller's list is the switch's -- add what it names,
  remove every other server, a hand-added one too (2026-10-31; until then
  only what the bridge had added). `<state_dir>/sonic-managed.json` still
  records them, with the managed SNMP community and the pins;
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
  handles it like the reboot cmd: `sudo reboot` (verified live: back in
  ~2 min, the bridge reconnected); UniFi's Restart is never emulated.
  **The reboot can take the SSH session down before `sudo reboot`
  returns** (DX010, 2026-11-01, with disunified-l2d running: the switch went
  down 9 s after the request and the call hit the 90 s apply deadline,
  logged as `reboot: context deadline exceeded`). A session that drops
  after the command was sent (`sshrun.SessionDropped`: the channel closing
  without an exit status, a reset or closed connection, the deadline) is
  the reload under way: logged, reported as `reboot: switch reload
  issued`, and the dead connection closed so the next collect dials
  afresh. Dial and session-open errors and a non-zero exit stay errors
  (the command was never sent, or refused).
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
- **The kernel loses zebra's routes on a carrier blink on 202511
  (`kroute.go`, `docs/drivers/sonic.md` §6.4.1, 2026-11-15).** 202511's
  zebra.conf has `zebra nexthop kernel enable` (sonic-buildimage #25117;
  202505 forced it off). Kernel 6.12 deletes a nexthop object and every
  route using it, without a netlink message, when its interface loses
  carrier. zebra re-creates the object on up, never the route, unless it
  re-resolved the route while the interface was down (FRRouting/frr#20747,
  open). l2d's spanning-tree start blinks the Bridge's carrier for ~90 ms
  (every port blocking until the dummy forwards). At boot that cost the
  DX010 its IPv4 default for 11 h, while FRR said installed and offloaded.
  Facts that cost time:
  - **The fix is SONiC's own knob.** The driver writes `DEVICE_METADATA
    zebra_nexthop disabled` (saved), and `no zebra nexthop kernel enable`
    into the running zebra.
  - **The repair** takes the running config's `ip route 0.0.0.0/0` lines
    out and back in, when the kernel misses FRR's installed default on two
    collects in a row or still has it through an `nhid`.
  - **Re-install through `vtysh -c` lines, never `vtysh -f`.** A file is
    one mgmtd transaction in FRR 10, so a `no` and an add of the same route
    change nothing.
  - **The symptom to look for**: FRR's nexthop without `fib` while the
    route says installed.
  - **`ip nexthop del id <n>` reproduces the end state**: zebra re-creates
    the object within 10 ms and leaves the route out.
  - **`show running-config` from vtysh carries `password` lines**:
    collect.py takes only the lines it needs.
  - **A dummy port's `carrier off` under a bridge reaches the bridge only
    after linkwatch's 1 s rate limit**: under 1 s it is coalesced and
    nothing happens.
- **At boot the bridge starts before the switch is one (`starting.go`,
  `docs/drivers/sonic.md` §6.4.2, 2026-11-15).** `After=swss.service` is
  met once the swss container runs. On the DX010's boot Start collected
  15 s before vlanmgrd made `Vlan4094`, with CONFIG_DB whole, APPL_DB
  portless and no in-band address. `informloop.Run`'s first reconcile ran
  on that snapshot 40 s later: Start's 20 `config --help` reads take
  ~30 s. Start now collects again (5 s, up to 5 min, then fails for
  `main` to retry) until APPL_DB has `PORT_TABLE:PortInitDone` and the
  kernel carries the dialled address CONFIG_DB names. Runtime collects
  are unchanged. **A boot's timeline is in `/var/log/syslog`, not the
  journal**: `journalctl -b` holds the kernel, systemd and the bridge,
  but the containers' daemons (`swss#portsyncd`, `swss#intfmgrd`,
  `swss#orchagent`, `bgp#bgpcfgd`) log only to syslog.
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
  forwards jumbo" note is not printed for it. **A LAG member's MTU is never
  `config interface mtu`**: SONiC refuses it on a member whatever the value
  ("Error: 'interface_name' is in portchannel!", probed 2026-10-27), and
  under `set -e` that killed every later batch until the LAG went. The
  PortChannel moves by `hset 'PORTCHANNEL|…' mtu` and each member's own
  `hset 'PORT|<member>' mtu` with it (APPL_DB follows, probed); a member
  whose PortChannel already runs the wanted MTU is satisfied. **The loop's
  first-push hold leaves the MTU out** (`withoutMTU`, Clint 2026-10-31,
  §10.2): the DX010's ports ran SONiC's 9100 against a new device's jumbo
  off and the hold never released; jumbo follows UniFi from the first push
  like NTP and the hostname.
- **Address-move guards:** the dialled address must be static, in-band
  (Vlan/Ethernet/PortChannel/Loopback) and in CONFIG_DB (VLAN_INTERFACE,
  INTERFACE, PORTCHANNEL_INTERFACE -- added to `cfg_mgmt` -- or
  LOOPBACK_INTERFACE); a DHCP lease and eth0 (MGMT_INTERFACE: one entry,
  an add replaces it, its own `gwaddr`) are declined with the reason.
  **SONiC holds at most three nameservers** ("The maximum number (3) of
  nameservers exceeded", probed): removals go before adds.
- **Write budget:** the driver implements `devicemodel.ApplyBudget` (90 s);
  other drivers get the collect budget unless `control.apply_timeout` is
  set.
- **NTP is sourced from the in-band interface (§10.6).** SONiC's
  `NTP|global src_intf` comes from `init_cfg.json` as `eth0`;
  `chrony.conf.j2` writes `bindacqdevice <src_intf>` only while that
  interface has an address in its CONFIG_DB table, so once eth0 had one
  chrony bound to the dead port (every source `^?`). With NTP managed, the
  driver sets `sonic-db-cli CONFIG_DB hset 'NTP|global' src_intf <iface>`
  (no CLI on 202505; hostcfgd regenerates chrony.conf) to the interface
  carrying the dialled address when its address is in an in-band CONFIG_DB
  table (`ntpSource`); never when `NTP|global` is absent or the bridge
  dials eth0. `collect.py` reads it as `cfg_ntp_global` (captures before
  2026-11-01 lack it; the parser treats that as absent).
- **chrony's boot hook leaves every source offline (§10.6).** Two seconds
  after chronyd starts, a hook runs `chronyc onoffline` -- before Vlan4094
  and FRR's static default exist (~30 s later) -- and nothing runs it
  again (SONiC's `/etc/network/if-up.d/chrony` fires for ifupdown
  interfaces only). `collect.py` reads `chronyc -n -c sources` (CSV,
  reach in octal) and `chronyc -n activity` (the text form: "N sources
  online/offline"), read-only as admin. **Decide on `activity`**: in
  `sources` an offline source is `?` with reach 0, the same as an online
  one not yet polled (right after `onoffline`). With NTP managed and
  unchanged in that apply, servers configured, the source interface with
  carrier and 0 online / >0 offline, `chronyNudge` runs `sudo chronyc
  onoffline` once per collect (guarded by the snapshot pointer), logged
  once per episode, never counted as a setting. Fixtures, real:
  `chrony-synced.txt`, `chrony-offline.txt` (`sudo chronyc offline`),
  `chrony-after-onoffline.txt`.
- **NTP/syslog hostnames are pinned.** SONiC takes addresses only, and a
  pool answers a different one on every lookup: re-resolving each push
  churned `ntp add <new>; ntp del <old>` every cycle (live). The first
  resolution is kept in `sonic-managed.json` (`pinned`, keyed `ntp|<host>`
  / `syslog|<host>`) while the hostname stays in the push and the address
  stays configured; it is re-resolved when the address is gone from the
  switch (by hand, or at a bridge start) or the hostname left and came back.
  A name that does not resolve is logged once and skipped.
- **`/etc/sonic/snmp.yml` re-adds communities (§10.1).** The snmp
  container's `start.sh` runs `snmp_yml_to_configdb.py`, which adds every
  community the file names (`snmp_ro|rwcommunity`, `…communities`) to
  CONFIG_DB on every start, and `config snmp community add|del` restart
  that container; every image ships `snmp_rocommunity: public` there and
  `/etc/sonic` survives `sonic_installer`. A CONFIG_DB delete alone came
  straight back (live on the DX010: del + save every ~80 s). `collect.py`
  prints the file's names (`snmp_yml`), `System.SNMPCommunities` includes
  them, and the plan drops the unwanted ones from the file first (`bash -s
  -- disunified-snmp-yml`: PyYAML, other keys kept -- the importer exits 1
  without `snmp_location` --, temp file + `os.replace`, names on stdin).
  The FixtureRunner re-imports the file on every `config snmp community`
  call (`SNMPStart`), so a wrong order fails the idempotence check.
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
- **RSTP and loop protection without `FEATURE|stp`: disunified-l2d
  (`l2.go`, `l2d.py`, `bridge-stp.sh`, `disunified-l2d.service`,
  `build-mstpd.sh`; `docs/drivers/sonic.md` §11.5) -- built 2026-11-01; the
  mechanism verified live by hand on the DX010 the same night (root port,
  ASIC states, flush, BPDU guard, auto edge, loop protection, teardown) and
  the driver's own path after a deploy; on by default, `options.l2d: off` opts
  out.** l2d keeps mstpd's settings by reconciling against mstpd's own JSON
  report every 2 s, never on a reload signal alone: the driver writes the
  config twice per push (bridge, then ports) and a reload signal during a
  sync used to be lost (a down port's BPDU guard never reached mstpd, live). Live facts: tcpdump shows the trapped BPDUs tagged 4094 and mstpd
  parses them anyway; a transient unit holding mstpd in its cgroup cannot be
  started again under the same name; `docker rmi` needs the `name@digest`
  form; never `pidof` in l2d (sysvinit's logs a mid-scan exit to syslog at
  ERR, past any stderr redirect): mstpd's liveness is its pid file and
  `/proc`, read in-process. Facts that shape it, from the 202505 source: StpOrch
  creates a SAI STP object per instance id on first use and a VLAN mapped into
  one blocks every port without a state there, so port states go first (ASIC_DB
  `STP_PORT` count checked) and the VLAN after; deleting a VLAN removes every
  port state of its instance, so **one instance per VLAN, id = VLAN id**; a
  port-state key for a port orchagent does not know stops that queue for good
  (`return`), so only APPL_DB+STATE_DB ports are written and stale keys are
  dropped with a plain `Table` delete. mstpd is built on the switch in an
  alpine container (static; no libtool, or `-static` links musl's loader);
  the kernel hook must exist before `stp_state 1` or the kernel runs its own
  STP; `COPP_TRAP|stp` (`always_enabled`) traps BPDUs and goes with STP off.
  A first enable is armed with a 180 s dead-man timer the driver cancels over
  a fresh connection. Loop protection is `switch.port.N.keepalive`; `ld_mode`
  is a forced-speed key, not loop protection.
- **STP port settings only with `FEATURE|stp`** (the PVST path; without it
  disunified-l2d takes them, above). `collect.py` prints
  `cfg_storm` (`PORT_STORM_CONTROL`) and `platform_json`: `no`, or
  platform.json's `interfaces` block reduced to each parent's `index`,
  `lanes` and `breakout_modes` (older captures say `yes`).
- **A 25G lane cannot run RS-FEC, and that is the chip, not a table
  (tested 2026-11-16, `docs/fec-by-standard.md` §5).** The CLI, orchagent
  (`isFecModeSupported`), and the chip's SDK shell (`phy control xe9
  CL91=1`: "Operation failed", while the same command works on a 100G
  port) all refuse it. The SAI is the one layer that says
  `SAI_STATUS_SUCCESS` to `SAI_PORT_FEC_MODE_RS` on a lane, and it then
  runs the lane with **no FEC** (`CL74 = False, CL91 = False`): never
  bypass orchagent's FEC check. `bcmcmd "phy control <xeN>"` shows a
  port's real FEC (`CL74` FC, `CL91` RS); `bcmcmd "show pmap"` maps SONiC's
  lane numbers (the physical column) to the chip's port names. A split
  lane is the standard's `25GBASE-CR-S` host: CA-N and CA-S DACs, no CA-L,
  no standard 25G optic.
- **FEC follows the standard where UniFi says auto (§10.3, §11.37,
  `optic.go`, 2026-11-17).** A push without `port.N.fec` gets the first
  mode the standard permits for the cage's module at the interface's
  configured speed that STATE_DB `supported_fecs` lists
  (`internal/fecpolicy`, shared with `arista-eos`; `moduleMedium` maps
  xcvrd's compliance name; never add a FEC table here). An empty cage or
  an unknown module is left alone with a note; so is a module whose
  permitted modes the port lacks (CA-25G-L or a 25G optic on a lane:
  "cannot link within the standard"). Auto always means the preferred
  mode: a port configured otherwise is rewritten, link or no link (Clint,
  2026-11-17), which drops a link whose far end stays on the other mode;
  a FEC set in UniFi overrides auto. **After a breakout in the same plan the FEC
  waits one cycle** (`swState.fecsStale`): the new interfaces' list is
  not read yet, and portconfig's refusal of a mode they lack would end
  the batch after the breakout. Those writes are `plan.own`, and
  `PlanPorts` plans without them (`plan.controllerOnly`), so the
  first-push hold and fresh-port withholding never wait on something no
  controller setting can match. Live fact: the DX010's 100G CWDM4 uplink
  stayed down until RS was set by hand.
- **Speed "auto" follows the module too (§10.9, `moduleSpeed`,
  `moduleRuns`).** With no manual speed, a cage holding a known module
  keeps a configuration (ports x lane speed) the module runs and otherwise
  takes its native one: a breakout to the mode with that many ports that
  runs that speed (`breakoutSpeed` now takes a `laneCfg` and a writer), else
  a lane speed. xcvrd's SFF-8636 info has no "far end broken out" field, so
  a code that fits a straight cable and a breakout (CR4/CA-25G-L, AOC/ACC,
  SR4) runs both and keeps a split made for it. An empty cage or an unknown
  module keeps the old rule (split stays split; unsplit at native speed).
  The controller-only plan must skip these writes **entirely**, not just
  leave them uncounted: a planned join leaves the rejoined port admin down
  and VLAN-less until the plan's own startup, which would count as a change
  on every cycle and hold the first push for good (the mutation is tested).
  A planned breakout also forgets STATE_DB's `supported_fecs` for the cage
  (read at the old speed). Live facts (DX010, 2026-11-01, by hand): `config
  interface speed` 40000/100000 and a breakout to 4x25G and back, each
  without a swss/syncd restart, Ethernet0 unaffected; an empty cage split
  through UniFi stays split on auto (no module, no opinion).
- **The optic's name is its compliance code (§10.4).** xcvrd stores an
  SFF-8636 module's `specification_compliance` as Python's `str()` of a
  dict; `complianceName` parses it (`'Extended Specification Compliance'`
  when the 10/40G code says Extended). UniFi's own SONiC switch reports
  the same string (`100G CWDM4`) as `sfp_compliance`.
- **The optic's faults are the module's own flags (§10.10, `optic_fault.go`,
  2026-11-14).** `sfp_rxfault` on a UniFi switch is the module's loss of
  receive signal. It is not the link, and on the ECS it is not a
  threshold: its STATE_DB `rx_los` matched the controller's flags port
  for port. xcvrd keeps them in `TRANSCEIVER_STATUS_FLAG|<first
  interface>` as `rx<k>los` / `tx<k>fault` per channel. Each is the string
  `True`/`False`/`N/A`, the latched SFF-8636 bytes 3-4, re-read every 60 s.
  Note that `TRANSCEIVER_STATUS` holds only tx-disable on 202505+, and there
  is no `TRANSCEIVER_DOM_FLAG` for a QSFP28 (sff8636.py has no dom flags).
  The rules:
  - any channel's fault counts;
  - only with `TRANSCEIVER_INFO` present;
  - a split cage's lane k takes its channels in order.

  The section joins `laterSections` from `optic_fault.go`'s `init`. Every
  `collect*.txt` must start under the contract tests, so a capture in a
  new image directory needs that image's `config-help.txt` beside it.
- **Breakout (`breakout.go`) -- built 2026-10-27; 4x25G split and the join back verified live on the DX010 2026-11-01 (no swss/syncd restart, the uplink unaffected); on 202511 every mode, the lanes at 10G and 1G, and the ACL rebind on a split and a join, 2026-11-07 (§11.11).** A speed no mode lists that every current lane's STATE_DB `supported_speeds` has (1G on a 25G lane) is a lane speed: portconfig checks only that, and the split cage claims it. **The loop must not take a split it wrote for a new port**: `informloop.applyPorts` records the ports a push writes, or a join pushed in the cycle after the split is withheld for good (live, 2026-11-07). The controller refuses a speed outside the port's stored `speed_caps` (`api.err.InvalidInterfaceSpeedDuplex`) until an inform after the split has carried the new caps. A
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
- **A split cage reports its lanes' form, a whole one QSFP28 (`splitForm`,
  `docs/drivers/sonic.md` §10.3, live 2026-11-16).** The controller takes a
  `fec_mode` by the port's reported media and speed: QSFP28 at 40G and 100G,
  SFP28 at 25G, `api.err.InvalidFecMode` otherwise; no capability bit is
  involved. The UI's drawer sends a `fec_mode` with every forced speed of
  25G or more, so it cannot split a whole cage to 25G, nor join a split one
  by a forced speed: those are REST saves without `fec_mode`, or Auto with
  a module. The form is by the fastest speed a lane runs, never its
  configured speed (a lane switch's lanes too). A cage with a lane switch
  attached and the lane switch's uplink keep `cageForm` (QSFP28): the
  join's forced 100G is saved on the parent's cage
  (`docs/breakout-lag.md` §7).
  **xcvrd misses a module inserted after boot** on the DX010 (no
  `TRANSCEIVER_INFO`, while `sfputil show eeprom` reads it). The driver
  reads such a cage itself (`xcvr_read.go`, `docs/drivers/sonic.md`
  §11.36): `sudo sfputil show presence`, then `show eeprom -p` for the
  present cages xcvrd has no record of, the answer standing in for
  `TRANSCEIVER_INFO` until xcvrd has one; at start, once a minute while a
  cage is present and unread, every five minutes otherwise. sfputil's
  labels are xcvrd's fields by their display names, padding included. Why
  xcvrd misses the module is open (the register).
  **A port without link reports its configured FEC** (`lanePort`):
  STATE_DB has an operating `fec` only with link, and a split cage's down
  lanes configured `fc` were stored with no `fec_mode`
  (`docs/drivers/breakout.md` §16.5).
- **Time zone (`zones.go`).** The controller pushes a POSIX rule
  (`system.timezone=MST7MDT,M3.2.0,M11.1.0`), and `config clock timezone`
  accepts only names from `timedatectl list-timezones`. The driver reads
  the switch's zone list with each zone's rule (`tail -n 1` of its TZif
  file; ~0.7 s, once per run, `zones.txt` is that capture) and writes the
  zone named like the rule (`MST7MDT`), else by its abbreviation, else the
  first match; a current zone with the same rule (America/Denver) is kept.
  A zone name in the push is taken as is. `DEVICE_METADATA timezone` is the
  read side (`System.Timezone`; `UTC` on the VS). Verify live.
- **Hostname** from UniFi, no opt-in (2026-10-31; before that
  `control.hostname`, on by default for sonic only), because UniFi's own
  SONiC switches always take the UniFi name as their hostname (§9,
  2026-10-28): `config hostname <name>` then
  `config save -y` (hostcfgd sets the Linux hostname). SONiC's YANG caps
  the whole name at 63 characters, not per label. Verified live on the VS
  (`docs/drivers/sonic.md` §6.6). **A rename leaves syslog on the old
  name** (DX010, 2026-11-11): `rsyslog-config.sh` renders `$(hostname)`
  into rsyslog.conf's `$template SONiC*` lines as text, runs only at boot
  or on a syslog-server change, and hostcfgd never re-runs it; a plain
  `systemctl restart rsyslog` reloads the stale file. `collect.py`'s
  `rsyslog_tmpl` prints those template lines; once the Linux hostname
  equals CONFIG_DB's and a template still names another, the plan runs
  `systemctl restart rsyslog-config` -- never in the renaming batch
  (hostcfgd sets the Linux hostname after `config hostname` returns), so a
  rename reaches syslog one cycle later. Fixture:
  `sonic-igmp-202511.20261106-f2e25f54c/rsyslog-stale.txt`.
- **Locate (`locate.go`) -- verified live on the DX010 2026-11-01 (LED toggles ~1/s, green after).** No LED in the `config`
  CLI; the platform API's `Chassis.set_status_led("off"|"green")` (DX010:
  `/sys/devices/platform/leds_dx010/leds/dx010:green:stat`) returns False
  on failure, so the command exits on its result. Toggled once a second
  while locating, green after; the VS's platform API raises
  `FileNotFoundError` (no `/etc/sonic/vs_chassis_metadata.json`): logged
  once per run, then report-only. pmon's system-health may set the LED
  too. `Start` also checks `config clock timezone` and `config hostname`.
- **LACP and the hardware tables (§11.4, `hardware.go`, 2026-11-01).**
  `collect.py`'s later sections (`laterSections`: older captures lack
  them) are `state_lag_member` (STATE_DB `LAG_MEMBER_TABLE`, tlm_teamd's
  copy of `teamdctl <pc> state dump` -- no `docker exec`), `crm`
  (COUNTERS_DB `CRM:STATS`) and `hwmon` (sysfs chips: name, i2c device,
  `*_input`/`*_label`). A member is active when link up, `runner.selected`,
  `runner.aggregator.selected` and the partner's SYNC bit (0x08);
  `00:00:00:00:00:00` is no partner. Fans are one row per drawer
  (`Fan<N>`, mean of the rotors, `direction` as thermalctld says it); a
  supply's fan belongs to the supply. Community psud keeps the **output**
  power and one temperature; the ECS reports the **input** power, so the
  supply's PMBus chip is read too (`psuChips`: DX010 `PSU-1` = `10-005a`,
  `PSU-2` = `11-005b`, from the platform's `psu.py`; hwmon numbers move
  across boots). The FDB capacity is CRM used + available (8196 on the
  DX010). The shapes are held to the ECS's inform
  (`controller-10.6.109/inform-ecs-aggregation-uswf066.json`); nothing read
  off the ECS itself is kept in the tree (Clint, 2026-11-01: no text or code
  copied from it). teamd's records in the tests are upstream SONiC's own
  test data (`docs/fixtures/sonic-utilities-202505/`, Apache-2.0).
- **DHCP guarding and IP ACL rules (`acl.go`, `docs/drivers/sonic.md`
  §11.7.6, built 2026-11-01, verified live on the DX010 the same evening,
  §11.7.7: `VLAN_ID 4094` matches the stand-in's untagged frames, the
  guard's DROP beats a `DUI_IP_ACL` FORWARD, 512+ entries per table, ACL
  counter polling is off on the image so `aclshow` reads N/A).** Two tables of the driver's
  own, `DUI_DHCP_GUARD` (a type of its own since 2026-11-14, next item) and
  `DUI_IP_ACL` (L3), bound ingress to every port that is
  not a PortChannel member and every PortChannel with members (the CLI's
  `get_acl_bound_ports`; orchagent refuses a member). **Never `config acl
  update`**: acl-loader adds a DROP-all-IPv4 `DEFAULT_RULE` to every ingress
  L3 table it loads. Tables: `config acl add table <t> L3 -p <list> -s
  ingress` (also the rebind: the CLI replaces the entry, orchagent updates
  the ports) and `config acl remove table` (leaves the rules in CONFIG_DB, so
  they go first). Rules: `sonic-db-cli CONFIG_DB hset|del 'ACL_RULE|DUI_…'`,
  a changed one `del` then `hset` (orchagent re-creates a rule from the whole
  hash). The list field is `ports@` and is in Python set order: compare as a
  set. STATE_DB `ACL_TABLE_TABLE|<t>` / `ACL_RULE_TABLE|<t>|<r>` hold
  `Active`, `Inactive`, `Pending creation`, `Pending removal`; a rule whose
  table is missing gets no status (orchagent waits). `ApplyPorts` re-runs
  the ACL apply after its batch (breakout children, a new PortChannel).
  `collect.py` reads only `DUI_*` and CRM's ACL counters; the DX010 has
  three free ingress tables per PORT and LAG bind point. MAC rules go to
  `DUI_MAC_ACL`, type literally `L2` (sonic-acl.yang allows SRC_MAC/DST_MAC
  on no other type), only on an image with the kit's sonic-swss 0003: stock
  202505 and 202511 have no L2 table type and no outer-MAC match. `Start`
  greps the running orchagent for the patch's error string and claims
  `max_custom_mac_acls` 128 only then; without it a MAC push is refused
  with a log. A MAC set whose Block would drop the switch's own management
  frames is held (`cutsManagementMAC`). The UI refuses the management
  network for a MAC rule, and the DX010's one link carries only that
  VLAN, so no UI rule there matches a frame that reaches it (§11.7.8).
  **No ACL drop keeps a frame addressed to the switch from its CPU** on
  202511's libsaibcm `13.2.1.100` (traced 2026-11-15, §11.7.8). Every
  table is an ingress field-processor group at priority 100 whose entry
  is a plain `Drop`. That drop leaves the trap group's (GID 1, priority
  -1) `GpCopyToCpu` alone, for ip2me and DHCP alike, L3 and L2 tables
  alike. So judge a drop by the chip's counter, never by `tcpdump` on the
  switch: `bcmcmd "fp show group <gid>"` for the entry's stat id, then
  `bcmcmd "fp stat get si=<n> t=Packets"`. §11.7.7's CPU-side reading
  was on 202505's `13.2.1.36`.
  **The kernel drops that copy instead (`acl_cpu.go`, §11.32, live by
  hand and then through the driver from a UI rule, 2026-11-15, §11.32.5)**,
  as the ECS's tables keep blocked traffic off its own
  address (a switched host behind internal port `Ethernet56`, not a trap).
  The L3 tables are mirrored into iptables' **`raw`** table: caclmgrd
  flushes and deletes every filter-table chain on any `ACL_TABLE` event,
  the driver's own included (seen live: IPv4 `raw` untouched). The marker
  is `/run/disunified/acl-cpu` (collect.py `acl_cpu`), so a reboot
  rewrites it. It is the reconcile's only view of the kernel: a hand
  `iptables -t raw -F` that leaves the marker is not repaired. Testing it:
  a LAN host's raw socket can send from a documentation source address
  (Vlan4094's `rp_filter` is loose), so a rule needs no real host's
  address; dhclient holds `0.0.0.0:68` for eth0, so a 68 listener binds
  the switch's own address with `SO_REUSEADDR`. **Never write an
  `ACL_RULE|DUI_…` row without its `ACL_TABLE` row**: caclmgrd dies on every
  rule event whose table is absent, and again at each restart while the row
  stays (§11.7.10). For a rule that reaches no chip, give it a portless
  table (`type L3 stage ingress`, no `ports@`), and remove the table after
  the rules. The driver deletes orphan rules behind such a placeholder.
  **A set that would block the bridge's own SSH to the switch is held**
  (`cutsManagement`): the rules bind to the uplink, and the DX010 has no
  other way in; `collect.py`'s `ssh` section is `SSH_CONNECTION`'s client
  and server address. The push, captured on the DX010 2026-11-01
  (`controller-10.6.109/system_cfg-dx010-acl.txt`): `max_custom_ip_acls`
  alone gates it; Any is `ip=any`, TCP/UDP two groups, a Network side a
  gateway/prefix CIDR. Fixtures: `collect-acl.txt` (with the `ssh`
  section), and the DX010's own `config-help.txt` beside it (the older
  DX010 captures' rigs still borrow the VS's).
- **DHCP guarding's count per port (`acl_count.go`, `docs/drivers/sonic.md`
  §11.7.9, built and verified by hand on the DX010 2026-11-14).** One
  counted DROP per port, `V<v>_IN_<port>` at 8500 with `IN_PORTS`, between
  the FORWARDs and the shared DROP; `Health.RogueDHCPDrops` is each port's
  sum. Facts that cost time:
  - **The `L3` type has no `IN_PORTS`, and `L3V4V6` (which has) is
    unsupported here** (STATE_DB `ACL_STAGE_CAPABILITY_TABLE|INGRESS`
    `supported_L3V4V6 false`). So the guard has a CONFIG_DB `ACL_TABLE_TYPE`
    row of its own name: `MATCHES@`, `ACTIONS@`, `BIND_POINTS@`.
    swss-common strips the `@` before orchagent reads them.
  - **The type row and the table.** `config acl add table` takes any type
    name, and there is no CLI for the row. YANG makes the table's type a
    leafref: the row goes in before the table and comes out after it.
  - **orchagent refuses an `IN_PORTS` naming a port it does not have**
    ("Failed to locate port"), marks the rule `Inactive` and never retries.
    The driver writes counted rules only for ports in STATE_DB `PORT_TABLE`
    and rewrites an `Inactive` one.
  - **`IN_PORTS` is a plain string in YANG**, so `config interface breakout
    -f` does not clear it as a dependency. The port batch deletes the
    lanes' rules before the breakout.
  - **ACL counters reach COUNTERS_DB only with `FLEX_COUNTER_TABLE|ACL
    FLEX_COUNTER_STATUS enable`** (what `counterpoll acl enable` writes;
    off on the image). The driver turns it on with the guard.
    `ACL_COUNTER_RULE_MAP` maps `<table>:<rule>` to the counter oid, and
    `COUNTERS:<oid>` holds `SAI_ACL_COUNTER_ATTR_PACKETS`. A deleted
    rule's entry leaves the map.
  - **Testing on the DX010.** A reply can only arrive on Ethernet0, the
    one link.
    - A LAN host can send one: UDP from source port 67 to the switch's 68
      (macOS binds 67 without root).
    - Keep every test rule's match to that host's own address.
    - The deployed bridge leaves alone a `DUI_` table it does not manage
      (`DUI_RDTEST`). A hand-made `DUI_DHCP_GUARD` is one it would remove on
      its next cycle while the push says snooping off (by its code; not
      waited for): work right after an inform.
  - **Fixture:** `sonic-igmp-202511.20261106-f2e25f54c/collect-rdtest.txt`,
    with that test table and its live counts.
- **On the switch itself (`options.transport: local`, built 2026-11-02;
  `docs/drivers/sonic-on-switch.md`).** `sshrun.Local` runs every command
  through `bash -c` as the service's own user, and **that user must be the
  one `ssh:` names** (`Open` checks `user.Current()`): the keys script
  writes `$HOME/.ssh/authorized_keys` and `collect.py` reads the managed
  block from `$HOME`. `ssh:`'s host stays the switch's own in-band address
  (reported, and where a move starts). Nothing is dialled, so **the probe
  is the proof**: `options.probe` (main fills it from the inform URL's
  host:port for every driver; sonic uses it only on `local`) gets a TCP
  connect bound to the new address in `ApplyAddress` and an unbound one in
  `confirmL2` before the dead-man cancel; a failure undoes or keeps the
  timer, as a failed dial does. Without `SSH_CONNECTION` `collect.py`'s
  `ssh` section is empty, so the ACL hold takes the probe's host as the far
  end. **Only `/host` and `/etc/sonic` survive an image install**: the
  config, `env` and state live in `/host/disunified` (`/etc/disunified` is
  postinst's symlink to it), and the package comes back because
  **`rc.local`'s first boot installs every deb in the new image's
  `platform/<platform>/`** -- the upgrade script copies
  `/host/disunified/disunified_*.deb` there. postinst restarts with
  `--no-block` for that reason: it runs inside rc.local.
- **A VLAN's MTU is two things, and SONiC sets only one** (`vlanMTUs`,
  2026-11-02, `docs/drivers/sonic.md` §12). `vlanmgrd` makes the kernel
  `Bridge` at a fixed 9100 and every `Vlan<id>` netdev inherits it; the
  VLAN table's `mtu` reaches APPL_DB and the router interface but never the
  netdev (`setHostVlanMtu` in `cfgmgr/vlanmgr.cpp` is never called, 202505
  and 202511). With jumbo off that left the switch's own TCP at MSS 9048
  behind 1500-byte ports: the on-switch bridge's inform never got out. The
  driver writes both with the ports' MTU -- the CONFIG_DB field and `ip
  link set dev Vlan<id> mtu`, capped at the Bridge's MTU -- diffed against
  CONFIG_DB and `ip -j addr`'s `mtu`. The kernel half is lost on a swss
  restart or reboot; the start-up reconcile restores it after a reboot, and
  the next cycle's reconcile after a swss restart -- the loop reconciles
  whether or not the inform was answered (2026-11-02), so a bridge whose own
  informs the MTU breaks still repairs it. **Which VLANs:** the native
  stand-in, the push's `switch.vlan.*` list and the routed networks' VLANs
  (`routedVLANs`), which the list leaves out while no port carries them:
  until 2026-11-15 such a VLAN stayed at 9100 (`docs/drivers/sonic.md`
  §11.13). A VLAN the L3 plan makes is made at the ports' MTU, as
  `EnsureVLANs` makes the pushed ones. **An attached lane switch's VLANs
  too** (2026-11-15, §11.29.4): each VLAN takes the lowest MTU of the pushes
  listing it (`Collector.mtuWant`, each plan's last applied port MTU); a
  lane plan writes only VLANs the device's push does not list, so it never
  touches the native stand-in. The FixtureRunner models a new VLAN's
  netdev at 9100 only with `VLANNetdevs` (the recorded replays predate it).
- **IPv6 on the management interface (`ipv6.go`, `docs/drivers/sonic.md`
  §11.22, 2026-11-14).** SLAAC follows the management network's IPv6 in
  the controller (`rest/networkconf` `ipv6_interface_type` not `none`; the
  push carries no IPv6 key; `informloop/ipv6.go` asks through
  `devicemodel.ManagementIPv6Controller`). Facts that cost time:
  - **SONiC forwards on every router interface**, and Linux then ignores
    RAs unless `accept_ra` is 2. SONiC's own RA sysctls are for eth0 only.
    The driver writes 2 on the interface it manages the switch at
    (`ntpSource`) and nowhere else, with `echo 2 | sudo tee …/accept_ra`.
  - **A kernel-only address gets no ip2me route.** Inbound traffic then
    rides the default trap at the default CoPP group's 600 pps (an upload:
    0.9 MB/s). An APPL_DB `INTF_TABLE:<iface>:<addr>/<len>` row through
    ProducerStateTable fixes it: the ip2me group's 6000 pps, IPv4's ~8.8
    MB/s back to back. On 202511 intfsorch adds only
    that host route for a row: FRR already passes the /64 and the RA's
    default to the chip.
  - **Off is five steps**: the rows, `accept_ra` 1, `ip -6 addr flush …
    scope global dynamic`, `ip -6 route flush … proto ra`, and `ip -6 route
    del` of each on-link prefix the RA left (`proto kernel` with an expiry,
    which outlives the address).
  - **Lost on a reboot (sysctl) and a swss restart (rows)**; the reconcile
    restores both. CONFIG_DB-backed addresses and their rows are never
    touched.
  - **The RA's MTU option (9216) is ignored** above the link's 1500, so
    §12's MTU trap does not come back through IPv6.
  - Fixtures: `sonic-igmp-202511.20261106-f2e25f54c/collect-ipv6-{off,
    off-leftover-route,slaac,on}.txt`.
- **IGMP snooping (`igmp.go`, `docs/drivers/sonic.md` §8.10, end of §8.14;
  built 2026-11-03) -- only on the repo's 202511 image (`FEATURE|l2mcd`)
  and only with `options.igmp: on` (off until §8.9 H4, the traffic test with
  cabled hosts).** Measured on the DX010 by hand the same day: the IGMP traps
  install, the SAI takes the per-VLAN flag and programs (*,G) entries (the
  chip's `ipmc table show`); **optimised flood is refused** (the wildcard
  L2MC entry INVALID_PARAMETER, the VLAN's unknown-multicast group
  NOT_IMPLEMENTED) and orchagent retries it twice a second while a row says
  enable, so every `L2MC_SUPPRESS` row is deleted and
  `forward_unknown_multicast=false` only logged. **l2mcd programs a member
  only on an oper-up port**. **l2mcd and l2mcmgrd log to `/var/log/l2mcd.log`,
  not syslog**: an L2MC line in orchagent's syslog is explained there (a
  breakout's "deletes for a new lane" were another port's, found only in that
  file, §11.25.3). `bcmcmd "port ceN lb=mac"` brings a cage up
  with no cable (keep that port in a test VLAN only). Writes are whole rows
  through one `sudo python3 - disunified-l2mc` script (`ConfigDBConnector`,
  the ops as a JSON string on its last line), never a CLI call per field: a
  32-port VLAN's reserved groups are 224 rows. **l2mcmgrd reads
  `L2MC_STATIC_MEMBER`**; `L2MC_STATIC_GROUP` (`static-members@`) is the
  CLI's bookkeeping, kept in step. **The CLI refuses `vlan member del` of a
  port a static group names in that VLAN**, so the port plan deletes those
  rows first (`vlanMembers`), and `ApplyPorts` re-runs `applyIGMP` after
  the batch; the FixtureRunner models the refusal. **A breakout keeps the
  cage's L2MC rows** (§11.25.3, live 2026-11-15): the kit's sonic-buildimage
  0010 made every interface in the snooping YANG a string, so `-f` sees no
  dependency; the old port's rows become the new port's of the same name
  and a vanished lane's name nothing. The batch takes them out before the
  breakout (`l2mcUnbind`, one `python3 -c ... disunified-l2mc` line) and
  `applyIGMP` writes the new ports' after; the FixtureRunner's
  `SavedDangling` catches a save that writes one. `config vlan del` is
  refused while snooping is on (the VLAN delete, §11.29, comes after
  the IGMP apply has taken the VLAN's rows out). Version
  3 always (at 2 l2mcd ignores v3 reports); query interval = (membership -
  max response) / 2. The querier address has no l2mcd field (queries leave
  from the VLAN's own address, or 0.0.0.0: the patch in §8.11). **Live on
  the DX010 since 2026-11-05 (`options.igmp: on`):** the controller pushed
  the keys only after a `cmd/devmgr force-provision`; the DX010's push has
  `igmp_querier` empty; **l2mcd queries from 0.0.0.0 even with
  `INTERFACE_TABLE|Vlan4094|<addr>` present, wins every election, and learned
  no mrouter port** from the gateway's queries; the traps hand it every
  VLAN's IGMP. Push fixture: `controller-10.6.109/system_cfg-dx010-igmp.txt`.
  Fixture:
  `sonic-igmp-202511.20261102-e53777446/collect-snooping.txt` (the H3 state,
  `cfg_l2mc` section) and its `config-help.txt`.
- **A port leaving its last VLAN can be stranded by the kit's L2MC orch
  (`docs/drivers/sonic.md` §8.15, 2026-11-14; fixed by sonic-swss 0004,
  which the DX010 runs since 2026-11-14: `igmp-202511.20261114-42cccc87a`).**
  Before 0004, `removeBridgePort` sets the bridge port admin down, then
  refuses while `m_l2mc_count` is not zero, and `addBridgePort` reuses it as
  it is: the next VLAN added lands on a port that learns nothing until a
  swss restart, and the retried delete takes a re-added membership of the
  same VLAN away. The count leaks when a port that is a group member and a
  router port at once (the uplink, with UniFi's reserved groups; a looped
  test cage) loses link: router-port members were never counted. Facts that
  cost time:
  - **It is not the driver's order.** The leak came nine minutes before the
    move; the statics-before-delete order the CLI demands is right. Read
    orchagent's `l2mc_count` notices in syslog before blaming a batch.
  - **"No L2MC group member in ASIC_DB" is the leak's signature**, not a
    sign the references are gone: the count is orchagent's own.
  - **On an image without 0004** (`Start`: `l2mcFixCheckCommand`) the plan
    follows every delete that empties an interface with `bridgePortWait`
    (ASIC_DB, ≤ 10 s, fails the batch on a leak); `collect.py`'s
    `asic_bridge_port` shows a stranded one (warned once, its VLANs left
    alone with a note). The FixtureRunner models the refusal
    (`OrchL2MCFix`, `L2MCLeaked`, `Stranded`); fixture
    `sonic-igmp-202511.20261106-f2e25f54c/collect-stranded.txt`.
  - **On the patched image the plan adds no wait** and the count follows
    the chip: router-port members counted, a loop-and-drop back to 0, a
    native move getting a new bridge port admin true (live 2026-11-14,
    `sonic-igmp-202511.20261114-42cccc87a/l2mc-0004-live-20261114.txt`).
    The wait is for other images (the VS, an older build).
  - **Do not aggregate the uplink on an image without 0004**: taking
    Ethernet0 out of Vlan4094 would strand the only way in. (The DX010 has
    it now; check `show version` before relying on that.)
  - **A MAC-looped test cage is a router port within seconds** (live
    2026-11-14: RSTP Backup/discarding does not stop the trapped queries),
    so on an unpatched image every loop-and-drop leaks its count: never
    move such a cage out of its last VLAN before a swss restart or reboot.
    The race with members but no router port needs a cabled host. The
    looped one ran on the patched image 2026-11-15 (§8.15): each router
    member is `removed with its port`, the count goes to 0, a new bridge
    port admin true (`l2mc-0004-stepc-20261115.txt`).
  - The orch tests run in the build host's bookworm slave
    (`tests/mock_tests`, `--gtest_filter=L2mcOrchTest.*`), as root
    (`docker run -u root`), with the tree's `target/debs/bookworm` installed.
- **A LAG's L2MC member keeps the egress ports it was made with
  (`docs/drivers/sonic.md` §8.16, measured on a test LAG 2026-11-15; fixed
  by sonic-swss 0005, not yet in an image).** The legacy-TH SAI programs
  one egress per LAG member distributing at creation and never follows a
  change. A removal after another member started distributing is refused
  ITEM_NOT_FOUND (`mcast egress remove failed with error Entry not found`).
  Before 0005, `RemoveL2mcEntry` had removed the entry first and kept the
  record: every later removal logs `rv:-7` and the group never comes back
  into the chip until a swss restart or reboot. Facts that cost time:
  - **`rv:-7` at a PortChannel or VLAN removal is the symptom**: find the
    first `Failed to remove mrouter from l2mc group member` (the trigger is
    the last group member leaving), and the LAG's
    `setDistributionOnLagMember` lines between the member's creation and it.
  - **Tolerating ITEM_NOT_FOUND does not help**: sairedis keeps the member
    after a refused remove, so the group can never be removed either.
  - **A refused member cannot come out with the original set either** (the
    SAI took the ports before the failing one); with no member distributing
    it can. Never do that to the uplink: it is the uplink down.
  - **Testing it needs no cabled LAG**: an LACP aggregate of looped cages
    never distributes, but a ProducerStateTable write of APPL_DB
    `LAG_MEMBER_TABLE:<pc>:<port>` `status enabled|disabled` makes orchagent
    set `EGRESS_DISABLE`, and an `L2MC_MEMBER_TABLE` row makes the member;
    `bcmcmd "mc show"` lists the egress ports. Taking a cage into an
    aggregate means its `DUI_MAC_ACL` binding out first (`ports@` hset;
    AclOrch rebinds as a diff) and the PortChannel's MTU by `hset`.
- **l2mcd drops a static member when the same group's learned membership
  ages out on that port** (seen on PortChannel1, 2026-11-15, §8.16): it
  sends `Group-DEL ... static:0`, and the static is not put back; its own
  dump (`l2mcd_ctl -s <f> -v <vid>`) then lists fewer ports in `Static-Group`
  than CONFIG_DB names. Open (the register).
- **Port isolation, MAC filter, egress limit, LLDP-MED, Voice VLAN,
  router ports (`portfeat.go`, `docs/drivers/sonic.md` §11.12, built and
  verified live on the DX010 2026-11-07).** Isolation is one SAI isolation
  group `DUI_ISOLATION` (members = bind ports = the isolated ports, a member
  as its PortChannel); the MAC filter is `learn_mode drop` on the port or
  its PortChannel plus static FDB entries per allowed MAC in every VLAN the
  port carries, off is `learn_mode hardware` **then** the hdel (portmgrd
  never passes a removed field on); the egress limit is
  `SCHEDULER|DUI_EGRESS_<kbps>` (bytes, pir = kbps x 125) named by
  `PORT_QOS_MAP|<lane> scheduler`; LLDP-MED and the Voice VLAN are lldpd
  custom TLVs (OUI 00,12,bb subtype 1 `00,01,04` / with voice `00,03,04`,
  subtype 2 the network policy) through `docker exec lldp lldpcli`, read
  back by collect.py's `lldp_med` (`<port> <caps>|- <policy>|-`). **APPL_DB
  only through ProducerStateTable**: a plain `sonic-db-cli APPL_DB hset`
  is never seen by orchagent (probed); the plan collects those writes into
  one `python3 ... disunified-appl '<json>'` call at the end of the batch.
  The group, the FDB entries and the TLVs are lost on a swss or lldp
  restart and come back with the next reconcile. **lldpd also drops a
  port's TLVs when a breakout deletes it, lane 1's included though it keeps
  the name** (DX010, 2026-11-16): the breakout's effect forgets them in the
  plan's copy, so the split's own batch writes them on every new lane
  (`docs/drivers/breakout.md` §16.10). **A split or join the module rule
  makes is reported to the loop through `PlanLayout`**
  (`devicemodel.LayoutPlanner`): `PlanPorts` leaves it out, and the loop
  took such a cage for a new port and withheld it until a restart. **`learn_mode` is in no
  YANG model**: a breakout's batch hdels every `learn_mode` before `config
  interface breakout` and sets it again after (the ASIC keeps dropping). A
  static FDB entry on a port without link is not reported in the MAC table
  (UniFi showed allowed MACs as clients). The Multicast Router Port is
  `switch.vlan.<slot>.igmp_mroute_port.K` and becomes `L2MC_MROUTER` in
  `igmp.go`. Fan mode is a per-device setting (below). Fixtures: `collect-portfeat.txt`,
  `controller-10.6.109/system_cfg-dx010-portfeat-*.txt`.
- **Layer 3 switching (`l3.go`, `docs/drivers/sonic.md` §11.13, built and
  verified live on the DX010 2026-11-07).** Claimed wherever `FEATURE|bgp`
  is enabled. A routed network is `config interface ip add Vlan<id>`
  (`config vlan add` first: no port need carry it); routes are whole
  STATIC_ROUTE rows (aligned comma lists, or bgpcfgd drops the row); the
  controller's Inter-VLAN routing transit puts its default route beside the
  management gateway, which drops to distance 200 -- only while the transit
  VLAN reaches a port with link (a `Vlan<id>` netdev is up with no member,
  and FRR would black-hole the switch's own traffic); `defaultRoute()`
  skips the driver's hop. DHCP server: busybox `udhcpd` in a transient unit
  `disunified-dhcpd-Vlan<id>` (community SONiC has no DHCP server); relay:
  `config vlan dhcp_relay` (each call restarts dhcp_relay); Isolate Network:
  `ISO_k` rules in `DUI_IP_ACL`. Ownership is `sonic-managed.json`'s `l3`.
  **`config interface ip remove` of an interface's last address refuses
  while FRR lists a static route through it and runs vtysh on stdin**: the
  plan removes routes first, waits for FRR (`disunified-rif-wait`), and
  gives the removal `</dev/null`. **Every `runBatch` ends in an echo marker**;
  output without it is an error (a command that reads stdin swallows the
  rest of a `bash -s` batch and bash exits 0 -- live, 2026-11-07). Fixtures:
  `collect-l3.txt`, `controller-10.6.109/system_cfg-dx010-l3-*.txt`
  (scrubbed with `sanitize-controller.py --subnet`).
- **BGP (`bgp.go`, `docs/drivers/sonic.md` §11.14, built and verified live
  on the DX010 2026-11-07).** UniFi's BGP on a switch is the operator's
  uploaded FRR file (`switch.bgp.1.raw_config_base64`); claimed beside L3
  routing. Loaded with `docker exec -i bgp sh -c '… mktemp … cat > "$f" &&
  vtysh -f "$f" …'`: **never `vtysh -f /dev/stdin`** -- FRR 10's vtysh forks
  a child per daemon and each reads the file, so only the first got it
  (live: exit 0, bgpd untouched). The driver owns the top-level objects the
  file opens (`sonic-managed.json` `bgp`), never one bgpd already ran
  (`LOCAL_VLAN_IPV4_PREFIX` is SONiC's), replaces them whole when the file
  changes, and loads the file again when collect.py's `frr_bgp` (object
  names only) lacks one (a bgp container restart renders bgpd from its
  templates). vtysh exits 2 on a bad line and echoes it: log line numbers
  only. A refused stanza's note carries its first word (a second after
  `interface`/`router`/`ip`…): two words would have logged `password <secret>`
  (caught by the test before any deploy).
- **OSPF (`ospf.go`, §11.14, built and verified live 2026-11-07).** Claimed
  beside L3 (`feature_caps2` 4). SONiC's separated routing mode never starts
  ospfd (only frrcfgd's mode, `frr_mgmt_framework_config true`, does -- the
  driver refuses there), so the driver renders the router into
  `/etc/frr/disunified-ospfd.conf` in the bgp container (`/etc/sonic/frr` on
  the host; frr's, 0600: it holds the keys) and runs `ospfd -d -A 127.0.0.1
  -P 0 -f` on it; collect.py's `frr_ospf` is "running" and the file's sha256
  only. Changed router = restart ospfd; ospfd gone (bgp restart) = start;
  router deleted = stop + rm. A test peer on the kernel `Bridge` needs the
  `Vlan<id>`'s MTU or OSPF stalls in ExStart (MTU check): 9100 on
  2026-11-07, the ports' MTU since 2026-11-15 (§11.13); the peer's
  ospfd needs zebra and mgmtd beside it. Area ids are dotted:
  `sanitize-controller.py` keeps 0.0.0.0/8.
- **Routed ports and aggregates, DHCP fixed IPs (`l3.go`, `control.go`,
  `docs/drivers/sonic.md` §11.16, captured, built and verified live
  2026-11-08).** Claimed with L3 routing (`feature_caps2` 16). A routed port
  is a routing interface `type=port`/`port=<idx>` whose own keys still say
  `opmode=switch` and native VLAN untagged; a routed aggregate is
  `type=lag`/`lag=<first member>`, and **the controller pushes only that
  first member** (the rest are in no key). The loop sets
  `PortDesired.Routed` (no VLAN). **The device-wide apply runs before the
  ports**, so the address waits (note) while the port is a VLAN member, and
  `ApplyPorts` re-runs `applyL3` after its batch: one cycle. 202511's CLI:
  `ip add` refuses a VLAN or PortChannel member and silently does nothing
  (exit 0) on a PORT `mode` trunk/access; `vlan member add` refuses a router
  interface, so `syncVLANs` holds VLANs off an interface with an address
  (note) until the L3 apply removes it. A fixed IP is udhcpd `static_lease`
  (busybox keeps a static address from other clients, in range or not).
  VRRP is an MC-LAG group's option only: not claimed. The FixtureRunner
  models both refusals. Fixtures: `controller-10.6.109/
  system_cfg-dx010-{routed,fixedip}-*.txt`.
- **Fan mode (`fan.go`, `docs/drivers/sonic.md` §11.15, verified live on
  the DX010 2026-11-07).** The device's `fan_mode: default | quiet` (the
  controller keeps Fan Mode only for a gateway). Only
  `x86_64-cel_seastone-r0` with F2B airflow: Default = the stock table
  (MINTEMP 26, MINSTOP/MINPWM 89), Quiet = patch 0012's (35, 51/51 = 20 %;
  never lower). **Write `fancontrol-<airflow>`, not just `fancontrol`**:
  `/etc/init.d/fancontrol.sh` copies the direction file (from the CPLD's
  GPIO, recorded in `fan_airflow`) over `fancontrol` at boot, and pmon
  copies that into its `/etc/fancontrol` at start; the driver writes the
  direction file, `cp`s it over `fancontrol`, `docker cp`s it into pmon and
  `supervisorctl restart fancontrol` there (the fans run full for a few
  seconds). The read-back is all three (`collect.py` `fancontrol`); any one
  out of step reads as no mode and is rewritten. At Default the stock curve
  runs the fans at ~41 % (PWM 102) even with the intake under MINTEMP, as it
  did before 0012.
- **STP changes are l2d's count of kernel state changes (`state_changes` in
  `DUI_STP_PORT_TABLE`, 2026-11-15, `docs/drivers/sonic.md` §11.23).**
  mstpd's `num_transition_fwd`/`blk` stayed 0/1 through a looped cage's 368
  link changes (a Backup port is discarding up or down) and read 1 on a
  never-up port; the controller's STP-flap alert is a rate of
  `stp_state_change_count`, so the driver reads l2d's count (2 per drop)
  and falls back to mstpd's for an older l2d. Deployed 2026-11-15 (812503a):
  a cage flapped every ~6.5 s raised the DX010's own STP-flap alert, and a
  BPDU-guarded looped cage its BPDU-guard alert (§11.23.4). Facts that cost time:
  - **A looped cage with BPDU guard is shut by mstpd within a second**
    ("Received BPDU on BPDU Guarded Port"), but only the netdev; l2d holds
    the chip's port down since (next item). mstpd clears `bpdu guard
    error` only at the next link-up.
  - **`pkill -f <pattern>` over SSH matches the remote shell's own command
    line** when the pattern is in it, and kills the session (exit 255): use
    a script file, or `pgrep -f "[x]yz"` with a pattern the command line
    does not contain.
  - **The loopback toggle is slow**: each `bcmcmd "port ceN lb=..."` takes
    about 0.8 s, so a 2.5 s period cycles every ~4 s. Measure the rate from
    the payloads, not the period.
  - **`bcmcmd` reads stdin.** In a script fed over SSH (`ssh switch 'bash -s' <
    script`) the first `bcmcmd` swallows the rest of the script as drivshell
    input, and the steps after it never run (2026-11-15). Copy the script to
    the switch and run it with `< /dev/null`, and give each `bcmcmd` in it
    `< /dev/null` too.
- **A PortChannel member must hold no chip STP port (`l2d.py`
  `in_bridge`, `repair_strays`, `docs/drivers/sonic.md` §11.34,
  2026-11-15).** StpOrch's `addStpPort` creates a bridge port for a port
  that has none, so a state for a port leaving Bridge, taken after
  portsorch removed its bridge port, gave it a new bridge port and an STP
  port in every instance. When the port then joins a PortChannel the chip
  refuses the PortChannel's own STP ports ("Lag member N exists as STP
  PORT ... in STG n"), on the join and on every l2d restart, and the
  PortChannel's VLANs are not forwarded on that member in the chip:
  `bcmcmd "stg show"` is the check (`Block`/`Disable` on a member in a
  VLAN's STG while mstpd forwards). The ECS has the same shape l2d now
  keeps: a row for the PortChannel, none for its members. Facts that cost
  time:
  - **The kernel unlinks `brif/<port>` before it announces the leaving
    port disabled** (`del_nbp`), so "gone from brif" tells a departure from
    a link drop at the notification itself.
  - **The stray's bridge port has no orchagent path out** but a reboot;
    its STP ports go through a producer DEL (a no-op for an instance it
    holds none in).
  - **Only an up port triggers it**: a down port is already disabled, so
    leaving makes no state change (Ethernet4 was clean, Ethernet0 not).
- **A deleted VLAN's STP instance can come back with no VLAN (`l2d.py`
  `forget_vlan`, `repair_orphans`, `unmap_instance`; `docs/drivers/sonic.md`
  §11.35, 2026-11-15).** portsorch's `removeVlan` removes the instance and
  its STP ports. But orchagent runs portsorch before StpOrch in each pass,
  so a busy orchagent lets the VLAN's DEL overtake states l2d queued before
  it. `addStpPort` then makes the instance again, and StpOrch never removes
  one but when a VLAN leaves it. A deploy that restarted l2d 15 ms before
  the prune left nine. Facts that cost time:
  - **`bcmcmd "stg show"` is the check**: `contains 0 VLANs` with ports in
    it. ASIC_DB's STP objects carry no instance id: StpOrch keeps the ids
    in memory, so l2d keeps the ids of gone VLANs in
    `/run/disunified-l2d/stp-retired`.
  - **The one removal path is a VLAN mapped in and out**: a VLAN made in
    orchagent alone (APPL_DB `VLAN_TABLE` through the producer), mapped
    into the id and unmapped, then removed. Never for an id CONFIG_DB,
    ASIC_DB or APPL_DB has a VLAN for.
  - **Reproduce it with one late state**: `config vlan del`, then a
    producer SET `<port>:<vid>` right after `removeVlan`. A quiet delete
    is clean.
- **BPDU guard holds the chip's port down until the UI's Re-enable
  (`l2d.py` "BPDU guard", `recover.go`, `docs/drivers/sonic.md` §11.28,
  verified live on a looped cage 2026-11-15).** l2d holds a port mstpd
  reports tripped with APPL_DB `admin_status down`, as loop protection
  does. It is released by the controller's `port-recovery` (the driver
  writes STATE_DB `DUI_PORT_RECOVERY|<interface>`), by mstpd no longer
  guarding the port, or by STP off. The driver's fault follows l2d's
  `bpdu_guard_held`. Facts that cost time:
  - **portmgrd undoes an APPL_DB hold** whenever anything writes the
    port's CONFIG_DB row: it copies `admin_status` and runs `ip link set
    up`. l2d re-asserts both every pass, for loop protection's hold too.
  - **Release on mstpd's report, not on the config.** Released one pass
    before mstpd's guard went off, the port linked into the guard and was
    shut again with the guard off: left down (live).
  - **mstpd's error outlives a release with no link**, so a released port
    is not a new trip until its kernel state changes (a link-up). That
    holds even if the guard is switched off and on.
  - **The MAC loopback keeps the chip's PHY up on a disabled port**
    (`bcmcmd "port ce16"`: `*ce16 (Disabled ...`): APPL_DB `oper_status`
    stays up and the port reports up while held. The admin state is
    `bcmcmd ps`'s `!ena` and ASIC_DB `SAI_PORT_ATTR_ADMIN_STATE false`.
    A peer losing link needs a cable to see.
  - Fixtures: `sonic-igmp-202511.20261115-6c8eb1cf7/collect-bpduguard-held.txt`,
    `controller-11.0.99/reply-port-recovery.ndjson`.
- **Storms are detected, never held (2026-11-14, `docs/drivers/sonic.md`
  §11.20, §11.21).** UniFi's UI text: a storm anomaly is incoming traffic
  of the type over the Storm Control threshold, and Storm Control
  restricts; only loop protection and BPDU guard disable a port. The ECS
  does not claim storm control at all (`feature_caps` bit 8 clear), and its
  `ERROR_DISABLE` rows exist for every cause, detector or not. l2d's
  `StormWatch` reads `IF_IN_BROADCAST_PKTS` / `IF_IN_MULTICAST_PKTS` (MAC
  receive counters: before the policer, known multicast too) every second
  for each port with a `PORT_STORM_CONTROL` row, compares 5 s of them with
  kbps x 125 / 64 frames/s (x 1.25 to start, for the 1 s poll), and counts
  the frames above it into STATE_DB `DUI_STORM_TABLE` -> collect.py
  `l2_storm` -> `StormDropsBroadcast` / `StormDropsMulticast`; no fault.
  l2d runs for storm control alone. Unknown unicast: next item. **A port named by snooping's static groups cannot leave its VLAN
  by hand** (`config vlan member del` refuses), so a MAC-looped test cage
  stays in Vlan4094 and only RSTP's block keeps its frames off the LAN.
  Fixtures: `collect-storm.txt`, `storm-counters.ndjson`.
- **Unknown-unicast storms come from sFlow samples (2026-11-14,
  `docs/drivers/sonic.md` §11.24).** Probed on the DX010 first: the SAI
  refuses every `SAI_ACL_*_FIELD_*_NPU_META_DST_HIT` at table create
  (status -0x10000 + the field's index) and answers `get_policer_stats` for
  a storm policer with zeros while the chip's meter is red. The chip's
  ingress sampler sees frames before storm control drops them, but **the
  legacy-TH SAI samples only with `linux-bcm-genl` loaded** (its
  `/proc/bcm/genl/psample/rate`; else `cannot open knet link sample proc`),
  which the legacy-TH module package builds and never shipped: the image
  kit's sonic-buildimage 0014. It must load before syncd makes its netifs
  (loaded later it knows no ifindex). l2d's `UcastSampler` configures
  sFlow in CONFIG_DB as `config sflow` does for each port with
  `unknown-unicast` storm control: `SFLOW_SESSION|all` down first, the
  port's session (rate kbps x 125 / 64 / 25, at least 256), `SFLOW|global`
  up, `FEATURE|sflow` on, marked in `/etc/sonic/disunified/l2d-sflow` and
  turned off after sflowmgrd emptied APPL_DB. **APPL_DB sessions alone
  sample nothing**: coppmgrd installs the `sample_packet` trap only
  through the feature (its `always_enabled` path writes a trap-only group
  row, copporch fails it MANDATORY_ATTRIBUTE_MISSING and orchagent's
  `gOrchUnhealthy` then logs ERR every loop until it restarts). An
  operator's feature is left alone. l2d reads psample's `packets` group,
  and counts a sample as flooded
  when its DMAC is unicast, `OIFINDEX` 0xffff (bcm-genl: not a front port,
  not the CPU), not the switch MAC and not in the FDB for its VLAN.
  `StormWatch.usample` compares the sampled bytes/s with kbps x 125 (3
  standard errors to start or end) and adds the frames above it to
  `DUI_STORM_TABLE` `unicast` -> `StormDropsUnicast`. collect.py
  `genl_psample` says whether the image can; without it the driver logs
  once and nothing changes. **Hand-pushed ASIC_DB probes: SIGSTOP orchagent
  for the push** -- sairedis' `RedisChannel::wait` takes any `getresponse`
  as its own. Proven live 2026-11-15 on `igmp-202511.20261115-6c8eb1cf7` (cage 15, 15 745 frames above a 1000 kbps limit, `StormDropsUnicast` in `-collect-once`). Fixtures: `sonic-igmp-202511.20261115-6c8eb1cf7/` (`ucast-samples.ndjson`, `collect.txt`, `collect-ucast-storm.txt`).
- **No storm-control drop count exists (2026-11-14, `docs/drivers/sonic.md`
  §11.19)** -- hence the detection above. orchagent never registers the storm
  policers for statistics (no COUNTERS_DB key, no `POLICER` flex group), SAI
  has no storm drop reason for a `DEBUG_COUNTER`, and the Tomahawk's
  `IFP_STORM_CONTROL_METERS` are token buckets with no counter; the SDK's
  one debug trigger (`bcmDbgCntRxStormControlDrop`) covers all three types
  together. Do not report a total as a type.
- **A fresh capture** is `ssh <switch> 'python3 -' < internal/drivers/sonic/collect.py
  > raw` (as the login user, read-only), then `scripts/sanitize-sonic.py raw
  <image dir>/collect-<name>.txt` beside that image's `config-help.txt`. A
  reply log replayed over it is scrubbed with `sanitize-controller.py --ip
  <real>=<test>` for every address the capture's scrub mapped (pair the raw
  and scrubbed lines: same line count), its `resolv.host.1.name` set to the
  capture's `host-1` (`replies-dx010-vlan-prune.ndjson`, 2026-11-15).
- **Scrub**: `scripts/sanitize-sonic.py`. IPv6 link-local addresses are
  rewritten too (their EUI-64 half is a real MAC). A hardware capture also
  carries serials (chassis, PSUs, optics), the LLDP neighbour's system name
  (a site device name: `neighbor-N`) and the bridge's authorized_keys block
  (the site's public keys: `<type> SCRUBBEDKEY<N> key-N`); the communities
  of `cfg_snmp` and `snmp_yml` share one `community-N` mapping. The FDB keys put a MAC
  right after `Vlan4094:`; the MAC pattern must not refuse a preceding `:`.
  The capture's own `version` section keeps the build host string from
  `sonic_version.yml` (a CI host, not site data). The switch's own LLDP
  name becomes `host-1.example.net` once a DNS domain is set (it is the
  FQDN then).
- **202511 (the custom IGMP image, `igmp-202511.20261101-22cac574e`;
  `docs/drivers/sonic.md` §11.8, 2026-11-02; every mode verified live
  2026-11-07 once eth0's row was gone, §11.11).** `config interface breakout`
  validates CONFIG_DB with sonic-yang first: a dangling leafref anywhere
  refuses the split (`COPP_TRAP|stp` needs `COPP_GROUP|queue4_group1`, which
  l2d now writes with the trap; `MGMT_INTERFACE|eth0|...` needs
  `MGMT_PORT|eth0`, which the driver adds before a breakout when missing;
  and every `MGMT_INTERFACE` row needs a `gwaddr` of its own family, which
  `config interface ip add eth0 <ip>` without a gateway never writes -- the
  driver then refuses the breakout with a note (`mgmtNoGateway`) and never
  invents a gateway: interfaces.j2 makes it a default route on eth0 in the
  management table, which the syslog servers' forced rules follow; check a
  unit read-only with `sonic_yang.loadData()` over `sonic-cfggen -d
  --print-data`). **Port mirroring does not work on the DX010 on 202511**:
  STATE_DB `SWITCH_CAPABILITY|switch` `PORT_INGRESS_MIRROR_CAPABLE` /
  `PORT_EGRESS_MIRROR_CAPABLE` are `'false'`, the CLI refuses the session,
  and a `MIRROR_SESSION` row written past it makes orchagent create the SAI
  session without binding the source port ("setUnsetPortMirror: Port
  ingress mirror is not supported by the ASIC") and leak the object on
  removal. The driver refuses the mirror with a note
  (`portMirrorUnsupported`); the ASIC_DB object alone proves nothing, read
  orchagent's syslog. **The SAI itself does bind ports** (probed
  2026-11-02 through ASIC_DB's op queue: `SAI_STATUS_SUCCESS`, `bcmcmd
  "mirror show"` programmed): the gate is orchagent's capability query,
  fixable in the image (sonic.md §11.9).
  Upstream knows: sonic-buildimage #28139 (Broadcom TH5, same lie; Broadcom:
  fixed in SAI 15.2, backport to 14.3 -- not in any pinned version as of
  2026-10-30, and the DX010's legacy-TH train 13.2.1.x gets no fixes at
  all). **Treat every `SWITCH_CAPABILITY` `false` on this SAI as a claim,
  not a fact**: Broadcom's 2026 release notes correct capability tables
  repeatedly (RX SNR, WRED ECN stats, FEC corrected bits, TAM bind points);
  before building around a refusal, probe the SAI through ASIC_DB in one
  script (§11.9: op `Sset`, re-read every oid first). (Not so, though noted
  here until 2026-11-14: every PORT write adds `dhcp_rate_limit: 300` --
  the field was on every port before; below.) `config spanning-tree interface` spells `bpdu-guard` (202505:
  `bpdu_guard`); `config` gains `igmp-snooping`, `mld-snooping`,
  `dhcpv4_relay`. portconfig prints its FEC refusal (`fec fc is not in
  ['none', 'rs']`) on **stdout** with exit 1, so `sshrun`'s error carries
  the stdout tail too; a FEC outside STATE_DB `supported_fecs` is skipped
  with a note. **`config portchannel member add` refuses a port an ACL
  table binds** (`get_port_acl_binding`, 202505 too): the joining lanes
  leave the driver's `DUI_` tables inside the batch (`plan.aclUnbind`) and
  `applyACL` binds the PortChannel after; a memberless PortChannel with the
  driver's LACP settings that no port wants is removed (`ownOrphan`). The
  2026-11-02 pushes are `controller-10.6.109/system_cfg-dx010-{fec-cl74,
  breakout-25g,mirror,storm-rate,aggregate-acl}.txt`; the re-run after the
  deploy is `sonic-igmp-202511.20261101-22cac574e/rerun-20261102.txt`; the image's capture
  and `config --help` are `sonic-igmp-202511.20261101-22cac574e/`
  (`rig202511` in the tests). **Never push a bare `set` into
`ASIC_STATE_KEY_VALUE_OP_QUEUE`**: the producer prefixes ops with `S`/`D`
and the consumer strips one character, so `set` arrives as `et`, syncd
throws, and the whole swss stack restarts (2026-11-02, §11.9 of the
write-up: the op is `Sset`; in sync mode a failed SAI call is only
logged). **A swss restart recreates the kernel `Bridge`** (stp_state 0,
mstpd drops it): l2d re-runs its enable path when it sees that (ae2ce0e);
before that fix the switch ran without spanning tree until the service
was restarted.
- **`PORT dhcp_rate_limit` is a field nothing enforces (202505 and 202511,
  2026-11-14, `docs/drivers/sonic.md` §11.17).** The CLI
  (`config interface dhcp-mitigation-rate`) and YANG are sonic-utilities#3301;
  the enforcement, sonic-swss#3130 (portmgrd's `tc` ingress police on UDP
  67), is unmerged and not in the kit: every front-panel netdev has only its
  root `fq_codel`, no ingress filter (`dhcp-rate-20261114.txt`). The 300s
  come from db_migrator's `version_3_0_0` on a fresh config; a breakout's
  re-created ports have none (Ethernet4 since 2026-11-01).
- **The DHCP rate (anomaly 32) is counted by disunified-l2d (`l2d.py`
  `DhcpRate`, 2026-11-14, §11.17).** A detection, never a hold: UniFi's
  model does not shut the port. Facts that cost time:
  - **The image's CoPP caps DHCP at 100/s for the whole switch**
    (`queue4_group3`, shared with LLDP and UDLD; measured: a 400/s sender
    is seen at exactly 100/s), so a count at the CPU can never pass 300.
    l2d moves the `dhcp_relay` trap to `COPP_GROUP|queue3_group1` (600/s)
    while it counts: the group row first (sonic-yang leafref), the trap row
    with the image's `trap_ids` after; stopping deletes the trap row
    (coppmgr restores `copp_cfg.json`'s) and the group two seconds later.
    coppmgr leaves the old group's APPL_DB `trap_ids` naming dhcp while the
    ASIC's trap is in the new group: read ASIC_DB, not APPL_DB.
  - **Trapped frames reach the hostif tagged** (802.1Q, VLAN 4094), and a
    packet socket may see them with or without the tag: the BPF filter and
    the classifier take both. `recvfrom`'s interface is the ingress port.
  - **Only a client's own request counts** (op 1, `giaddr` 0): a server's
    replies and a relay's forwarded requests never count against their port.
  - Read side: STATE_DB `DUI_DHCP_RATE_TABLE|<port>` (`over`, `peak`,
    `last`), `collect.py` `l2_dhcp_rate`, summed over a cage's lanes into
    `Health.DHCPRateDrops`. Testing: a LAN host sending BOOTREQUESTs to the
    switch's own address arrives on the uplink through the same trap.
    Fixtures: `collect-dhcprate.txt`, `dhcp-frames-20261114.txt`
    (`testdata/l2d_dhcp_rate.py` runs the filter through a cBPF interpreter).
- **Port anomalies 1024/2048 have no source on the image (256 and 128 do
  since, below;
  `docs/drivers/sonic.md` §11.18, assessed 2026-11-14,
  `sonic-igmp-202511.20261106-f2e25f54c/anomaly-probes.txt`).** Before
  building a per-port counter, know: ACL counter polling is **off**
  (`FLEX_COUNTER_TABLE|ACL` disable), so no ACL rule counter reaches
  COUNTERS_DB until something turns it on; the SAI offers **one**
  `PORT_INGRESS_DROPS` debug counter in all, with no learning-drop reason;
  the kit's `L2` ACL type has no `IN_PORTS` match, so a rule counter in
  `DUI_MAC_ACL` cannot name its port; STATE_DB `PORT_OPERR_TABLE` (MAC
  local/remote fault per oper-down) counts ordinary drops of a healthy link,
  not a failed port; the image has no 802.1X authenticator (202511's
  `src/sonic-pac` is unbuilt; 802.1X is hostapd under l2d since, below).
- **DoS (1024) and PHY failure (2048): the ECS has nothing to copy**
  (§11.18.4, §11.18.1; read-only 2026-11-14). Its `ERROR_DISABLE|denial-of-service`
  row is configuration only: nothing in its databases, chip attributes, CoPP
  or push configures a DoS detector, and its inform carries no `anomalies`
  key. Do not build a DoS hold from the row. The Tomahawk's own checks are
  Broadcom switch controls (`bcmcmd sc`, `DosAttack*`, all off except
  `DosAttackMACSAEqualMACDA`), invisible to SAI and lost on a `syncd`
  restart; the SDK has per-port receive debug-counter triggers for them
  (`RDBGC8_SELECT` is 0). Turning a check on changes forwarding on every
  port, the uplink included: only with a definition captured from a real
  UniFi switch. `dos-probes.txt` in the 202511 fixture directory has the
  reads.
- **MAC locking, anomaly 256 (`l2d.py` `MacLockGuard`, `l2.go` `mac_lock`,
  `docs/drivers/sonic.md` §11.18.2, built 2026-11-14, verified by hand on
  an empty cage).** The driver's MAC filter stays `learn_mode drop` + static
  FDB in CONFIG_DB/APPL_DB (enforced from boot); l2d moves a secured port to
  `cpu_trap` in APPL_DB only after its ebtables chain `DUI_MACLOCK` (listed
  sources RETURN, the rest of the port DROP; from INPUT and FORWARD) is in
  place, reads the netdev on a packet socket, and counts each frame from an
  unlisted unicast source in STATE_DB `DUI_MACLOCK_TABLE|<interface>`
  (`drops` since boot, `last_mac`); `collect.py` `l2_maclock` ->
  `Health.MACLockingDrops`. **The port stays up**: the controller's alert
  text for 256 describes a detection, not a disabled port (a first build
  held it down; replaced the same day). Facts that cost time:
  - **Unknown sources reach the CPU under `cpu_trap` through the default
    CoPP group** (queue 0, 600 packets/s for the whole switch): 626 of a
    20,000-frame burst; under `drop` none. The chip's `bcmcmd ps` `lrn`
    column shows the mode: C, D, FA.
  - **mstpd reads BPDUs on its own packet sockets**, past ebtables: a looped
    test port receives its own BPDUs and goes backup/discarding, and StpOrch
    then blocks it in every VLAN -- a loopback test needs the installed l2d
    paused (`kill -STOP` its main pid, `-CONT` after) and a hand-made tagged
    VLAN that no instance maps. A looped port in Vlan4094 also receives the
    LAN's IGMP (snooping's static groups flood every port): keep it
    trapping or dropping while looped, never learning in hardware.
  - **Never move a test port's native VLAN through UniFi** (2026-11-14,
    cage 9): leaving its only VLAN while snooping's static groups name it,
    orchagent's `removeBridgePort` sets the bridge port admin down, refuses
    the removal ("still has references to a Layer 2 multicast group
    member"), and the next VLAN add reuses the admin-down bridge port: ASIC_DB
    `SAI_BRIDGE_PORT_ATTR_ADMIN_STATE false`, chip `lrn` D whatever the learn
    mode. Only a swss restart or a reboot clears it.
  - **macOS has no `AF_PACKET`**: `testdata/l2d_maclock.py` sets the constant
    before l2d opens its fake sockets.
- **The lane switch's parent (`lanes.go`, `docs/drivers/breakout.md` §0;
  built 2026-11-14, fixture- and replay-tested, never live).** A split
  cage keeps its lanes as ports of their own (`cagePort.lanes`); the plan
  has a lane scope (`planFor(cage, ...)`: ports are the lanes, aggregates
  `PortChannel<100*cage+lag>`, mirrors `dui<100*cage+lane>`) and an owned
  cage (split, a child attached) keeps only the breakout and the admin
  state in the cage plan -- a cage speed the mode runs writes nothing, the
  module rule keeps the mode only, a join dissolves the child's aggregates
  and mirrors first (`dissolveLanes`). Each plan owns its PortChannel range
  (`plan.mine`): a lane in any other is left in it, VLAN-less and at its
  PortChannel's MTU. One exception (2026-11-15, `docs/drivers/breakout.md`
  §7c): the device's plan deletes an empty `ownOrphan` in the lane range of
  a cage with no lane switch attached (`plan.detachedLane`), what a detached
  lane switch left; one with members stays, noted. `lockApply` serialises `Collect`, `ApplyPorts`,
  `ApplyLanes`, `EnsureVLANs`, `ApplyDevice`, `ApplyManagementIPv6`, `ApplyAddress` and `Reboot`; the fan mode, the SSH keys and locate stay outside it on purpose (they share nothing with a plan). Facts from the capture
  (`sonic-igmp-202511.20261106-f2e25f54c/collect-4x25g.txt`): a 25G lane's
  `supported_speeds` is 1000,10000,25000 and its `supported_fecs` none,fc
  (so the Flex XG aggregate push's `cl-91` is refused); the controller's
  aggregate is `lag=1`, so the first lane aggregate is `PortChannel<100*cage+1>`.
  STP port settings stay the cage's (the lane switch claims no STP):
  `l2SetPorts` (l2.go) names every bridge port a cage's interfaces make --
  a lane aggregate without lane 1 included -- with the cage's settings, and
  `ApplyLanes` re-derives them after a lane batch (§11.25.1, live
  2026-11-15).
- **`config portchannel del` does not look at ACL_TABLE** (§11.25.2, live
  on the DX010 2026-11-15): a deleted PortChannel a table still binds stays
  in the table, orchagent keeps the LAG (`removeLag: Failed to remove ref
  count 1`, retried ~7/s until no table binds it), and the next `config
  interface breakout` is refused by sonic-yang (`Invalid value
  "PortChannel<n>" in "ports" element`). Every delete the plan makes is
  preceded by `aclUnbind` of that PortChannel; the plan's breakout effect
  takes the old ports out of the tables as `-f` does. The FixtureRunner
  models both (`HeldLAGs`, the breakout's leafref check). Fixture:
  `sonic-igmp-202511.20261115-6c8eb1cf7/portchannel-del-acl-20261115.txt`.
  **A table that is not the driver's** (an operator's; collect.py reads
  every `ACL_TABLE` entry since 2026-11-15, §11.25.4) loses the names the
  aggregate takes and gets their replacements (a joining lane's
  PortChannel, a deleted PortChannel's former members) through an `hset`
  of `ports@` alone, never `config acl add table` (it rewrites the whole
  entry); no port left is an `hdel` of the field, as breakout `-f` leaves
  it. Noted each time; only the driver's tables are checked for `Active`.
- **802.1X (`l2d.py` `Dot1xGuard`, `l2.go` `dot1x`, `build-hostapd.sh`,
  `docs/drivers/sonic.md` §11.26, built 2026-11-15, verified by hand on an
  empty cage).** hostapd 2.11 (wired driver, a RADIUS client) built on the
  switch like mstpd, one per controlled port under l2d; the port closed by
  the MAC filter's CONFIG_DB `learn_mode drop`; each host hostapd
  authorises gets a static FDB entry (APPL_DB, ProducerStateTable) and a
  RETURN in ebtables `DUI_DOT1X`; failures (`CTRL-EVENT-EAP-FAILURE2`) in
  STATE_DB `DUI_DOT1X_TABLE` -> `Health.Dot1XFailures` -> anomaly 128.
  No image change. Facts that cost time:
  - **The EAPOL trap is the macsec feature's** in the image's
    `copp_cfg.json` and not installed; `COPP_TRAP|dui_eapol` with
    `always_enabled` installs it, as STP's. A trapped EAPOL frame reaches
    the port's netdev **tagged with the port's VLAN**.
  - **ebtables INPUT decides whether hostapd sees EAPOL at all**: the
    bridge passes a link-local frame to the port's protocol sockets
    (hostapd's `ETH_P_PAE`) only after NF_BR_LOCAL_IN; a port-wide DROP
    starved it while tcpdump (an `ETH_P_ALL` tap) still showed the frame.
    The chain accepts EAPOL first, untagged and 802.1Q.
  - **hostapd 2.11 built without `CONFIG_DEBUG_SYSLOG` has no `-s`**
    (usage and exit 1); `logger_syslog` in its config sends its events to
    syslog.
  - **The RADIUS secrets never go in l2.json** (collect.py reads it as
    the login user): `/etc/sonic/disunified/dot1x-radius`, root 0600,
    rewritten once per bridge start; the bridge keeps the sha in memory
    only.
  - **The MAC filter's plan deletes every static FDB entry it does not
    want**: l2d's admitted hosts (`DUI_DOT1X_TABLE` `authorized`) are
    wanted while their port is controlled.
  - **802.1X's `drop` is not a MAC filter on the read side**: read as one
    (`port_security_enabled` with no allowed MACs), the controller's port
    panel shows the port Disabled, its own rule for an empty list (§11.26.11).
  - **A looped test cage makes hostapd a station of the switch's own
    MAC** (its own frames come back): l2d ignores the switch's MAC.
  - **The DX010 had no IPv4 default route in the kernel on 2026-11-15**
    (FRR: installed, offloaded; `ip route`: none), so the switch reached
    no IPv4 host off the LAN: the build took a tarball placed beside the
    script (the hash decides).
  - **The push (11.0.99)**: `switch.dot1x.status`, `switch.radius.auth.N.
    {ip,port,secret}`, `switch.port.N.dot1x.mode` (force_authorized on
    every port by default; auto = Credentials = the port opened to every
    host by the first that authenticates, multi_host = Multiple
    Credentials = each host on its own (not captured, §11.26.9),
    mac_based with
    `dot1x.idle_timeout`, force_unauthorized). The Unauthorized VLAN
    reaches no push. hostapd is built only once a port authenticates.
  - Fixtures: `sonic-igmp-202511.20261115-6c8eb1cf7/dot1x-live.txt` (the
    hand runs: hostapd's events and station replies, the guard's writes),
    `collect-dot1x.txt`.
  - **MAB (`mac_based`, §11.26.8) is l2d's own RADIUS client**: hostapd
    2.11's `macaddr_acl` is read only for 802.11 frames, and the wired
    driver never asks about a host that sends no EAPOL. User name and
    password are the MAC upper case without separators (the controller's
    RADIUS-user help text). hostapd's integrated server takes `"<MAC>"
    MACACL "<MAC>"` users for a test.
  - **A looped cage's frames never reach `cpu_trap` in a VLAN whose STP
    instance does not forward the port**: the chip drops them at ingress
    first (`RPORTD`); only trapped protocols (EAPOL, ARP) get through.
    So a MAB test host sends ARP requests: they reach l2d on the blocked
    cage in Vlan4094 itself, and an admitted host's ARP keeps its idle
    timer going (§11.26.10). The loop also brings back the LAN's
    multicast (static groups flood a blocked port too): MAB asks about each
    source and counts the refusal.
  - **Over a restart l2d keeps what the previous one admitted**
    (§11.26.10, live 2026-11-15): the STATE_DB row's hosts are held in the
    chain and in an open Credentials port's learning until settled, and
    settled before the first chain write; a MAC-authenticated host's last
    frame is the row's `mab_seen`, written in `main`'s `finally` on SIGTERM
    and `hdel`ed by the next l2d; after a SIGKILL or a crash, the row's
    `mab_heard` (rewritten as l2d runs, never 5 s behind) plus 5 s
    (§11.26.13; a silent host goes by idle + 5 s + a pass after its last
    frame, however many kills). Before that fix every start dropped the
    admitted hosts in the kernel for 40-100 ms. Check a restart with a
    5 ms `ebtables -L DUI_DOT1X` poll and a Redis MONITOR, not snapshots.
  - **Every ebtables write l2d makes is one `ebtables-restore --noflush`**
    (`eb_restore`, §11.26.12, 2026-11-15). The switch's `ebtables` is 1.8.11
    on nf_tables (`ebtables -V`), so each `ebtables` call is a transaction of
    its own: a flush and one append per rule passed every frame until the
    DROP was back. A restore is one transaction: a declared chain is
    emptied, a refused line refuses the batch. `ebtables-legacy-restore`
    keeps a declared chain's rules, hence the explicit `-F`. SONiC's
    `database.sh` loads `/etc/ebtables.filter.cfg` with a plain
    `ebtables-restore`, which empties the whole filter table: both guards
    re-read their chain's count and jumps (`eb_state`) and write it again.
    The fakes run `testdata/l2d_ebtables.py`, which prints `WINDOW` when a
    closed port passes a never-admitted source between two writes.
    **A looped cage cannot exercise the chain's DROP**: it is STP-blocked, so
    the kernel bridge drops its frames before ebtables, and the only trapped
    link-local frame (which reaches INPUT in any port state) is EAPOL, which
    the chain accepts. Prove a write with `xtables-monitor -e` (one `NEWGEN`
    per transaction) or in an `ip netns` bridge; seeing the DROP itself needs
    a cabled host on a forwarding port.
  - **A rig sharing one `SonicV2Connector` across threads segfaults in
    `_swsscommon`** (silently, no traceback): lock every call or give each
    thread its own. `docker exec database redis-cli monitor` outlives a
    `killpg` of its pipeline: kill the in-container process by pid too.
  - **ARP proves nothing in such a probe** (its own trap): use an
    ethertype no trap matches (0x88B5). In a VLAN without the switch's
    address no copy of a known host's frames reaches the CPU at all.
  - **The bridge's reply log (`inform-log/`) kept every push verbatim**,
    the RADIUS secret included, until 2026-11-15; it now redacts every
    pushed secret by key (`internal/unificfg/secrets.go`, `docs/install.md`
    §8). An older file still holds them: grep it, not just the journal,
    when checking where a secret went.
- **UniFi's client Block (`block.go`, `docs/drivers/sonic.md` §11.33,
  built 2026-11-15, fixture- and replay-tested).** `MB_<mac>` rules in
  `DUI_MAC_ACL` exactly as the ECS holds them (DROP, PRIORITY 1023, a bare
  SRC_MAC, no VLAN), outside the MAC rules' limit, each half of the table
  kept while the other is held or unknown; the kernel's copy is ebtables
  `DUI_MAC_BLOCK` from INPUT (marker `/run/disunified/mac-block`, collect.py
  `mac_block`), not the IPv4 raw mirror, so ARP and IPv6 are covered too.
  **`swState.clone` lists its fields by hand**: a new field left out of it
  is lost after every chip batch (the chain was written one apply late
  until it was added). Scrubbed placeholders collide across captures
  (a reply's first MAC and a collect's switch MAC are both
  `02:00:00:00:00:01`): seed a reply log's clients with
  `sanitize-controller.py --mac` or the driver holds them as the switch.
- **Hand tests on a switch that several sessions share take a lock on the
  switch**: write `/tmp/dui-dx010.lock` **on the switch** (over its SSH,
  not in the Mac's `/tmp`) with your session's name and what you are
  doing, wait while another session holds it, remove it when done. On
  2026-11-16 one session took it on the Mac, saw no lock, and installed a
  package in the minute a sibling held the switch's.
  It covers anything the deployed bridge or another session's test would
  see: a stopped or replaced bridge, a split cage, a hand-made CONFIG_DB
  row, an l2d run by hand. Read-only commands need no lock. **Take it
  with noclobber** (`set -C; echo '<who, what>' > /tmp/dui-dx010.lock`),
  so that a held lock refuses the write. A plain `>` replaced a held lock
  on 2026-11-15, while a test's tables were still bound to Ethernet0.
  **To wait, retry that write itself** every few seconds, in one loop
  detached on the switch (`setsid nohup … &`, its output in a file you
  watch): the lock passes from one session to the next within seconds, so
  a poll that reads the file and writes later misses the gap, and a loop
  under a background `ssh` outlives a dropped session -- check `pgrep` for
  a second one before you release, or it takes the lock again after you
  (2026-11-15). (The
  convention lived only in session briefs until 2026-11-15; one session
  used a different file name.)
- **A cage left up as a non-edge port (looped, or facing a switch) for more
  than about five minutes gets pinned to `stp.edge=disabled`.** That is the
  controller's Auto STP Edge (`docs/drivers/sonic.md` §11.23.3). It does
  not go back by itself, and l2d keeps auto edge off on the cage. Un-pin it
  at the end of the test with one call, which needs no override change and
  no UI:
  `POST /proxy/network/v2/api/site/<site>/ports/<mac>/reset-auto-edge` with
  body `[{"port_idx":N}, ...]`, using the API key (answer 204). Then check
  that the next push says `switch.port.N.stp.edge=auto`. Do not use the
  port editor's Edge-then-Restore saves: the save rewrites the whole
  override (BPDU guard on and more).
