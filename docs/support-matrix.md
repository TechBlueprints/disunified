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
| Rogue DHCP Server Detection: the driver's `table bridge disunified` drops and counts untrusted DHCP replies from guest NICs (untagged and tagged VLANs, plain and firewalled NICs), one count per frame, written and deleted whole | the driver's own write run on a PVE 9.1.6 node inside a throwaway network namespace (`fixtures/proxmox-9.1.6/dhcpguard-netns-test.*`, 2026-11-14); `dhcpguard_test.go` |

### Untested — expected to work — PRs welcome

| Gap | Why it should work | What the PR needs |
|---|---|---|
| **PVE 8.x** | `collect.sh` reads sysfs, `/etc/pve/.vmlist`, `qm`/`pct`, `ethtool` — all unchanged between 8 and 9 | a capture from an 8.x node as `docs/fixtures/proxmox-8.<x>/` (`scripts/sanitize-proxmox.py`), the collector test parameterised over both fixture dirs, `-collect-once` output in the PR |
| **A single node (no cluster)** | `/etc/pve/.vmlist` exists on a standalone node too; numbering has nothing to be cluster-wide *over* | a capture from a standalone node; confirm `-collect-once` and adoption |
| **A bridge other than `vmbr0`** (`options.bridge`) | the option and the tag form (`unifi.pN.c.vmbr1`) exist in `tags.go` and the collector test covers the parse | a live run against a second bridge; a fixture with two bridges |
| **An 802.3ad / balance bond as a LAG** | modelled in §1b; only active-backup has run | a capture from a node with an LACP bond; the LAG rows in the contract test |
| **Rogue DHCP Server Detection end to end**: the controller pushing `switch.dhcp_snoop.status` to a node that claims it, the table on the node's own bridge, the rogue-DHCP alert (64) | the rule set ran on the node's kernel in a namespace (above); the push is the one the ECS gets (`controller-10.6.109/system_cfg-ecs-dhcp-snoop.txt`) | the push from a node's reply log in the replay fixture; the table read back by `-collect-once`; a rogue reply from a throwaway guest and the alert in the UI ([`drivers/proxmox.md` §4c](drivers/proxmox.md)) |
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
- No storm (1/2/4) or DHCP-rate (32) anomaly: a node applies no limiter,
  and none is built on a production host; the node's own uplinks are not
  DHCP-guarded (the upstream switch guards the LAN).

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
- **Rogue DHCP Server Detection** and the other switch-detected anomalies:
  not built. macvlan's bridge mode delivers a container's frames to the
  host's other macvlan containers inside the driver, so no hook on the
  parent NIC sees every reply ([`drivers/podman.md` §8](drivers/podman.md)).
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

**Storm-control drops (2026-11-14), verified live for unknown unicast:**
`show storm-control`'s per-type `drop` fills the port's storm counters, and
the controller's broadcast/multicast/unicast storm bits follow their growth.
On 2026-11-14 an unknown-unicast storm was made on the uplink at 0.01 % (the
lowest level EOS keeps, about 1 Mb/s at 10G), the drop count grew (captured,
`storm-control-drops/`), and the deployed bridge's inform raised the
controller's "Unicast Storm Detected" alert for that port
(`PORT_UNICAST_STORM`, "Incoming unknown unicast packets on <switch> Port 1
exceed the Storm Control threshold.", 2026-11-14 23:25:59 UTC)
([`drivers/arista-eos.md`](drivers/arista-eos.md) §9.8). `clear counters`
does not reset the count. **Untested live:** bits 1 and 2 (a broadcast or
multicast storm above 1 Mb/s floods the whole LAN; the same code path, keyed
by EOS's type name) and a link flap's effect on the count. A PR needs a
host on a test port sending broadcast or multicast above the level.

**Fan Mode, verified live 2026-11-14** through the device's `fan_mode:
default | quiet` (the controller keeps Fan Mode only for a gateway: on
Network 11.0.99 the switch's Settings panel has no Fan Mode and its record
no `sys_fan_mode`). Default is EOS's automatic control (`no environment
fan-speed`); Quiet is `environment fan-speed override 30`, a fixed speed
(the trays then report 39 %, against 43 % under auto). Applied through the
deployed bridge, read back (`overrideFanSpeed` 30, captured), a Default
round trip and back, seven hours of temperatures under Quiet (the CPU
sensor 67.7 °C against a 90 °C alert; [`drivers/arista-eos.md`](drivers/arista-eos.md)
§8). The switch runs Quiet. A PR for another model needs its `show system
environment cooling` under an override and a temperature watch.

**DHCP guarding (Rogue DHCP Server Detection) and rogue DHCP reporting
(2026-11-14), claimed.** One ingress IPv4 ACL per interface,
`DUI_IP_ACL_<interface>` with `counters per-entry`: the trusted servers'
replies pass, every other UDP 67 -> 68 is dropped and counted, and the count
is the port's `RogueDHCPDrops` (anomaly bit 64). The 7160 matches no VLAN in
an ACL, so a port is guarded only when every VLAN it carries is guarded
(default), or with `options.dhcp_guard: edge` every port but the uplink, the
gateway's port and switch-facing ports
([`drivers/arista-eos.md`](drivers/arista-eos.md) §9.2).

| What | Status |
|---|---|
| The lists on the two test ports, rendered by the driver, committed under a timer: active in the hardware, separate ACL ids and counters per list, the read-back and a `-collect-once` reporting 0 | **live**, 2026-11-14 (§9.1, §9.6) |
| The VLAN qualifier dropped by EOS (mask 0) and flagged "not fully supported"; one list on two ports sharing its counters; abort of a session pending its timer | **live**, 2026-11-14 (§9.1) |
| Plan, rewrite, removal, a foreign ACL left alone, modes, attribution (lanes summed, aggregate on its first member) | fixture-tested over the live captures (`acl_test.go`, `acl_counters_test.go`) |
| A rogue reply dropped and counted on the right port, a trusted one and ARP/IPv6 passing, the count through a collect, the controller's "Rogue DHCP server detected" alert | **live**, 2026-11-14, on the uplink, the lists written by the driver's own code (§9.8) |
| Binding the lists on every interface | **live**: one or two 50 ms pings lost through the uplink (§9.8); a 500-rule rewrite on the uplink loses about 200 ms (10 pings at 20 ms), none with `hardware access-list update default-result permit`, which fails open for that window instead and is not set by the driver (**live**, 2026-11-15, §9.7 item 1) |
| The controller's push of guard groups to this switch | **untested**: Rogue DHCP Server Detection stays off site-wide (Clint's decision); with it off the push is `switch.dhcp_snoop.status=disabled` and nothing is written |
| **IP ACL rules, claimed (`max_custom_ip_acls` 32) 2026-11-14**: the chip's ACL capacity measured on the test ports (one counter pool of 4095 for every list, two hash entries per counted rule, 20094 entries in five lists without an error, ACL ids 1-5 in bind order); the claim holds for every interface (72 with every cage split) carrying the guard and 32 rules, up to 22 trusted servers per port | **live** for the capacity (§9.1, `docs/fixtures/arista-eos-4.26.14M/acl-capacity/`); the rules themselves fixture- and replay-tested (`TestReplayAristaIPRulePushes`, the ECS's and the DX010's captured pushes) |
| A list the hardware cannot program (`Error in ACL commit`, a warning on the commit): the session aborted, the change held with the lists it wrote | the warning **captured live** through eAPI and over SSH, 2026-11-15 (1000 DSCP+TTL rules on test port 2, aborted; `acl-commit-warning/`); the abort and the hold fixture-tested (`acl_warning_test.go`); never met through the deployed bridge (within the claim the room is measured) |
| More lists than five at once (one per interface, up to 72) | **live**, 2026-11-14: 57 lists, 57 port ACL ids, every one active (§9.8); 72 (every cage split) not tried |
| The end-to-end IP rule round through the controller (rule scoped to the Arista, pushed, written, deleted, removed) and a rule matching traffic | **live**, 2026-11-14 (§9.8); the round found the session-name defect fixed on branch `claude/arista-finish-60f078` (each change its own session, §9.3) |
| A rule on a port carrying a client's traffic, a rewrite under that traffic | **untested**: needs a host on a test port |
| Which mode a site runs | the operator's choice; `exact` guards no port on a switch of all-VLAN trunks with any unguarded network (the deployment keeps `exact`: detection is off, §9.7) |

Not claimed: **MAC ACL rules** (no MAC ACL binds to a switchport on the 7160), and
**the DHCP rate limit** (anomaly bit 32: EOS 4.26.14M has no per-port DHCP
rate limit; a QoS policer is in bits per second and counts only conforming
packets here; a PR needs a policer's exceeded count shown rising under
traffic, §9.5).

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

## `sonic` — community SONiC switch (control verified live on the virtual switch, build 202505.1238524, with the exceptions below; the Celestica DX010 adopted on build 202505.1238526, 2026-10-31, retested in full on the patched 202511 image 2026-11-02 and running the bridge on the switch itself since then -- [`drivers/sonic-running.md`](drivers/sonic-running.md) maps the deployments and the image)

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
| **Live, the full retest on the Celestica DX010 on the custom 202511 image** (`igmp-202511.20261101-22cac574e`, 2026-11-02, driven from the UI through the deployed bridge, unattended, every change restored and the restore confirmed in the controller and in every affected device's push): port state and name; VLANs (native, Allow All on Default, Block All, Custom, the stand-in's limit logged for Allow All on another native); forced 40G/100G, RS-FEC on and off, auto; storm control 1 % broadcast + multicast and off; aggregation of two cages and its removal; per-port STP (Edge with BPDU guard, Infrastructure with BPDU guard on a down port, STP off, loop protection, back to auto) and the bridge priority through disunified-l2d; Locate; the device rename (`config hostname`, the first live run); the emulated upgrade; Rogue DHCP Server Detection with a tagged guarded network on a cage (the `V2_*` guard group with `VLAN_ID 2`) and one IP ACL rule, both removed in the designed order; Jumbo Frames on and off on all 32 ports, the 100G uplink included, with no link event; NTP, syslog, time zone and the four SSH keys read back equal to the push; Restart (the session dropped under `sudo reboot`, the reload logged as issued, SSH back in 78 s, the saved config, the static `eth0` and the in-band NTP source intact). Found and fixed on the captures the same night, then **deployed and re-run live the same day (14:25-14:56 UTC)**: FC-FEC the cage cannot run (skipped with a note, the speed applied, no re-push), storm control in Rate mode (one note, nothing written), **a port an ACL table binds joining a PortChannel** (the lanes leave the driver's table inside the batch, the PortChannel is bound after, and both are undone on removal) and the orphan PortChannel's removal. | [`drivers/sonic.md`](drivers/sonic.md) §11.8 |
| **Port mirroring on the DX010 on 202511**: on the stock 202511 orchagent the SAI reports none and the session is created without binding the source port, so the driver refuses it with a note (b26fb67); **on this repo's patched image it works** -- verified live 2026-11-02, cage 5 mirrored onto cage 4 from the controller, both directions in the chip's mirror table, removal clean ([`drivers/sonic.md`](drivers/sonic.md) §11.10). Breakout, refused on 202511 until 2026-11-07 by sonic-yang (eth0's gateway-less `MGMT_INTERFACE` row), works since the row was removed (next row). A PR that closes mirroring's gap: mirroring through an ACL mirror rule, or a SAI whose capability query says yes, verified with traffic on a destination port. | [`drivers/sonic.md`](drivers/sonic.md) §11.8 |
| **Breakout on the DX010 on 202511, every mode live** (2026-11-07, `igmp-202511.20261106-f2e25f54c`, driven by the controller's REST as the UI sends it, cage 2 empty, restored and a zero-change push after): eth0's `MGMT_INTERFACE` row removed (eth0 on DHCP, `metric 202`; hostcfgd needed a manual `interfaces-config` restart); 4x25G, **the lanes at 10G**, **2x50G** and the join, each checked in CONFIG/APPL/STATE_DB, mstpd and the STP tables (new lanes in every VLAN's instance, Ethernet0 untouched), the IGMP reserved groups (extended to new lanes, removed for gone ones), VLAN membership and the controller's port; **the `DUI_IP_ACL` rebind on a split and a join** (one IP rule scoped to the DX010: `-f` drops the parent from `ports@`, the driver rebinds with the children, `Active` throughout). Two fixes from the run, deployed (29e71ae) and verified live: a join pushed in the cycle after a split was withheld for good (the loop took the new layout for a port the controller knew nothing about); **1G on a split cage's lanes** (claimed from STATE_DB, refused by the plan before) | [`drivers/sonic.md`](drivers/sonic.md) §11.11; `replay_sonic_breakout_test.go`, `breakout_lanes_test.go` |
| The optic's fault flags (`sfp_rxfault` / `sfp_txfault`) from xcvrd's `TRANSCEIVER_STATUS_FLAG` (the module's per-channel loss of signal and transmit fault; [`drivers/sonic.md`](drivers/sonic.md) §10.10): the healthy state | `-collect-once` against the DX010 (2026-11-14, `igmp-202511.20261114-42cccc87a`): the uplink's 100G CWDM4 read present with no fault, no fault on any other cage; `optic_fault_test.go` on that capture (`collect-optic.txt`) |

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
| ~~A reboot whose SSH session drops before `sudo reboot` returns (DX010, 2026-11-01: logged as `reboot: context deadline exceeded`), now the reload issued (`TestRebootSessionDrops`, `TestSessionDropped`, the recorded Restart through the loop)~~ **verified 2026-11-02**: the UI's Restart logged "the connection dropped while `sudo reboot` ran (context deadline exceeded): the switch is going down" then "reboot: switch reload issued"; SSH back 78 s later with the saved config | done (§11.8) |
| Speed "auto" from the module (§10.9; `speed_test.go`, `hold_sonic_test.go` on the DX010's real first push with cage 1 at 40G) | the speed and breakout commands are verified by hand on the DX010 (40G and back, 4x25G and back, no restart) but the driver's own module-driven write never ran: a split or a forced speed on a cage holding a 100G module, set back to auto in the UI -- the logged join or speed, the link back; a breakout cable (CA-25G-S/N) in a cage; any module but the CWDM4 |
| The first-push hold without the MTU (§10.2; `hold_sonic_test.go` on the DX010's real held push) | on the DX010 the hold was released by setting 1500 by hand, so the fix never ran live: a newly adopted SONiC switch at 9100 with jumbo off -- its first push applied (1500 on every port), not held |
| `-device-ssh-key` / `DUI_DEVICE_SSH_KEY` (§10.5) | a `-collect-once` with `-device-ssh-key <key>.pub` against an agent holding several keys, the chosen one authenticating |
| LACP state on the DX010 (2026-11-01; held to the ECS's own tables and inform, never read from a community SONiC LAG) | the second core cable in and the aggregate up: `LAG_MEMBER_TABLE` filled by tlm_teamd on 202505, the head port's `lacp_state` both active with the core's system id as partner, the controller's record showing `lacp_state`/`partner_system_id`; and a member pulled reading `active: false` |
| The DX010's hardware tables in the controller (2026-11-01; deployed 2026-11-01; `mac_table_capability` **is dropped for `USWF07D` too**, checked on the stored record 2026-11-02) | ~~the controller keeping `mac_table_capability` for `USWF07D` (it drops it for USW models)~~, the PSU rows' input power, fan RPM and three temperatures and the drawers' `direction` in the UI, `total_max_power` as the summed draw (was the capacity), `ipv6`, `sfp_rev` on cage 1 |
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
| Dynamic port breakout: a link at each speed (every mode and the 10G/1G lane speeds are verified to configure, §11.11, on empty cages) | a 4x25G and a 2x50G breakout cable in a DX010 cage with a far end: the split from the UI, each lane up at its speed, the FEC the far end needs (`supported_fecs` is `none,fc` per 25G lane, `none,rs` per 50G lane) |
| An optic fault (§10.10): `sfp_rxfault` / `sfp_txfault` from a module's own `rx<k>los` / `tx<k>fault`, and a split cage's lanes by channel (`optic_fault_test.go`: the real healthy entry with one flag set, since no capture or upstream test data holds a `True`) | the one module the DX010 has is healthy and cannot be made to fault from outside. Needs a module with a dark receiver (an optic with no fibre, or the far end shut), its `TRANSCEIVER_STATUS_FLAG` captured with `rx<k>los` `True`, the inform's `sfp_rxfault` true and the controller's "Receive SFP Signal Loss" alert for that port; a transmit fault (no alert of that kind is captured yet) and a split module's lane order likewise |

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
`feature_caps` 128 and `max_custom_ip_acls` 128. **Verified live on the DX010
2026-11-01** (§11.7.7): the push (the IP count alone gates it; the five shapes
the ECS round lacked), the guard on the native VLAN dropping an untrusted
server's replies even under a matching ACL Allow, an IP rule written and
removed, 512+ entries per table. That run read the drop at the switch's
CPU, on 202505's SAI. On the 202511 legacy-TH SAI an ACL drop leaves the
traps' CPU copy alone, so a dropped reply addressed to the switch still
reaches its CPU (neither table type differs, traced 2026-11-15, §11.7.8).
There the chip's counters are the evidence: the guard's entry counted its
frames on the stand-in, and switched replies are dropped.

**Which port a rogue server is behind** (§11.7.9, built 2026-11-14): one
counted DROP per port (`IN_PORTS`) above the guard's shared DROP, in a table
type of the driver's own, with ACL counter polling on; each port reports the
sum as `Health.RogueDHCPDrops`, which sets `anomalies` bit 64 on growth.
**Verified by hand on the DX010 2026-11-14**: replies to the switch counted
only on the rule of the port they entered (a higher-priority rule for an
empty cage stayed at 0), a trusted server's replies uncounted, the shared
DROP still catching a port without its own rule, the type valid under
sonic-yang, 2557 entries available on it; and the branch's `-collect-once`
reported the drops on port 1 and 0 on the other 31. Fixture- and
replay-tested (`acl_count_test.go`, `TestReplaySonicRogueDHCPPerPort`: bit 64
on the right port only).

| Gap | What the PR needs |
|---|---|
| The rogue-DHCP alert through the deployed bridge | Rogue DHCP Server Detection on, an untrusted reply arriving on a DX010 port, and the controller's "Rogue DHCP server detected on <device> Port N" read from its device alerts for that port |
| A rogue reply on a port other than the uplink, and on an aggregate member (the table binds the PortChannel, the counted rule names the member) | a second linked port (and an aggregate): the count rising on that port's rule alone |
| A breakout or a join with counted rules in place (fixture-tested: the lanes' rules go before the breakout, the new lanes' come once STATE_DB has them) | the split and join of an empty cage with detection on: no stalled breakout, the new lanes' rules `Active` |
| Replacing a deployed `L3` guard with the counted one (fixture-tested) | a bridge update while detection is on: the old table, then the type, table and rules, all `Active` |
| More than 256 guard entries (counted rules dropped per VLAN with a note; fixture-tested) | many guarded VLANs on a split switch, CRM's entry counts on the guard table |
| A guard on a VLAN other than the native stand-in (the DX010 carries only VLAN 1) | a guarded tagged network on a DX010 port: the push's guard group, `VLAN_ID <v>` matched on tagged frames |
| MAC rules: a switched frame dropped (written, `Active` and removed live on the DX010 2026-11-06 on the image with the kit's sonic-swss 0003; the chip's entry counted exactly the blocked MAC's frames, but frames to the switch itself still reach its CPU through the traps' copy, as they do under any ACL drop on this SAI, the guard's included (traced 2026-11-15), and no second port had a link, §11.7.8) | two linked ports on a VLAN the rule names: frames from the blocked MAC not forwarded and a control MAC's forwarded, then the rule's removal |

### IGMP snooping (the custom image only)

The controller's per-VLAN snooping settings as `L2MC` rows for l2mcd, the
Micas series built into the repo's 202511 image (`contrib/sonic-image`), and
the seven reserved groups as static groups on every member of a snooping
VLAN ([`drivers/sonic.md`](drivers/sonic.md) §8.10, §8.14). Built 2026-11-03,
fixture- and replay-tested on the DX010's capture from the hardware test
(`igmp_test.go`, `informloop/replay_sonic_igmp_test.go`, the ECS's real
push); claims what the ECS claims (`feature_caps` 256 and
`IGMP_SNOOPING_ADVANCED`, `igmp_snoop_caps` 31). **Behind `options.igmp:
on`, off by default.** **The driver's path verified live on the DX010
2026-11-05** with it on (§8.14): the claim, the controller's push after a
force-provision (`system_cfg-dx010-igmp.txt`, replayed), l2mcd enabled,
VLAN 1's row and the reserved groups written, the chip holding them and
three LAN groups, quiet reconciles; with the series' l2mcd fixes (0008-0010, installed by hand) the uplink learned as the router port beside the gateway's IGMPv2 queries, the switch's group queries from the VLAN's address, and no per-packet lines to the log host. On the DX010 (§8.14 H2-H3, by hand): the IGMP traps,
the per-VLAN flag and a (*,G) entry in the chip's IPMC table verified;
optimised flood refused by the SAI, so `forward_unknown_multicast=false` is
logged and the VLAN keeps flooding unknown multicast. The querier address,
the router-port expiry and report suppression have no l2mcd setting and are
logged.

| Gap | What the PR needs |
|---|---|
| Traffic (§8.9 H4): a joined stream reaching only its port, the queries on the wire, a leave; IGMP of a VLAN without snooping still crossing the switch with the traps in place | two hosts cabled to the switch on a test VLAN, a source, `show interfaces counters` on the unjoined port; then `options.igmp` on by default |
| The fixed l2mcd (0008-0010) in an image: on the DX010 it is a binary installed by hand into the running l2mcd container (2026-11-05: Ethernet0 learned as the router port, the gateway the querier, group queries from the VLAN's address, no per-packet syslog), lost on a container recreate | the next image build with the series' l2mcd, installed, and the same checks after a reboot (`docs/drivers/sonic.md` §8.14) |
| The switch as querier from the VLAN's address (with no other querier on the VLAN) | a VLAN without a router's queries, a general query captured from the switch: never seen live, only in the harness (`query-src-*`) |
| A querier source address on a VLAN where the switch has none (0.0.0.0) | the querier-address field of §8.11 |
| **A port leaving its last VLAN, and sonic-swss 0004** (`drivers/sonic.md` §8.15): on the image without 0004 a native-VLAN move on 2026-11-14 stranded a test cage (bridge port refused, admin down, reused: no learning until a swss restart) after its L2MC count had leaked when the port, a group member and a router port at once, lost link; the uplink was exposed the same way. Without 0004 the driver waits for the bridge port (live on a down cage, the leak reproduced at count 3; the stranded-port report fixture-tested, `bridge_port_test.go`). **0004 installed on the DX010 2026-11-14 (`igmp-202511.20261114-42cccc87a`) and proven live:** the same loop and link drop on a test cage counts the router-port members (10) and returns to 0, and a native move and back through the controller runs with no wait, a new bridge port admin true each time, no refusal (`sonic-igmp-202511.20261114-42cccc87a/l2mc-0004-live-20261114.txt`). Not live: a move while the cage is looped (members programmed: the race) | the looped-cage move on the patched image (no cable needed), or a host cabled to a test port |
| Unknown multicast to the router ports only on a Tomahawk-1 | a SAI that accepts the wildcard L2MC entry or `SAI_VLAN_ATTR_UNKNOWN_IPV4_MCAST_OUTPUT_GROUP_ID`; the legacy-TH 13.2.1.x train does not |

### Port isolation, MAC filter, egress limit, LLDP-MED, Voice VLAN, router ports (2026-11-07)

Claimed once the collector reads the switch's side (PORT_ISOLATION,
PORT_SECURITY, EGRESS_RATE_LIMIT, LLDP_MED), each push captured from the UI
on the DX010's empty cages and replayed (`informloop/replay_sonic_portfeat_test.go`),
**written, read back from CONFIG_DB, APPL_DB, ASIC_DB and lldpd, and
removed live on the DX010** ([`drivers/sonic.md`](drivers/sonic.md) §11.12):
isolation as one SAI isolation group (APPL_DB, ProducerStateTable), the MAC
filter as learn mode drop plus static FDB entries in the port's VLANs, the
egress limit as a port shaper (`SCHEDULER` + `PORT_QOS_MAP`), LLDP-MED and
the Voice VLAN as lldpd's TIA TLVs per port, the Multicast Router Port as
`L2MC_MROUTER` (IGMP on). The group, the FDB entries and the TLVs do not
survive a swss or lldp restart; the next cycle's reconcile writes them
again. A breakout takes `learn_mode` (in no YANG model) out of CONFIG_DB
for its validation and puts it back.

| Gap | What the PR needs |
|---|---|
| Enforcement: an isolated port's frames kept from another isolated port, a non-listed source dropped on a secured port, the shaper's rate, an LLDP-MED phone taking the voice VLAN | two or more linked ports (hosts on rack day) |
| The router port in l2mcd (APPL_DB `L2MC_MROUTER_TABLE`): l2mcd lists a port only once it is oper up, and the cage used was empty | a linked port made a router port, its row in `L2MC_MROUTER_TABLE` |
| LLDP-MED topology notify | an SNMP trap path; not modelled |

### 802.1X port authentication (2026-11-15)

hostapd (wired driver, 2.11, built on the switch as mstpd is) under
disunified-l2d, one per controlled port; an unauthorised port held closed
with the MAC filter's `learn_mode drop`, each authenticated host admitted
with a static FDB entry and a kernel RETURN, EAPOL trapped with an
`always_enabled` CoPP row; failed attempts counted per port into
`Health.Dot1XFailures`, whose growth is anomaly 128. No image change
([`drivers/sonic.md`](drivers/sonic.md) §11.26). **The controller's push
captured on Network 11.0.99** from the UI on the DX010's empty cages (the
device's 802.1X Control, UniFi Integrated and External RADIUS, each of the
four methods, off), parsed and replayed
(`informloop/replay_sonic_dot1x_test.go`).

**Verified by hand on the DX010** (empty cage 31 in MAC loopback, a test
RADIUS server on the switch, a scripted EAP-MD5 supplicant with a test MAC):
the trap in ASIC_DB and EAPOL reaching the port's netdev; an accepted host
authorised and admitted (APPL_DB and ASIC_DB static FDB entry), a refused
one counted; l2d's restart adopting the running hostapd and its host; a
logoff removing the admission; the teardown. Fixture- and replay-tested:
the driver's claim, closed ports, l2d's config and the root-only secrets
file, the admitted hosts' FDB entries kept, the read-back.

**The driver's path verified live through the deployed build** (2026-11-15,
§11.26.7): from the UI, 802.1X Control on, each method on an empty cage,
External RADIUS, off; the cage closed and opened, hostapd built by the
driver's own unit, l2d's config, the trap and the chain, the secrets file
root-only, the read-back. Two fixes followed (the secrets file now goes
with 802.1X, a released port's STATE_DB mode is cleared), verified the same
evening through a test deb of the branch.

**MAC-based authentication (MAB)** built the same evening (§11.26.8):
l2d's own RADIUS request for a host that sends no EAPOL (hostapd's wired
driver has none), the MAC upper case without separators as user name and
password (the controller's own help text), the push's idle timeout
honoured. Proven by hand on the DX010 against hostapd's integrated RADIUS
server (an accept admitted, a reject counted, the idle timeout, the
teardown) and replayed on those answers (`TestL2dMAB`); through the driver
(a test deb) the push's idle timeout reaches l2d and the cage is trapped
behind its drop.

| Gap | What the PR needs |
|---|---|
| An 802.1X accept and reject through the driver-run hostapd, and MAB, against the gateway's RADIUS server | a RADIUS user on the gateway's server (a site-wide change), then a loop test as §11.26.8's in a VLAN no STP instance maps |
| What a UniFi switch sends in a MAB request beside the user name and password | a UniFi switch with a MAC Address port (a production change) |
| Unauthorized VLAN, RADIUS-assigned VLAN | the first reaches no push on 11.0.99; the second (`dynvlan`) is disabled in every capture; neither built |
| Forwarding: an admitted host's frames through the chip, an unadmitted one's dropped | two linked ports (rack day) |
| A cabled supplicant against the gateway's RADIUS server; the controller's 128 alert | a host on a linked port, the bridge deployed |
| 802.1X on an aggregate | not offered: an aggregate's members stay open |

### Layer 3 switching: routed networks, static routes, DHCP (2026-11-07)

Claimed wherever FRR runs (`FEATURE|bgp`): L3 routing with 64 interfaces and
256 static routes, the ECS's numbers. Every push captured from the UI on the
DX010 and replayed (`informloop/replay_sonic_l3_test.go`), **written, read
back and removed live** ([`drivers/sonic.md`](drivers/sonic.md) §11.13): a
routed network as the switch's address on its VLAN, the controller's
Inter-VLAN routing transit, switch static routes (next hop, interface,
black hole) as STATIC_ROUTE rows for FRR, DHCP Server mode as busybox udhcpd
in a transient unit (a client on the network leased an address), DHCP Relay
through the VLAN's relay list (a DISCOVER relayed), Isolate Network as
`DUI_IP_ACL` rules (Active). The transit's default route is written only
while the transit VLAN reaches a port with link, the management gateway
kept behind it at distance 200; the switch's own management address is
never the driver's.

**Routed ports and DHCP fixed IPs (2026-11-08).** Claimed with L3 routing
(`feature_caps2` 16): the port editor's Routing and Routing + Aggregating,
and a client's Fixed IP on a network the switch routes with DHCP Server
mode. Every push captured on the DX010 and replayed
(`informloop/replay_sonic_routed_test.go`), **verified live**
([`drivers/sonic.md`](drivers/sonic.md) §11.16). A routed port leaves every
VLAN and gets its address in one cycle, and returns to switching cleanly. A
routed aggregate is a PortChannel with the address and no VLAN. A fixed IP is
udhcpd's `static_lease`: each test MAC leased its address, and no other
client got it. Network 11.0.99 pushes a routed aggregate exactly as
10.6.109 did (re-captured 2026-11-15, `controller-11.0.99/system_cfg-dx010-routed-agg-*.txt`,
replayed by `informloop/replay_sonic_routed_net11_test.go`): the first
member alone, the others in no key.

| Gap | What the PR needs |
|---|---|
| The default route through the transit | VLAN 4040 (the Inter-VLAN routing network) allowed on the uplink at both ends; then the row `<mgmt>,<transit>` / `200,0` and FRR selecting the transit |
| Routed traffic, isolation dropping it, a relayed reply | a host on a routed network's port (rack day), and a DHCP server with a scope for the relayed network |
| Traffic over a routed port, its connected route (installed when the port comes up), OSPF or a static route over one | a cabled peer on a routed port (rack day) |
| A routed aggregate of two or more members | the controller (10.6.109 and 11.0.99) pushes only the aggregate's first member; a push naming the others, then a cabled peer |
| VRRP (`feature_caps` 268435456) | not a single switch's: UniFi offers it only as an MC-LAG group's L3 Automatic Failover, so it needs MC-LAG (a second switch) first |

### BGP (2026-11-07)

Claimed beside L3 routing (`feature_caps` 33554432): Settings -> Routing -> BGP
offers the switch as a Device, and the controller pushes the operator's
uploaded FRR file whole (`switch.bgp.1.raw_config_base64`). Every push
captured on the DX010 (made, off, changed, deleted) and replayed
(`informloop/replay_sonic_bgp_test.go`); **verified live** with an FRR peer
in a container on the switch ([`drivers/sonic.md`](drivers/sonic.md)
§11.14): the file loaded into the bgp container, the session Established,
the file's route-map filtering the peer's routes, a learnt route in the
kernel and APPL_DB, nothing written on the cycles after, the file loaded
again after bgpd lost it, replaced whole when changed, removed when the
router was deleted. Objects the switch runs on its own are never taken;
stanzas for anything but bgpd are left out with a note.

| Gap | What the PR needs |
|---|---|
| A real bgp container restart | `systemctl restart bgp` with a router loaded (simulated live by removing the objects) |
| A session over a front-panel port, a `vrf` router | a peer cabled to a port; a VRF on the switch |

### OSPF (2026-11-07)

Claimed beside L3 routing (`feature_caps2` 4): Settings -> Routing -> OSPF
offers the switch as a Device, its areas holding the networks the switch
routes. Every push captured on the DX010 (defaults, MD5, plain text, a
passive interface in a stub area, NSSA, deleted) and replayed
(`informloop/replay_sonic_ospf_test.go`); **verified live** with an FRR
peer on VLAN 3999 of the switch's kernel bridge
([`drivers/sonic.md`](drivers/sonic.md) §11.14): ospfd started by the
driver in the bgp container on the pushed router, the adjacency Full with
MD5, routes learnt both ways (the peer's into the kernel, APPL_DB and the
ASIC; the DX010's default and connected networks to the peer), nothing
written on the cycles after, ospfd started again after it was killed,
restarted on a change, stopped when the router was deleted. Refused with a
note under frrcfgd's mode, which owns ospfd.

| Gap | What the PR needs |
|---|---|
| An adjacency over a front-panel port; a stub or NSSA area with a peer | a peer cabled to a port |
| A real bgp container restart | `systemctl restart bgp` with the router running (simulated live by killing ospfd) |
| A plain-text key over 8 characters | FRR's limit; check what FRR does with the UI's longer key |

### Fan mode (2026-11-07)

The device's `fan_mode: default | quiet` (UniFi's Fan Mode panel exists
only for gateways on Network 10.6.109: the controller never stores a
switch's `sys_fan_mode`, checked live with the gateway's shape). On the
DX010 (`x86_64-cel_seastone-r0`, F2B airflow): Default is the stock
fancontrol floor (MINTEMP 26, PWM 89 = 35 %), Quiet patch 0012's (MINTEMP
35, PWM 51 = 20 %), written to `fancontrol-F2B`, `fancontrol` and pmon's
copy and read back from all three every cycle. **Verified live**
([`drivers/sonic.md`](drivers/sonic.md) §11.15): each mode written once and
zero writes after, fancontrol restarted in pmon, the fans at the new speed
(Default ~41 %, Quiet 20 %, all OK), a bridge restart writing nothing, the
ASIC 58 -> 53.75 °C at Default and back up 49.6 -> 54.1 °C over 9 min at
Quiet, still rising toward the bench's 56-58 (high 70). Fixture- and loop-tested
(`fan_test.go`, `replay_sonic_fan_test.go`). A B2F unit and other platforms
offer nothing; a PR for one needs its fancontrol table and a measured floor
(the ASIC's settled temperature at it).

### DHCP packet rate (anomaly 32) -- counted at the CPU (2026-11-14)

UniFi raises "Incoming DHCP packets exceed 300/sec" on a port; in its model
the port keeps running. On 202505 and 202511 `PORT dhcp_rate_limit` is only a
field (sonic-swss#3130 unmerged), so disunified-l2d counts each ingress
port's DHCP client requests at the CPU, puts the `dhcp_relay` trap in a CoPP
group of its own at 600 packets/s while it does (the image's group caps the
whole switch at 100/s), and reports the requests above 300 in any one second
as `DHCPRateDrops` ([`drivers/sonic.md`](drivers/sonic.md) §11.17).
Verified live on the DX010: the cap at 100/s with the image's group, 400/s
seen and 397 over the threshold with the driver's, everything restored;
fixtures `collect-dhcprate.txt`, `dhcp-frames-20261114.txt`. **Through the
deployed build 2026-11-15** (Network 11.0.99): the same burst raised the
controller's `PORT_DHCP_STORM` alert for port 1 on the first inform after it.
A detection with no port hold, and no flood test on another port or CPU
reading at 600/s planned (Clint, 2026-11-14).

| Gap | What the PR needs |
|---|---|
| What a UniFi switch counts | the ECS (4.0.1) shows no detector and its DHCP CoPP rate (256) is below the threshold; a capture of a real UniFi switch raising the alert, to compare with |

### Port anomalies the switch cannot report yet (2026-11-14)

Assessed live on the DX010 (read-only; `docs/fixtures/sonic-igmp-202511.20261106-f2e25f54c/anomaly-probes.txt`,
[`drivers/sonic.md`](drivers/sonic.md) §11.18). No bit below is set: the
switch has no source that means exactly the condition.

| Bit | Why not | What the PR needs |
|---|---|---|
| 2048 PHY failure | no PHY between the ASIC and the cages; the ASIC's per-port MAC local/remote fault (`PORT_OPERR_TABLE`) is a link-down reason, counted on the working uplink's ordinary drops. The ECS reports nothing for it either (no external PHY described, no cause, no `anomalies` key; read-only 2026-11-14) | an inform from a real UniFi switch reporting 2048 and what had failed on it; then a DX010 signal for a port the ASIC cannot bring up whatever the far end does |
| 1024 DoS | the ECS's `denial-of-service` error-disable cause is a configuration row with nothing observable behind it (no detector in its databases, chip or CPU path; no push key; no UI setting; no `anomalies` in its inform), so there is no condition to copy; the Tomahawk's DoS header checks (`DosAttack*`, off but SA=DA) could be counted per port through a free receive debug counter, outside SAI | an inform from a real UniFi switch reporting 1024 and the traffic that set it; then the matching checks turned on, counted per port into `Port.Health` and set on growth, verified with a cabled host ([`drivers/sonic.md`](drivers/sonic.md) §11.18.4) |

### IPv6 on the management interface (2026-11-14)

SLAAC on the interface the bridge manages the switch at, while the
controller runs IPv6 on the management network (`rest/networkconf`
`ipv6_interface_type`; the push has no IPv6 key): `accept_ra` 2 and an
APPL_DB `INTF_TABLE` row per SLAAC address for its ip2me route
([`drivers/sonic.md`](drivers/sonic.md) §11.22). Fixture-tested on four
DX010 captures (`ipv6_test.go`), the loop and the REST read on the
controller's own networkconf (`informloop/ipv6_test.go`,
`unifiapi/ipv6_test.go`).

| Gap | What the PR needs |
|---|---|
| Off through the driver (the rows, `accept_ra` 1, the address and the RA's routes removed; run by hand on the DX010 only) | the management network's IPv6 Interface Type set to None in the controller on a site where that is safe, the teardown in the bridge's log, `ip -6 addr`/`route` and APPL_DB `INTF_TABLE` read back, a zero-change reconcile after |
| A prefix change (a new delegated prefix: the old row goes, a row for the new address comes) | the gateway's delegated prefix changing under a running bridge, or a second prefix advertised on the management network |
| DHCPv6 on the management network, a static IPv6 address in a switch's IP Settings | not built: no capture of either |

### Refused (logged, not claimed)

IGMP snooping on an image without l2mcd or with `options.igmp` off,
MAC ACL
rules on an image without the kit's sonic-swss 0003 (no L2 table type in the stock orchagent); an IP ACL rule shape
no capture shows, or a set that would block the bridge's own SSH to the
switch (the rules of that kind are held); breakout on a platform without `platform.json`
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

## `stub` — a device described by a file, and `children:`

An instrument, not a device ([`drivers/stub.md`](drivers/stub.md)), and the
loop's reported child (`children:` on any device, [`install.md`](install.md) §3).

| What | Verified | How |
|---|---|---|
| `stub` adopted as `USFXG` from a file (4x25G lanes + a 100G uplink, then 2x50G, then one port at 75000), re-read every cycle, nothing written for any push | live, Network 10.6.109, 2026-11-02 | `docs/breakout-lag.md` §6.6-6.7; `internal/drivers/stub/stub_test.go` over `docs/fixtures/stub/` |
| `children:` placing a separately bridged device on a parent's port: both entries, LLDP only, MAC only (each places), neither (the placement is kept) | live on the `podman` driver as the parent, port down, 2026-11-02 | `docs/fixtures/controller-10.6.109/rest-device-flexxg-placement.json`; `internal/informloop/children_test.go` over the DX010 capture |
| The controller's push for a UniFi aggregate on the stub's lanes | captured | `docs/fixtures/controller-10.6.109/system_cfg-flexxg-aggregate.txt` |

Untested — PRs welcome: `children:` with an `arista-eos` or `sonic`
parent (the loop does the same for every driver; the proof is one
placement in the parent's record), and a child on a port that is up with
its own LLDP neighbour (the child is then reported in the MAC table only;
whether the controller places by that alone next to another neighbour is
the open question).

## `breakout` — a split cage as its own switch (the lane switch)

A split cage of a bridged switch presented as a five-port switch of its own
(`USFXG`: ports 1-4 the lanes, 5 the cable), reading and writing through its
parent driver ([`drivers/breakout.md`](drivers/breakout.md)). Built
2026-11-14 with `sonic` as the parent; `arista-eos` became the second
parent on 2026-11-15 (§7b). **No lane switch has been adopted**; the
controller side it relies on was proven with the stub (above). On the
Arista the lane batches themselves were run on the switch's empty test
cage by hand (below).

### Verified (fixtures and replay)

| What | How |
|---|---|
| The child's snapshot from the DX010's real 4x25G capture (lanes with their aliases, supported speeds and media, port 5 the uplink with the parent as its neighbour), and at 2x50G after a re-split through the parent (two lanes, two empty slots) | `internal/drivers/breakout/breakout_test.go`; `internal/drivers/sonic/lanes_test.go` |
| The child's inform held to the real switches' (wire contract, every-port types, value shapes) | `internal/device/contract_breakout_test.go`, the `breakout` row of `contractPayloads` |
| The controller's Flex XG aggregate push replayed through a child loop onto the parent: `PortChannel201` on lanes 2-4, the FEC note, both loops' next cycle writing nothing; the join removing it before the breakout; the first-push hold | `internal/informloop/replay_breakout_test.go` |
| The parent's plan on an owned cage: only the breakout and the admin state; a lane speed the child forced not undone; an unowned lane aggregate left alone | `lanes_test.go` |
| A lane aggregate without lane 1 takes the cage's STP settings in l2d (§14.4): live on the DX010 by hand (a probe built from the branch, mstpd `admin edge`/`bpdu guard` yes on `PortChannel2901`) | `TestLaneAggregateTakesTheCageSTP`; `docs/drivers/sonic.md` §11.25.1 |
| A join with a lane aggregate the DUI ACL tables bind (§14.5): the tables rebound without the PortChannel before `portchannel del`; the order before the fix refused live by sonic-yang, the fixed order run live by hand | `TestLaneJoinUnbindsTheACLTables`, `fixture_breakout.go`'s leafref check; `sonic-igmp-202511.20261115-6c8eb1cf7/portchannel-del-acl-20261115.txt` |
| **`arista-eos` as the parent**, on the 7160's empty test cage captured split on 2026-11-15 (`fixtures/arista-eos-4.26.14M/split-cage54*/`: split; lanes 3-4 in channel group 1541 with a lane mirror; the Flex XG batch's result; the first try's errdisabled lanes): `Lanes(54)` and the captured cage 50, a lane's state/name/VLAN/storm, the refused lane speed, a lane-to-lane mirror (session 1543), the aggregate and its undo, the owned cage (admin state and speed only, nothing counted by `PlanPorts`, the disabled cage holding every lane down), the join dissolving the lane groups first, a lane group left alone with no child attached, the write lock; the Flex XG push through a child loop (`channel-group 1541` on Ethernet54/2-4, `Port-Channel1541` Allow All) with both loops' next cycle writing nothing against the switch as that batch left it, and the join | `internal/drivers/arista-eos/lanes_test.go`; `internal/informloop/replay_arista_breakout_test.go` |
| The Arista's lane batches accepted by EOS 4.26.14M: the split, the Flex XG aggregate batch (with RS on the 25G lanes), a lane mirror session, and the join's `no monitor session` / `no channel-group` / `speed forced 100gfull`, each in a commit-timer session on the empty cage, aborted after | live 2026-11-15, by hand ([`drivers/arista-eos.md`](drivers/arista-eos.md) §10) |

### Built, untested live — PRs welcome

| Gap | What the PR needs |
|---|---|
| Adoption and placement of a real lane switch on a `sonic` parent | the steps of [`drivers/breakout.md`](drivers/breakout.md) §15 on a DX010 (or any SONiC switch with `platform.json` breakout modes): the child's record placed under the cage (`uplink_remote_port`), the parent's `downlink_lldp_macs`, both records scrubbed into `docs/fixtures/controller-<ver>/` |
| Adoption and placement of a lane switch on the `arista-eos` parent | [`drivers/breakout.md`](drivers/breakout.md) §11b: a child entry in the container's `config.yaml` on a split cage, adopted by exact MAC, placed under the cage; then the lane settings with a far end (the test cage has no module and no link, so no lane has come up, no LACP partner, no lane MAC or neighbour has been seen, and whether the 7160 runs a lane mirror beside the device's own session is not known) |
| A lane aggregate to a real LACP partner | a breakout DAC to a bonded host: `teamdctl PortChannel<100*cage+1> state` (on the Arista `show port-channel 1541 detailed` / `show lacp neighbor` for `Port-Channel<1000+10*cage+1>`) with the partner, the child's `lacp_state`/`partner_system_id` in its stored record, traffic on the members; the push and the reply log scrubbed into fixtures |
| A join with an aggregate present through a deployed bridge with a lane switch attached | the join's batch from the journal (the hand run of 2026-11-15 replayed its commands, sonic.md §11.25.2), the tables `Active` after, part of §15 |
| A forced lane speed (one lane at 10G beside 25G lanes) | the lane up at 10G with its neighbour, the other lanes unaffected; whether the Tomahawk accepts mixed lane speeds in one cage is not known |
| The stored-speed quirk after a re-split (§14.1) | the child's `rest/device` `port_overrides` after the aggregate push (the speed keys, captured), then a rule that clears a lane's forced speed when the mode changes |
| The disconnect seen once after the aggregate push (§14.2) | the child's record across the push on a real parent: connected throughout, or the same `MISSED_INFORM` with the reply log showing the informs answered |

### Not built — PRs welcome

| Gap | What the PR needs |
|---|---|
| STP port settings, isolation, port security, egress limit, LLDP-MED on the lane switch (today the cage's, on every lane) | the switch-wide computations taking both loops' intent (`l2SetPorts` in `sonic/l2.go`, `portFeatures`); the claims then on the child |
| A lane switch that appears by itself on a split (§12) | the loop adding and removing devices at run time, and the bridge forgetting a retired child |

---

## Every driver — per-port anomaly fields

`port_table[].anomalies` is what the switch itself detected, and the
controller raises an alert for each bit it sets (`docs/feature-map.md` §2).
Until 2026-11-14 the bridge filled it from the controller's own
`custom_anomalies` table, so a Proxmox node's bond leg that bounced was
reported as a rogue DHCP server (64) and the SONiC switch's topology
changes as a DHCP packet storm (32), seen live in the controller's device
alerts on 2026-11-13.

| What | Where / when |
|---|---|
| `anomalies` is 0, or 512 for a port held by loop protection, or a storm/DHCP-rate/rogue-DHCP or MAC-locking bit on growth of the port's drop counters (`MACLockingDrops` filled by `sonic` since 2026-11-14, `drivers/sonic.md` §11.18.2; `RogueDHCPDrops` filled by `sonic` and `proxmox` since 2026-11-14, `drivers/sonic.md` §11.7.9; `StormDrops*` by `arista-eos`, row below, and broadcast/multicast by `sonic`'s storm detection, `drivers/sonic.md` §11.21; `DHCPRateDrops` by `sonic` since 2026-11-14, `drivers/sonic.md` §11.17); nothing else sets a bit | `internal/device/anomalies_test.go` (the regression: a flap, STP changes, optic alarms, growing errors, drops and FEC on the Arista's capture all report 0), `contract_sonic_stp_test.go` (512 on the DX010's live capture) |
| Storm drops per type from the Arista (`show storm-control` `drop`), split cages summed | `internal/drivers/arista-eos/features_test.go` over the 4.26.14M capture with all three types; read live (counts 0) on 2026-11-14 |
| `sonic`: MAC locking (bit 256): a secured port in `cpu_trap` behind the kernel drop, each frame from an unlisted source counted per interface (`MACLockingDrops`), the port left up | by hand on the DX010 2026-11-14, an empty cage in MAC loopback (`maclock-live.txt`, `collect-maclock.txt`, [`drivers/sonic.md`](drivers/sonic.md) §11.18.2); `l2d_maclock_test.go`. Untested: a host on a cabled port, a secured PortChannel, the controller's 256 alert |
| `custom_anomalies` not sent; the controller computes it | `internal/device/contract_test.go` `portOmissions`, with the fixtures' evidence |
| Raw fields the controller derives its own alerts from: `link_down_count`, `sfp_found`, `sfp_rxfault`, `sfp_txfault` (`sfp_rxfault` raises "Receive SFP Signal Loss", `SFP_RECEIVED_FAULT`: `controller-11.0.99/system-log-device-alert-sfp-rx.json`; filled by `arista-eos` from its alarm thresholds and by `sonic` from the module's own flags since 2026-11-14, [`drivers/sonic.md`](drivers/sonic.md) §10.10; the others send false) | sent as before; the contract tests, `optic_fault_test.go` |
| `stp_state_change_count` (`[{change_count, mst: 0}]`, every port) from `Port.Health.STPChanges` (`arista-eos`, `proxmox`, `sonic`; 0 on the others): the controller's STP-flap alert carries it as its `COUNT` | captured 2026-11-14 on Network 11.0.99 from the only switch on the site that raised the alert, a USW Aggregation (its inform and the alerts, `fixtures/controller-11.0.99/`); no real inform carries `stp_state_change_sum`; `internal/device/stp_changes_test.go`, and the inform is a contract reference ([`feature-map.md`](feature-map.md) §2) |
| The controller's link-flap alert on a bridged port: from `link_down_count`'s growth between two informs | **seen live on the DX010 2026-11-15** (an empty cage flapped by the chip's MAC loopback: raised above 0.32 drops/s, not at 0.24; `fixtures/controller-11.0.99/system-log-device-alert-bridged.json`, `alert-rates-20261115.json`, `internal/device/alert_rates_test.go`; [`drivers/sonic.md`](drivers/sonic.md) §11.23) |
| The controller's STP-flap alert on a bridged port: from `stp_state_change_count`'s growth between two informs | **seen live on a stub switch 2026-11-15** (raised at 0.27 changes/s, not at 0.19; same fixtures). On `sonic` the count is disunified-l2d's count of the port's kernel state changes since 2026-11-15, 2 per drop on a looped cage (`l2d_state_changes_test.go` over the DX010's own journal); deployed 2026-11-15, and **the DX010's own STP-flap alert seen** the same night (cage 18 flapped at 0.15 drops/s: 0.30 changes/s, no link-flap alert; `system-log-device-alert-dx010-deployed.json`, [`drivers/sonic.md`](drivers/sonic.md) §11.23.4) |
| `error_disabled` + `error_disabled_reason` (1 loop protection, 2 BPDU guard) on a port the device's protection shut down: the controller's loop-protection and BPDU-guard alerts | **each alert seen live on a stub port 2026-11-15**; the DX010's own BPDU guard (a looped cage, `collect-bpduguard.txt`) raised nothing without the fields; `contract_sonic_stp_test.go`, `anomalies_test.go`. **Deployed 2026-11-15; the BPDU-guard alert seen from the DX010's own driver** (cage 17 looped with BPDU guard on, §11.23.4). Loop protection on the DX010 not exercised: it needs two forwarding looped cages in a VLAN of their own, which the bridge's Vlan4094 membership rules out while it runs (§11.23.4). On `arista-eos` and `proxmox` the faults are mapped but never seen live |
| `arista-eos`: `RogueDHCPDrops` (bit 64) from the guard's per-port ACL counter | read live 2026-11-14 at 0; a count above zero and the controller's alert are untested ([`drivers/arista-eos.md`](drivers/arista-eos.md) §9.6) |

Open — PRs welcome:

| Gap | What the PR needs |
|---|---|
| The STP-flap alert on `arista-eos` | EOS keeps a port's STP change count per link-up (absent while down, from 0 after); the driver holds it since 2026-11-15 so it never falls ([`drivers/arista-eos.md`](drivers/arista-eos.md) §4). A PR needs a capture of an interface going down and up again in STP (`show spanning-tree topology status detail` before, while down, after) -- the 7160's test ports have no link and no MAC loopback -- and the alert seen on the Arista's record |
| The controller's SFP alerts on a bridged port, and link flap on the other drivers (`link_down_count` is the driver's link changes halved: exact on `sonic` and on `arista-eos`, `proxmox` and `podman` since 2026-11-15, [`feature-map.md`](feature-map.md) §2) | an optic fault on a bridged port and the alert read from the controller's device alerts; the rate rule itself is known (above) |
| BPDU guard taking the chip's port down on `sonic` | mstpd shuts the netdev and StpOrch blocks the port, but SONiC's oper status stays up (a cabled peer would see link) and the port is reported `up` ([`drivers/sonic.md`](drivers/sonic.md) §11.23); a PR needs the chip's port taken down for the hold and brought back by the operator's re-enable, live on a looped cage |
| Storm bits (1/2/4) seen raising an alert | the Arista fills the counters and the DX010 its broadcast/multicast detection (above); a storm on a test port and the alert read from the controller |
| Storm drops on the SONiC switch | **no drop count exists on the DX010's Broadcom image** ([`drivers/sonic.md`](drivers/sonic.md) §11.19): orchagent polls no policer statistics, and the Tomahawk's storm meters have no counter. Detected instead since 2026-11-14 (next row). For a true drop count a PR needs a per-type count the chip really keeps (on a Broadcom XGS chip, three of the SDK's receive debug counters, one per storm type, if its drop triggers can be told apart -- `bcmDbgCntRxStormControlDrop` is one trigger for all three), or a SAI that answers `get_policer_stats` for a storm-control policer, plus orchagent polling it; then a capture and a live storm |
| Storm detection on the SONiC switch (bits 1, 2) | **verified by hand on the DX010 2026-11-14** ([`drivers/sonic.md`](drivers/sonic.md) §11.21): disunified-l2d compares each storm-controlled port's receive counters (broadcast; multicast, known groups included) over 5 s with the limit in 64-byte frames and counts the frames above it; a MAC-looped empty cage flooded from the CPU above a 1000 kbps limit was found and counted, a flood under it was not, the uplink's traffic never reads as a storm (`storm_watch_test.go`, `contract_sonic_storm_test.go`). **Not exercised:** a cabled source on a forwarding port, the UI's storm control with the detection running, the controller's alert (not deployed), split cages and PortChannel members. Unknown unicast (bit 4): next row. A PR needs a cabled storm on a test port in its own VLAN and the alert read from the controller |
| Unknown-unicast storm detection on the SONiC switch (bit 4) | **built 2026-11-14** ([`drivers/sonic.md`](drivers/sonic.md) §11.24): no counter separates flooded unicast, the SAI refuses the ACL field for a missed FDB lookup and answers storm-policer statistics with zeros (all probed on the DX010), so disunified-l2d samples each storm-controlled port with sFlow and counts the samples the switch flooded against the limit in bytes. Needs an image whose SAI can sample: on the legacy-TH SAI, the image kit's sonic-buildimage 0014 (`linux-bcm-genl`); without it nothing is reported and the driver says so once. **Verified by hand on the DX010 2026-11-15** on `igmp-202511.20261115-6c8eb1cf7`: a MAC-looped empty cage alone in its own VLAN, flooded from the CPU at twice a 1000 kbps limit, was found 5 s in and counted (15 745 frames above it); a flood under it, a known destination, the switch's MAC and broadcast were not; the worktree driver reported it as the port's `StormDropsUnicast` (`storm_ucast_test.go`, `contract_sonic_storm_test.go`). **Through the deployed build 2026-11-15:** an unknown-unicast level set and removed in the UI on an empty cage started and stopped the sampling (`FEATURE|sflow`, the session rows and l2d's marker came and went; replayed in `TestReplaySonicDX010Network11`). **Not exercised:** a cabled source, a forwarding port with other members (the test port was alone in its VLAN), a storm against a UI-set level, the controller's alert (needs a storm), split cages and PortChannel members, a sampling rate high enough to meet the CoPP cap. A PR needs a cabled storm on a test port and the alert read from the controller |
| DHCP rate (32) and rogue DHCP (64) bits (32 on `sonic`: built, counted live on the DX010's uplink, the alert raised through the deployed build 2026-11-15; 64 on `sonic`: built, the count verified by hand, the alert not yet seen; on `proxmox`: built, namespace-tested, no push seen yet) | the plumbing is built (`Port.Health` drop counters, growth-based, `anomalies_test.go`); a driver that fills `DHCPRateDrops` or `RogueDHCPDrops` from the device's own drop counters, with a capture of them and a live alert |
| Any other `anomalies` bit | a driver that really detects the condition and a capture or live alert showing the bit's meaning; for `sonic`, 128, 1024 and 2048 are assessed in [`drivers/sonic.md`](drivers/sonic.md) §11.18 (256 is built, §11.18.2) |

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
