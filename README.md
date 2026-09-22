# disunified

A bridge that makes a non-UniFi device appear as a real, adopted UniFi
device inside the UniFi Network controller — ports, stats, topology, and
control. What it supports today is switches and a rack PDU; the inform,
adoption and control machinery underneath carries no vendor or device-type
assumptions.

The controller only speaks its own device protocol. This bridge speaks the
device side of it (the inform protocol, adoption, capabilities) on behalf of
hardware it reads and configures through the vendor's API. UniFi becomes the
place that device is managed from.

## Supported devices

Everything below is verified live on UniFi Network 10.6 (a UniFi OS gateway),
end to end from the UniFi UI — read (ports or outlets, stats, topology) and
control (the controller's config applied back to the device).

| Device | Driver | Presented to UniFi as | Verified on |
|---|---|---|---|
| Arista EOS switch | `arista-eos` | `UDC48X6` ("USW Leaf") | DCS-7160-48TC6-F, EOS 4.26.14M |
| Proxmox VE node (`vmbr0`) | `proxmox` | `UDC48X6` ("USW Leaf"), or `USWF07D` ("ECS Core") | three-node PVE 9.1 cluster |
| APC switched rack PDU | `apc-pdu` | `USPPDUP` ("Smart Power PDU Pro") | AP7931, NMC AOS 3.9.2 |

`disunified -list-drivers` prints what a given build supports. Adding another
device means adding a driver: the neutral model, the wire protocol, adoption
and the inform loop carry no vendor assumptions. The model a driver claims
has to be one the bundled catalogue carries as `type: usw` — which is what
`-list-models` prints, and which is how UniFi files its own power devices as
well as its switches. See
[**Working on this repo with an AI agent**](#working-on-this-repo-with-an-ai-agent)
and [`docs/adding-a-device.md`](docs/adding-a-device.md).

What each driver does today:

- **`arista-eos`** — a physical Arista switch read over eAPI (or SSH).
  Adoption, live port stats and counters, LLDP and MAC-table topology
  (clients appear behind the right port), optics, FEC, fans/PSUs; and control
  of port enable, names, speed, VLAN membership (plus VLAN creation), FEC,
  storm control, BPDU guard, STP mode/priority, IGMP snooping, and QSFP
  breakout split/join. EOS 4.26.14M is the last train for the 7160
  ([`docs/drivers/arista-eos.md`](docs/drivers/arista-eos.md)).
- **`proxmox`** — a Proxmox VE node's virtual switch, `vmbr0`: one port per
  guest NIC, numbered from 1 and the same on every node (the number lives in
  the guest's own Proxmox tags, `unifi.p25.c`), the node's physical NICs and
  bond members as the top ports counting down from 54. VM clients appear
  behind their ports in the topology, and control of port state and VLANs is
  written back as the guest's `tag`/`trunks`. The node's own networking
  underneath is reported, never reconfigured (apart from an optional,
  modelled-but-untested LACP conversion). Only cluster-wide numbering
  (`numbering: cluster`, the default) has been tested live
  ([`docs/drivers/proxmox.md`](docs/drivers/proxmox.md)).
- **`apc-pdu`** — an APC switched rack PDU, whose outlets become outlets the
  controller can see, name and switch. The card has no API, so the driver uses
  two transports: SNMPv1 to read identity and outlet state and to switch an
  outlet, and a partial `config.ini` uploaded over FTP to set an outlet's name
  (the name objects are read-only over SNMP). Writes need an SNMP community of
  access type `Write+`, and `control.outlets` — the power-device equivalent of
  `control.ports` — is off by default, because an outlet carries real load
  ([`docs/drivers/apc-pdu.md`](docs/drivers/apc-pdu.md)).

## Status: experimental, no warranty

This is experimental, hobby software from one home user. Read this before
running it against anything you care about.

- **No warranty of any kind.** It is provided "as is", without warranty of
  any kind, express or implied, and without any liability for damages or
  losses arising from its use (see [`LICENSE`](LICENSE)). If it misconfigures your
  device, takes down your network, or locks you out, that is on you.
- **It writes to your device.** With `control` enabled it applies whatever
  the controller pushes: it replaces the device's VLAN configuration,
  disables and re-enables ports, changes speeds and breakouts, STP, LACP,
  storm control, NTP and syslog, and reboots the device when the controller
  asks. A wrong click in the UniFi UI, a controller bug, or a bug here can
  cut off the device, the hosts behind it, or the bridge itself.
- **Verified on very little hardware:** the three drivers above, against UniFi
  Network 10.6 on a UniFi OS gateway. Any other device, OS version or
  controller version is untested. Several features are marked as modelled but
  never verified live in [`docs/feature-map.md`](docs/feature-map.md).
- **It speaks an undocumented protocol** reverse-engineered by the
  unifi-emu project and by watching real devices. A controller update can
  change it and break this bridge silently, or make the controller push
  something it never pushed before.
- **Start safe:** run `-collect-once` and then monitoring only; back up the
  device's running config; keep an out-of-band way into it; turn on `control`
  for one unused port before `ports: all` (or one outlet before
  `outlets: all`); use it only on controllers and devices you own or are
  authorized to administer.

## How it works

```
UniFi controller  <── inform (TNBU/AES-GCM, every ~70 s) ──  disunified  <── eAPI/SSH/SNMP ──  the device
                  ── system_cfg pushes / adoption ──>                         ── config diffs ──>
```

- [`internal/devicemodel`](internal/devicemodel) — the vendor-neutral model of a device and the
  driver contract.
- `internal/drivers/<driver>` — one driver per vendor/OS (`arista-eos`, `proxmox`, `apc-pdu`), each with its own [`CLAUDE.md`](CLAUDE.md) of working notes; its write-up is `docs/drivers/<driver>.md`, its captures `docs/fixtures/<driver>-<version>/`, its scrub script `scripts/sanitize-<driver>.py`, and its wire-contract and replay cases `internal/device/contract_<driver>_test.go` and `internal/informloop/replay_<driver>_test.go`.
- [`internal/device`](internal/device) — the inform session (forked from unifi-emu), payload,
  capability claims, persisted adoption state.
- [`internal/unificfg`](internal/unificfg) — parses the controller's `system_cfg` pushes.
- [`internal/informloop`](internal/informloop) — collect → inform → apply/reconcile, every cycle.
- [`internal/unifimodel`](internal/unifimodel) — picks the UniFi model to claim from the port layout, or the
  power-device model from the outlets.
- [`internal/unifiapi`](internal/unifiapi) — controller REST API, used only to name the device and
  its ports after the device itself on first provision.

## Install

[`docs/install.md`](docs/install.md) — about 20 minutes: get it, `-collect-once` to check the
device, one small YAML config ([`deploy/config.example.yaml`](deploy/config.example.yaml)) with secrets in
the environment, adopt in the UI, then turn on control. Or hand the whole
thing to an agent — see [**Installing with an AI agent**](#installing-with-an-ai-agent).

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
export DUI_DEVICE_USER=stu DUI_DEVICE_PASS=...
./disunified -collect-once -device-url https://192.0.2.3/command-api   # prints the snapshot + suggested model
./disunified -config config.yaml                                       # bridges every device in the file
```

The config lists devices under `devices:` (the pre-rename `switches:` key is
still accepted). Identity (MAC, serial, hostname, uplink), the port layout,
the port speed capabilities and the UniFi model to claim all come from the
device. The adopted key lives under `state/`; keep it, one bridge per device.
Every controller reply is recorded under `inform-log/`.

> The current environment-variable prefix is `DUI_` (`DUI_DEVICE_USER`,
> `DUI_UNIFI_API_KEY`, …) and the current flags are `-device-url` /
> `-device-ssh`. The pre-rename `STU_*` names and `-switch-url` / `-switch-ssh`
> flags are still read, so an existing deployment keeps working unchanged.

## Installing with an AI agent

The whole install is written to be handed to a coding agent (Claude Code was
used to build and to deploy this project). Paste one of these and fill in the
angle-bracketed parts:

1. *"Install disunified on the Podman (or Docker) host I have running at
   `<host>`, bridging my `<vendor>` device at `<address>` to the UniFi
   controller at `<controller>`. Use [`docs/install.md`](docs/install.md); start read-only
   (`control.ports: off`), and stop before turning on `control` so I can check
   the device in the UniFi UI first."*
2. *"My `<vendor>` device is showing in the UniFi controller under Pending
   Adoption through disunified. Walk me through adoption and then turn on
   control for one unused port only (port `<N>`), following
   [`docs/install.md`](docs/install.md) steps 4–5, and confirm the round-trip reaches the
   device before enabling `ports: all`."*
3. *"disunified is deployed on `<host>` and the UniFi controller was upgraded
   to Network `<version>`. Check the reply log under `inform-log/`, confirm the
   device is still adopted and reporting, and tell me if anything regressed."*

## Working on this repo with an AI agent

The repo is written to be driven by a coding agent as well. Prompts that work,
to paste as they are and fill in:

1. *"Add a driver for a `<vendor/OS>` device using its API documentation
   at `<URL>`, following [`docs/adding-a-device.md`](docs/adding-a-device.md). Capture and scrub the
   fixtures from my device at `<address>` first, write the tests against
   them, then integrate it with my UniFi controller at `<controller>` and
   verify it end to end. Test only on port `<N>`, which is unused."*
2. *"My `<vendor>` device is adopted through disunified but the UniFi
   UI's `<setting>` has no effect on it. Read the reply log under
   `inform-log/` and [`docs/feature-map.md`](docs/feature-map.md), find out whether the
   controller pushed it and what the driver did with it, and fix or
   document it."*
3. *"Open a pull request against `TechBlueprints/disunified` adding
   my `<vendor>` driver. Follow [`CONTRIBUTING.md`](CONTRIBUTING.md): file the issue first,
   include the scrubbed fixtures and the tests, run
   `scripts/check-site-info.sh`, and write the PR description as the
   reproducible verification script the maintainer can run."*
4. *"My UniFi controller was upgraded to Network `<version>`. Capture a
   fresh support-bundle inform from a real device and the new controller
   replies, refresh the fixtures under `docs/fixtures/controller-<version>/`,
   and tell me which wire keys changed."*

### For an AI agent adding a new device

Read, in this order: [`CLAUDE.md`](CLAUDE.md) (repo map and the rules that came from
live failures), [`docs/adding-a-device.md`](docs/adding-a-device.md) (the checklist, with the proof
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
equipment who wanted to manage more of their own hardware from the same
controller. It is not affiliated with, endorsed by, or supported by Ubiquiti
Inc. "UniFi" and "Ubiquiti" are trademarks of Ubiquiti Inc., used here only to
describe what the software talks to. The device protocol it speaks comes from
the open-source unifi-emu library (see [`docs/prior-art.md`](docs/prior-art.md)); use it on
controllers you own, at your own risk and with no warranty of any kind.

This work exists strictly for integration: it lets a device you already
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
