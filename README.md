# disunified

A bridge that makes a non-UniFi switch appear as a real, adopted UniFi
switch inside the UniFi Network controller — ports, stats, topology, and
control. The initial release ships two drivers: **Arista EOS** (a physical
switch) and **Proxmox VE** (a node's virtual switch, `vmbr0`, whose guests
become the ports).

The controller only speaks its own device protocol. This bridge speaks the
device side of it (the inform protocol, adoption, capabilities) on behalf of
a switch it reads and configures through the vendor's API. UniFi becomes the
place the switch is managed from.

Verified live on Network 10.6.106 with an Arista DCS-7160-48TC6-F (EOS
4.26.14M) claimed as a UniFi Data Center 100G-48X6: adoption, live port
stats and counters, LLDP and MAC-table topology (clients appear behind the
right port), optics, FEC, fans/PSUs; and control of port enable, names,
speed, VLAN membership (plus VLAN creation), FEC, storm control, BPDU guard,
STP mode/priority, IGMP snooping, and QSFP breakout split/join — all
end-to-end from the UniFi UI.

Also verified on the same controller: each node of a three-node **Proxmox
VE 9.1** cluster as a 54-port USW Leaf (`UDC48X6`; the 32-port ECS Core
profile, `USWF07D`, was verified first and still works with `ports: "32"`).
The switch is the
node's virtual switch, `vmbr0`: one port per guest NIC, numbered from 1
and the same on every node (the number lives in the guest's own Proxmox
tags, `unifi.p25.c`), free slots drawn as empty, disabled cages, and the
node's physical NICs and bond members as the top ports counting down from
54. VM clients appear behind their ports in the topology, the node sits
behind the UniFi aggregation switch via lldpd on its active uplink, and
control of port state and VLANs is written back as the guest's
`tag`/`trunks` ([`docs/drivers/proxmox.md`](docs/drivers/proxmox.md)). The node's own networking
underneath (NICs, bond, failover) is reported, never reconfigured, apart
from an optional LACP conversion that is modelled and untested. The bridge
holds no state beyond the adopted key. **Only cluster-wide numbering
(`numbering: cluster`, the default) has been tested; per-node numbering
exists but has not been run live.**

## Status: experimental, no warranty

This is experimental, hobby software from one home user. Read this before
running it against anything you care about.

- **No warranty of any kind.** It is provided "as is", without warranty of
  any kind, express or implied, and without any liability for damages or
  losses arising from its use (see [`LICENSE`](LICENSE)). If it misconfigures your
  switch, takes down your network, or locks you out, that is on you.
- **It writes to your switch.** With `control` enabled it applies whatever
  the controller pushes: it replaces the switch's VLAN configuration,
  disables and re-enables ports, changes speeds and breakouts, STP, LACP,
  storm control, NTP and syslog, and reboots the switch when the controller
  asks. A wrong click in the UniFi UI, a controller bug, or a bug here can
  cut off the switch, the hosts behind it, or the bridge itself.
- **Verified on very little hardware:** one Arista DCS-7160-48TC6-F on EOS
  4.26.14M and one Proxmox VE 9.1 cluster, against UniFi Network 10.6 on a
  UniFi OS gateway. Any other switch, OS version or controller version is
  untested. Several features are marked as modelled but never verified live
  in [`docs/feature-map.md`](docs/feature-map.md).
- **It speaks an undocumented protocol** reverse-engineered by the
  unifi-emu project and by watching real devices. A controller update can
  change it and break this bridge silently, or make the controller push
  something it never pushed before.
- **Start safe:** run `-collect-once` and then monitoring only; back up the
  switch's running config; keep an out-of-band way into the switch; turn
  on `control` for one unused port before `ports: all`; use it only on
  controllers and switches you own or are authorized to administer.

## How it works

```
UniFi controller  <── inform (TNBU/AES-GCM, every ~70 s) ──  disunified  <── eAPI/SSH ──  the switch
                  ── system_cfg pushes / adoption ──>                         ── config diffs ──>
```

- [`internal/switchmodel`](internal/switchmodel) — the vendor-neutral model of a switch and the
  driver contract.
- `internal/drivers/<driver>` — one driver per vendor/OS (`arista-eos`, `proxmox`), each with its own [`CLAUDE.md`](CLAUDE.md) of working notes; its write-up is `docs/drivers/<driver>.md`, its captures `docs/fixtures/<driver>-<version>/`, its scrub script `scripts/sanitize-<driver>.py`, and its wire-contract and replay cases `internal/device/contract_<driver>_test.go` and `internal/informloop/replay_<driver>_test.go`.
- [`internal/device`](internal/device) — the inform session (forked from unifi-emu), payload,
  capability claims, persisted adoption state.
- [`internal/unificfg`](internal/unificfg) — parses the controller's `system_cfg` pushes.
- [`internal/informloop`](internal/informloop) — collect → inform → apply/reconcile, every cycle.
- [`internal/unifimodel`](internal/unifimodel) — picks the UniFi model to claim from the port layout.
- [`internal/unifiapi`](internal/unifiapi) — controller REST API, used only to name the device and
  its ports after the switch on first provision.

## Install

[`docs/install.md`](docs/install.md) — about 20 minutes: get it, `-collect-once` to check the
switch, one small YAML config ([`deploy/config.example.yaml`](deploy/config.example.yaml)) with secrets in
the environment, adopt in the UI, then turn on control.

Every release publishes a multi-arch image (linux/amd64, linux/arm64), so the
install is a directory with [`deploy/compose.yaml`](deploy/compose.yaml), your `config.yaml` and your
`env` — nothing to build:

```sh
docker pull ghcr.io/techblueprints/disunified:latest   # :v1.2.3 to pin, :edge to follow main
docker compose up -d                                       # deploy/compose.yaml; or the Quadlet unit in deploy/
```

From a checkout instead (also how you add a driver; Go 1.26+):

```sh
go build ./cmd/disunified
export STU_SWITCH_USER=stu STU_SWITCH_PASS=...
./disunified -collect-once -switch-url https://192.0.2.3/command-api   # prints the snapshot + suggested model
./disunified -config config.yaml                                       # bridges every switch in the file
```

Identity (MAC, serial, hostname, uplink), the port layout, the port speed
capabilities and the UniFi model to claim all come from the switch. The
adopted key lives under `state/`; keep it, one bridge per switch. Every
controller reply is recorded under `inform-log/`.

## Working on this repo with an AI agent

The repo is written to be driven by a coding agent (Claude Code was used
for all of it). Prompts that work, to paste as they are and fill in:

1. *"Install disunified on the Podman (or Docker) host I have running
   at `<host>`, bridging my `<vendor>` switch at `<address>` to the UniFi
   controller at `<controller>`. Use [`docs/install.md`](docs/install.md); start read-only,
   and stop before turning on `control` so I can check the device in the
   UniFi UI first."*
2. *"Add a driver for a `<vendor/OS>` switch using its API documentation
   at `<URL>`, following [`docs/adding-a-switch.md`](docs/adding-a-switch.md). Capture and scrub the
   fixtures from my switch at `<address>` first, write the tests against
   them, then integrate it with my UniFi controller at `<controller>` and
   verify it end to end. Test only on port `<N>`, which is unused."*
3. *"My `<vendor>` switch is adopted through disunified but the UniFi
   UI's `<setting>` has no effect on it. Read the reply log under
   `inform-log/` and [`docs/feature-map.md`](docs/feature-map.md), find out whether the
   controller pushed it and what the driver did with it, and fix or
   document it."*
4. *"Open a pull request against `TechBlueprints/disunified` adding
   my `<vendor>` driver. Follow [`CONTRIBUTING.md`](CONTRIBUTING.md): file the issue first,
   include the scrubbed fixtures and the tests, run
   `scripts/check-site-info.sh`, and write the PR description as the
   reproducible verification script the maintainer can run."*
5. *"My UniFi controller was upgraded to Network `<version>`. Capture a
   fresh support-bundle inform from a real switch and the new controller
   replies, refresh the fixtures under `docs/fixtures/controller-<version>/`,
   and tell me which wire keys changed."*

### For an AI agent adding a new switch

Read, in this order: [`CLAUDE.md`](CLAUDE.md) (repo map and the rules that came from
live failures), [`docs/adding-a-switch.md`](docs/adding-a-switch.md) (the checklist, with the proof
each step needs), [`internal/drivers/CLAUDE.md`](internal/drivers/CLAUDE.md) (rules for the vendor
layer), [`docs/unifi-models.md`](docs/unifi-models.md) (which UniFi model to claim and how
the catalogue is refreshed), and [`docs/feature-map.md`](docs/feature-map.md) (every protocol
feature and its status). Then follow [`CONTRIBUTING.md`](CONTRIBUTING.md): issue first, PR
against it, fixtures captured and scrubbed before code, tests against the
fixtures, and end-to-end verification on a real controller with a browser
automation tool driving the UniFi UI. The Arista EOS driver is the
reference; copy its shape, not its commands.

## Not affiliated with Ubiquiti

This is an independent, unofficial project by a home user and fan of UniFi
equipment who wanted to manage more switches from the same controller. It
is not affiliated with, endorsed by, or supported by Ubiquiti Inc. "UniFi"
and "Ubiquiti" are trademarks of Ubiquiti Inc., used here only to describe
what the software talks to. The device protocol it speaks comes from the
open-source unifi-emu library (see [`docs/prior-art.md`](docs/prior-art.md)); use it on
controllers you own, at your own risk and with no warranty of any kind.

This work exists strictly for integration: it lets a switch you already
own be managed from a UniFi Network controller that you are authorized to
use and that runs on genuine Ubiquiti hardware. It does not replace, clone
or bypass any Ubiquiti product, licence or service, and it is not intended
to be run against a controller you do not administer.

If Ubiquiti sees this repository and has any concerns, they are welcome to
contact me: open an issue on this repository, message
[@cgoudie](https://github.com/cgoudie) on GitHub, or reach me on Discord
(user ID `360473903417786371`; I am on the official Ubiquiti Discord server).

## License and credit

MIT — see [`LICENSE`](LICENSE) (attribution and trademark notes in [`NOTICE`](NOTICE)). The inform wire format, crypto and model catalogue come
from [jamesbraid/unifi-emu](https://github.com/jamesbraid/unifi-emu) (MIT),
whose protocol documentation made this possible; [`internal/device`](internal/device) is a fork
of its inform session.
