# Arista side: EOS 4.26.14M on a DCS-7160-48TC6-F

Captured and verified against Clint's switch on 2026-09-19/20. **Everything here was run
against the real device.** Raw command output lives in
[`docs/fixtures/arista-eos-4.26.14M/`](../fixtures/arista-eos-4.26.14M), scrubbed by [`scripts/sanitize-arista-eos.py`](../../scripts/sanitize-arista-eos.py) (MACs, IPs, serials,
hostnames and descriptions rewritten; structure and types untouched).

## 1. The version is pinned for good

- EOS **4.26.14M**, i686, hardware revision 11.00. Confirmed live via `show version`.
- **4.26 is the last train that supports the 7160 series.** Arista's end-of-support notice
  states that EOS 4.27 and later will not support the DCS-7160 models
  (<https://www.arista.com/en/support/advisories-notices/end-of-support/17443-end-of-software-support-for-7160-series>).
  So the output schemas in the fixtures are the schemas this switch will have forever. Write
  the collector against the fixtures, not against current Arista docs.
- No PoE: `show poe` is `% Invalid input`. Drop it from the poll list; report `port_poe: false`.
- Two commands from the plan were renamed on this train: `show environment temperature` and
  `show environment cooling` are deprecated in favour of `show system environment temperature`
  / `show system environment cooling` (the old names error out).

## 2. Transports

The driver speaks two transports; both run the same command list and parse
the same JSON, and every batch is prefixed with `enable` because eAPI and a
fresh SSH session start at privilege 1 whatever the user's level (`show
interfaces flow-control` refuses to run there). The account must therefore
reach enable mode **without an enable password**; writes also need config
mode and end with `write memory`.

- **eAPI** (`url: https://<switch>/command-api`, `username_env` +
  `password_env`): JSON-RPC 2.0, method `runCmds`, HTTP basic auth, TLS
  1.2 with RSA key exchange (EOS 4.26 refuses ECDHE). This is the primary
  transport and the only one that can run text-format commands, which is
  where the FEC codeword counters come from (`show interfaces <lane> phy
  detail`): over SSH they stay unset and the satisfaction score counts
  frame errors only. eAPI has
  to be enabled on the switch first (one config block, in enable mode):

  ```
  configure
  management api http-commands
     no shutdown
  end
  write memory
  ```

  Once it is up, `https://<switch>/explorer.html` is the **Command API
  Explorer**: the exact JSON schema of every show command *for the running
  EOS version*, the authoritative reference for 4.26.14M.
- **SSH** (`ssh: user@switch`): key authentication only (ssh-agent, then
  `~/.ssh/id_ed25519` / `id_rsa`, or `options.ssh_key`), host key checked
  against `known_hosts` (`options.known_hosts`; loading it is mandatory),
  no password support. Commands are sent in one session with `| json`
  appended; any `% ...` line fails the batch. Needs nothing enabled on the
  switch. The fixtures were captured this way.

For an eventual on-switch deployment, eAPI over the unix socket
(`protocol unix-socket`, `/var/run/command-api.sock`, no authentication) is
the on-box path (<https://arista.my.site.com/AristaCommunity/s/article/arista-eapi-101>);
it has not been tried.

## 3. Prior art for the API

| Project | Relevance | Verdict |
|---|---|---|
| [aristanetworks/goeapi](https://github.com/aristanetworks/goeapi) — official Go client, BSD-3 | JSON-RPC transport, INI profiles, typed `module/` structs for `show version`, `show interfaces switchport`, system | **Stale**: last release v1.0.0 (2022-07-26), "Go 1.5+", best-effort support. The typed modules cover almost none of what we need. Use it, at most, as a reference for the JSON-RPC envelope; a hand-written client is ~60 lines. |
| [enix/arista-eapi-exporter](https://github.com/enix/arista-eapi-exporter) — Prometheus exporter, Python, MIT | Polls `show version`, `show interfaces`, `show hardware capacity` etc. via eAPI, YAML-driven command list, supports HTTPS-insecure and unix socket | Good operational reference for a polling loop; reads `memFree`/`memTotal`/`uptime` from `show version` and `bandwidth`/`description` from `show interfaces` — same fields we map. |
| [arista-eosplus/pyeapi](https://github.com/arista-eosplus/pyeapi) — official Python client | `api/interfaces.py` etc. parse the same JSON; useful to see which keys Arista themselves consider stable | Reference only (bridge is Go). |
| [ansible-collections/arista.eos](https://github.com/ansible-collections/arista.eos) `eos_eapi` module | Shows the exact config lines to enable eAPI/unix-socket and the `show management api http-commands` state model | Reference for §2. |
| [eos-eapi](https://docs.rs/eos-eapi/latest/eos_eapi/) Rust crate | Minimal JSON-RPC client; confirms how small a client needs to be | Reference only. |

eAPI itself is JSON-RPC 2.0, method `runCmds`, params `{version: 1, cmds: [...], format: "json"}`,
POST to `/command-api` with HTTP basic auth. This envelope has not changed since EOS 4.12, so
version-specific concerns are entirely in the per-command output, which is what the fixtures pin.

## 4. Command → UniFi field mapping (from the fixtures)

What the driver runs, all present in [`docs/fixtures/arista-eos-4.26.14M/`](../fixtures/arista-eos-4.26.14M):

- **At startup, and again every 30 polls** (the media map and hardware
  table): `show version`, `show hostname`, `show interfaces status`,
  `show inventory`, `show interfaces transceiver properties`,
  `show interfaces hardware`.
- **Every poll**: `show version`, `show interfaces`, `show lldp neighbors
  detail`, `show spanning-tree`, `show processes top once`, `show system
  environment temperature`, `show mac address-table`, `show interfaces
  transceiver`, `show interfaces error-correction`, `show interfaces
  flow-control`, `show port-channel summary`, `show system environment
  cooling`, `show system environment power`, `show interfaces switchport`,
  `show vlan`, `show storm-control`, `show ip igmp snooping`, `show
  running-config`, `show interfaces status errdisabled`, `show spanning-tree
  root detail`, `show interfaces transceiver dom thresholds`, `show
  spanning-tree topology status detail`, `show ip arp`, `show hardware
  capacity`.
- **Every poll, text, eAPI only**: `show interfaces <lane> phy detail` for
  each up lane (FEC counters).

`Start` runs the startup set and one full poll, so a missing command or a
bad credential fails immediately. The core mappings:

### `port_table` from `show interfaces` → `interfaces["EthernetN"]`

| UniFi field | EOS source | Notes |
|---|---|---|
| `port_idx` | parsed from `name` | `Ethernet1`..`Ethernet48` → 1..48; `Ethernet49/1`..`Ethernet54/1` → 49..54 |
| `ifname`, `name` | `name`, `description` | description is empty on all ports today |
| `up` | `interfaceStatus == "connected"` | enum seen: `connected`, `notconnect`; `disabled`/`errdisabled` exist on EOS but not in the capture |
| `enable` | `interfaceStatus != "disabled"` | |
| `speed` (Mbps) | `bandwidth / 1_000_000` | 100G ports report `100000000000`; down ports report `0` in `show interfaces status` but the configured value in `show interfaces` |
| `full_duplex` | `duplex == "duplexFull"` | |
| `mtu` | `mtu` | 9214 on the QSFP ports |
| `media` | `interfaceType` from `show interfaces status` | seen: `10GBASE-T` (×48), `100GBASE-CR4`, `100GBASE-CWDM4`, `Not Present`, `N/A` (breakout lanes) |
| `rx_bytes`, `tx_bytes` | `interfaceCounters.inOctets`, `.outOctets` | |
| `rx_packets`, `tx_packets` | `inUcastPkts+inMulticastPkts+inBroadcastPkts` (or `inTotalPkts`), same for out | |
| `rx_errors`, `tx_errors` | `interfaceCounters.totalInErrors`, `.totalOutErrors` | `inputErrorsDetail`/`outputErrorsDetail` break them down |
| `rx_dropped`, `tx_dropped` | `interfaceCounters.inDiscards`, `.outDiscards` | |
| `stp_state` | `show spanning-tree` → `spanningTreeInstances.MST0.interfaces[name].state` | `forwarding` etc.; only ports participating in STP appear (3 of 62) — default the rest to `disabled` |
| `port_poe`, `poe_*` | — | always false / absent, no PoE hardware |
| `is_uplink` | LLDP neighbour that is a UniFi device on `Ethernet49/1` | or simply the port facing the upstream aggregation switch |

### Breakout ports need a rule

`Ethernet50` is configured as 4×25G breakout, so `show interfaces` lists `Ethernet50/1..50/4`,
while every other QSFP slot is a single `EthernetNN/1`. `Ethernet52/1` reports
`interfaceType: "Not Present"` (no optic). The controller-side profile (UDC48X6) has one
QSFP28 port per slot, so the collector must **fold lanes into their parent slot**: port 50 is
"up" if any lane is up, counters summed. Also present: `Management1`, the OOB port, reported
as `System.OOBInterfaces` (link state, address) for the loop's in-band warning; and
`Port-Channel1..4` (defined, no members), `Vlan*`.

### `lldp_table` from `show lldp neighbors` → `lldpNeighbors[]`

`{port, neighborDevice, neighborPort, ttl}` per entry; `show lldp neighbors detail` adds the
chassis ID (MAC), management address, system description and capabilities for the UniFi
`lldp_table` `chassis_id`/`port_id` fields.

### `sys_stats`

- `cpu`: `100 - cpuInfo["%Cpu(s)"].idle` from `show processes top once`
- `mem_total`, `mem_used`, `mem_buffer`: `memInfo.physicalMem` (kB) from the same command, or
  `memTotal`/`memFree` from `show version`
- `uptime`: `show version` → `uptime` (seconds, float)
- optional temperature: `show system environment temperature` (`cardSlots[].tempSensors[]`)
- optics: `show interfaces transceiver` gives `rxPower`/`txPower`/`temperature` per port —
  UniFi's SFP detail view reads `sfp_*` fields; worth wiring in phase 0.5

### Health signals, all verified on 4.26.14M

These fill `Port.Health`, which goes out as raw port fields
(`link_down_count`, `stp_state_change_count`, `sfp_rxfault`/`sfp_txfault`)
and the satisfaction score, never as `anomalies` bits (`docs/feature-map.md`
§2: that field is what a switch detected, and the controller raises an alert
per bit; it derives its own link-flap and SFP alerts from the raw fields).

- `show interfaces` → `interfaceCounters.linkStatusChanges` (link flaps).
- `show spanning-tree topology status detail` → `topologies.Cist.interfaces[].numChanges`.
  (`show spanning-tree`'s `interfaces[].inconsistentFeatures` names the
  guards holding a port; no port field carries it, so it is not read.)
- `show storm-control` → `interfaces[<lane>].trafficTypes.{broadcast,multicast,unknown-unicast}.drop`
  → `StormDropsBroadcast` / `StormDropsMulticast` / `StormDropsUnicast` (a split
  cage adds its lanes), which `internal/device` turns into the `anomalies`
  storm bits 1/2/4 when a count grows between informs. The same command
  already gives the configured levels. Captured with all three types set
  on a test port (`docs/fixtures/arista-eos-4.26.14M/storm-control-all-types/`,
  JSON and the CLI's table, whose `Drops` column is the same number); an
  interface without storm control is absent. Arista's
  `ARISTA-STORM-CONTROL-MIB` defines the count, `aristaStormControlIfDrops`,
  as a Counter64 per interface and traffic class ("packets dropped ... as a
  result of this entry's storm control threshold level"), i.e. cumulative.
  4.26.14M has no `clear storm-control` (the CLI answers `% Unrecognized
  command`); whether `clear counters` or a link flap resets the count is
  not known -- a count that goes down sets nothing. **Never seen above 0:**
  the two test ports have no link, so no storm has been dropped on this
  switch; the read path ran live from a `-collect-once` with the three
  levels on cage 54 (2026-11-14).
- `show interfaces transceiver dom thresholds` → per-parameter `channels` +
  `threshold{lowAlarm,highAlarm}` for optics with DOM (none for DACs/empty cages).
- FEC codeword counters exist **only in text**: `show interfaces EthernetX phy detail`
  prints `FEC corrected codewords N …` / `FEC uncorrected codewords N …` (first
  number is the counter), and for every port — 10GBASE-T included — `PCS err blocks N`
  (per PCS section) and `PCS high BER ok|…` (printed, not read: no port field
  carries them). Copper ports show
  `Forward Error Correction None` (no LDPC counters exposed). `show interfaces error-correction` is config/status only;
  `... error-correction detail`, `... fec` are invalid on 4.26. The eAPI transport
  runs text via `format: "text"` (`TextRunner`); one command per FEC lane.
- Rogue DHCP replies dropped per port (`RogueDHCPDrops`, anomaly bit 64): the
  `packetCount` of the guard's drop entry in the port's own ACL, `show ip access-lists
  DUI_IP_ACL_<interface>` (§9.6). Read only while the driver has lists; 0 on the live
  read of 2026-11-14 (no traffic on the test ports).

## 5. Identity for the UniFi descriptor

From `show version`: `systemMacAddress` (the bridge should present the Arista's real MAC so
the controller's client list and LLDP data line up), `serialNumber`, `modelName`, `version`.
The firmware string the controller sees is digits and dots, as a real device's is:
`4.26.14M` goes out as `4.26.14` (`device.WireVersion`, `docs/wire-shapes.md` §5).

## 6. Not on this switch / not used

- gNMI is disabled (`show management api gnmi` → `enabled: false`); the driver polls eAPI instead.
- No PoE. No `show poe`.
- No LED control, so locate only reports `locating`.
- No per-port L2 MTU (`l2 mtu` unsupported): jumbo frames are always forwarded and UniFi's
  "jumbo off" is logged, not applied. No per-port LLDP-MED toggle on 4.26 either.
- **No rogue-DHCP-server protection in EOS's own DHCP snooping.** UniFi's "Rogue DHCP Server
  Detection" (Global Switch Settings, site-wide; `switch.dhcp_snoop.status`) drops DHCP server
  replies from untrusted servers. EOS 4.26's `ip dhcp snooping` is a different feature: it
  intercepts DHCP on the listed VLANs to insert Option 82 (`ip dhcp snooping information
  option`) or to bridge them (`ip dhcp snooping bridging`); there is no trusted-port model (`ip
  dhcp snooping trust` is `invalid command`, probed 2026-09-20 in an aborted config session).
  Enabled without the information option it reports "not operational" on every VLAN and does
  nothing. The guard is therefore built from ingress ACLs, one per interface (§9), claimed since
  2026-11-14; until then the controller never sent the key (verified 2026-09-20: it sent
  `enabled`/`disabled` only while the claim was in place).

## 7. In-band management (2026-09-20)

The bridge reaches the switch at `Vlan1` (`ip address <in-band address>/<mask>`, default
VRF, same VRF eAPI and SSH listen in) so the management address sits behind
the 100G uplink like a UniFi switch's. `Management1` is addressless with
`no lldp transmit` / `no lldp receive`. EOS refuses two interfaces in one
subnet in the same VRF, so the address had to *move*, not be added.

Moving a management address remotely: use a configuration session with a
timed commit — `configure session X`, the changes, `commit timer 00:03:00`;
verify over the new address; confirm from exec mode with
`configure session X commit` (entering the session again fails with
"pendingCommitTimer"); then `write memory`. A stale pending session blocks
every other commit ("another session … is pending commit timer") — abort it
with `configure session X` + `abort`. eAPI over the old address times out
once the address moves; that is expected.

## The switch's own address

The controller pushes the device's IP Settings in every system_cfg, and the
driver applies a **static** setting (no key since 2026-10-31) to the
interface that carries the address the bridge itself connects to (Vlan1 on
this switch, in-band; see §2c). "Using DHCP" is declined on this driver with a
logged reason: a move is confirmed by dialling the new address, and a lease
cannot be known in advance, so the switch keeps its static address. The pushed form on 10.6.106 is
`netconf.1.ip` / `netconf.1.netmask`, `route.1.gateway` (with
`route.1.ip=0.0.0.0`) and `resolv.nameserver.N.ip`.

An address change severs the bridge's own connection, so it is made inside a
**configuration session committed with a timer** (`configure session
dui-address` … `commit timer 00:02:00`, verified on 4.26.14M, fixture
`show-configuration-sessions.txt`): the switch applies the change at once and
reverts it by itself unless it is confirmed. The driver then dials the switch
at the new address; only if `show version` answers there does it confirm
(`configure session dui-address commit`, then `write memory`) and switch its
own transport over. If the new address does not answer, it returns an error
and lets the timer put the old address back. The apply is a diff against the
running configuration, so re-sending what the switch already has does
nothing.

After a successful move the bridge keeps working at the new address until it
restarts; the operator then updates `url:` in the config file.

## 8. Fan Mode (built 2026-11-07, untested live)

UniFi's Fan Mode (Default / Quiet) mapped onto EOS 4.26. The switch was
off the network when it was built, so nothing below has run on it yet.

**Where the mode comes from.** Not from the controller: Network 10.6.109
keeps `sys_fan_mode` only for a gateway (UDM, UXG), never for a switch, so
no switch gets a Fan Mode panel or a `system.fan.mode` push
(`docs/drivers/sonic.md` §11.12). It is the device's `fan_mode: default |
quiet` in `config.yaml`; unset leaves the fans alone. The loop compares it
with the mode the driver read back on every collect and calls
`ApplyFanMode` only when they differ (`internal/informloop/fanmode.go`).
The inform still reports `sys_fan_mode` (caps 3, the current mode).

**What EOS offers.** Two settings and nothing else (Arista's manual,
*Environment Commands*; the 4.26 syntax is the first return-day check):

- `environment fan-speed auto` (the default; `no environment fan-speed` /
  `default environment fan-speed` restore it) -- the switch's own thermal
  control. Each temperature sensor has a target, overheat and critical
  threshold, and the fans follow the sensors.
- `environment fan-speed override <percent>` -- a **fixed** speed, 30 to 100
  % of maximum, held "regardless of the switch temperature conditions".
  Arista calls it unsupported outside its engineers' direction, and some
  editions of the page say it can void the warranty (the 7160 is past its
  end of software support).

No minimum or ceiling form is documented. The 4.26.14M JSON of
`show system environment cooling` carries `coolingMode` (`"automatic"`),
`overrideFanSpeed` (0) and `minFanSpeed` (0); there is no CLI for the last.
The manual does not say whether an override still lets the alert threshold
("all fans run at maximum speed") raise the fans; the overheat shutdown
(`environment overheat action`, shutdown by default) is separate and left
alone.

The capture under auto: all four fan trays at 42 %, the PSU fans at 32 %,
29 °C ambient, the hottest sensor 63 °C against its 60 °C target (overheat
90, critical 95).

**The mapping** (Clint, 2026-11-07: "fixed override unless the arista can
do stepping on top of the 30%", then "quiet is the floor you can set"; EOS
documents no stepping on top of an override):

> **Quiet is a fixed speed, not a floor.** The 7160 has no fan minimum that
> EOS's thermal control can still raise. The only knob is the fixed
> override, and the lowest value EOS accepts is **30 %** (the manual's
> range, to be confirmed on 4.26.14M). Under Quiet, EOS's automatic control
> does not raise the fans as the switch heats up. Only the undocumented
> alert threshold and the overheat shutdown act. Clint was told this
> before the merge (2026-11-07).

| UniFi | EOS | Read back from `show system environment cooling` |
|---|---|---|
| Default | `no environment fan-speed` (auto) | `overrideFanSpeed` 0 (captured) |
| Quiet | `environment fan-speed override 30` | `overrideFanSpeed` 30 (not captured) |

Both modes are offered (`System.FanModes`). The write is a diff against
the poll: nothing when the switch is already in the configured mode, and an
override set by hand at any other speed reads as neither mode and is put
back to the configured one on the next collect. One batch: `enable`,
`configure`, the command, `end`, `write memory` (`fanmode.go`; tests in
`fanmode_test.go` and `internal/informloop/fanmode_arista_test.go`).
`coolingMode` is not read: its value under an override is not captured.

**When the switch is back** (in this order; record each output, scrub it
with `scripts/sanitize-arista-eos.py`, commit it under
`docs/fixtures/arista-eos-4.26.14M/`):

0. The deployed container must carry this code (a rebuild needs Clint's
   go), and the Arista's entry in `config.yaml` needs no `fan_mode` until 3.
1. Read-only: `show system environment cooling` and `show system
   environment temperature` under auto (the baseline for 3).
2. The syntax on 4.26.14M, in a session that is aborted, not committed:
   `configure session dui-fan-probe`, `environment fan-speed ?`,
   `environment fan-speed override ?`, `abort`. Confirm `auto` and
   `override` exist and that the range starts at 30; if it does not, change
   `quietFanPct` in `fanmode.go` before step 3.
3. Quiet: `fan_mode: quiet` on the Arista in `config.yaml`, bridge
   restarted. The bridge log shows `fan mode default -> quiet` and one batch
   with `environment fan-speed override 30`, and
   it must not fail on the CLI's override warning (the SSH transport fails
   a batch on any `% ` line; eAPI returns warnings apart). Capture `show
   system environment cooling` (as `show-system-environment-cooling-
   override30.json`) and `show running-config`; the next cycle writes
   nothing.
4. Watch `show system environment temperature` for 15 minutes under Quiet:
   every sensor must stay below its overheat threshold. If the hottest
   sensor climbs toward overheat, set Default and record the numbers here;
   do not provoke the alert threshold to see whether it overrides the
   override.
5. Default: `fan_mode: default`, bridge restarted: `no environment
   fan-speed`, the cooling output back
   to `overrideFanSpeed` 0 and `coolingMode` `automatic`, the fans back to
   the baseline from 1.
6. In the UI, open the Arista's device Settings and check for a Fan Mode
   panel; also check `sys_fan_mode` on its stored record (CLAUDE.md §5).
   On 10.6.109 neither is expected: no switch's record keeps the field
   (CLAUDE.md §5). Record what a newer Network release does.
7. Add the override capture to `fanmode_test.go` (Quiet read back, Quiet
   idempotent) and mark the feature verified in this section, the support
   matrix and the feature map.


## 9. DHCP guarding, rogue DHCP reporting and switch ACLs (2026-11-14)

The controller's pushes for both features were captured on Network 10.6.109
from an ECS Aggregation, which claims them: `switch.dhcp_snoop.status` with
one `switch.dhcp_guard.N.*` group (VLAN, server) per trusted DHCP server of
each guarded network, and the switch ACL rules as `switch.acl.ip.N.*` and
`switch.acl.mac.N.*`. The keys, what each UI setting sends and the scrubbed
fixtures are in [`sonic.md`](sonic.md) §11.7.2; the controller sends a
switch only the guards of the VLANs it carries (§11.7.7 there). This section
is the EOS half. The code is `acl.go` (lists, plan, session) and
`acl_counters.go` (read-back, counters).

The guard was first built on 2026-11-01 as one shared list with a
`vlan N 0xfff` qualifier on each rule, while the switch was unreachable.
The measurements of 2026-11-14 (§9.1) showed that design could not work,
and it was replaced before it ever ran.

### 9.1 What the 7160 does with an ACL on 4.26.14M (measured 2026-11-14)

All of this was done on the two test ports (Ethernet2, copper, unused;
Ethernet54/1, an empty cage), inside configuration sessions that were
committed under `commit timer` and then aborted, or never committed. After
each, the running config, `show ip access-lists summary`, `show platform
acl` and the chip's port ACL table were read back empty. The commands and
outputs are fixtures in `docs/fixtures/arista-eos-4.26.14M/`.

- **No VLAN match.** A rule written `deny vlan 1 0xfff udp any eq 67 any
  eq 68` is stored as `deny udp any eq bootps any eq bootpc`: the JSON keeps
  `vlan.id` 1 with `vlan.mask` 0, which is any VLAN
  (`show-ip-access-lists-DUI_PROBE.json`). The masks `0xfff`, `4095` and
  `0x0fff` and VLANs 1, 10 and 4010 all came out the same way
  (`show session-config diffs` in an uncommitted session). The list's
  summary then says "Some rules are not fully supported by hardware"
  (`notFullySupportedByHardware: true`,
  `show-ip-access-lists-summary-vlan-qualifier.json`), and the chip's ACL key
  profile lists SIP, DIP, ports, protocol, DSCP, TTL, MACs and TCP flags, no
  VLAN (`show platform xp acl`, the MGKS table). **The 2026-11-01 design
  would have applied every guarded VLAN's server list to all VLANs at once**:
  VLAN 1's drop would have dropped VLAN 2's trusted replies.
- **One list on two interfaces is one set of entries.** `DUI_PROBE` bound
  to both test ports: one hardware ACL id (`ipv4PAcl:1`, PortCfg `PAclId` 1
  for both ports), one set of four entries, one counter index per entry
  (`show-platform-xp-acl-one-list-two-ports.txt`). A per-entry count would
  be the sum over every port carrying the list.
- **Separate lists are separate.** Two lists, one per test port: ids 1 and
  2, each with its own entries and its own counter indices
  (`show-platform-xp-acl-per-port-lists.txt`). No "not fully supported"
  flag without the VLAN qualifier.
- **Counters.** `counters per-entry` in a list (offered on 4.26.14M; the
  built-in `default-control-plane-acl` carries it) gives each rule
  `counterData.packetCount` in `show ip access-lists <name>` JSON; a rule
  nothing has matched has an empty `counterData`, a remark none
  (`show-ip-access-lists.json` holds real counts on the control-plane list).
  The counter indices were handed out downwards from 4095, which suggests a
  4096-entry counter pool; that is an inference, not a measurement.
- **Cost.** A permit `udp host S eq 67 any eq 68` took two hardware entries,
  the drop and the closing `permit ip any any` one each. The MaskGroup
  tables hold 8192 entries each; `show hardware capacity`'s ACL rows lagged
  what the chip table showed and are not used for sizing.
- **A session pending its timer aborts at once**: `configure session NAME
  abort` reverts it immediately, with no wait for the timer (all four runs).
- **Hitless update is offered.** `hardware access-list update
  default-result permit` exists on this switch's CLI (the 2026-11-01 notes
  said, from the manual, that it does not). The driver does not set it;
  whether it makes a rewrite hitless here was not measured (no traffic on
  the test ports).
- **No MAC ACL on a switchport.** `mac access-list` exists (with a `vlan`
  qualifier), but `mac access-group` is "Unavailable command" on an Ethernet
  interface. MAC rules cannot be applied (§9.2).
- **EOS's own DHCP snooping** is still Option-82 insertion and bridging only
  (`ip dhcp snooping ?`: `bridging`, `information`, `vlan`; §6).

### 9.2 The mapping

**One IPv4 ACL per interface, `DUI_IP_ACL_<interface>`** (`/` written `-`:
`DUI_IP_ACL_Ethernet54-1`), with `counters per-entry`, bound inbound. The
targets are every Ethernet lane not in an aggregate and the Port-Channel of
every aggregate; `Management1` and VLAN interfaces never. An interface takes
one IPv4 ACL inbound, so a port's guard and the IP rules share its list:

```
counters per-entry
10 remark disunified dhcp-guard <hash>
20 permit udp host <server> eq 67 any eq 68      (one per trusted server of the port)
30 deny udp any eq 67 any eq 68                  (the guard's drop: its counter is the rogue count)
   remark disunified ip-rules <hash>
   <the controller's IP rules, in its order>
   remark disunified default
   permit ip any any
```

A list exists only on an interface that has a guard or IP rules; with
neither it is unbound and deleted. This is the body the driver rendered
for the two test ports and that ran on the switch
(`session-dui-acl-test-ports.txt`; `TestACLPlanIsTheBodyRunLive` holds the
plan to it, and planning again from the captured result writes nothing).

**Which ports are guarded.** UniFi guards per VLAN (a VLAN with no server
list is not guarded), and the 7160 cannot match a VLAN. The guard is
therefore decided per port, from the VLANs the port carries (its access
VLAN, or its trunk's native and allowed VLANs, every VLAN for `allowed vlan
all`), and on a guarded port it trusts the servers of every guarded VLAN it
carries. `options.dhcp_guard` picks the rule:

- **`exact` (the default): a port is guarded only when every VLAN it
  carries is guarded.** The guard then drops exactly what UniFi asked to
  drop, with one widening: a server trusted on one of the port's VLANs is
  trusted on its other VLANs too. **It never drops a reply UniFi did not
  ask to drop.** Its reach depends on the site: on a switch where every
  port is a trunk carrying every VLAN (UniFi's default port profile) and
  some network is unguarded, no port is guarded. The driver logs each
  skipped port with the unguarded VLANs it carries.
- **`edge`: every port carrying a guarded VLAN is guarded, except the
  uplink, the port the gateway's MAC is learned on, and every port whose
  LLDP neighbour is a bridge.** Behind those sit the gateway and other
  switches, where servers of unguarded VLANs answer. On a guarded edge port
  a DHCP server of an unguarded VLAN is dropped too, unless it is listed in
  some network's DHCP Guarding. With no exempt port known the driver falls
  back to `exact` and logs it.

`exact` is the default because it cannot cut a network's DHCP; `edge` is
what makes the guard (and the alert) reach client ports on a site of
all-VLAN trunks. Which one a deployment runs is the operator's choice, made
per device in `config.yaml`; UniFi has no setting for it, its own switches
being able to match a VLAN.

- **The guard comes first**, so a reply from an untrusted server is dropped
  even where a UniFi Allow rule would let it through, as on the DX010.
- **IP rules** map field for field (built 2026-11-01, unchanged):

  | Model (from the push) | EOS |
  |---|---|
  | Permit / Block | `permit` / `deny` |
  | protocol All / TCP / UDP | `ip` / `tcp` / `udp` |
  | a side's prefix, none | the prefix (`host A` for a /32), `any` |
  | one port | `eq N` |
  | the range `1-65535`, or none | nothing (any port) |
  | any other range | `range A B` (the UI refuses ranges; never pushed) |

  A port on an All rule cannot be written in EOS and is refused, and one
  rule that cannot be written holds the whole rule list (a partial list
  could let through what a later Block meant to stop). The rules go into
  every interface's list.
- **The closing `permit ip any any`** makes UniFi's default-allow hold over
  EOS's implicit deny. An IPv4 ACL does not see non-IP traffic such as ARP,
  or IPv6 (not measured: no traffic on the test ports).
- **Diffing by remark.** Each block's remark carries a hash of the lines the
  driver rendered; a list is current when its blocks' hashes and line counts
  match, it has `counters per-entry`, no line outside a block, and it is
  bound where it should be. EOS rewrites rules for display (`eq 67` reads
  back as `eq bootps`), so text is never compared. A block the controller
  leaves alone (snooping not pushed, rules held) is carried over as the
  switch shows it. A rewrite removes every sequence number (`no SEQ`) and
  writes the lines at 10, 20, 30 ...; only the changed interfaces' lists are
  touched.
- **An interface that already carries another IPv4 ACL inbound is left
  alone and logged.** The captured switch has a hand-made `mDNS-block`
  list, bound nowhere.
- **MAC rules are refused** (`max_custom_mac_acls` 0): no MAC ACL binds to a
  switchport on the 7160 (§9.1).
- The shared `DUI_IP_ACL` of the first design is removed if found; it was
  never deployed.

### 9.3 Never cutting off management

The bridge reaches the switch in-band, through the uplink. Every ACL change
is made like an address move (§7):

1. Open `configure session dui-acl`, make the changes, then `commit timer
   00:02:00`. The switch applies the change at once and reverts it by itself
   unless it is confirmed.
2. Wait five seconds, because the hardware is programmed after the commit
   returns.
3. Dial the switch on a **new** connection and ask for `show version`.
4. Only if that answers, confirm the session (`configure session dui-acl
   commit`) and `write memory`.

The driver asks the loop for a 60-second write budget (`devicemodel.ApplyBudget`).
If the budget runs out first, the change is not confirmed and reverts; that
is not taken to mean that the rules cut the bridge off. If the switch does
not answer, the driver leaves the timer to revert and **holds that change**
until the controller's rules differ. A batch EOS rejects is aborted. A
pending session blocks every other commit, the address session and the
bridge's ordinary configuration batches too, until it is confirmed,
aborted or reverted. In `exact` mode the uplink is never guarded when it
carries an unguarded VLAN; in `edge` mode never at all.

### 9.4 Claims

- **DHCP snooping is claimed** (`feature_caps` 128) since 2026-11-14: the
  guard's lists were programmed and read back active on the switch, the
  read-back runs in the poll, and the default mode never drops what UniFi
  did not ask to drop. With Rogue DHCP Server Detection off (the site's
  setting on 2026-11-14) the controller pushes `switch.dhcp_snoop.status=disabled`
  and nothing is written.
- **IP rules are not claimed** (`max_custom_ip_acls` 0). Each rule is copied
  into every interface's list (57 on this switch), and what was measured is
  two lists. How many port ACL ids the chip takes, and whether 57 lists with
  `counters per-entry` fit its counter pool, were not measured, since lists
  can be bound to the two test ports only. A rough bound from the counter
  indices (a pool of 4096, an inference): about 70 counted entries per
  interface, so a claim near the ECS's 512 would not fit.
- **MAC rules are not claimed** (§9.1).

### 9.5 The DHCP rate question (anomaly bit 32)

UniFi's "Incoming DHCP packets exceed 300/sec on <device> Port N" is a
switch's own per-port DHCP rate limit dropping frames (`DHCPRateDrops`).
**EOS 4.26.14M on the 7160 has no such limit**, and nothing in the driver
fills the field. What was looked at, all on 2026-11-14:

- DHCP configuration: `ip dhcp snooping ?` is `bridging`, `information`,
  `vlan`; on an interface `ip dhcp ?` is `relay`, `snooping`. There is no
  rate or limit keyword. `show running-config all section dhcp` lists the
  relay and snooping defaults and nothing more.
- Control-plane policing: `show policy-map type copp copp-system-policy`
  has no DHCP class (bootps/bootpc reach the CPU through
  `default-control-plane-acl` line 90). CoPP shapes traffic to the CPU for
  the whole switch, not per port.
- A QoS policer per port: `class-map type qos` matching an ACL for UDP
  67/68, `policy-map type quality-of-service` with `police rate`, bound with
  `service-policy type qos input`. It programmed on both test ports
  ("Hardware programming status: Successful"; `show hardware capacity` read
  one entry in each PolicerSDE table and seven in QosTcamV4 while it was
  bound, and 0 used after its abort. Another session set and removed
  storm-control on the same test ports that hour; neither port carried it at
  the probe's time, as far as that session's log shows) with per-interface counters in the JSON
  (`show-policy-map-interface-Ethernet2-counters.json`). But the rate is in
  `bps`, `kbps` or `mbps` only (no packets per second), and the JSON says
  `redCounter: false`: only conforming packets are counted on this
  platform, so no count of the dropped frames has been shown to exist.
- `show platform xp counters drop` counts `IACL drop action` and `POLICER
  ACL` for the whole chip, with no port.

A policer would put a byte-rate cap on every port's DHCP, the uplink
included, would not match UniFi's packet rate, and would report nothing.
**It is not built.** A PR that wants it needs traffic on a test port showing
a policer's exceeded count increase, and a packet-to-byte rate argument the
controller's 300/sec can be held to.

### 9.6 Rogue DHCP reporting (anomaly bit 64)

`Port.Health.RogueDHCPDrops` is the count of DHCP server replies the guard
dropped on that port; `internal/device` turns its growth between informs
into bit 64, which the controller shows as "Rogue DHCP server detected on
<device> Port N".

- **Read** on every collect, only while the driver has lists: one batch of
  `show ip access-lists summary` and `show ip access-lists <list>` for each
  of its lists (about 15 KB of JSON per list). The count is the
  `packetCount` of the guard block's drop entry (the `deny` lines between
  the `dhcp-guard` remark and the next remark).
- **Attribution is exact**, because each interface's list is its own
  hardware ACL with its own counters (§9.1). The lanes of a split cage are
  summed into their port. An aggregate's list is reported on its first
  member, the port UniFi shows the aggregate by. A list bound to more than
  one interface (only by hand) is reported on none: its count is a sum.
- **The read-back** also compares each list's configured and active ingress
  interfaces and its `notFullySupportedByHardware` flag, and logs any
  difference when it changes.
- A rewrite of a port's list starts its counters at zero; the device layer
  raises no bit on a decrease.
- A failed read is logged and leaves the counts unreported for that cycle.

**Verified live:** a `-collect-once` from this branch's build, run while
the two test lists were on the switch, read them and reported
`RogueDHCPDrops` 0 on Ethernet2 and Ethernet54/1 (no traffic was sent).
**Not verified:** a count above zero, that is, a forged or real rogue reply
entering a guarded port. No host is cabled to either test port.

### 9.7 What waits

1. **Traffic on a test port** (Clint: a host cabled to port 2, which is
   guarded in `exact` mode only as an access port of a guarded VLAN, or in
   `edge` mode as a trunk): from that host, UDP 67 -> 68 from an untrusted
   address and from a trusted one; check that the untrusted reply is dropped
   and counted (`show ip access-lists DUI_IP_ACL_Ethernet2`), that the trusted
   one and ARP/IPv6 pass, and how long a rewrite of the bound list drops
   traffic, with and without `hardware access-list update default-result
   permit`.
2. **The end-to-end push** (the main session, after the merge): deploy,
   turn Rogue DHCP Server Detection on, read the guard groups this switch
   gets from the reply log and what the driver logs about each port, and
   restore the setting. In `exact` mode on today's all-VLAN trunks this
   writes nothing.
3. **Choose the mode** for the deployment (`exact` or `edge`, §9.2).
4. **IP rules**: bind more lists (on ports Clint frees for it) to measure
   the ACL id limit and the counter pool, then size `max_custom_ip_acls`.
