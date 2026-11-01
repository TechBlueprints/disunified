# Support matrix — what is verified, what is not, and how to extend it

Every driver here was written against one real device on one site and
verified end to end against Network 10.6.106. Nothing is claimed beyond
that. This file says, per driver, what has actually run, what is expected
to work but has not, what is expected *not* to work, and — for each gap —
exactly what a pull request needs to close it.

**This file is meant to be amended by contributors, including AI agents.**
Every gap below is labelled **PRs welcome** and says what the proof is; the
same phrase marks the corresponding places in the driver write-ups
(`grep -rn "PRs welcome" docs/`). The loop for closing one is at the end
([How to close a gap](#how-to-close-a-gap)), with a prompt to paste into an
agent.

The rule that makes a PR mergeable: **a capture, not a claim.** A new row in a
"verified" table comes with a scrubbed fixture under `docs/fixtures/`, a test
that reads it, and the live check recorded in the PR
([`CONTRIBUTING.md`](../CONTRIBUTING.md)).

---

## `proxmox` — Proxmox VE node as a switch

Write-up: [`drivers/proxmox.md`](drivers/proxmox.md). Fixture: [`fixtures/proxmox-9.1.6/`](fixtures/proxmox-9.1.6).

### Requirements on the node

- Root SSH with a key (the driver reads with one `collect.sh` exec per poll
  and writes with `qm set` / `pct set`; the Proxmox API cannot do the read side).
- `lldpd` installed (`apt-get install lldpd`); the driver writes its config
  unless `manage_lldpd: "false"`.
- A VLAN-aware bridge (`bridge-vlan-aware yes`). A plain bridge is read, but
  guest VLAN tags then mean per-VLAN bridges, which the driver does not model.
- `ethtool`, `chrony` and `iproute2` — present on a stock node. `mstpd` only
  if you want spanning tree ([`drivers/proxmox.md` §4b](drivers/proxmox.md)).

The driver writes two files on the node: `/etc/lldpd.d/disunified.conf`
(unless `manage_lldpd: "false"`) and, from the controller's NTP servers,
`/etc/chrony/sources.d/disunified.sources`, and comments out the `pool`/
`server` lines in `/etc/chrony/chrony.conf` so the node uses only UniFi's
servers. It never edits `/etc/network/interfaces`; when the site's VLANs are outside the bridge's
`bridge-vids` it warns and leaves the file to you.

### Verified

| What | Where / when |
|---|---|
| PVE **9.1.6** (Debian 13, kernel 6.17), three-node cluster, active-backup bond uplink | Clint's cluster, 2026-09-19/20 and daily since (deployed) |
| Read: guests as ports, NICs and bond members as top ports, MAC table, counters, optics, LLDP neighbours, uplink/parent placement | fixture + `internal/device/contract_proxmox_test.go` |
| Control: port state and VLANs (`link_down`, `tag`, `trunks`), IGMP snooping, STP/BPDU guard/port cost under `mstpd`, lldpd config, NTP via chrony | `internal/informloop/replay_proxmox_test.go` + live |
| Adoption safety: the first push after adoption is held while it would change ports; port overrides seeded from the guests' live VLANs | live re-adoption, 2026-09-20 |
| Models `UDC48X6` (USW Leaf, 54 ports) and `USWF07D` (ECS Core, 32 ports) | live |
| Cluster-wide guest numbering (`numbering: cluster`, the default) | live |

### Untested — expected to work — PRs welcome

| Gap | Why it should work | What the PR needs |
|---|---|---|
| **PVE 8.x** | `collect.sh` reads sysfs, `/etc/pve/.vmlist`, `qm`/`pct`, `ethtool` — all unchanged between 8 and 9 | a capture from an 8.x node as `docs/fixtures/proxmox-8.<x>/` (`scripts/sanitize-proxmox.py`), the collector test parameterised over both fixture dirs, `-collect-once` output in the PR |
| **A single node (no cluster)** | `/etc/pve/.vmlist` exists on a standalone node too; numbering has nothing to be cluster-wide *over* | a capture from a standalone node; confirm `-collect-once` and adoption |
| **A bridge other than `vmbr0`** (`options.bridge`) | the option and the tag form (`unifi.pN.c.vmbr1`) exist in `tags.go` and the collector test covers the parse | a live run against a second bridge; a fixture with two bridges |
| **An 802.3ad / balance bond as a LAG** | modelled in §1b; only active-backup has run | a capture from a node with an LACP bond; the LAG rows in the contract test |
| **Guests with several NICs on the bridge** beyond the two-NIC case | `VM-<id> net1` naming exists and is in the fixture | more shapes in a capture; nothing else expected |

### Untested live — modelled, marked in the code — PRs welcome

| Gap | Where it is marked | What the PR needs |
|---|---|---|
| **`numbering: node`** — per-node guest ports (48 per node) instead of 48 across the cluster | `drivers/proxmox.md` §1c; `deploy/config.example.yaml` | run it on a cluster with two nodes' guests numbered independently; confirm a migrated guest lands on a free slot on the destination; a fixture from each node |
| **LACP from the UniFi UI** converting a bond's mode through the Proxmox API | `drivers/proxmox.md` §3b | the round-trip on a node whose uplink you can afford to lose for a minute: aggregate in the UI → bond mode changes → un-aggregate → restored; the pushed `system_cfg` keys added to the replay fixture |

### Known limits (by design; a PR would be a design change, open an issue first)

- **48 guest ports across the whole cluster** with the default numbering:
  the 54-port model, minus the node's NICs. A bigger cluster needs
  `numbering: node` (untested, above).
- The node's own networking under the bridge (NICs, bond membership,
  address) is reported, never managed. The controller's IP Settings are not
  applied to a node.
- No PoE, no port power cycle (the model claims none).

---

## `podman` — Podman host as a switch

Write-up: [`drivers/podman.md`](drivers/podman.md). Fixture: [`fixtures/podman-5.8.2/`](fixtures/podman-5.8.2).

### Requirements on the host

- Root SSH with a key.
- **Rootful Podman** — the driver lists containers as root; rootless
  containers are invisible to it.
- **`python3` on the host.** `collect.sh` reduces `podman inspect` on the
  host with it so container environments (where API keys live) never leave
  the box. A host without `python3` — **Fedora CoreOS** is the common one —
  gets an empty container list.
- **netavark** as the network backend (the default since Podman 4.0 on new
  installs). The older **CNI** backend's `inspect` output is shaped
  differently and has not been seen.
- `iproute2`, `ethtool`, `nsenter` (util-linux) — stock on every host tried.
- **NetworkManager only for the address.** Reading works without it
  (the `nmcli` calls are guarded); on a netplan / systemd-networkd host the
  driver is read-only for the address and says so.

The driver keeps one file on the host: `/var/lib/disunified/podman-slots.json`
(container-endpoint → port, so names survive a container recreate).

### Verified

| What | Where / when |
|---|---|
| Podman **5.8.2** on **AlmaLinux 10.2**, rootful, netavark, the host a QEMU guest of a Proxmox node | Clint's Podman host, 2026-09-27 and since (deployed; it bridges itself) |
| Read: one port per container endpoint, macvlan endpoints as clients with MAC and address, bridge-network endpoints as ports without clients, counters from inside the netns, the host's NICs, a virtio uplink | fixture + `internal/device/contract_podman_test.go` |
| Slot persistence across container recreates; a shared-netns container (`--network container:x`) as its own port | fixture |
| `control.address`: the controller's static IP Settings applied through NetworkManager (`nmcli con mod` + `device reapply`) only when the uplink already carries the address; DHCP refused | live, 2026-09-27 |
| Adoption and placement under the node's `VM-<id>` port | live |

### Untested — expected to work — PRs welcome

| Gap | Why it should work | What the PR needs |
|---|---|---|
| **Other Fedora / RHEL / Debian / Ubuntu hosts** with `python3` and netavark | nothing distro-specific is read except `/etc/os-release` | a capture (`scripts/sanitize-podman.py`) as `docs/fixtures/podman-<version>/`, the collector test over it, `-collect-once` in the PR |
| **A bare-metal host** (a NIC with a real `ethtool` speed and media) | the code path exists (`drivers/podman.md` §2); only virtio has run | a capture from such a host; the uplink's speed/media rows in the contract test |
| **ipvlan networks** | handled on the macvlan path | a capture with an ipvlan network |
| **A host with several NICs / a bond** | top-port layout copied from the proxmox driver | a capture; confirm the uplink pick (`options.uplink` overrides the default-route NIC) |

### Expected **not** to work — PRs welcome, and they are real work

| Gap | Why | What the PR needs |
|---|---|---|
| **Fedora CoreOS** and other hosts without `python3` | the on-host reduce step is Python | rewrite the reduce in `internal/drivers/podman/collect.sh` without Python — `podman inspect --format` Go templates can print exactly the fields the driver reads — keeping the rule that `Env`, `Cmd`, `Args` and mounts never leave the host; a capture from FCOS; the existing fixture must still parse |
| **Rootless Podman** | containers belong to another user's `podman` | decide whether to run `collect.sh` as that user (`ssh <user>@host`, no `nsenter` without root) or to aggregate several users; open an issue first |
| **CNI backend** (Podman ≤ 3, or 4 upgraded in place) | different `inspect` shape; `InterfaceName` semantics differ | a capture from a CNI host and the parser cases for it |

### Not done (read-only by design today) — PRs welcome, open an issue first

- **Port control**: port state → `ip link set … down` inside the netns, or
  `podman network disconnect`; VLANs have no meaning on a Podman network.
- **LLDP** from the host: placement works by MAC without it.
- **Showing bridge-network (NAT'd) containers as something** without
  inventing LAN clients.

---

## `arista-eos` — Arista EOS switch

Write-up: [`drivers/arista-eos.md`](drivers/arista-eos.md). Fixture: [`fixtures/arista-eos-4.26.14M/`](fixtures/arista-eos-4.26.14M).

Verified on one switch, a **DCS-7160-48TC6-F on EOS 4.26.14M**, the last
EOS train for that model; every command was checked against that release.
Everything in [`feature-map.md`](feature-map.md) marked done ran live on it.

**PRs welcome:** any other Arista model or EOS train. The command set is
eAPI JSON (`show …` and config sessions); newer EOS may add fields and may
rename a few. What the PR needs: `docs/fixtures/arista-eos-<version>/` from
`scripts/sanitize-arista-eos.py`, the parser tests over it, and a live
round-trip on one unused port (state, VLAN, speed). Different port layouts
(a 7050, a 7280) also need the model choice checked against
[`unifi-models.md`](unifi-models.md).

**Built, untested live (2026-11-01):** an aggregate member's LACP bundling
(`lacp_state[].active`) from `show port-channel summary`'s `lagMember`,
fixture-tested on the captured Po1 (members down, not bundled). The
partner's system id (`partner_system_id`) is not reported: it needs `show
lacp neighbor` captured on 4.26.14M first. A PR needs that capture and a
live aggregate's `lacp_state` in the controller's record.

---

## `apc-pdu`, `apc-ups`, `apc-backups` — APC power devices

Write-ups: [`drivers/apc-pdu.md`](drivers/apc-pdu.md), [`drivers/apc-ups.md`](drivers/apc-ups.md), [`drivers/apc-backups.md`](drivers/apc-backups.md).

| Driver | Verified on | Path to the device | PRs welcome |
|---|---|---|---|
| `apc-pdu` | AP7931, NMC AOS 3.9.2 | SNMP read, the card's web form for outlet writes | other AP79xx/AP89xx PDUs and newer NMC firmware (the outlet write goes through the card's web form, so a firmware with different pages needs its own capture); an NMC3 |
| `apc-ups` | SMTL1500RM3UC, UPS 15.5 | Modbus TCP on the SmartConnect port | other Smart-UPS models with SmartConnect (the register map is NUT's `apc_modbus`); a Modbus-over-serial or -USB path; the first outlet-group command on any unit |
| `apc-backups` | BG500, UPS 05.3, NMC AOS 6.0.1 | the embedded NMC's web pages | other Back-UPS models with a card; the pages are scraped, so a different firmware needs its own capture |

Each needs the same proof as above: a scrubbed capture under
`docs/fixtures/<driver>-<version>/` (`scripts/sanitize-apc*.py`), tests over it,
and the live check in the PR. For anything that switches an outlet, say in
the issue which outlet is safe to switch.

**Verified live (2026-10-30):** the syslog host on `apc-pdu` (AOS 3.9.2) and
`apc-backups` (AOS 6.0.1) -- the controller's remote logging host in the
card's syslog section in `config.ini`, read back from the card, and the
card's own event (a wrong-community SNMP read) at the log host. **Untested:**
remote logging switched off in UniFi (`Syslog=disabled` is fixture-tested
only) and a host on a port other than 514. **Not done:** the controller's
time zone and NTP server on the cards -- both cards run NTP at a fixed
`-07:00` with daylight saving disabled, so from March to November their
syslog lines are stamped an hour behind.

---

## `sonic` — community SONiC switch (control verified live on the virtual switch, build 202505.1238524, with the exceptions below; the Celestica DX010 adopted on build 202505.1238526, 2026-10-31)

Write-up and specification: [`drivers/sonic.md`](drivers/sonic.md). Fixtures: [`fixtures/sonic-202505.1238524-ff3c0c9f6/`](fixtures/sonic-202505.1238524-ff3c0c9f6) (the VS guest) and [`fixtures/sonic-202505.1238526-ff3c0c9f6/`](fixtures/sonic-202505.1238526-ff3c0c9f6) (the DX010; the same commit); the reference for UniFi's own shapes is the ECS Aggregation's inform in [`fixtures/controller-10.6.109/`](fixtures/controller-10.6.109), and teamd's member records come from upstream SONiC's own test data ([`fixtures/sonic-utilities-202505/`](fixtures/sonic-utilities-202505), Apache-2.0).

### Verified

| What | Where / when |
|---|---|
| SONiC **202505.1238524-ff3c0c9f6**, the **virtual switch** (KVM image) as hardware SKU `Seastone-DX010`, L2 preset: 32 ports, one linked | `-collect-once` against the lab guest, 2026-10-27 |
| Read: ports from CONFIG_DB `PORT` (index, alias, admin state, speed, MTU), oper state and flap count from APPL_DB, SAI counters, VLAN membership, the FDB as the MAC table, identity and health, eth0 as the OOB port | fixture + `internal/drivers/sonic/collector_test.go` + `internal/device/contract_sonic_test.go` (as `USWF07D`) |
| SONiC **202505.1238526-ff3c0c9f6** on a **Celestica DX010** (`Seastone-DX010`, L2 preset), read side: adopted as `USWF07D` and placed behind its uplink's UniFi port (2026-10-31); the hardware tables against its capture -- 12 fans, 2 PSUs (power, ceiling, temperature), 5 sensors, a 100G CWDM4 module with DOM named by its compliance code, FEC codeword counters, `supported_speeds`/`supported_fecs`, the LLDP neighbour as the uplink, eth0 taken down on purpose read as admin down | live on the unit; `internal/drivers/sonic/hardware_test.go`, `internal/informloop/oob_test.go` ([`drivers/sonic.md`](drivers/sonic.md) §10) |
| The ECS Aggregation's hardware shapes on the DX010 (2026-11-01, a second capture with collect.py's `state_lag_member`, `crm` and `hwmon` sections): five fan drawers with airflow direction, the supplies' PMBus chips (three temperatures, fan RPM, input power), `psu_caps`/serials, `mac_table_capability` from CRM, `ipv6`, `displayable_version`, `sfp_rev` -- every ECS key but `psu_type` | `internal/drivers/sonic/hwtables_test.go` + `internal/device/contract_sonic_hw_test.go` against the ECS's own inform (`controller-10.6.109/inform-ecs-aggregation-uswf066.json`, printed by `mca-dump` on the switch) |
| **Live on the DX010 from the controller (2026-11-01, deployed build of 2026-10-31 18:54)**, each override restored to its pre-test value afterwards and a zero-change push confirmed: **Locate** (`set-locate`/`unset-locate`: the status LED `dx010:green:stat` toggling about once a second, steady green after; nothing in pmon fought the blink); a **forced speed** (cage 3 manual 40G: `config interface speed Ethernet8 40000`, then auto: `... 100000`); **breakout** (cage 2 manual 25G: `config interface breakout -f -y Ethernet4 '4x25G[10G]'`, the four children `Ethernet4`-`7` up, MTU 1500 and untagged in the native VLAN; joined back with a manual 100G: `'1x100G[40G]'` and the cage's VLAN restored; no prompt, no swss or syncd restart, the uplink up throughout); **Restart** (the UI command: `reboot requested`, the reload issued about 50 s later, SSH back 112 s after the switch went down, on the same image with the saved config intact, the bridge recovering after one failed collect, the next push writing nothing) | the bridge's log and `show interfaces status` on the unit; the commands are the ones `features_test.go` and `control_test.go` expect |
| LACP state (`lag_member`; `aggregate_num_ports`, `lacp_state[]`, `partner_system_id` on the lowest member) from teamd's STATE_DB `LAG_MEMBER_TABLE`: the driver's reading against upstream SONiC's own member records (bundled, link down, defaulted), and the DX010 capture's first two cages put in an aggregate at the model level held to the ECS's inform (its head key for key, the aggregate keys on its member, the `lacp_state` rows' keys and types) | `TestLACPOfUpstreamRecords`, `TestLACPInactive`, `TestSetLACP`, `TestWireContractSonicAggregate` |
| Adoption sequence, read-only (the ECS Core's recorded replies drive the real driver; nothing is written) | `internal/informloop/replay_sonic_test.go` |
| Port control against the capture: state, name, speed, FEC, jumbo, VLANs with the native mapping, LAG, mirror, storm control; STP port settings and breakout refused on the VS | `internal/drivers/sonic/control_test.go` (a FixtureRunner that applies each write to the capture's CONFIG_DB and refuses what SONiC refuses) + `internal/informloop/replay_sonic_control_test.go` (two recorded controller pushes through the loop) |
| Device control against the capture: NTP/syslog/SNMP with bridge-owned entries only, STP (PVST, feature-gated), SSH keys in a marked block, reboot, the static address (dial-proven, undone when the new address does not answer) | `internal/drivers/sonic/device_test.go` + `TestReplaySonicDeviceLevel` (a recorded device-level push through the loop) |
| The lab session's own replies with control on (`replies-sonic.ndjson`, 274 records: every push from the seeded one to the address moves, the restarts and the site's remote logging switched on, each step's exact commands, a zero-change reconcile on every noop in between), and the jumbo flip both ways with an aggregate present on the session's aggregate push | `internal/informloop/replay_sonic_live_test.go` |
| §7.1 features against the capture: the pushed time zone mapped onto a zone (the switch's zone list captured as `zones.txt`), the hostname (`control.hostname`; on by default for sonic since 2026-10-28, `internal/config/config_test.go`), locate (the LED toggled, stopped, the VS's failure logged once), breakout split/lane-speed/join on the DX010's `platform.json` (`dx010-platform.json`, `dx010-hwsku.json` beside the capture, upstream files) | `internal/drivers/sonic/features_test.go` + `internal/informloop/replay_sonic_locate_test.go` (the recorded set-locate/unset-locate through the loop) + the time zone in `TestReplaySonicPort2NameSpeedVLANs` |
| **Live, from the controller UI** on the VS guest (build 1238524, adopted as `USWF07D`, `control.ports: 2-32`): port state and name; Allow All, Block All, a custom tagged set; aggregation (finished by hand after the MTU refusal that pass; built by the driver in the final pass, next row) and its removal; mirroring and its removal; RS-FEC and storm control (the forced 100G was already the cage speed); jumbo off and on (1500 and the pushed 9216 on 31 ports); NTP servers (hostnames pinned); the controller's SSH keys in the managed authorized_keys block (the operator's lines untouched); Manage → Restart (`sudo reboot`, back in ~2 min, reconnected); IP Settings → Static, a move within the subnet with the runner and the persisted dial target following. Each push reached the switch within one inform cycle and the next reconcile reported zero changes. | [`drivers/sonic.md`](drivers/sonic.md) §6.6, 2026-10-27. The live runs found the PortChannel MTU rule, the apply budget, the NTP churn, the `_type: reboot` reply, `promote_secondaries`, the agent key order and the stale reported address; each is fixed and tested since. |
| **Live, final pass the same evening (build 12e3ae2 → d80f057)**: an aggregate built by the driver itself (PortChannel MTU set through CONFIG_DB before the members join); jumbo off and then on with that aggregate present (members' MTU by `hset`, 31 of 31 ports both times); the aggregate dissolved and its members restored; the reported `ip`/`connect_request_ip` following a move without a restart; the pushed time zone (`config clock timezone MST7MDT`) and hostname (`config hostname SONiCSeastone-DX010`, `control.hostname`); the site's syslog host once remote logging was switched on (`config syslog add`, rsyslog forwarding). | [`drivers/sonic.md`](drivers/sonic.md) §6.6, 2026-10-27 |

### Built, untested live — PRs welcome

| Gap | What the PR needs |
|---|---|
| Syslog host changed or removed from the UI (adding one is verified live, 2026-10-27) | the host changed and then remote logging switched off from Settings -> System, the `del`/`add` logged, the entry gone from `SYSLOG_SERVER`; an operator's own entry left alone |
| SNMP community | **needs the site-wide SNMP setting on** (Settings -> CyberSecure): without it the controller pushes no `switch.snmp.*` at all (enabling SNMP on the device only stores location/contact on the device record). A capture of that push, and of any location/contact keys in it, to map `config snmp location/contact` |
| A move whose new address does not answer (the new address and route removed again) | a move to an address the bridge cannot reach |
| A gateway change in IP Settings (`config route add` then the old route's `del`) | a static push with a new gateway, the route read back, the zero-change reconcile |
| SNMP off on a switch whose `/etc/sonic/snmp.yml` names a community (every image ships `public` there; the snmp container re-imports it at each start, so a CONFIG_DB delete alone came back and was redone every cycle on the DX010): the file is rewritten first (`bash -s -- disunified-snmp-yml`), then `config snmp community del` (§10.1; `snmp_yml_test.go`, the runner models the importer) | the DX010's file was fixed by hand before the fix, so the rewrite never ran live: a switch with the factory file and the site's SNMP off -- the rewrite logged, `snmp_location` kept, `SNMP_COMMUNITY` empty after the snmp restart, the next reconcile writing nothing |
| FEC from the module where UniFi pushes none (§10.3; `fec_test.go`) | the CWDM4 → RS case is verified by hand on the DX010 (its uplink stayed down until `config interface fec Ethernet0 rs`) but the driver's own write never ran (RS was already set): a module in another cage with FEC left at auto in the UI -- the logged `config interface fec`, the link up; any other module type (the table's other rows come from the IEEE clauses and MSAs) |
| NTP from the in-band interface (§10.6: `NTP\|global src_intf`, from the image's `eth0` to the dialled `Vlan<native_vlan>`; `ntp_source_test.go`, `TestDX010FirstPushSourcesNTPInBand`) | the DX010 was fixed by hand (`hset … src_intf Vlan4094`, chrony synced) before the driver could, so its write never ran live: a switch on the image's `src_intf eth0` with a configured eth0 address -- the logged hset, chrony's `bindacqdevice` on the Vlan interface, sources reachable |
| chrony nudged after a reboot (§10.6: `chronyc activity` counting every source offline, the in-band interface up → `sudo chronyc onoffline` once a cycle; `chrony_test.go`, `TestDX010PushNudgesChronyAfterAReboot`, on the synced, offline and after-`onoffline` states read from the unit) | a Restart from the UI on the DX010: the journal's "Source … offline", the bridge's one `chronyc onoffline` log line within a cycle, `chronyc sources` reaching the servers |
| A reboot whose SSH session drops before `sudo reboot` returns (DX010, 2026-11-01: logged as `reboot: context deadline exceeded`), now the reload issued (`TestRebootSessionDrops`, `TestSessionDropped`, the recorded Restart through the loop) | the next Restart from the UI on the DX010: `reboot: switch reload issued` and the bridge reconnecting |
| Speed "auto" from the module (§10.9; `speed_test.go`, `hold_sonic_test.go` on the DX010's real first push with cage 1 at 40G) | the speed and breakout commands are verified by hand on the DX010 (40G and back, 4x25G and back, no restart) but the driver's own module-driven write never ran: a split or a forced speed on a cage holding a 100G module, set back to auto in the UI -- the logged join or speed, the link back; a breakout cable (CA-25G-S/N) in a cage; any module but the CWDM4 |
| The first-push hold without the MTU (§10.2; `hold_sonic_test.go` on the DX010's real held push) | on the DX010 the hold was released by setting 1500 by hand, so the fix never ran live: a newly adopted SONiC switch at 9100 with jumbo off -- its first push applied (1500 on every port), not held |
| `-device-ssh-key` / `DUI_DEVICE_SSH_KEY` (§10.5) | a `-collect-once` with `-device-ssh-key <key>.pub` against an agent holding several keys, the chosen one authenticating |
| LACP state on the DX010 (2026-11-01; held to the ECS's own tables and inform, never read from a community SONiC LAG) | the second core cable in and the aggregate up: `LAG_MEMBER_TABLE` filled by tlm_teamd on 202505, the head port's `lacp_state` both active with the core's system id as partner, the controller's record showing `lacp_state`/`partner_system_id`; and a member pulled reading `active: false` |
| The DX010's hardware tables in the controller (2026-11-01; not deployed) | the controller keeping `mac_table_capability` for `USWF07D` (it drops it for USW models), the PSU rows' input power, fan RPM and three temperatures and the drawers' `direction` in the UI, `total_max_power` as the summed draw (was the capacity), `ipv6`, `sfp_rev` on cage 1 |
| LACP fast rate / min-links on a real LAG partner (2026-10-28: the driver creates `config portchannel add PortChannelN --fast-rate true --min-links 1`, as UniFi's own SONiC switches run their aggregates, and brings a managed PortChannel without them in line with `hset 'PORTCHANNEL\|PortChannelN' fast_rate true min_links 1`; `control_test.go` `TestLAGLACPFastRateAndMinLinks`, `replay_sonic_live_test.go` record 104) | an aggregate to a real LACP partner (the DX010 to a UniFi switch): `teamdctl PortChannel<n> state` showing fast rate and min ports 1, the partner reporting our fast timeout; and an older slow-rate PortChannel taking the hset, with the restart it needs to reach the running team (teammgrd reads both only at creation) |

### Hardware only (the DX010) — PRs welcome

Fans, PSUs and temperatures (pmon), optics and DOM (xcvrd), real speeds and
FEC, `supported_speeds`, PCS/FEC codeword counters and LLDP neighbours from
real cables are read from the DX010 and tested against its capture since
2026-10-31 (Verified, above).

Built and fixture-tested against the DX010's upstream `platform.json`, never
run on hardware:

| Gap | What the PR needs |
|---|---|
| Dynamic port breakout, the modes not yet run on hardware: 2x50G, and a lane speed change on a split cage (25G → 10G) | from the UI on an empty cage: the split, the lane speed on each child, the join; the logged commands and the zero-change reconcile (4x25G and the join back are verified, above) |

### STP and loop protection

Community images ship without the STP daemon (`INCLUDE_STP=y` is off), so the
driver runs RSTP itself where the image lacks `FEATURE|stp`: disunified-l2d on
the switch runs mstpd (built there from the upstream source) on the kernel
`Bridge` and mirrors its port states into the stock orchagent's StpOrch, one
STP instance per VLAN; loop protection (`switch.port.N.keepalive`) is a MAC-move
guard in the same daemon ([`drivers/sonic.md`](drivers/sonic.md) §11.5). Built
2026-11-01, on by default since the driver's own path passed live the same
day (`l2d: off` opts out); fixture- and replay-tested
(`l2_test.go` on `collect-l2.txt`, the DX010 collected read-only;
`replay_sonic_l2_test.go`, the ECS's real spanning-tree keys through the loop;
the read side on `collect-l2-running.txt`, captured with l2d running).
**The mechanism is verified live by hand on the DX010** (§11.5's table): mstpd
joining the LAN's RSTP with Ethernet0 the root port and forwarding, the ASIC
following (`STP_PORT` objects, the VLAN in its own instance), a restart that
adopts mstpd, the topology-change flush, BPDU guard and auto edge on a veth
port, loop protection on two empty cages, the teardown. **The driver's own path
passed live** after a deploy: the build, the install, the first enable under
the dead-man cancelled over a fresh connection, the bridge priority from the
UI. **What still needs a run:** BPDU guard on a down port from the UI after
the reconciliation fix (`l2d_reconcile_test.go`; the coordinator's re-test),
and the guard and loop protection tripping on a real second link, which the
DX010's single cable cannot give. With
`FEATURE|stp` the PVST path (`config spanning-tree`) is untested with the
daemon.

### DHCP snooping and switch ACL rules

UniFi's Rogue DHCP Server Detection with each network's trusted servers, and
the Policy Table's IPv4 ACL rules, as the driver's own L3 ACL tables
(`DUI_DHCP_GUARD`, `DUI_IP_ACL`) bound to every port and PortChannel
([`drivers/sonic.md`](drivers/sonic.md) §11.7.6). Built 2026-11-01 from the
controller's pushes to the ECS Aggregation, fixture- and replay-tested on a
DX010 capture read for it (`acl_test.go`, `informloop/acl_test.go`); claims
`feature_caps` 128 and `max_custom_ip_acls` 128.

| Gap | What the PR needs |
|---|---|
| The push to a claiming community switch | a harmless rule set scoped to the switch from the UI, its `system_cfg` from the reply log (which `switch_caps` count gates it), scrubbed into a fixture |
| IP rule shapes no capture shows: UDP, TCP/UDP, Any as a side, a Network as a side, a source port | that push carrying each shape; until then a rule of that shape holds the IP rules (logged) |
| The hardware: `VLAN_ID` on the native-VLAN stand-in (untagged), a guard drop against an ACL forward, the table's entry capacity | documentation-range rules under the dead-man timer of §11.3, CRM's per-table counters |
| MAC rules | an orchagent patch (an `L2` table type, `SRC_MAC`/`DST_MAC`) in the custom image series (§8, §11.7.5) |

### Refused (logged, not claimed)

IGMP snooping, port isolation, 802.1X, LLDP-MED off (community SONiC has none
of them); MAC ACL rules (no L2 table type in the stock orchagent); IP ACL rule
shapes no capture shows (UDP, TCP/UDP, Any, a source port: the rules of that
kind are held); breakout on a platform without `platform.json`
(the VS), a speed no breakout mode runs, and a breakout of a cage in an
aggregate or mirror session; a pushed time zone no zone on the switch
matches; a hostname that is not a DNS name of at most 63 characters; STP,
the per-port STP settings and loop protection on an image without the STP
daemon while `l2d` is off, or on a switch with neither mstpd nor docker.

### Untested — expected to work — PRs welcome

| Gap | Why it should work | What the PR needs |
|---|---|---|
| **Other hardware** (the DX010 is read since 2026-10-31) | the same Redis tables and `config` CLI; pmon fills `FAN_INFO`/`PSU_INFO`/`TEMPERATURE_INFO`, xcvrd fills `TRANSCEIVER_INFO`/`_DOM_SENSOR`, and the parser reads them as it does the DX010's | a capture from the unit as `docs/fixtures/sonic-<build-id>/` (`scripts/sanitize-sonic.py`), the collector test over it, `-collect-once` in the PR |
| **Other SONiC branches** (`202411`, master) and other hardware SKUs | `swsscommon`, the table names and the `config` groups `Start` checks are stable across branches | a capture per branch; `Start`'s `--help` checks pass |

### Known limits (by design)

- **UniFi's VLAN 1 is `Vlan<native_vlan>` on the switch** (default 4094):
  SONiC cannot create VLAN 1. Pick an id no UniFi network uses
  ([`drivers/sonic.md`](drivers/sonic.md) §5.2a).
- **SSH to a stock SONiC** needs `rsa-sha2-*` host-key signatures (its only
  host key is RSA, SHA-1 `ssh-rsa` is refused); `internal/sshrun` offers
  them for an `ssh-rsa` known_hosts entry.
- **The bridge user needs passwordless sudo** (`config` needs root even for
  `--help`); the image's `admin` has it.
- **An address move needs the switch's host key in known_hosts under the new
  address** before the change: the bridge dials it with the same known_hosts.
- **Writes are many ~250 ms CLI calls**: a first full push on 31 ports is
  ~16 s; `control.apply_timeout` (default 90 s) bounds a write apart from the
  20 s collect.

---

## How to close a gap

1. **Open an issue** naming the row in this file, your device/OS version,
   and the output of `disunified -collect-once` (scrubbed).
2. **Capture first.** Run the driver's collector against your device
   (`-collect-once`, or the driver's `collect.sh` over SSH for the host
   drivers), scrub it with `scripts/sanitize-<driver>.py`, and commit it as
   `docs/fixtures/<driver>-<version>/`. Run `scripts/check-site-info.sh` —
   it must pass; if it flags something, fix the capture, never the checker.
3. **Point the tests at it.** The collector tests in
   `internal/drivers/<driver>/` load the fixture by path
   (`collector_test.go`); add a case for yours beside the existing one so
   both stay covered.
4. **Run it live** against your controller with no `control` block (the
   first push after adoption is held until the controller's seed matches
   the device), then exercise the controls from the UI on one unused port.
   Record in the PR what you saw: the `-collect-once` snapshot, the adoption log lines, what the UniFi
   UI showed, and for control the pushed keys and the commands written.
5. **Move the row** in this file from the gap table to the verified table,
   with the version and the date, and fix the driver write-up where it
   said "untested".

A prompt that does the above with a coding agent, to paste and fill in:

> *"The `<driver>` driver in `TechBlueprints/disunified` lists `<gap>` as
> untested in `docs/support-matrix.md`. I have `<device / OS version>` at
> `<address>`. Following `CONTRIBUTING.md` and the "How to close a gap"
> steps in `docs/support-matrix.md`: capture and scrub a fixture from it,
> extend the collector tests to cover it, run the bridge against my UniFi
> controller at `<controller>` read-only, then with control on port `<N>`
> only (it is unused), and open a PR that moves the row to verified with
> the evidence. Never put a real address, MAC, serial, name or key in a
> tracked file; `scripts/check-site-info.sh --staged` must pass before
> every commit."*
