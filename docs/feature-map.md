# UniFi switch feature map: every protocol feature, both directions

Goal (Clint, 2026-09-19): a comprehensive bridge that maps **all** UniFi switch features the
protocol carries, in both directions, before deployment. This is the inventory to build
against. Status legend: **done** = live and verified on the controller/switch; **report** =
device→controller only; **apply** = controller→device; **todo**; **n/a** = the Arista has no
such hardware; **verify** = the key/field is known from prior art or real-firmware captures
but not yet observed from Clint's controller (Network 10.6.106) — capture it before building.

**Every EOS batch leads with `enable`**: eAPI and fresh SSH sessions start at privilege 1 and
`show interfaces flow-control` refuses to run there; eAPI also does not expand abbreviated
keywords (`show interface …` fails with "Incomplete token").

Two channels exist:

- **Device → controller**: the inform payload (`port_table`, switch-level fields). What the
  UI *shows*.
- **Controller → device**: `setparam.system_cfg` (the UniFi device config file, observed) and
  `setstate` (`port_overrides`/`port_table`, per prior art, **not yet observed** on 10.6.106),
  plus `cmd` (`reboot`, `upgrade`, `setdefault`, `set-locate`, `set-adopt`…; `power-cycle` only for PoE ports). What the UI
  *controls*.

The controller only sends a `system_cfg` key when the feature is non-default in the site,
so the captured file ([`fixtures/controller-10.6.106/system_cfg.txt`](fixtures/controller-10.6.106/system_cfg.txt)) shows defaults only.
**Every "verify" row needs a capture: Clint sets the feature in the UI (port 2 for port-level
settings), the bridge records the delta.** That is the capture campaign in §4.

## 0. Two drivers, one map

Each table carries a status column per driver. **Arista** is the DCS-7160
on EOS 4.26.14M ([`docs/drivers/arista-eos.md`](drivers/arista-eos.md)); **Proxmox** is a VE 9.1
node's `vmbr0` ([`docs/drivers/proxmox.md`](drivers/proxmox.md)), which covers a subset by design:
a Linux bridge has no per-port speed, FEC control, storm control, mirror or
flow control, so those capabilities are not claimed and the UI hides them.
STP on Proxmox is claimed and honoured only on a node whose bridge runs
under mstpd ([`docs/drivers/proxmox.md`](drivers/proxmox.md) §4b). Both verified live on Network
10.6.106, 2026-09-19/20.

## 1. Device → controller: switch-level fields

| Field | Meaning | Arista source (EOS 4.26.14M) | Arista | Proxmox |
|---|---|---|---|---|
| `mac`, `serial`, `model`, `model_display`, `version`, `ip`, `hostname`, `inform_url` | identity | `show version`, `show hostname`; model string claimed (UDC48X6) | done | done (bridge MAC, DMI serial, node hostname) |
| `uptime`, `time` | clocks | `show version` uptime | done | done |
| `cfgversion`, `x_authkey`, `default`, `_default_key`, `state`, `fw_caps` | protocol state | session | done | done |
| `sys_stats` (`cpu`, `mem_total`, `mem_used`, `mem_buffer`) | health | `show processes top once` | done | done (`/proc/stat`, `/proc/meminfo`) |
| `general_temperature`, `has_temperature` | temperature | `show system environment temperature` (hottest sensor) | done | done (hwmon: coretemp) |
| `overheating` | bool | same command, `systemStatus != temperatureOk` | done | done (hwmon critical thresholds) |
| `fan_level`, `has_fan`, `fan_table` | cooling | `show system environment cooling` | done (controller shows fan_level; `fan_table` rows use the real keys: index, label, present, status, speed, target_speed) | done (hwmon: dell_smm) |
| `psu_table`, `total_max_power`, `power_consumption` | power supplies | `show system environment power` | done — rows must use the real keys (`psu_idx`, `label`, `present`, `online`, `power`, `power_capacity`, `temp`); with invented keys the UI showed both supplies as "Not Installed" (fixed 2026-09-19) | n/a (no PSU sensors on a workstation-class node) |
| `total_max_power`, `power_source`, `psu_*` | PSU | `show system environment power` | done (`total_max_power` stored; `power_consumption` ignored) | n/a |
| `port_table` | per port, see §2 | `show interfaces` + status + STP + LLDP | done | done |
| `ethernet_table` | mgmt NICs | static | done | done |
| `lldp_table` (`local_port_idx`, `local_port_name`, `chassis_id`, `port_id`, `is_wired`) | topology to other devices | `show lldp neighbors detail` | done | done (lldpcli on the NICs) |
| `mac_table` (`mac`, `port_idx`, `vlan`, `age`, `uptime`) | which clients sit behind which port — **this is what places downstream devices under the switch in the topology and client list** | `show mac address-table` (`unicastTable.tableEntries`) | **done — verified: `stat/sta` shows a wired client with `sw_mac` = the Arista and the right `sw_port`** | done (bridge FDB, dynamic entries) |
| `uplink` (**the string `"eth0"`**), `if_table` (that interface: ip, netmask, num_port, counters), `port_table[].is_uplink`, `lldp_table` | which port faces the controller; the list's Uplink/Parent columns, "Connected To", `uplink_depth`, child nodes in the topology map | port 49 (LLDP to the aggregation switch) | **done, verified 2026-09-20.** A real switch reports `uplink` as the *name* of its management interface in `if_table`, not an object; the controller composes the uplink record (`uplink_source: lldp_uplink`, remote port, media, speed) from that plus `is_uplink` and LLDP. Sending an object had it ignored for a day. Also needs the management address in-band (§2c) and no LLDP on the OOB port | done, verified 2026-09-20 (the active bond slave; the node runs lldpd on it) |
| `stp_version` (`rstp`/`stp`/`disabled`), `stp_priority` | STP | `show spanning-tree` (`protocol`, `bridge.priority`) | sent (`rstp`, `"32768"`); the controller keeps the settings on the config side (`switch.stp.*`, §3) and stores these as null | sent `disabled` unless the bridge is under mstpd, then its version and priority |
| `system-stats` (`cpu`, `mem`, `uptime` as strings), `sys_stats.loadavg_*` | Memory Usage / System Statistics graphs in Insights | `show processes top once` (`timeInfo.loadAvg`) | done, verified in the UI 2026-09-20 | done |
| `connect_request_ip`/`_port`, `gateway_mac`, `ethernet_table` (`eth0` + `srv0`), `service_mac` | reachability and interfaces, as real switches report | `show ip arp` (gateway MAC = ARP for the controller host), `show interfaces` (Management1 MAC = service MAC, reported only while that port is unplugged) | done; stored by the controller | done (`ip neigh` for the gateway MAC) |
| `mac_table_capability` | MAC Table Pressure card | `show hardware capacity` (L2/FDB 131072) | sent; **controller drops it** — only the ECS aggregation (USWF066) has it on this site, USW models do not; likely model/firmware gated. Low value | n/a (a Linux FDB has no fixed capacity) |
| `root_switch` | STP root bridge MAC (the topology's root marker; UniFi switches report it) | `show spanning-tree root detail` (`instances.*.rootBridge.macAddress`; the switch's own MAC when it is the root) | done | n/a (no STP root to name unless under mstpd) |
| `jumboframe_enabled`, `mtu` | switch-wide MTU | `show interfaces` mtu | sent true; controller stores false (config-side setting, like STP). Per-port `jumbo` is stored | sent from the bridge MTU; report only |
| `flowctrl_enabled` | global flow control | `show interfaces flow-control` (full spelling; needs enable) | done | n/a (not claimed) |
| `dot1x_portctrl_enabled` | 802.1X | n/a unless configured; report false | todo | n/a (report false) |
| `dhcp_server_table`, `ip_table` | L3 features | Arista is pure L2 here | n/a | n/a |
| `ssh_session_table` | active SSH sessions | `show users` | todo (cosmetic) | todo (cosmetic) |
| `led_override`, `locating` | LED/locate | no LED control via eAPI on 7160 | report false | report only (no LED) |
| `speedtest-status`, `isolated`, `selfrun_beacon`, `bootrom_version` | misc | static | done | done |

## 2. Device → controller: `port_table` per port

| Field | Arista source | Arista | Proxmox |
|---|---|---|---|
| `port_idx`, `ifname`, `name`, `media`, `is_uplink`, `poe_caps`, `port_poe` | profile + collector | done | done (guests: QSFP28 100G virtio; NICs: media from `ethtool -m`) |
| `up`, `enable`, `speed`, `full_duplex`, `mtu`, `stp_state` | `show interfaces`, `show spanning-tree` | done | done (`ip -d link`, bridge port state) |
| `rx_bytes/packets/errors/dropped`, `tx_*` | `interfaceCounters` | done | done (`ip -s link`) |
| `rx_multicast`, `rx_broadcast`, `tx_multicast`, `tx_broadcast` | `interfaceCounters` | done | done |
| `sfp_found` | line protocol `notPresent` | done | done (module present per `ethtool -m`; free slots are empty cages) |
| `sfp_vendor`, `sfp_part`, `sfp_serial`, `sfp_compliance` | `show inventory` (`xcvrSlots`) + `show interfaces transceiver` (`mediaType`, `vendorSn`) | done (controller shows vendor/part) | done (`ethtool -m`) |
| `sfp_temperature`, `sfp_voltage`, `sfp_current`, `sfp_rxpower`, `sfp_txpower` | `show interfaces transceiver` (`temperature`, `voltage`, `txBias`, `rxPower`, `txPower`) | done; DAC cables (port 53) have no DOM, so those stay absent | done for the module as a whole (`ethtool -m`); no per-lane DOM |
| `autoneg` | `show interfaces status` `autoNegotiateActive` | done. `speed_caps` todo | done (`ethtool`); `speed_caps` from the supported link modes |
| `flowctrl_rx`, `flowctrl_tx` | `show interfaces flow-control` | done | n/a |
| `jumbo` | mtu > 1518 | done | done |
| `op_mode` (`switch`/`aggregate`) | `show port-channel summary` membership | done (mirror todo) | done: `aggregate` only for an 802.3ad/balance bond's slaves |
| `aggregated_by` | `show port-channel summary` | done (Po1-4 exist, no members) | done (the bond) |
| `stp_pathcost` | `show spanning-tree` `cost` | done | the bridge port's cost (kernel or mstpd) |
| `mac_table_count`, `link_down_count`, `stp_state_change_count`, `sfp_rxfault`/`sfp_txfault`, `ifname` (vendor name), setting echoes (`stp_port_mode`, `stp_edge_port`, `lldpmed_enabled`, `port_keepalive_enabled`, `isolation`, `egress_rate_limit_kbps_enabled`, `port_security_*`, `locating`) | what a real switch's port entry carries | `Port.Health`, `Port.MACs`, config state | done 2026-09-20; stored by the controller | done (carrier changes, FDB count, config echoes) |
| `anomalies` (bitmask), `satisfaction` (0-100), `satisfaction_reason` | per-port Anomaly / Experience columns | derived in [`internal/device`](../internal/device) (`portAnomalies`) from `Port.Health` + counters. Bit values as the Network 10.6 UI uses them: 1/2 optic rx/tx outside its alarm thresholds (`show interfaces transceiver dom thresholds`); 4 errdisable `xcvr-*`; 8 STP guard inconsistency (`show spanning-tree` `inconsistentFeatures`); 16/32 STP changes since last inform ≥3 / ≥1 (`show spanning-tree topology status detail`); 64 link changes ≥2 since last inform (`linkStatusChanges`) or errdisable `link-flap`; 512 errdisable loop-protect; 1024 uplink below its top speed; 2048 errdisable bpduguard; 4096 errors, FEC uncorrected codewords or PCS errored blocks grew, or PCS high-BER (`show interfaces X phy detail`, text, every up port — copper included); 8192 drops grew; 32768 errdisable portsec. Satisfaction follows this controller's own switches (30 sampled 2026-09-19): 100, −10 reason 1 if the port ever dropped packets, −15 reason 2 if it ever counted errors; −10 for a slow uplink (our rule). Not derivable on EOS 4.26: 128 MCLAG, 256 PoE budget | done, verified live | done: link changes (64), errors (4096) and drops (8192) from the kernel counters; no optic, FEC or STP bits |
| `dot1x_mode`, `dot1x_status` | n/a | report `auto`/`disabled` | n/a |
| `portconf_id`, `port_security_*`, `isolation` | echo from controller | merged from pushed config | merged from pushed config |
| `mac_table` per port | `show mac address-table` | done | done (FDB per bridge port) |
| `fec_mode` (`rs-fec`/`fc-fec`/`disabled`) | `show interfaces error-correction` (`errCorrEncodingStatus`: reedSolomon / fireCode / disabled) | **done, reported** (controller stores it: 49/51/53 rs-fec, breakout lanes fc-fec). Apply side needs the UI capture | reported for the NICs from `ethtool --show-fec`; not controllable |
| `poe_*` | n/a (no PoE) | n/a | n/a |

## 3. Controller → device: `system_cfg` keys (observed + expected)

Observed on 10.6.106 (defaults) are marked **obs**; the rest are from the real-firmware
config format (wvengen, UniFi switch `/tmp/system.cfg` write-ups) and must be **verified**
by capture before implementing.

| Key family | Meaning | EOS translation | Arista | Proxmox |
|---|---|---|---|---|
| `switch.port.N.status=enabled/disabled` **obs** | admin state | `[no] shutdown` | done | done (`link_down` on the guest NIC), verified live |
| `switch.port.N.name` **obs** | port name | `description` (controller default names `SFP28 N`/`QSFP28 N` and profile `Port N` → `no description`) | done, verified live | report only: names come from the guest (`VM-<id>`), UniFi renames are not written back |
| `switch.port.N.ld_mode=disabled` **obs** (loop detection?) | | ignore for now | verify | ignore |
| `switch.port.N.opmode=aggregate` + `switch.port.N.lag=<id>` **obs** | link aggregation | `channel-group <id> mode active` on every lane; the Port-Channel carries the VLAN config | **done, verified live** (ports 2+3) | bond to/from 802.3ad through the Proxmox API on the node's physical ports, **untested live** ([`docs/drivers/proxmox.md`](drivers/proxmox.md) §3b) |
| `switch.port.N.opmode=mirror` + `switch.port.N.mirror_port=<src>` **obs** | port mirroring (N is the destination) | `monitor session <N> source Ethernet<src> both` / `destination Ethernet<N>` | **done, verified live** | n/a (not claimed) |
| `switch.vlan.<slot>.port.N.mode=untagged\|tagged\|exclude` **obs** (slot → `switch.vlan.<slot>.id`; no lines = "Allow All": every VLAN tagged, VLAN 1 untagged) | per-port VLAN membership | trunk + `switchport trunk native vlan` + `switchport trunk allowed vlan <list>\|all`; "Block All" (native only) = access port | **done, verified live** (port 2: native 1, tagged 2+10 → `allowed vlan 1-2,10`) | done (`tag`/`trunks` on the guest NIC), verified live |
| `switch.vlan.N.id/mode/status` **obs** | site VLAN list | `vlan <ids>` created before ports reference them; extra VLANs on the switch are left alone | **done, verified live** (69, 4000 created) | checked against `bridge-vids` when the bridge limits them; a VLAN-aware bridge carries all |
| `switch.managementvlan` **obs** | mgmt VLAN | Ma1 is out-of-band; report only | report | report |
| `switch.mtu`, `switch.jumboframes` **obs** | jumbo | 7160: no per-port L2 MTU (`l2 mtu` unsupported); always forwards jumbo; logged when UniFi asks for off | report only | report only |
| `switch.port.N.autoneg=disabled` + `speed=10000` + `duplex=enabled` **obs** (absent = auto) | link speed | `speed forced 10gfull` (keyword table in `eos/vlan.go`); auto → `speed auto` on copper only — optical ports run forced by hand and are never set to auto by the bridge | **done, verified live** | n/a (not claimed; virtio has no speed) |
| `switch.port.N.fec=cl-91\|cl-74` **obs** (absent = leave alone) | FEC | `error-correction encoding reed-solomon\|fire-code`; `no error-correction encoding` on explicit disable | **done, verified live** (port 54) | n/a (not claimed) |
| `switch.port.N.flowctrl.*` **verify** | flow control | `flowcontrol receive/send on\|off` | todo | n/a |
| `switch.port.N.isolation` **verify** | port isolation | `switchport port-security`? / protected ports — EOS lacks a direct equivalent; ACL-based | verify | n/a |
| `switch.stp.status/version/priority`, `switch.port.N.stp.port_mode`, `.stp.bpdu_guard` **obs** | STP | `spanning-tree mode rstp\|none`, `spanning-tree priority N`; port_mode=disabled → `spanning-tree portfast` + `bpdufilter enable`; bpdu_guard → `spanning-tree bpduguard enable` | **done, verified live** | under mstpd: version follows (`mstpctl setforcevers`), priority pinned at 61440, per-port BPDU guard; otherwise logged and ignored |
| `switch.port.N.stormctrl.status/type/bcast/mcast/ucast` **obs** | storm control (percent) | `storm-control broadcast\|multicast\|unknown-unicast level X` | **done, verified live** | n/a (not claimed) |
| `switch.port.N.lldpmed.opmode` **obs** | LLDP-MED | no per-port MED toggle on EOS 4.26; logged when disabled | report only | n/a |
| `switch.port.N.dot1x.*`, `switch.dot1x.*` **verify** | 802.1X | out of scope unless asked | n/a | n/a |
| `switch.port.N.poe.*` | PoE | n/a | n/a | n/a |
| `switch.vlan.<slot>.igmp_*` **obs** | IGMP snooping per VLAN | `[no] ip igmp snooping vlan N` (`-control-igmp`) | **done, verified live** | done, bridge-wide (`multicast_snooping`, `control.igmp`) |
| `switch.dhcp_snoop.status` **obs** (Settings → Networks → Global Switch Settings → Rogue DHCP Server Detection, site-wide) | block DHCP servers on non-uplink ports | none: EOS 4.26 DHCP snooping is Option-82 insertion (plus `bridging`) with no trusted-port model (`ip dhcp snooping trust` is invalid), so it cannot block a rogue server; `ip dhcp snooping` + `vlan` list sat "not operational" when tried | **n/a, verified 2026-09-20**: not claimed, so the controller does not push the key (it pushed it only while the capability was claimed; both pushes are replies 14-15 of the replay fixture and write nothing). Note the controller sends one global flag with no per-VLAN keys, so even a capable switch would get it on every VLAN, DMZ VLANs included | n/a (not claimed, never pushed) |

| `ntpclient.N.server` **obs** | NTP | `[no] ntp server X` (`-control-ntp`) | **done, verified live** | done (a chrony sources file, `control.ntp`) |
| `syslog.status/ip/port` **obs** | remote syslog | `[no] logging host X [port]` (`-control-syslog`) | done (no host set in UniFi yet) | logged and ignored |
| `snmp.*` **verify** | SNMP | `snmp-server community` | todo | logged and ignored |
| `sshd.auth.key.N.*` **obs** | site SSH keys | `username <bridge user> ssh-key [secondary] …` (EOS: 2 keys per user; `-control-ssh-keys`) | **done, verified live** | logged and ignored |
| `users.N.name/password` **obs** (MD5-crypt), `sshd.auth.key.N.*` | device login for the UniFi terminal | **Not served.** The UI's Manage → Debug terminal is a WebRTC session the controller asks the device to build (`build-ssh-session` inform cmd), not SSH to the device IP; it is hidden by not claiming `fw_caps` UTERM (4). An SSH gateway that honours these credentials over plain SSH is parked on branch `ssh-gateway` (`docs/ssh-gateway-status.md` there). Keys are still installed on the switch user (`-control-ssh-keys`) | parked | parked (same WebRTC terminal) |
| `unifi.*`, `netconf.*`, `dhcpc.*`, `resolv.*`, `route.*`, `bridge.*` **obs** | UniFi OS internals | ignore | n/a | n/a |
| `system.timezone`, `locale.timezone` **obs** | clock | `clock timezone` | todo (safe) | todo |

### `setstate` (prior art; **not yet observed** on 10.6.106)

`port_overrides` / `port_table` config objects. If 10.6 sends them for some edits (port
profiles are the likely case), they carry the same intent as `system_cfg` in JSON form.
The session already stores and echoes them; the apply path must treat them as a second
source of the same per-port intent.

### `cmd`s

| cmd | Meaning | Arista | Arista status | Proxmox |
|---|---|---|---|---|
| `set-adopt`, `setdefault` | adoption lifecycle | session | done | done |
| `reboot` | reboot the device | emulated by default; `-control-reboot` → `write memory` + `reload now` | done | emulated only |
| `upgrade`, `upgrade2` | firmware | emulated: accept, report the requested version from then on (persisted in `State.Firmware`); `firmware:` in config overrides the default | done | emulated |
| `set-locate` / `unset-locate` (10.6 names; `locate`/`unlocate` too) | blink LEDs | reports `locating`; no LED control on the 7160 | done, replayed | reports `locating` |
| `speed-test`, `traceroute`, `ping` | diagnostics | `ping`/`traceroute` via eAPI, report results | todo (nice to have) | todo |
| `power-cycle` (`port_idx`) | PoE power cycle | `shutdown`, 3 s, `no shutdown` on every lane (for a PoE-capable driver) | n/a here: the controller only issues it for a PoE port powering a device (`api.err.InvalidTargetPort` otherwise, verified 2026-09-20), and the UI's "Power Cycle" appears only on such ports; the 7160 has no PoE, so no capture exists | implemented as a `link_down` bounce; the controller never issues it for a non-PoE port |
| `cable-test` | TDR | EOS has no TDR on 7160 | n/a | n/a |
| `clear-counters`? | reset stats | `clear counters` | verify | verify |

## 4. Capturing a new key

Capabilities are claimed (`switch_caps`, `fw_caps`, `speed_caps` per driver),
so the port settings drawer and the switch settings show every control the
driver can honour. To turn a **verify** row into a mapping: change one
thing at a time in the UniFi UI on the driver's test port, read the delta
in the reply log (`inform-log/<name>/<mac>.ndjson`; `State.SystemCfg` keeps
the last applied file), scrub it and add it under
[`docs/fixtures/controller-10.6.106/`](fixtures/controller-10.6.106) as
`system_cfg-<what>.txt`, then add the key to
[`internal/unificfg`](../internal/unificfg) and its parser test, and the
expected switch commands to the driver's replay test. No port power cycle
can be captured here: the controller offers it only for a PoE port that is
powering a device.

## 5. Model choice (the SFP28-vs-copper question)

The controller's hardware DB (unifi-emu's catalogue, Network 10.4.57; the 10.6.106 DB can
be regenerated from Clint's controller with unifi-emu's `modelgen`) contains **no switch
with 10GBASE-T ports at all** — `10GbE` media only appears on gateways and APs. The only
models with 48 front ports and QSFP28 uplinks are:

| Model | Front | Uplinks | Notes |
|---|---|---|---|
| `UDC48X6` (current) | 48x SFP28 | 6x QSFP28 | exact port and uplink count; shipping model; controller names it "USW Leaf" |
| `USWF066` | 48x SFP28 | 6x QSFP28 | same layout, internal code, older firmware string |
| `USWF006`/`USWF007` | 48x GE | 4x SFP28 + 2x QSFP28 | copper front, but only 2 of the 6 100G ports fit |
| `US648P` | 48x GE | 6x SFP+ | copper front, PoE implied, uplinks render as 10G SFP+ |

**Correction:** the profile entry for UDC48X6 lists `knownUnsupportedFeatures`
(DOT1X, LLDP_MED, EGRESS_RATE_LIMIT) but the UI still offered the LLDP-MED checkbox once
the capability was claimed, so that list does not hide controls; treat it as advisory.

**Update 2026-09-19, verified on the controller:** the per-port `media` the *device* reports
in `port_table` **is** stored and drawn once capabilities are accepted (`udapi_version`
present): reporting `10GbE` on ports 1-48 turned their icons from SFP cages into plain RJ45
squares on UDC48X6, while 49-54 stay QSFP28. The controller's hardware DB has no "10G copper"
class at all — every RJ45 port is `standard`, and the speed comes from the device. So the
copper-vs-SFP28 question is solved without changing model; only the default port *names*
("SFP28 N") still come from the profile.

Also seen on Clint's controller (10.6.106):
the "internal" `USWF0xx` codes are real ECS products — his aggregation switch is a `USWF066`,
display name `ECSAGG` (Enterprise Campus Switch Aggregation, 48x SFP28 + 6x QSFP28, firmware
4.0.1.995). `USWF006`/`USWF007` (48 `standard` + 4 SFP28 + 2 QSFP28, PoE true/false) are
therefore the ECS 48-port models. The EAV-XG-24-PoE is not offered by 10.6.106.

What the media label affects: the port icon; which **speed options** the UI offers on the
port (SFP28: 1/10/25G; 10GBASE-T: 100M/1G/2.5G/5G/10G); PoE controls (none on SFP28,
correct for us); and possibly an "SFP module not detected" hint on empty SFP28 ports —
the bridge currently omits `sfp_found` on the copper ports, so watch what the UI shows.
It does **not** affect adoption, stats, VLANs, STP, or enable/disable. Recommendation:
stay on `UDC48X6`; if the UI shows a module warning on ports 1-48, report `sfp_found: true`
there (harmless) rather than change model.
