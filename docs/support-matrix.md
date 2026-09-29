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
