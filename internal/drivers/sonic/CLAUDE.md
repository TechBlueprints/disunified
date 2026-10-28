# sonic — facts that cost time

Read `internal/drivers/CLAUDE.md` first. Specification and plan:
`docs/drivers/sonic.md` (§5 is the contract). Built 2026-10-27 on the podman
driver's pattern, read-only, against the SONiC virtual switch (`202505.1238524-ff3c0c9f6`,
hardware SKU `Seastone-DX010`, L2 preset); no hardware yet.

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
- **Address (§6.4), `address.go`:** static only; add the new address (and
  route/DNS), dial it with `Redial` (same user, key, known_hosts) and run
  `true`, then remove the old one through the new runner and save; on no
  answer the new address (and route) are removed again. known_hosts must
  carry the switch's key under the new address. Default route lives in
  `STATIC_ROUTE|default|0.0.0.0/0` (`cfg_route`), DNS in `DNS_NAMESERVER`
  (`cfg_dns`).
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
- **STP port settings only with `FEATURE|stp`**; breakout (a lane speed on a
  4-lane cage, or a cage speed on a split one) is refused with a log
  (no `platform.json` on the VS, and the breakout write is not built).
  `collect.py` prints `platform_json` (yes/no) and `cfg_storm`
  (`PORT_STORM_CONTROL`).
- **Scrub**: `scripts/sanitize-sonic.py`. IPv6 link-local addresses are
  rewritten too (their EUI-64 half is a real MAC). The FDB keys put a MAC
  right after `Vlan4094:`; the MAC pattern must not refuse a preceding `:`.
  The capture's own `version` section keeps the build host string from
  `sonic_version.yml` (a CI host, not site data).
