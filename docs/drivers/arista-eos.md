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
- **No rogue-DHCP-server protection.** UniFi's "Rogue DHCP Server Detection" (Global Switch
  Settings, site-wide; `switch.dhcp_snoop.status`) drops DHCP server packets on non-uplink
  ports. EOS 4.26's `ip dhcp snooping` is a different feature: it intercepts DHCP on the listed
  VLANs to insert Option 82 (`ip dhcp snooping information option`) or to bridge them
  (`ip dhcp snooping bridging`); there is no trusted-port model (`ip dhcp snooping trust` is
  `invalid command`, probed 2026-09-20 in an aborted config session). Enabled without the
  information option it reports "not operational" on every VLAN and does nothing. The driver
  therefore does not claim the capability, and the controller then never sends the key
  (verified 2026-09-20: it sent `enabled`/`disabled` only while the claim was in place). The
  controller also sends one site-wide flag with no per-VLAN keys, so an all-VLAN application
  would have reached the L2-only DMZ VLANs too.

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
   On 10.6.109 neither is expected, because the switch handler drops the
   field. Record what a newer Network release does.
7. Add the override capture to `fanmode_test.go` (Quiet read back, Quiet
   idempotent) and mark the feature verified in this section, the support
   matrix and the feature map.
