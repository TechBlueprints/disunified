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
(unless `manage_lldpd: "false"`) and, only with `control.ntp`,
`/etc/chrony/sources.d/disunified.sources`. It never edits
`/etc/network/interfaces`; when the site's VLANs are outside the bridge's
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
- **NetworkManager only for `control.address`.** Reading works without it
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

---

## `sonic` — community SONiC switch (control verified live on the virtual switch, build 202505.1238524, with the exceptions below; hardware pending)

Write-up and specification: [`drivers/sonic.md`](drivers/sonic.md). Fixture: [`fixtures/sonic-202505.1238524-ff3c0c9f6/`](fixtures/sonic-202505.1238524-ff3c0c9f6).

### Verified

| What | Where / when |
|---|---|
| SONiC **202505.1238524-ff3c0c9f6**, the **virtual switch** (KVM image) as hardware SKU `Seastone-DX010`, L2 preset: 32 ports, one linked | `-collect-once` against the lab guest, 2026-10-27 |
| Read: ports from CONFIG_DB `PORT` (index, alias, admin state, speed, MTU), oper state and flap count from APPL_DB, SAI counters, VLAN membership, the FDB as the MAC table, identity and health, eth0 as the OOB port | fixture + `internal/drivers/sonic/collector_test.go` + `internal/device/contract_sonic_test.go` (as `USWF07D`) |
| Adoption sequence, read-only (the ECS Core's recorded replies drive the real driver; nothing is written) | `internal/informloop/replay_sonic_test.go` |
| Port control against the capture: state, name, speed, FEC, jumbo, VLANs with the native mapping, LAG, mirror, storm control; STP port settings and breakout refused on the VS | `internal/drivers/sonic/control_test.go` (a FixtureRunner that applies each write to the capture's CONFIG_DB and refuses what SONiC refuses) + `internal/informloop/replay_sonic_control_test.go` (two recorded controller pushes through the loop) |
| Device control against the capture: NTP/syslog/SNMP with bridge-owned entries only, STP (PVST, feature-gated), SSH keys in a marked block, reboot, the static address (dial-proven, undone when the new address does not answer) | `internal/drivers/sonic/device_test.go` + `TestReplaySonicDeviceLevel` (a recorded device-level push through the loop) |
| The lab session's own replies with control on (`replies-sonic.ndjson`, 274 records: every push from the seeded one to the address moves, the restarts and the site's remote logging switched on, each step's exact commands, a zero-change reconcile on every noop in between), and the jumbo flip both ways with an aggregate present on the session's aggregate push | `internal/informloop/replay_sonic_live_test.go` |
| §7.1 features against the capture: the pushed time zone mapped onto a zone (the switch's zone list captured as `zones.txt`), the hostname (`control.hostname`), locate (the LED toggled, stopped, the VS's failure logged once), breakout split/lane-speed/join on the DX010's `platform.json` (`dx010-platform.json`, `dx010-hwsku.json` beside the capture, upstream files) | `internal/drivers/sonic/features_test.go` + `internal/informloop/replay_sonic_locate_test.go` (the recorded set-locate/unset-locate through the loop) + the time zone in `TestReplaySonicPort2NameSpeedVLANs` |
| **Live, from the controller UI** on the VS guest (build 1238524, adopted as `USWF07D`, `control.ports: 2-32`): port state and name; Allow All, Block All, a custom tagged set; aggregation (finished by hand after the MTU refusal) and its removal; mirroring and its removal; RS-FEC and storm control (the forced 100G was already the cage speed); jumbo off and on (1500 and the pushed 9216 on 31 ports); NTP servers (hostnames pinned); the controller's SSH keys in the managed authorized_keys block (the operator's lines untouched); Manage → Restart (`sudo reboot`, back in ~2 min, reconnected); IP Settings → Static, a move within the subnet with the runner and the persisted dial target following. Each push reached the switch within one inform cycle and the next reconcile reported zero changes. | [`drivers/sonic.md`](drivers/sonic.md) §6.6, 2026-10-27. The live runs found the PortChannel MTU rule, the apply budget, the NTP churn, the `_type: reboot` reply, `promote_secondaries`, the agent key order and the stale reported address; each is fixed and tested since. |

### Built, untested live — PRs welcome

| Gap | What the PR needs |
|---|---|
| Jumbo flipped with a LAG present (the PortChannel's MTU moves first) | the flip on the guest with an aggregate, the commands logged, the zero-change reconcile |
| Syslog host changed or removed from the UI (adding one is verified live, 2026-10-27) | the host changed and then remote logging switched off from Settings -> System, the `del`/`add` logged, the entry gone from `SYSLOG_SERVER`; an operator's own entry left alone |
| SNMP community | **needs the site-wide SNMP setting on** (Settings -> CyberSecure): without it the controller pushes no `switch.snmp.*` at all (enabling SNMP on the device only stores location/contact on the device record). A capture of that push, and of any location/contact keys in it, to map `config snmp location/contact` |
| The reported address following a move (`ip`, `connect_request_ip`) | the controller's device list showing the new address one inform after a move, without a bridge restart |
| A move whose new address does not answer (the new address and route removed again) | a move to an address the bridge cannot reach |
| A gateway change in IP Settings (`config route add` then the old route's `del`) | a static push with a new gateway, the route read back, the zero-change reconcile |
| A forced speed change (live, 100G was pushed on a cage already at 100G, so no speed command ran) | a forced lower speed the cage runs without a breakout (40G on a 1x100G[40G] cage, hardware) |
| The driver's own aggregate creation (live it stopped at the PortChannel MTU refusal and was finished by hand; the MTU step is fixed and replayed since) | an aggregate built from the UI by the current driver, the commands logged, the zero-change reconcile |
| Time zone (`system.timezone`, a POSIX rule, mapped onto the switch's zone with that rule: `config clock timezone MST7MDT`) | the push applied on the guest, `show clock` in the new zone, the zero-change reconcile; an operator's zone with the same rule (America/Denver) left alone |
| Hostname (`control.hostname`: `config hostname <resolv.host.1.name>`, `config save`) | the flag on, the hostname changed on the guest (prompt, `DEVICE_METADATA`), the bridge's log prefix following on the next collect |

### Hardware only (the DX010) — PRs welcome

Fans, PSUs and temperatures (pmon), optics and DOM (xcvrd), real speeds and
FEC, `supported_speeds`, PCS/FEC codeword counters, LLDP neighbours from
real cables. The parser reads each table when present; none has seen real
data.

Built and fixture-tested against the DX010's upstream `platform.json`, never
run on hardware:

| Gap | What the PR needs |
|---|---|
| Dynamic port breakout (split 1x100G → 4x25G / 2x50G, lane speed 25G → 10G, join back) | on an empty cage with a 4x25G DAC: each step from the UI, the logged commands, the children in `show interfaces status`, the cage's VLANs/MTU/state on every child, the zero-change reconcile; whether the breakout prompts for tables without YANG (stdin is `/dev/null`, so it would abort the batch) |
| Locate (the status LED toggled off/green once a second through the platform API, green after) | the LED blinking from the UI's Locate and green after; whether pmon's system-health daemon fights the blink |

### STP

Feature-gated: the driver claims STP, BPDU guard and port cost only when
`FEATURE|stp` exists. Community images ship without the STP daemon
(`INCLUDE_STP=y` is off in the build), so on build 1238524 every STP setting
is refused with a log and not claimed; untested with the daemon.

### Refused (logged, not claimed)

IGMP snooping, port isolation, 802.1X, LLDP-MED off, DHCP snooping (community
SONiC has none of them); breakout on a platform without `platform.json`
(the VS), a speed no breakout mode runs, and a breakout of a cage in an
aggregate or mirror session; a pushed time zone no zone on the switch
matches; a hostname that is not a DNS name of at most 63 characters; STP
and the per-port STP settings on an image without the STP daemon.

### Untested — expected to work — PRs welcome

| Gap | Why it should work | What the PR needs |
|---|---|---|
| **Real hardware** (the DX010 first) | the same Redis tables and `config` CLI; pmon fills `FAN_INFO`/`PSU_INFO`/`TEMPERATURE_INFO`, xcvrd fills `TRANSCEIVER_INFO`/`_DOM_SENSOR`, and the parser reads them when present | a capture from the unit as `docs/fixtures/sonic-<build-id>/` (`scripts/sanitize-sonic.py`), the collector test over it, `-collect-once` in the PR |
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
4. **Run it live** against your controller, read-only first
   (`control.ports: off`), then one unused port. Record in the PR what you
   saw: the `-collect-once` snapshot, the adoption log lines, what the UniFi
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
